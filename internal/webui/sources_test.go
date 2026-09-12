package webui

import (
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

	"github.com/shukiv/gniza/internal/node"
	"github.com/shukiv/gniza/internal/nodestore"
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
	mysql := filepath.Join(root, "mysql")
	script := "#!/bin/sh\ncase \"$*\" in *information_schema*) printf 'shop\\t4096\\nblog\\t4096\\n' ;; *) cat >/dev/null ;; esac\n"
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
	for _, want := range []string{"What to back up", "Nothing is backed up yet", shop,
		`name="mysql" value="shop"`, `name="mysql" value="blog"`, "PostgreSQL: no databases to offer"} {
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
					Folders []string
					MySQL   []string
				}
			}
		}
	}
	if err := json.Unmarshal(getPage(handler, "/accounts?add=1", true).Body.Bytes(), &answered); err != nil {
		t.Fatal(err)
	}
	if answered.Data.Choose == nil || strings.Join(answered.Data.Choose.Candidates.MySQL, ",") != "blog,shop" ||
		strings.Join(answered.Data.Choose.Candidates.Folders, ",") != shop {
		t.Errorf("the page as data does not carry the candidates: %+v", answered.Data.Choose)
	}
	// The offer costs a look at the databases and the containers, so the
	// terminal's five-second read of the list does not carry it; it asks
	// with add=1 when a or c is pressed.
	var polled struct {
		Data struct {
			Choose *struct {
				Candidates struct{ Folders, MySQL []string }
			}
		}
	}
	if err := json.Unmarshal(getPage(handler, "/accounts", true).Body.Bytes(), &polled); err != nil {
		t.Fatal(err)
	}
	if polled.Data.Choose == nil || len(polled.Data.Choose.Candidates.Folders)+len(polled.Data.Choose.Candidates.MySQL) != 0 {
		t.Errorf("the list as data looks at the candidates: %+v", polled.Data.Choose)
	}
	if !strings.Contains(body, `closest("[data-fill]")`) {
		t.Error("the page offers folders with data-fill links but nothing fills the field")
	}

	added := postForm(server, handler, "/accounts/add", url.Values{"path": {shop}, "mysql": {"shop"}})
	if added.Code != http.StatusSeeOther || !strings.Contains(added.Header().Get("Location"), "kind=ok") {
		t.Fatalf("adding = %d %s %s", added.Code, added.Header().Get("Location"), added.Body.String())
	}
	body = getPage(handler, "/accounts", false).Body.String()
	for _, want := range []string{">shop</a>", shop, "MySQL: shop", `href="?p=accounts&amp;add=1" data-dialog="add-source" data-dialog-fetch`, "?p=accounts/remove"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page with a source lacks %q", want)
		}
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

	again := postForm(server, handler, "/accounts/add", url.Values{"path": {shop}, "name": {"shop2"}})
	if again.Code != http.StatusOK || !strings.Contains(again.Body.String(), "already backed up as shop") {
		t.Errorf("the same folder twice = %d, %q", again.Code, firstLine(again.Body.String(), "already"))
	}
	nothing := postForm(server, handler, "/accounts/add", url.Values{})
	if nothing.Code != http.StatusOK || !strings.Contains(nothing.Body.String(), "nothing was chosen") {
		t.Errorf("an empty form = %d", nothing.Code)
	}

	removed := postForm(server, handler, "/accounts/remove", url.Values{"account": {"shop"}})
	if removed.Code != http.StatusSeeOther || !strings.Contains(removed.Header().Get("Location"), "kind=ok") {
		t.Fatalf("removing = %d %s", removed.Code, removed.Header().Get("Location"))
	}
	if !strings.Contains(getPage(handler, "/accounts", false).Body.String(), "Nothing is backed up yet") {
		t.Error("after removing, the page does not go back to the form")
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
	refused := postForm(server, handler, "/accounts/add", url.Values{"path": {"/srv"}})
	if refused.Code != http.StatusSeeOther || !strings.Contains(refused.Header().Get("Location"), "kind=error") {
		t.Errorf("adding on a panel server = %d %s", refused.Code, refused.Header().Get("Location"))
	}
}

func firstLine(body, containing string) string {
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, containing) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}
