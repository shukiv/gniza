package webui

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/node"
	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/panel"
	"github.com/shukiv/gniza/internal/plain"
	"github.com/shukiv/gniza/internal/vault"
)

// newPlainServer builds the operator pages over a plain provider whose
// catalog is the store, with a fake mysql that knows two databases and
// one folder under the root.
func newPlainServer(t *testing.T) (*Server, http.Handler, string) {
	t.Helper()
	return plainServer(t, false)
}

// plainServer is newPlainServer with, when asked, a psql that knows one
// database, erp, owned by the login role erp_owner, and the superuser.
func plainServer(t *testing.T, postgres bool) (*Server, http.Handler, string) {
	t.Helper()
	root := t.TempDir()
	www := filepath.Join(root, "www")
	shop := filepath.Join(www, "shop")
	if err := os.MkdirAll(shop, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shop, "index.php"), []byte("<?php"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(shop, "public"), 0o755); err != nil {
		t.Fatal(err)
	}
	mysql := filepath.Join(root, "mysql")
	script := "#!/bin/sh\ncase \"$*\" in *schema_privileges*) printf \"shop\\t'shop_app'@'localhost'\\tINSERT,SELECT\\n\" ;; *user_privileges*) printf \"'root'@'localhost'\\tALL PRIVILEGES\\n\" ;; *\"WHERE user = \"*) printf 'mysql_native_password\\t2A41\\n' ;; *mysql.user*) printf 'root\\tlocalhost\\tmysql_native_password\\nshop_app\\tlocalhost\\tmysql_native_password\\n' ;; *VERSION*) printf '11.8.6-MariaDB\\n' ;; *information_schema*) printf 'shop\\t4096\\nblog\\t4096\\n' ;; *SHOW\\ GRANTS*) printf 'GRANT SELECT, INSERT ON `shop`.* TO `shop_app`@`localhost`\\n' ;; *) cat >/dev/null ;; esac\n"
	if err := os.WriteFile(mysql, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	store, err := nodestore.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	settings := nodestore.DefaultSettings()
	settings.StagingRoot = filepath.Join(root, "staging")
	settings.ResticCache = filepath.Join(root, "cache")
	settings.ConfigDir = filepath.Join(root, "config")
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	keyHex, err := vault.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(root, "master.key")
	if err := os.WriteFile(keyPath, []byte(keyHex), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := vault.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	v, err := vault.New(key)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	psql := filepath.Join(root, "no-psql")
	if postgres {
		psql = filepath.Join(root, "psql")
		pg := "#!/bin/sh\ncase \"$*\" in *pg_database_size*) printf 'erp\\t8192\\terp_owner\\n' ;; *pg_authid*) printf 'postgres\\tt\\tt\\tt\\tt\\tt\\tt\\t\\nerp_owner\\tt\\tf\\tf\\tf\\tf\\tf\\tSCRAM-SHA-256$4096:c2FsdA==$c3RvcmVk:c2VydmVy\\n' ;; *) cat >/dev/null ;; esac\n"
		if err := os.WriteFile(psql, []byte(pg), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	provider := &plain.Provider{Roots: []string{www}, Catalog: store, MySQLPath: mysql,
		PsqlPath: psql, DockerPath: filepath.Join(root, "no-docker"),
		PodmanPath: filepath.Join(root, "no-podman"), PostgresUser: "-", Log: log}
	engine, err := node.New(node.Config{
		Store: store, Vault: v, Provider: provider, Log: log, HookSpool: filepath.Join(root, "hooks"),
	})
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(engine, log)
	if err != nil {
		t.Fatal(err)
	}
	return server, server.Handler(), shop
}

func getPage(handler http.Handler, path string, json bool) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	if json {
		request.Header.Set("Accept", "application/json")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func postForm(server *Server, handler http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	form.Set("csrf", server.csrfToken)
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// TestOnAServerWithoutAPanelTheAccountsPageListsTheSources: the page is
// the list of what was chosen, named Sources, with Remove on each row
// and nothing to choose from -- the choosing is the schedule form's.
// As data it carries what there is to choose from when asked with
// add=1, for the terminal, and a refused choice's reason.
func TestOnAServerWithoutAPanelTheAccountsPageListsTheSources(t *testing.T) {
	server, handler, shop := newPlainServer(t)

	page := getPage(handler, "/accounts", false)
	body := page.Body.String()
	if page.Code != http.StatusOK {
		t.Fatalf("GET /accounts = %d", page.Code)
	}
	for _, want := range []string{">Sources<", `aria-label="Sources"`, "Nothing is backed up yet", `href="?p=schedule"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the empty page lacks %q", want)
		}
	}
	for _, gone := range []string{"What to back up", "data-tab=", "accounts/add", "add=1", `name="mysql"`,
		"No Plain server accounts found", "/var/cpanel/users"} {
		if strings.Contains(body, gone) {
			t.Errorf("the page still carries %q", gone)
		}
	}

	var answered struct {
		Data struct {
			Accounts []json.RawMessage
			Choose   *struct {
				Candidates struct {
					Folders []struct{ Path, ChosenAs string }
					MySQL   []struct {
						Name  string
						Users []string
					}
				}
			}
		}
	}
	if err := json.Unmarshal(getPage(handler, "/accounts?add=1", true).Body.Bytes(), &answered); err != nil {
		t.Fatal(err)
	}
	if k := answered.Data.Choose; k == nil || len(k.Candidates.MySQL) != 2 || k.Candidates.MySQL[0].Name != "blog" ||
		k.Candidates.MySQL[1].Name != "shop" || strings.Join(k.Candidates.MySQL[1].Users, ",") != "'shop_app'@'localhost'" ||
		len(k.Candidates.Folders) != 1 || k.Candidates.Folders[0].Path != shop {
		t.Errorf("the page as data does not carry the candidates: %+v", answered.Data.Choose)
	}
	// The offer costs a look at the databases and the containers, so the
	// terminal's five-second read of the list does not carry it; it asks
	// with add=1 when a or c is pressed.
	var polled struct {
		Data struct {
			Choose *struct {
				Candidates struct{ Folders, MySQL []json.RawMessage }
			}
		}
	}
	if err := json.Unmarshal(getPage(handler, "/accounts", true).Body.Bytes(), &polled); err != nil {
		t.Fatal(err)
	}
	if polled.Data.Choose == nil || len(polled.Data.Choose.Candidates.Folders)+len(polled.Data.Choose.Candidates.MySQL) != 0 {
		t.Errorf("the list as data looks at the candidates: %+v", polled.Data.Choose)
	}

	added := postForm(server, handler, "/accounts/add", url.Values{"tab": {"folders"}, "folder": {shop}})
	if added.Code != http.StatusSeeOther || !strings.Contains(added.Header().Get("Location"), "kind=ok") || !strings.Contains(added.Header().Get("Location"), "1+source%3A+shop") {
		t.Fatalf("adding = %d %s %s", added.Code, added.Header().Get("Location"), added.Body.String())
	}
	databases := postForm(server, handler, "/accounts/add", url.Values{"tab": {"mysql"}, "mysql": {"shop", "blog"}})
	if location := databases.Header().Get("Location"); databases.Code != http.StatusSeeOther || !strings.Contains(location, "2+sources%3A+mysql-shop%2C+mysql-blog") {
		t.Fatalf("adding the databases = %d %s", databases.Code, location)
	}
	body = getPage(handler, "/accounts", false).Body.String()
	for _, want := range []string{">shop</a>", shop, ">mysql-shop</a>", "MySQL: shop", "?p=accounts/remove"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page with a source lacks %q", want)
		}
	}
	if strings.Contains(body, "Nothing is backed up yet") || strings.Contains(body, `name="mysql"`) {
		t.Error("the page with a source still says nothing is backed up, or offers the choosing")
	}

	// A refusal is said with its reason: to a script, on the page it is
	// sent back to; to the terminal, as the form's error, so its form
	// stays open.
	again := postForm(server, handler, "/accounts/add", url.Values{"tab": {"folders"}, "folder_path": {shop}, "folder_name": {"shop2"}})
	if reason := refusal(t, again); !strings.Contains(reason, "already backed up as shop") {
		t.Errorf("the same folder twice is refused with %q", reason)
	}
	// Part of a tab refused is said with the part that was added.
	mixed := postForm(server, handler, "/accounts/add", url.Values{"tab": {"folders"}, "folder": {filepath.Join(shop, "public"), filepath.Join(shop, "index.php")}})
	if location := mixed.Header().Get("Location"); mixed.Code != http.StatusSeeOther || !strings.Contains(location, "kind=warn") || !strings.Contains(location, "Not+added") {
		t.Errorf("a folder refused beside one added = %d %s", mixed.Code, location)
	}
	if reason := refusal(t, postForm(server, handler, "/accounts/add", url.Values{"tab": {"mysql"}})); !strings.Contains(reason, "nothing was chosen") {
		t.Errorf("an empty form is refused with %q", reason)
	}
	nothing := postData(server, handler, "/accounts/add", url.Values{"tab": {"mysql"}})
	var drawn struct{ Data struct{ FormError string } }
	if err := json.Unmarshal(nothing.Body.Bytes(), &drawn); nothing.Code != http.StatusOK || err != nil || !strings.Contains(drawn.Data.FormError, "nothing was chosen") {
		t.Errorf("an empty form as data = %d %v %q", nothing.Code, err, drawn.Data.FormError)
	}

	removed := postForm(server, handler, "/accounts/remove", url.Values{"account": {"shop"}})
	if removed.Code != http.StatusSeeOther || !strings.Contains(removed.Header().Get("Location"), "kind=ok") {
		t.Fatalf("removing = %d %s", removed.Code, removed.Header().Get("Location"))
	}
	// The folder leaves the list and is the browser's to offer again;
	// the databases chosen stay chosen.
	after := getPage(handler, "/accounts", false).Body.String()
	if strings.Contains(after, ">shop</a>") || !strings.Contains(after, ">mysql-shop</a>") {
		t.Errorf("after removing, the list says: %s", firstLine(after, "</a>"))
	}
	if listing := browse(t, handler, filepath.Dir(shop)); len(listing.Entries) != 1 || listing.Entries[0].ChosenAs != "" {
		t.Errorf("after removing, the browser says %+v", listing.Entries)
	}
	for _, name := range []string{"mysql-shop", "mysql-blog", "public"} {
		removed := postForm(server, handler, "/accounts/remove", url.Values{"account": {name}})
		if removed.Code != http.StatusSeeOther {
			t.Fatalf("removing %s = %d", name, removed.Code)
		}
	}
	if !strings.Contains(getPage(handler, "/accounts", false).Body.String(), "Nothing is backed up yet") {
		t.Error("after removing everything, the page does not say so")
	}
}

// refusal is the reason a post was sent back with, from the page it was
// sent to.
func refusal(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	location, err := url.Parse(response.Header().Get("Location"))
	if response.Code != http.StatusSeeOther || err != nil || location.Query().Get("kind") != "error" {
		t.Fatalf("a refusal = %d %s", response.Code, response.Header().Get("Location"))
	}
	return location.Query().Get("msg")
}

// postData posts a form the way the terminal does, asking for the answer
// as data.
func postData(server *Server, handler http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	form.Set("csrf", server.csrfToken)
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// TestAPanelServerKeepsItsAccountsPage: the choosing is not offered where
// a panel lists the accounts, and the routes refuse politely.
func TestAPanelServerKeepsItsAccountsPage(t *testing.T) {
	server, _ := newBrowserServer(t, "correct horse battery staple")
	handler := server.Handler()
	body := getPage(handler, "/accounts", false).Body.String()
	if strings.Contains(body, "What to back up") || strings.Contains(body, "accounts/add") {
		t.Error("a panel server's accounts page offers the choosing")
	}
	if !strings.Contains(body, ">Accounts<") {
		t.Error("the rail no longer says Accounts on a panel server")
	}
	refused := postForm(server, handler, "/accounts/add", url.Values{"folder_path": {"/srv"}})
	if refused.Code != http.StatusSeeOther || !strings.Contains(refused.Header().Get("Location"), "kind=error") {
		t.Errorf("adding on a panel server = %d %s", refused.Code, refused.Header().Get("Location"))
	}
	if browsed := getPage(handler, "/accounts/browse?dir=/", true); browsed.Code != http.StatusNotFound {
		t.Errorf("browsing on a panel server = %d", browsed.Code)
	}
}

// TestTheFolderBrowserListsOneDirectoryAtATime: the folders tab reads
// the server's directories as data, one at a time, with the ones chosen
// marked; a path that is not a directory is refused with the reason.
func TestTheFolderBrowserListsOneDirectoryAtATime(t *testing.T) {
	server, handler, shop := newPlainServer(t)
	www := filepath.Dir(shop)
	listing := browse(t, handler, www)
	if listing.Dir != www || listing.Parent != filepath.Dir(www) || len(listing.Entries) != 1 ||
		listing.Entries[0].Name != "shop" || listing.Entries[0].Path != shop || listing.Entries[0].ChosenAs != "" {
		t.Errorf("browsing www = %+v", listing)
	}
	if added := postForm(server, handler, "/accounts/add", url.Values{"folder": {shop}}); added.Code != http.StatusSeeOther {
		t.Fatalf("adding = %d", added.Code)
	}
	if listing := browse(t, handler, www); listing.Entries[0].ChosenAs != "shop" {
		t.Errorf("after adding, the browser says %+v", listing.Entries)
	}
	// Inside the chosen folder: its folder first, then its file, and
	// everything there is the source's already.
	if inside := browse(t, handler, shop); len(inside.Entries) != 2 || inside.Entries[0].Name != "public" || inside.Entries[0].Kind != "folder" ||
		inside.Entries[1].Name != "index.php" || inside.Entries[1].Kind != "file" || inside.Within != "shop" {
		t.Errorf("browsing shop = %+v", inside)
	}
	file := getPage(handler, "/accounts/browse?dir="+url.QueryEscape(filepath.Join(shop, "index.php")), true)
	if file.Code != http.StatusBadRequest || !strings.Contains(file.Body.String(), "is a file") {
		t.Errorf("browsing a file = %d %s", file.Code, file.Body.String())
	}
	if top := browse(t, handler, ""); top.Dir != "/" || top.Parent != "" {
		t.Errorf("browsing nothing = %+v", top)
	}
	// The page's script drives the browser and the picked table.
	page := getPage(handler, "/accounts", false).Body.String()
	for _, want := range []string{"[data-toggle]", "data-pick", "data-picked", "data-path-error"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page does not carry %q", want)
		}
	}
}

func browse(t *testing.T, handler http.Handler, dir string) panel.Listing {
	t.Helper()
	response := getPage(handler, "/accounts/browse?dir="+url.QueryEscape(dir), true)
	if response.Code != http.StatusOK {
		t.Fatalf("browsing %q = %d %s", dir, response.Code, response.Body.String())
	}
	var listing panel.Listing
	if err := json.Unmarshal(response.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	return listing
}

func firstLine(body, containing string) string {
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, containing) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// TestOnAServerWithoutAPanelTheScheduleFormChoosesWhatToBackUp: the
// tables of what to back up sit in the schedule form where the account
// picker is on a panel server. A fresh row ticked there is added when
// the schedule is saved; a row already on the list is ticked by name.
func TestOnAServerWithoutAPanelTheScheduleFormChoosesWhatToBackUp(t *testing.T) {
	server, handler, shop := newPlainServer(t)
	store := server.engine.Store()
	destination, err := store.PutDestination(nodestore.Destination{
		Name: "usb", Type: "local", Config: map[string]string{"root": t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}
	noted := time.Now()
	repository, err := store.PutRepository(nodestore.Repository{
		DestinationID: destination.ID, Path: "gniza", RecoveryNotedAt: &noted, InitialisedAt: &noted,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Nothing chosen yet: the form carries the tables with the databases
	// ticked and the folder not, and says the ticks go on the list.
	page := getPage(handler, "/schedule", false).Body.String()
	for _, want := range []string{
		"data-choose-in-schedule", `data-tabs="folders"`,
		`name="mysql" value="shop" aria-label="shop" checked`,
		`data-folder-browser data-browse="?p=accounts/browse"`, "No folder yet",
		`id="folder_path"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the schedule form on a plain server lacks %q", want)
		}
	}
	if strings.Contains(page, "Which accounts") || strings.Contains(page, "<div data-account-picker") || strings.Contains(page, `name="scope"`) {
		t.Error("the schedule form still carries the panel's account picker, or a choice of scope")
	}
	// The terminal's read of the page as data does not look at the
	// candidates.
	var polled struct {
		Data struct{ Choose *chooseView }
	}
	if err := json.Unmarshal(getPage(handler, "/schedule", true).Body.Bytes(), &polled); err != nil {
		t.Fatal(err)
	}
	if polled.Data.Choose != nil && len(polled.Data.Choose.Candidates.MySQL) != 0 {
		t.Errorf("the schedule page as data looks at the candidates: %+v", polled.Data.Choose)
	}

	// A schedule with nothing ticked backs up nothing, and is refused
	// rather than saved empty; so is a script's "everything" while the
	// list is empty.
	base := url.Values{"name": {"Nightly"}, "cron": {"0 2 * * *"}, "mode": {"split"}, "enabled": {"1"},
		"repository": {repository.ID}, "keep_daily": {"7"}, "keep_weekly": {"4"}, "keep_monthly": {"6"}}
	for _, form := range []url.Values{withScope(base, ""), withScope(base, "all")} {
		empty := postForm(server, handler, "/schedule/save", form)
		if location := empty.Header().Get("Location"); empty.Code != http.StatusSeeOther || !strings.Contains(location, "kind=error") || !strings.Contains(location, "Tick+something") {
			t.Fatalf("an empty schedule = %d %s", empty.Code, location)
		}
	}
	empty := postForm(server, handler, "/schedule/save", withScope(base, "all"))
	if location := empty.Header().Get("Location"); empty.Code != http.StatusSeeOther || !strings.Contains(location, "kind=error") || !strings.Contains(location, "Tick+something") {
		t.Fatalf("an empty schedule = %d %s", empty.Code, location)
	}
	if policies, _ := store.Policies(); len(policies) != 0 {
		t.Fatalf("the empty schedule was saved: %+v", policies)
	}

	// Ticking the shop database and the folder adds both. Every source
	// on the list is ticked, so the schedule covers everything, now and
	// later, the way "Back up all" needs it.
	ticked := withScope(base, "")
	ticked["mysql"] = []string{"shop"}
	ticked["folder"] = []string{shop}
	saved := postForm(server, handler, "/schedule/save", ticked)
	if location := saved.Header().Get("Location"); saved.Code != http.StatusSeeOther || !strings.Contains(location, "kind=ok") {
		t.Fatalf("saving = %d %s", saved.Code, location)
	}
	policies, err := store.Policies()
	if err != nil || len(policies) != 1 {
		t.Fatalf("policies = %+v, %v", policies, err)
	}
	if !policies[0].AllAccounts() {
		t.Errorf("with every source ticked the schedule covers %q, not everything", policies[0].Accounts)
	}
	list := getPage(handler, "/accounts", false).Body.String()
	for _, want := range []string{">shop</a>", ">mysql-shop</a>"} {
		if !strings.Contains(list, want) {
			t.Errorf("the Sources list lacks %q after the schedule was saved", want)
		}
	}

	// Editing it: the rows on the list are boxes that post the source's
	// name, ticked as the schedule has them; the other database is a
	// fresh row, not ticked.
	edit := getPage(handler, "/schedule?edit="+policies[0].ID, false).Body.String()
	for _, want := range []string{
		`name="source" value="mysql-shop" aria-label="shop" checked data-existing data-same="mysql-shop"`,
		`name="source" value="shop" aria-label="shop" checked data-existing data-same="shop"`,
		`<td class="cpr:mono">` + shop + `</td>`,
		`name="mysql" value="blog" aria-label="blog">`,
	} {
		if !strings.Contains(edit, want) {
			t.Errorf("the edit form lacks %q: %s", want, firstLine(edit, `name="source"`))
		}
	}
	// Saving the edit with one source ticked by name and the other
	// database fresh adds that one, and the schedule covers only the
	// two ticked, since the folder was left out.
	again := withScope(base, "")
	again.Set("id", policies[0].ID)
	again["source"] = []string{"mysql-shop"}
	again["mysql"] = []string{"blog"}
	edited := postForm(server, handler, "/schedule/save", again)
	if location := edited.Header().Get("Location"); edited.Code != http.StatusSeeOther || !strings.Contains(location, "kind=ok") {
		t.Fatalf("saving the edit = %d %s", edited.Code, location)
	}
	policies, _ = store.Policies()
	if got := strings.Join(policies[0].Accounts, ","); got != "mysql-shop,mysql-blog" {
		t.Errorf("after the edit the schedule covers %q", got)
	}
	// The TUI edits a schedule the way a panel's form does: scope=
	// selected with the covered names as account=, no source= at all.
	tui := withScope(base, "selected")
	tui.Set("id", policies[0].ID)
	tui["account"] = []string{"mysql-shop"}
	fromTUI := postForm(server, handler, "/schedule/save", tui)
	if location := fromTUI.Header().Get("Location"); fromTUI.Code != http.StatusSeeOther || !strings.Contains(location, "kind=ok") {
		t.Fatalf("saving the edit as the TUI posts it = %d %s", fromTUI.Code, location)
	}
	policies, _ = store.Policies()
	if got := strings.Join(policies[0].Accounts, ","); got != "mysql-shop" {
		t.Errorf("after the TUI's edit the schedule covers %q", got)
	}
	again["source"] = []string{"mysql-shop", "mysql-blog"}
	if edited := postForm(server, handler, "/schedule/save", again); edited.Code != http.StatusSeeOther {
		t.Fatalf("saving the edit again = %d", edited.Code)
	}
	// The schedules table calls them sources.
	table := getPage(handler, "/schedule", false).Body.String()
	if !strings.Contains(table, "mysql-shop, mysql-blog") {
		t.Error("the schedules table does not name the sources")
	}

	// A script's "everything" needs no ticks; a source named that is
	// not on the list is refused with the rest and the schedule is still
	// saved, with a warning.
	everything := withScope(base, "all")
	everything.Set("name", "Weekly")
	everything.Set("cron", "0 3 * * 0")
	everything["source"] = []string{"gone"}
	warned := postForm(server, handler, "/schedule/save", everything)
	if location := warned.Header().Get("Location"); warned.Code != http.StatusSeeOther || !strings.Contains(location, "kind=warn") || !strings.Contains(location, "gone%3A+nothing+of+that+name") {
		t.Fatalf("saving with a name not on the list = %d %s", warned.Code, location)
	}
	policies, _ = store.Policies()
	if len(policies) != 2 {
		t.Fatalf("policies after the second save: %d", len(policies))
	}
	table = getPage(handler, "/schedule", false).Body.String()
	if !strings.Contains(table, "Everything chosen") || !strings.Contains(table, "3 sources") {
		t.Errorf("the schedules table does not say Everything chosen, 3 sources: %s", firstLine(table, "Everything"))
	}

	// A source chosen in the form's first shape, a folder outside the
	// roots with a database beside it, is behind its database row; its
	// folder still gets a row of its own, and the two tick together.
	chooser, _ := server.engine.Chooser()
	site := t.TempDir()
	if err := chooser.AddSource(context.Background(), panel.Source{Name: "site", Path: site, MySQL: []string{"shop"}}); err != nil {
		t.Fatal(err)
	}
	form := getPage(handler, "/schedule", false).Body.String()
	for _, want := range []string{
		`name="source" value="site" aria-label="site" checked data-existing data-same="site"`,
		`<td class="cpr:mono">` + site + `</td>`,
		`name="source" value="mysql-shop"`,
	} {
		if !strings.Contains(form, want) {
			t.Errorf("the form with a folder-and-database source lacks %q: %s", want, firstLine(form, `value="site"`))
		}
	}
}

func withScope(base url.Values, scope string) url.Values {
	form := url.Values{}
	for key, values := range base {
		form[key] = append([]string(nil), values...)
	}
	if scope != "" {
		form.Set("scope", scope)
	}
	return form
}

// A source made from a container is that container's row while the
// container is there; once the container is gone the source keeps a
// row of its own on the folders tab, so it can still be seen and
// removed.
func TestASourceWhoseContainerIsGoneKeepsAFolderRow(t *testing.T) {
	v := chooseView{
		Candidates: panel.Candidates{Containers: []panel.ContainerCandidate{{Engine: "docker", Name: "web"}}},
		Sources: []panel.Source{
			{Name: "web-data", Path: "/var/lib/docker/volumes/web/_data", Container: &panel.ContainerRef{Engine: "docker", Name: "web"}},
			{Name: "old-data", Path: "/var/lib/docker/volumes/old/_data", Container: &panel.ContainerRef{Engine: "docker", Name: "old"}},
			{Name: "site", Path: "/srv/site"},
		},
		containerAs: map[string]string{"docker/web": "web-data", "docker/old": "old-data"},
	}
	var names []string
	for _, source := range v.FolderSources() {
		names = append(names, source.Name)
	}
	if got := strings.Join(names, ","); got != "old-data,site" {
		t.Errorf("the folders tab rows are %q, want old-data,site", got)
	}
}

// The MySQL tab lists every database user the server has with what it
// can reach, beside the databases, each table with a box in its header;
// a user ticked is kept with the source that dumps its database; one
// with rights on no database backed up is refused by name, and the
// server's own are not offered.
func TestADatabaseUserTickedIsKeptWithItsDatabases(t *testing.T) {
	server, handler, _ := newPlainServer(t)
	noted := time.Now()
	destination, err := server.engine.Store().PutDestination(nodestore.Destination{Name: "usb", Type: "local", Config: map[string]string{"root": t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.engine.Store().PutRepository(nodestore.Repository{DestinationID: destination.ID, Path: "gniza", RecoveryNotedAt: &noted, InitialisedAt: &noted}); err != nil {
		t.Fatal(err)
	}
	page := getPage(handler, "/schedule", false).Body.String()
	for _, want := range []string{
		`name="mysql_user" value="shop_app@localhost" aria-label="shop_app@localhost" checked data-needs="shop"`,
		`aria-label="root@localhost is the server's own user"`,
		`<h4 class="cpr:section-title cpr:mt-0 cpr:mb-2">Databases</h4>`,
		`<h4 class="cpr:section-title cpr:mt-0 cpr:mb-2">Users</h4>`,
		`data-tick-all aria-label="Tick every user"`,
		`<span class="cpr:mono">shop_app@localhost</span> <span class="cpr:text-base-content/60">INSERT, SELECT</span>`,
		"every database</span> <span class=\"cpr:text-base-content/60\" title=\"ALL PRIVILEGES\">ALL PRIVILEGES",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the MySQL tab lacks %q: %s", want, firstLine(page, "mysql_user"))
		}
	}
	// Ticked alone, the user has no source to be kept with.
	alone := postForm(server, handler, "/accounts/add", url.Values{"mysql_user": {"shop_app@localhost"}})
	if reason := refusal(t, alone); !strings.Contains(reason, "has rights on no database that is backed up; tick shop with it") {
		t.Errorf("a user without its database is refused with %q", reason)
	}
	// Ticked with its database, it is kept with the fresh source.
	both := postForm(server, handler, "/accounts/add", url.Values{"mysql": {"shop"}, "mysql_user": {"shop_app@localhost"}})
	if location := both.Header().Get("Location"); both.Code != http.StatusSeeOther || !strings.Contains(location, "kind=ok") || !strings.Contains(location, "mysql-shop+%28shop_app%40localhost+kept+with+it%29") {
		t.Errorf("a user with its database = %d %s", both.Code, location)
	}
	sources, _ := server.engine.Chooser()
	list, err := sources.Sources(context.Background())
	if err != nil || len(list) != 1 || strings.Join(list[0].MySQLUsers, ",") != "shop_app@localhost" {
		t.Fatalf("sources = %+v, %v", list, err)
	}
	// In the schedule form the kept user is the source's row.
	form := getPage(handler, "/schedule", false).Body.String()
	if !strings.Contains(form, `name="source" value="mysql-shop" aria-label="shop_app@localhost"`) {
		t.Errorf("the schedule form does not tick the user with its source: %s", firstLine(form, "shop_app@localhost"))
	}
	// A new schedule still ticks what is not chosen yet, sources or no
	// sources.
	if want := `name="mysql" value="blog" aria-label="blog" checked`; !strings.Contains(form, want) {
		t.Errorf("the new-schedule form lacks %q once a source exists: %s", want, firstLine(form, "blog"))
	}
}

// The PostgreSQL tab lists every role beside the databases, the way the
// MySQL tab lists its users, both ticked by default and neither
// showing a verifier; a role ticked is kept with the source that dumps
// the database it owns, and one ticked without its database is refused
// by name.
func TestAPostgreSQLRoleTickedIsKeptWithItsDatabase(t *testing.T) {
	server, handler, _ := plainServer(t, true)
	noted := time.Now()
	destination, err := server.engine.Store().PutDestination(nodestore.Destination{Name: "usb", Type: "local", Config: map[string]string{"root": t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.engine.Store().PutRepository(nodestore.Repository{DestinationID: destination.ID, Path: "gniza", RecoveryNotedAt: &noted, InitialisedAt: &noted}); err != nil {
		t.Fatal(err)
	}
	page := getPage(handler, "/schedule", false).Body.String()
	for _, want := range []string{
		`name="postgresql" value="erp" aria-label="erp" checked`,
		`<tr data-db="erp">`,
		`name="postgresql_user" value="erp_owner" aria-label="erp_owner" checked data-needs="erp"`,
		`aria-label="postgres is the server's own role"`,
		`data-tick-all aria-label="Tick every role"`,
		`title="Authenticates with scram-sha-256">scram-sha-256</span>`,
		`<span class="cpr:mono">erp</span> <span class="cpr:text-base-content/60">OWNER</span>`,
		`<span class="cpr:text-base-content/60">CREATEDB, SUPERUSER, CREATEROLE, REPLICATION, BYPASSRLS</span>`,
		`name="mysql_user" value="shop_app@localhost" aria-label="shop_app@localhost" checked data-needs="shop"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the PostgreSQL tab lacks %q: %s", want, firstLine(page, "postgresql_user"))
		}
	}
	for _, forbidden := range []string{"SCRAM-SHA-256$", "c2FsdA"} {
		if strings.Contains(page, forbidden) {
			t.Errorf("the page shows the verifier: %s", firstLine(page, forbidden))
		}
	}
	alone := postForm(server, handler, "/accounts/add", url.Values{"postgresql_user": {"erp_owner"}})
	if reason := refusal(t, alone); !strings.Contains(reason, "has rights on no database that is backed up; tick erp with it") {
		t.Errorf("a role without its database is refused with %q", reason)
	}
	both := postForm(server, handler, "/accounts/add", url.Values{"postgresql": {"erp"}, "postgresql_user": {"erp_owner"}})
	if location := both.Header().Get("Location"); both.Code != http.StatusSeeOther || !strings.Contains(location, "kind=ok") || !strings.Contains(location, "pg-erp+%28erp_owner+kept+with+it%29") {
		t.Errorf("a role with its database = %d %s", both.Code, location)
	}
	sources, _ := server.engine.Chooser()
	list, err := sources.Sources(context.Background())
	if err != nil || len(list) != 1 || strings.Join(list[0].PostgreSQLUsers, ",") != "erp_owner" {
		t.Fatalf("sources = %+v, %v", list, err)
	}
	form := getPage(handler, "/schedule", false).Body.String()
	if !strings.Contains(form, `name="source" value="pg-erp" aria-label="erp_owner"`) {
		t.Errorf("the schedule form does not tick the role with its source: %s", firstLine(form, "erp_owner"))
	}
}
