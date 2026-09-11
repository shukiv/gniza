package tui

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/shukiv/gniza/internal/answer"
)

// api is what the model reads and posts through: the client, or a fake
// in a test.
type api interface {
	Page(path string) (answer.Page, error)
	Post(path string, form url.Values) (Outcome, error)
}

// screen is one of the pages the plugins have.
type screen int

const (
	screenOverview screen = iota
	screenDestinations
	screenSchedules
	screenAccounts
	screenLogs
	screenRestore
	screenSettings
	screenCount
)

var screenNames = [screenCount]string{
	"Overview", "Destinations", "Schedules", "Accounts", "Logs", "Restore", "Settings",
}

// mode is what the keys mean right now.
type mode int

const (
	modeList mode = iota
	modeForm
	modeConfirm
	modeReveal
)

// refreshEvery is how often the screen being looked at is read again.
// A backup that was just asked for shows up as running within it.
const refreshEvery = 5 * time.Second

// Model is the whole interface: one screen showing, the others kept as
// they were last read.
type Model struct {
	api    api
	screen screen
	width  int
	height int

	pages  [screenCount]answer.Page
	loaded [screenCount]bool
	cursor [screenCount]int
	logTab string

	mode    mode
	form    *form
	confirm *confirmation
	reveal  *revealed

	// flash is the last message a form came back with, and err the last
	// thing that failed. Both are shown at the foot until the next.
	flash *answer.Flash
	err   string
	busy  string
	// refresh is how often the screen is read again on its own; zero
	// never, which is what a test wants.
	refresh time.Duration
}

// confirmation is a question with one irreversible answer.
type confirmation struct {
	question string
	warning  string
	path     string
	form     url.Values
	intent   intent
}

// revealed is a recovery key being shown, on this draw and no other.
type revealed struct {
	card         recoveryCard
	repositoryID string
	justCreated  bool
}

type recoveryCard struct {
	Destination   string
	Repository    string
	URI           string
	Password      string
	Hostname      string
	ResticOptions string
}

// intent is what a post was for, which decides what its outcome means.
type intent int

const (
	intentAct    intent = iota // show the message, read the screen again
	intentSubmit               // a form: refused, revealed, or done
)

type loadedMsg struct {
	screen screen
	page   answer.Page
	err    error
}

type postedMsg struct {
	intent  intent
	outcome Outcome
	err     error
}

type tickMsg time.Time

// New makes the interface over an api.
func New(client api) Model {
	return Model{api: client, width: 100, height: 30, logTab: "backups", refresh: refreshEvery}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.load(screenOverview), m.tick())
}

func (m Model) tick() tea.Cmd {
	if m.refresh == 0 {
		return nil
	}
	return tea.Tick(m.refresh, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m Model) load(which screen) tea.Cmd {
	path := m.pathOf(which)
	return func() tea.Msg {
		page, err := m.api.Page(path)
		return loadedMsg{screen: which, page: page, err: err}
	}
}

func (m Model) pathOf(which screen) string {
	switch which {
	case screenDestinations:
		return "/destinations"
	case screenSchedules:
		return "/schedule"
	case screenAccounts:
		return "/accounts"
	case screenLogs:
		return "/logs?tab=" + m.logTab
	case screenRestore:
		return "/restore"
	case screenSettings:
		return "/settings"
	}
	return "/"
}

func (m Model) post(path string, form url.Values, what intent) tea.Cmd {
	return func() tea.Msg {
		outcome, err := m.api.Post(path, form)
		return postedMsg{intent: what, outcome: outcome, err: err}
	}
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tickMsg:
		if m.mode == modeList {
			return m, tea.Batch(m.load(m.screen), m.tick())
		}
		return m, m.tick()
	case loadedMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
			return m, nil
		}
		m.err = ""
		m.pages[msg.screen] = msg.page
		m.loaded[msg.screen] = true
		if msg.page.Flash != nil {
			m.flash = msg.page.Flash
		}
		m.clampCursor(msg.screen)
		return m, nil
	case postedMsg:
		return m.posted(msg)
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m Model) key(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return m, tea.Quit
	}
	switch m.mode {
	case modeForm:
		return m.formKey(msg)
	case modeConfirm:
		return m.confirmKey(msg)
	case modeReveal:
		return m.revealKey(msg)
	}
	switch key := msg.String(); key {
	case "q":
		return m, tea.Quit
	case "1", "2", "3", "4", "5", "6", "7":
		return m.show(screen(key[0] - '1'))
	case "tab", "right", "l":
		return m.show((m.screen + 1) % screenCount)
	case "shift+tab", "left", "h":
		return m.show((m.screen + screenCount - 1) % screenCount)
	case "r":
		m.flash, m.err = nil, ""
		return m, m.load(m.screen)
	case "j", "down":
		m.cursor[m.screen]++
		m.clampCursor(m.screen)
		return m, nil
	case "k", "up":
		m.cursor[m.screen]--
		m.clampCursor(m.screen)
		return m, nil
	}
	return m.screenKey(msg.String())
}

func (m Model) show(which screen) (tea.Model, tea.Cmd) {
	m.screen = which
	m.flash, m.err = nil, ""
	return m, m.load(which)
}

func (m *Model) clampCursor(which screen) {
	rows := m.rowCount(which)
	if m.cursor[which] >= rows {
		m.cursor[which] = rows - 1
	}
	if m.cursor[which] < 0 {
		m.cursor[which] = 0
	}
}

// posted decides what a form's outcome means.
func (m Model) posted(msg postedMsg) (tea.Model, tea.Cmd) {
	m.busy = ""
	if msg.err != nil {
		if m.mode == modeForm && m.form != nil {
			m.form.err = msg.err.Error()
		} else {
			m.err = msg.err.Error()
		}
		return m, nil
	}
	if msg.outcome.Page == nil {
		m.flash = &answer.Flash{Kind: msg.outcome.Kind, Message: msg.outcome.Message}
		m.mode, m.form, m.confirm = modeList, nil, nil
		return m, m.load(m.screen)
	}

	// The handler drew a page instead of redirecting. Which page says
	// what happened.
	page := msg.outcome.Page
	var drawn struct {
		FormError       string
		Revealed        *recoveryCard
		RevealedID      string
		JustCreated     bool
		ConfirmHost     string
		HostFingerprint string
		HostKeyType     string
	}
	_ = json.Unmarshal(page.Data, &drawn)
	switch {
	case drawn.Revealed != nil:
		m.reveal = &revealed{card: *drawn.Revealed, repositoryID: drawn.RevealedID, justCreated: drawn.JustCreated}
		m.mode, m.form, m.confirm = modeReveal, nil, nil
		m.rememberPage(*page)
		return m, nil
	case drawn.ConfirmHost != "" && m.form != nil:
		// Nothing has been sent to that server yet. The form is still
		// here, so agreeing sends it again with the fingerprint.
		resend := m.form.values()
		resend.Set("confirm_fingerprint", drawn.HostFingerprint)
		m.confirm = &confirmation{
			question: fmt.Sprintf("%s answered with a %s key, fingerprint %s.",
				drawn.ConfirmHost, drawn.HostKeyType, drawn.HostFingerprint),
			warning: "Check it against the server before agreeing: everything sent " +
				"there afterwards trusts this key.",
			path: m.form.path, form: resend, intent: intentSubmit,
		}
		m.mode = modeConfirm
		return m, nil
	case drawn.FormError != "":
		if m.form != nil {
			m.form.err = drawn.FormError
			m.mode = modeForm
		} else {
			m.err = drawn.FormError
		}
		return m, nil
	}
	m.rememberPage(*page)
	if page.Flash != nil {
		m.flash = page.Flash
	}
	m.mode, m.form, m.confirm = modeList, nil, nil
	return m, m.load(m.screen)
}

// rememberPage keeps a page a post answered with, when it is one of the
// screens, so the list behind a card is current without another read.
func (m *Model) rememberPage(page answer.Page) {
	for which := screen(0); which < screenCount; which++ {
		if navOf(which) == page.Nav {
			m.pages[which] = page
			m.loaded[which] = true
		}
	}
}

func navOf(which screen) string {
	switch which {
	case screenOverview:
		return "dashboard"
	case screenSchedules:
		return "schedule"
	}
	return strings.ToLower(screenNames[which])
}

func (m Model) confirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		c := m.confirm
		m.busy = "Working…"
		return m, m.post(c.path, c.form, c.intent)
	case "n", "N", "esc", "q":
		m.confirm = nil
		if m.form != nil {
			m.mode = modeForm
		} else {
			m.mode = modeList
		}
		return m, nil
	}
	return m, nil
}

func (m Model) revealKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "n":
		id := m.reveal.repositoryID
		m.reveal = nil
		m.mode = modeList
		m.busy = "Noting…"
		return m, m.post("/destinations/recovery/note", url.Values{"repository": {id}}, intentAct)
	case "esc", "q":
		m.reveal = nil
		m.mode = modeList
		return m, m.load(m.screen)
	}
	return m, nil
}

// --- time in words ---

func when(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "never"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func ago(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "never"
	}
	return since(time.Since(*t)) + " ago"
}

func since(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}
