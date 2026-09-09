package webui_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/cpanel"
	"github.com/shukiv/gniza/internal/node"
	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/vault"
	"github.com/shukiv/gniza/internal/webui"
)

// directAdminProvider is a cPanel fake that answers to the other panel's
// name. Everything the pages under test read is the same on both; what
// they say about it was not.
type directAdminProvider struct{ *cpanel.Fake }

func (directAdminProvider) Name() string { return "DirectAdmin" }

// TestThePagesNameThePanelTheyAreServedOn covers copy that was wrong on
// every DirectAdmin server: the same templates serve both panels, and they
// said cPanel on both.
func TestThePagesNameThePanelTheyAreServedOn(t *testing.T) {
	root := t.TempDir()
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
	engine, err := node.New(node.Config{
		Store: store, Vault: v,
		Provider:  directAdminProvider{&cpanel.Fake{Root: filepath.Join(root, "panel")}},
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		HookSpool: filepath.Join(root, "hooks"),
	})
	if err != nil {
		t.Fatal(err)
	}
	server, err := webui.New(engine, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/?p=accounts", "/?p=settings", "/?p=logs&tab=lifecycle"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path, nil).WithContext(context.Background())
		server.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, recorder.Code)
		}
		body := recorder.Body.String()
		if !strings.Contains(body, "DirectAdmin") {
			t.Errorf("GET %s never names the panel it is served on", path)
		}
	}
}
