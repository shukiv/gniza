package tui

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/shukiv/gniza/internal/answer"
)

// fakeAPI answers pages from a map and records what was posted.
type fakeAPI struct {
	pages  map[string]any
	posted []post
	// answers is what a path posts back: a redirect by default, or a
	// page for the paths that draw one.
	answers map[string]Outcome
}

type post struct {
	path string
	form url.Values
}

func (f *fakeAPI) Page(path string) (answer.Page, error) {
	data, known := f.pages[path]
	if !known {
		data = map[string]any{}
	}
	body, _ := json.Marshal(data)
	nav := strings.TrimPrefix(strings.SplitN(path, "?", 2)[0], "/")
	if nav == "" {
		nav = "dashboard"
	}
	return answer.Page{Nav: nav, CSRF: "token", Data: body, Version: "v-test", Panel: "Plain server"}, nil
}

func (f *fakeAPI) Post(path string, form url.Values) (Outcome, error) {
	f.posted = append(f.posted, post{path: path, form: form})
	if outcome, drawn := f.answers[path]; drawn {
		return outcome, nil
	}
	return Outcome{Kind: "ok", Message: "Done: " + path}, nil
}

func pageOf(t *testing.T, nav string, data any) *answer.Page {
	t.Helper()
	body, _ := json.Marshal(data)
	return &answer.Page{Nav: nav, CSRF: "token", Data: body}
}

// drive runs the model's own commands to completion, the way the program
// would, and returns the settled model.
func drive(t *testing.T, m tea.Model, cmd tea.Cmd) Model {
	t.Helper()
	for cmd != nil {
		msg := cmd()
		if msg == nil {
			break
		}
		if batch, isBatch := msg.(tea.BatchMsg); isBatch {
			for _, c := range batch {
				if c == nil {
					continue
				}
				if inner := c(); inner != nil {
					m, _ = m.Update(inner)
				}
			}
			break
		}
		m, cmd = m.Update(msg)
	}
	return m.(Model)
}

// fresh is a model with the automatic reread off, so every command it
// returns is one a test can run to the end.
func fresh(t *testing.T, api api) Model {
	t.Helper()
	m := New(api)
	m.refresh = 0
	return drive(t, m, m.Init())
}

func press(t *testing.T, m Model, keys ...string) Model {
	t.Helper()
	for _, key := range keys {
		var msg tea.KeyMsg
		switch key {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "tab":
			msg = tea.KeyMsg{Type: tea.KeyTab}
		case " ":
			msg = tea.KeyMsg{Type: tea.KeySpace}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
		}
		next, cmd := m.Update(msg)
		m = drive(t, next, cmd)
	}
	return m
}

func typed(t *testing.T, m Model, text string) Model {
	t.Helper()
	for _, r := range text {
		m = press(t, m, string(r))
	}
	return m
}

func fixture() *fakeAPI {
	destination := map[string]any{
		"id": "d1", "name": "usb", "type": "local", "Endpoint": "/mnt/backups", "Status": "ok",
		"Repository": map[string]any{"id": "r1"},
	}
	return &fakeAPI{
		pages: map[string]any{
			"/": map[string]any{
				"Hostname": "lamp.example", "Sentence": "1 of 2 accounts is not covered.", "Severity": "bad",
				"Accounts":  []any{map[string]any{"User": "shop"}, map[string]any{"User": "blog"}},
				"Attention": []any{map[string]any{"User": "blog", "Because": "nothing has ever been backed up"}},
			},
			"/destinations": map[string]any{
				"Hostname": "lamp.example", "Destinations": []any{destination}, "Unnoted": []string{"usb"},
			},
			"/schedule": map[string]any{
				"Destinations": []any{destination},
				"Policies": []any{map[string]any{"id": "p1", "name": "Nightly", "schedule_cron": "0 2 * * *",
					"enabled": true, "repository_ids": []string{"r1"}}},
			},
			"/accounts": map[string]any{
				"Accounts": []any{
					map[string]any{"User": "blog", "Condition": "never", "Because": "nothing has ever been backed up"},
					map[string]any{"User": "shop", "Condition": "protected"},
				},
				"RunAll": map[string]any{"id": "p1", "name": "Nightly"},
			},
		},
		answers: map[string]Outcome{},
	}
}

// TestTheOverviewIsTheFirstScreen: what the interface opens with is the
// verdict the overview page leads with.
func TestTheOverviewIsTheFirstScreen(t *testing.T) {
	api := fixture()
	m := fresh(t, api)
	view := m.View()
	for _, want := range []string{"1 of 2 accounts is not covered.", "lamp.example", "blog", "nothing has ever been backed up", "Plain server"} {
		if !strings.Contains(view, want) {
			t.Errorf("the first screen lacks %q:\n%s", want, view)
		}
	}
}

// TestADestinationWhoseKeyIsNotSavedSaysSo: the warning the page carries
// is the one thing on the destinations screen that must not be missed.
func TestADestinationWhoseKeyIsNotSavedSaysSo(t *testing.T) {
	api := fixture()
	m := fresh(t, api)
	m = press(t, m, "2")
	view := m.View()
	for _, want := range []string{"usb", "NOT SAVED", "exists nowhere but this server"} {
		if !strings.Contains(view, want) {
			t.Errorf("the destinations screen lacks %q:\n%s", want, view)
		}
	}
}

// TestBackingUpTheAccountUnderTheCursor: b posts the account the cursor
// is on, and the redirect's message is shown.
func TestBackingUpTheAccountUnderTheCursor(t *testing.T) {
	api := fixture()
	m := fresh(t, api)
	m = press(t, m, "4", "j", "b")
	if len(api.posted) != 1 || api.posted[0].path != "/accounts/backup" || api.posted[0].form.Get("account") != "shop" {
		t.Fatalf("posted %+v", api.posted)
	}
	if !strings.Contains(m.View(), "Done: /accounts/backup") {
		t.Errorf("the message was not shown:\n%s", m.View())
	}
}

// TestAddingALocalDestinationEndsAtItsRecoveryKey: the form posts what
// the handler reads, and the page the handler answers with -- the key,
// shown once -- is drawn with the way to say it has been written down.
func TestAddingALocalDestinationEndsAtItsRecoveryKey(t *testing.T) {
	api := fixture()
	api.answers["/destinations/add"] = Outcome{Page: pageOf(t, "destinations", map[string]any{
		"Revealed":    map[string]any{"Destination": "usb2", "URI": "/mnt/b/lamp.example", "Password": "s3cret-words"},
		"RevealedID":  "r2",
		"JustCreated": true,
	})}
	m := fresh(t, api)
	m = press(t, m, "2", "a")
	m = typed(t, m, "usb2")
	m = press(t, m, "tab") // type: local is the first choice
	m = press(t, m, "tab")
	m = typed(t, m, "/mnt/b")
	m = press(t, m, "tab") // repository name, prefilled with the hostname
	m = press(t, m, "enter")

	if len(api.posted) != 1 {
		t.Fatalf("posted %+v", api.posted)
	}
	sent := api.posted[0]
	if sent.path != "/destinations/add" || sent.form.Get("name") != "usb2" || sent.form.Get("type") != "local" ||
		sent.form.Get("root") != "/mnt/b" || sent.form.Get("repo_path") != "lamp.example" {
		t.Errorf("the form sent %v", sent.form)
	}
	if sent.form.Has("host") {
		t.Errorf("a local destination sent sftp fields: %v", sent.form)
	}
	view := m.View()
	for _, want := range []string{"s3cret-words", "/mnt/b/lamp.example", "written down"} {
		if !strings.Contains(view, want) {
			t.Errorf("the recovery key screen lacks %q:\n%s", want, view)
		}
	}

	m = press(t, m, "n")
	if len(api.posted) != 2 || api.posted[1].path != "/destinations/recovery/note" || api.posted[1].form.Get("repository") != "r2" {
		t.Errorf("noting posted %+v", api.posted[1:])
	}
}

// TestARefusedFormStaysWithItsReason: the handler re-drew the form with
// why; the operator sees why and keeps what they typed.
func TestARefusedFormStaysWithItsReason(t *testing.T) {
	api := fixture()
	api.answers["/destinations/add"] = Outcome{Page: pageOf(t, "destinations", map[string]any{
		"FormError": "Give the destination a name.",
	})}
	m := fresh(t, api)
	m = press(t, m, "2", "a", "tab", "tab")
	m = typed(t, m, "/mnt/b")
	m = press(t, m, "tab", "enter")
	if m.mode != modeForm {
		t.Fatalf("the form was dropped; mode %d", m.mode)
	}
	view := m.View()
	if !strings.Contains(view, "Give the destination a name.") || !strings.Contains(view, "/mnt/b") {
		t.Errorf("the refused form does not say why or lost what was typed:\n%s", view)
	}
}

// TestAnSFTPHostKeyIsAgreedToBeforeAnythingIsSent: the handler answered
// with the fingerprint; agreeing sends the same form again with it.
func TestAnSFTPHostKeyIsAgreedToBeforeAnythingIsSent(t *testing.T) {
	api := fixture()
	api.answers["/destinations/add"] = Outcome{Page: pageOf(t, "destinations", map[string]any{
		"ConfirmHost": "backups.example", "HostFingerprint": "SHA256:abc", "HostKeyType": "ed25519",
	})}
	m := fresh(t, api)
	m = press(t, m, "2", "a")
	m = typed(t, m, "far")
	m = press(t, m, "tab", " ") // type: sftp
	m = press(t, m, "tab")
	m = typed(t, m, "backups.example")
	m = press(t, m, "tab", "tab")
	m = typed(t, m, "arkady")
	m = press(t, m, "tab")
	m = typed(t, m, "/home/arkady/gniza")
	m = press(t, m, "tab")
	m = typed(t, m, "pw")
	m = press(t, m, "tab", "tab", "enter")

	if m.mode != modeConfirm || !strings.Contains(m.View(), "SHA256:abc") {
		t.Fatalf("no fingerprint to agree to; mode %d:\n%s", m.mode, m.View())
	}
	delete(api.answers, "/destinations/add")
	m = press(t, m, "y")
	if len(api.posted) != 2 {
		t.Fatalf("posted %+v", api.posted)
	}
	again := api.posted[1].form
	if again.Get("confirm_fingerprint") != "SHA256:abc" || again.Get("password") != "pw" ||
		again.Get("host") != "backups.example" || again.Get("user") != "arkady" || again.Get("type") != "sftp" {
		t.Errorf("the agreed form sent %v", again)
	}
	if m.mode != modeList {
		t.Errorf("after agreeing the mode is %d", m.mode)
	}
}

// TestRemovingADestinationAsksFirst: d asks, n does nothing, y posts.
func TestRemovingADestinationAsksFirst(t *testing.T) {
	api := fixture()
	m := fresh(t, api)
	m = press(t, m, "2", "d")
	if !strings.Contains(m.View(), `Remove the destination "usb"?`) {
		t.Fatalf("no question:\n%s", m.View())
	}
	m = press(t, m, "n")
	if len(api.posted) != 0 {
		t.Fatalf("n posted %+v", api.posted)
	}
	m = press(t, m, "d", "y")
	if len(api.posted) != 1 || api.posted[0].path != "/destinations/delete" || api.posted[0].form.Get("id") != "d1" {
		t.Errorf("y posted %+v", api.posted)
	}
}

// TestASchedulePostsWhatTheHandlerReads: name, cron, mode, the toggles,
// retention and every destination ticked, under scope=all.
func TestASchedulePostsWhatTheHandlerReads(t *testing.T) {
	api := fixture()
	m := fresh(t, api)
	m = press(t, m, "3", "a")
	// The one destination is ticked for a new schedule; enter on the
	// last field submits with the defaults.
	for i := 0; i < 8; i++ {
		m = press(t, m, "tab")
	}
	m = press(t, m, "enter")
	if len(api.posted) != 1 {
		t.Fatalf("posted %+v", api.posted)
	}
	sent := api.posted[0].form
	for name, want := range map[string]string{
		"name": "Nightly", "cron": "0 2 * * *", "mode": "split", "enabled": "1", "include_system": "1",
		"keep_daily": "7", "keep_weekly": "4", "keep_monthly": "6", "scope": "all", "repository": "r1",
	} {
		if sent.Get(name) != want {
			t.Errorf("%s = %q, want %q (form %v)", name, sent.Get(name), want, sent)
		}
	}
}

// TestEditingAScheduleCarriesItsID: e opens the schedule under the cursor
// filled in, and saving names it so the handler edits rather than adds.
func TestEditingAScheduleCarriesItsID(t *testing.T) {
	api := fixture()
	m := fresh(t, api)
	m = press(t, m, "3", "e")
	if !strings.Contains(m.View(), "Edit the schedule") {
		t.Fatalf("no edit form:\n%s", m.View())
	}
	m = press(t, m, "ctrl+s")
	if len(api.posted) != 1 || api.posted[0].form.Get("id") != "p1" || api.posted[0].form.Get("repository") != "r1" {
		t.Errorf("posted %+v", api.posted)
	}
}

// TestTheKeysHelpNamesEveryScreen: the foot of the screen says what the
// keys do, so nobody has to read a guide to find the remove key.
func TestTheKeysHelpNamesEveryScreen(t *testing.T) {
	api := fixture()
	m := fresh(t, api)
	for key, want := range map[string]string{"2": "d remove", "3": "R run now", "4": "b back up", "5": "t next tab", "7": "u check"} {
		m = press(t, m, key)
		if !strings.Contains(m.View(), want) {
			t.Errorf("screen %s lacks %q in its help:\n%s", key, want, m.View())
		}
	}
}

// choosing is the accounts page of a server without a panel: what was
// chosen, and what could be.
func choosing(rows ...map[string]any) map[string]any {
	accounts := []any{}
	for _, row := range rows {
		accounts = append(accounts, row)
	}
	return map[string]any{
		"Accounts": accounts,
		"Choose": map[string]any{"Candidates": map[string]any{
			"Roots":      []string{"/var/www"},
			"Folders":    []string{"/var/www/shop", "/var/www/blog"},
			"MySQL":      []string{"shop", "shop_wp"},
			"PostgreSQL": []string{"erp"},
			"Containers": []any{
				map[string]any{"Engine": "docker", "Name": "web", "Image": "nginx:1", "Status": "Up 2 hours"},
				map[string]any{"Engine": "docker", "Name": "db", "Image": "mysql:8", "Status": "Up 2 hours", "Chosen": true},
			},
		}},
	}
}

// TestOnAServerWithoutAPanelTheScreenIsWhatToBackUp: the tab is named for
// what it is there, an empty page says how to start, and a chosen source
// says what it holds.
func TestOnAServerWithoutAPanelTheScreenIsWhatToBackUp(t *testing.T) {
	api := fixture()
	api.pages["/accounts"] = choosing()
	m := fresh(t, api)
	m = press(t, m, "4")
	view := m.View()
	for _, want := range []string{"4 What to back up", "Nothing is backed up yet", "add a folder", "add a container"} {
		if !strings.Contains(view, want) {
			t.Errorf("the empty screen lacks %q:\n%s", want, view)
		}
	}
	api.pages["/accounts"] = choosing(map[string]any{
		"User": "shop", "Condition": "protected",
		"Source": map[string]any{"path": "/var/www/shop", "mysql": []string{"shop", "shop_wp"}},
	}, map[string]any{
		"User": "web-data", "Condition": "never",
		"Source": map[string]any{"path": "/var/lib/docker/volumes/web_data/_data",
			"container": map[string]any{"engine": "docker", "name": "web", "mount": "/data"}},
	})
	m = press(t, m, "3", "4")
	view = m.View()
	for _, want := range []string{"2 sources", "/var/www/shop", "2 MySQL dbs", "docker web"} {
		if !strings.Contains(view, want) {
			t.Errorf("the screen lacks %q:\n%s", want, view)
		}
	}
}

// TestChoosingAFolderWithItsDatabases: a posts the folder typed and the
// databases ticked, as the handler reads them.
func TestChoosingAFolderWithItsDatabases(t *testing.T) {
	api := fixture()
	// The list is read without the offer; the offer is read for the key.
	api.pages["/accounts"] = map[string]any{"Accounts": []any{}, "Choose": map[string]any{}}
	api.pages["/accounts?add=1"] = choosing()
	m := fresh(t, api)
	m = press(t, m, "4", "a")
	if view := m.View(); !strings.Contains(view, "/var/www/shop, /var/www/blog") {
		t.Fatalf("the form does not say what was found under the roots:\n%s", view)
	}
	m = typed(t, m, "/var/www/shop")
	m = press(t, m, "tab")      // name, left empty
	m = press(t, m, "tab", " ") // MySQL shop: on
	m = press(t, m, "tab")      // MySQL shop_wp: left off
	m = press(t, m, "tab", " ") // PostgreSQL erp: on
	m = press(t, m, "enter")
	if len(api.posted) != 1 {
		t.Fatalf("posted %+v", api.posted)
	}
	sent := api.posted[0]
	if sent.path != "/accounts/add" || sent.form.Get("path") != "/var/www/shop" || sent.form.Get("name") != "" ||
		strings.Join(sent.form["mysql"], ",") != "shop" || strings.Join(sent.form["postgresql"], ",") != "erp" {
		t.Errorf("the form sent %v", sent.form)
	}
}

// TestChoosingAContainer: c offers the containers not chosen yet and posts
// the one picked.
func TestChoosingAContainer(t *testing.T) {
	api := fixture()
	api.pages["/accounts"] = map[string]any{"Accounts": []any{}, "Choose": map[string]any{}}
	api.pages["/accounts?add=1"] = choosing()
	m := fresh(t, api)
	m = press(t, m, "4", "c")
	view := m.View()
	if !strings.Contains(view, "web (docker · nginx:1") || strings.Contains(view, "mysql:8") {
		t.Fatalf("the container form offers the wrong containers:\n%s", view)
	}
	m = press(t, m, "enter")
	if len(api.posted) != 1 || api.posted[0].path != "/accounts/add" || api.posted[0].form.Get("container") != "docker/web" {
		t.Errorf("posted %+v", api.posted)
	}
}

// TestRemovingASourceAsksFirst: d asks, and y posts the removal.
func TestRemovingASourceAsksFirst(t *testing.T) {
	api := fixture()
	api.pages["/accounts"] = choosing(map[string]any{"User": "shop", "Source": map[string]any{"path": "/var/www/shop"}})
	m := fresh(t, api)
	m = press(t, m, "4", "d")
	if !strings.Contains(m.View(), `Stop backing up "shop"?`) {
		t.Fatalf("no question:\n%s", m.View())
	}
	m = press(t, m, "y")
	if len(api.posted) != 1 || api.posted[0].path != "/accounts/remove" || api.posted[0].form.Get("account") != "shop" {
		t.Errorf("posted %+v", api.posted)
	}
}

// TestTheSchedulesScreenSaysTheRecoveryKeyComesFirst: a destination whose
// recovery key has not been noted as stored elsewhere makes the screen
// say so, since the save is refused for it.
func TestTheSchedulesScreenSaysTheRecoveryKeyComesFirst(t *testing.T) {
	api := fixture()
	api.pages["/schedule"] = map[string]any{
		"Policies": []any{},
		"Destinations": []any{map[string]any{
			"id": "d1", "name": "Spare disk", "type": "local",
			"Repository": map[string]any{"id": "r1", "initialised_at": "2026-09-12T00:00:00Z"},
		}},
	}
	m := fresh(t, api)
	m = press(t, m, "3")
	if view := m.View(); !strings.Contains(view, "recovery key for Spare disk is still only on this server") {
		t.Errorf("the screen does not say the recovery key comes first:\n%s", view)
	}
}
