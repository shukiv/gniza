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
	script := "#!/bin/sh\ncase \"$*\" in *schema_privileges*) printf \"shop\\t'shop_app'@'localhost'\\n\" ;; *information_schema*) printf 'shop\\t4096\\nblog\\t4096\\n' ;; *) cat >/dev/null ;; esac\n"
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
	provider := &plain.Provider{Roots: []string{www}, Catalog: store, MySQLPath: mysql,
		PsqlPath: filepath.Join(root, "no-psql"), DockerPath: filepath.Join(root, "no-docker"),
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

// TestOnAServerWithoutAPanelTheAccountsPageIsWhatToBackUp: nothing is
// backed up until chosen; the page opens on the form, offers what the
// server has, records a choice, refuses the same folder twice, and
// forgets a choice on request. The same page answers as data with the
// candidates for the terminal.
func TestOnAServerWithoutAPanelTheAccountsPageIsWhatToBackUp(t *testing.T) {
	server, handler, shop := newPlainServer(t)

	page := getPage(handler, "/accounts", false)
	body := page.Body.String()
	if page.Code != http.StatusOK {
		t.Fatalf("GET /accounts = %d", page.Code)
	}
	// One tab per kind; the databases are ticked by default and say whose
	// they are; a client that is not there says so on its tab.
	for _, want := range []string{"What to back up", "Nothing is backed up yet",
		`data-tab="folders"`, `data-tab="mysql"`, `data-tab="postgresql"`, `data-tab="containers"`,
		`data-folder-browser data-browse="?p=accounts/browse" data-dir="/"`, `data-go="` + filepath.Dir(shop) + `"`,
		`name="mysql" value="shop" aria-label="shop" checked`,
		`name="mysql" value="blog" aria-label="blog" checked`, "&#39;shop_app&#39;@&#39;localhost&#39;",
		"No PostgreSQL database is offered", "data-tick-all", "data-tick-count"} {
		if !strings.Contains(body, want) {
			t.Errorf("the empty page lacks %q", want)
		}
	}
	if strings.Contains(body, "No Plain server accounts found") || strings.Contains(body, "/var/cpanel/users") {
		t.Error("the page still talks about a panel's accounts")
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
	// The tabs and the boxes are driven by the page's own script, which
	// listens on the document since the form may be fetched later.
	for _, want := range []string{`closest("[data-tab]")`, "data-tick-all", "gniza:loaded"} {
		if !strings.Contains(body, want) {
			t.Errorf("the script does not handle %s", want)
		}
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
	for _, want := range []string{">shop</a>", shop, ">mysql-shop</a>", "MySQL: shop", `href="?p=accounts&amp;add=1" data-dialog="add-source" data-dialog-fetch`, "?p=accounts/remove"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page with a source lacks %q", want)
		}
	}
	// A database chosen already is shown greyed on its tab, not offered
	// again; the tab asked for is the one that opens.
	form := getPage(handler, "/accounts?add=1&tab=mysql", false).Body.String()
	if !strings.Contains(form, `name="mysql" value="shop" aria-label="shop" disabled`) || !strings.Contains(form, `data-tabs="mysql"`) || !strings.Contains(form, ">mysql-shop<") {
		t.Errorf("the MySQL tab after choosing: %s", firstLine(form, `value="shop"`))
	}
	// With a source chosen the list does not look at the candidates
	// either: Add fetches the form, which does.
	if strings.Contains(body, "blog</option>") || strings.Contains(body, `name="mysql"`) {
		t.Error("the list page carries the form's offer")
	}
	if asked := getPage(handler, "/accounts?add=1", false).Body.String(); !strings.Contains(asked, `name="mysql" value="blog"`) || !strings.Contains(asked, "data-drawer-content") {
		t.Errorf("the asked-for form lacks the offer or the drawer content:\n%s", firstLine(asked, "mysql"))
	}
	if strings.Contains(body, "Nothing is backed up yet") {
		t.Error("the page still says nothing is backed up")
	}

	again := postForm(server, handler, "/accounts/add", url.Values{"tab": {"folders"}, "folder_path": {shop}, "folder_name": {"shop2"}})
	if again.Code != http.StatusOK || !strings.Contains(again.Body.String(), "already backed up as shop") || !strings.Contains(again.Body.String(), `data-tabs="folders"`) {
		t.Errorf("the same folder twice = %d, %q", again.Code, firstLine(again.Body.String(), "already"))
	}
	// Part of a tab refused is said with the part that was added.
	mixed := postForm(server, handler, "/accounts/add", url.Values{"tab": {"folders"}, "folder": {filepath.Join(shop, "public"), filepath.Join(shop, "index.php")}})
	if location := mixed.Header().Get("Location"); mixed.Code != http.StatusSeeOther || !strings.Contains(location, "kind=warn") || !strings.Contains(location, "Not+added") {
		t.Errorf("a folder refused beside one added = %d %s", mixed.Code, location)
	}
	nothing := postForm(server, handler, "/accounts/add", url.Values{"tab": {"mysql"}})
	if nothing.Code != http.StatusOK || !strings.Contains(nothing.Body.String(), "nothing was chosen") || !strings.Contains(nothing.Body.String(), `data-tabs="mysql"`) {
		t.Errorf("an empty form = %d", nothing.Code)
	}

	removed := postForm(server, handler, "/accounts/remove", url.Values{"account": {"shop"}})
	if removed.Code != http.StatusSeeOther || !strings.Contains(removed.Header().Get("Location"), "kind=ok") {
		t.Fatalf("removing = %d %s", removed.Code, removed.Header().Get("Location"))
	}
	// The folder is offered again by the browser, the databases chosen
	// stay chosen.
	after := getPage(handler, "/accounts?add=1", false).Body.String()
	if strings.Contains(after, ">shop</a>") || strings.Contains(after, `name="folder" value="`+shop+`"`) {
		t.Errorf("after removing, the folder is still on the list: %s", firstLine(after, `name="folder"`))
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
		t.Error("after removing everything, the page does not go back to the form")
	}
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
	if inside := browse(t, handler, shop); len(inside.Entries) != 1 || inside.Entries[0].Name != "public" {
		t.Errorf("browsing shop = %+v", inside.Entries)
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
	for _, want := range []string{`closest("[data-go]")`, "data-pick", "data-picked"} {
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
// What to back up tables sit in the schedule form where the account
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
		`data-folder-browser data-browse="?p=accounts/browse" data-dir="/"`, "No folder yet",
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
			t.Errorf("the What to back up list lacks %q after the schedule was saved", want)
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
