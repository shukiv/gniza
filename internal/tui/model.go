package tui

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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

// --- drawing ---

var (
	styleTitle  = lipgloss.NewStyle().Bold(true)
	styleActive = lipgloss.NewStyle().Bold(true).Reverse(true)
	styleDim    = lipgloss.NewStyle().Faint(true)
	styleOK     = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleWarn   = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styleBad    = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true)
	styleCursor = lipgloss.NewStyle().Reverse(true)
)

func (m Model) View() string {
	var b strings.Builder
	b.WriteString(m.header())
	b.WriteString("\n\n")
	switch m.mode {
	case modeForm:
		b.WriteString(m.form.view(m.width))
	case modeConfirm:
		b.WriteString(m.confirmView())
	case modeReveal:
		b.WriteString(m.revealView())
	default:
		b.WriteString(m.screenView())
	}
	b.WriteString("\n\n")
	b.WriteString(m.footer())
	return b.String()
}

func (m Model) header() string {
	page := m.pages[m.screen]
	var tabs []string
	for which := screen(0); which < screenCount; which++ {
		label := fmt.Sprintf(" %d %s ", which+1, screenNames[which])
		if which == m.screen {
			tabs = append(tabs, styleActive.Render(label))
		} else {
			tabs = append(tabs, label)
		}
	}
	title := "Gniza"
	if page.Panel != "" {
		title += " · " + page.Panel
	}
	if page.Version != "" {
		title += " · " + page.Version
	}
	line := styleTitle.Render(title)
	if page.Update != nil {
		line += "  " + styleWarn.Render("update available: "+page.Update.Version)
	}
	return line + "\n" + strings.Join(tabs, "")
}

func (m Model) footer() string {
	var lines []string
	for _, work := range m.pages[m.screen].Running {
		line := work.Doing + " " + work.Account
		if work.Detail != "" {
			line += " · " + work.Detail
		}
		if work.Known {
			line += fmt.Sprintf(" · %.0f%%", work.Percent)
		}
		lines = append(lines, styleWarn.Render(line))
	}
	if m.busy != "" {
		lines = append(lines, styleDim.Render(m.busy))
	}
	if m.err != "" {
		lines = append(lines, styleBad.Render(m.err))
	}
	if m.flash != nil {
		style := styleOK
		switch m.flash.Kind {
		case "warn":
			style = styleWarn
		case "error":
			style = styleBad
		}
		lines = append(lines, style.Render(m.flash.Message))
	}
	lines = append(lines, styleDim.Render(m.keysHelp()))
	return strings.Join(lines, "\n")
}

func (m Model) keysHelp() string {
	switch m.mode {
	case modeForm:
		return "tab/shift+tab move · space toggles or cycles · enter submits on the last field · ctrl+s submits · esc cancels"
	case modeConfirm:
		return "y yes · n no"
	case modeReveal:
		return "n I have written it down · esc back"
	}
	common := "1-7 screens · j/k move · r reread · q quit"
	if keys := m.screenKeys(); keys != "" {
		return keys + " · " + common
	}
	return common
}

func (m Model) confirmView() string {
	c := m.confirm
	out := c.question + "\n"
	if c.warning != "" {
		out += "\n" + styleWarn.Render(c.warning) + "\n"
	}
	return out + "\nProceed? (y/n)"
}

func (m Model) revealView() string {
	card := m.reveal.card
	var b strings.Builder
	if m.reveal.justCreated {
		b.WriteString(styleTitle.Render("The destination is ready. This is its recovery key.") + "\n\n")
	} else {
		b.WriteString(styleTitle.Render("Recovery key") + "\n\n")
	}
	b.WriteString(styleBad.Render("It exists nowhere but this server until you write it down somewhere else.") + "\n")
	b.WriteString(styleBad.Render("Without it, the backups on this destination cannot be read if this server is lost.") + "\n\n")
	rows := [][2]string{
		{"Server", card.Hostname},
		{"Destination", card.Destination},
		{"Repository", card.Repository},
		{"Restic URI", card.URI},
		{"Password", card.Password},
	}
	if card.ResticOptions != "" {
		rows = append(rows, [2]string{"Restic options", card.ResticOptions})
	}
	for _, row := range rows {
		fmt.Fprintf(&b, "%-16s %s\n", row[0], row[1])
	}
	b.WriteString("\nKeep it in a password manager, not in a file on this machine.\n")
	b.WriteString("Press n once it is written down; the warning on the destinations screen stops then.")
	return b.String()
}

// table draws rows under headers, the cursor's row highlighted, in the
// width there is.
func table(width int, cursor int, headers []string, rows [][]string) string {
	if len(rows) == 0 {
		return ""
	}
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = lipgloss.Width(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && lipgloss.Width(cell) > widths[i] {
				widths[i] = lipgloss.Width(cell)
			}
		}
	}
	// The last column takes what is left; the others are capped so one
	// long error does not push everything off the screen.
	room := width - 2
	for i := range widths[:len(widths)-1] {
		if widths[i] > 32 {
			widths[i] = 32
		}
		room -= widths[i] + 2
	}
	if room < 12 {
		room = 12
	}
	widths[len(widths)-1] = room

	line := func(cells []string) string {
		parts := make([]string, len(widths))
		for i := range widths {
			cell := ""
			if i < len(cells) {
				cell = cells[i]
			}
			parts[i] = pad(cell, widths[i])
		}
		return strings.Join(parts, "  ")
	}
	var b strings.Builder
	b.WriteString(styleDim.Render(line(headers)) + "\n")
	for i, row := range rows {
		text := line(row)
		if i == cursor {
			text = styleCursor.Render(text)
		}
		b.WriteString(text + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// pad cuts or fills a cell to a width, by display cells rather than bytes.
func pad(text string, width int) string {
	text = strings.ReplaceAll(text, "\n", " ")
	if lipgloss.Width(text) > width {
		runes := []rune(text)
		for lipgloss.Width(string(runes))+1 > width && len(runes) > 0 {
			runes = runes[:len(runes)-1]
		}
		text = string(runes) + "…"
	}
	return text + strings.Repeat(" ", max(0, width-lipgloss.Width(text)))
}

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
