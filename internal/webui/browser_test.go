package webui

import (
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

	"github.com/shukiv/gniza/internal/cpanel"
	"github.com/shukiv/gniza/internal/node"
	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/vault"
)

// A browser door with a password set, serving over plain HTTP as a
// loopback listener would, in front of a node with a synthetic panel.
func newBrowserServer(t *testing.T, password string) (*Server, http.Handler) {
	t.Helper()
	root := t.TempDir()
	file := filepath.Join(root, "web", "password")
	if err := SetPassword(file, password); err != nil {
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
	engine, err := node.New(node.Config{
		Store: store, Vault: v,
		Provider:  &cpanel.Fake{Root: filepath.Join(root, "panel")},
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		HookSpool: filepath.Join(root, "hooks"),
	})
	if err != nil {
		t.Fatal(err)
	}

	server, err := New(engine, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithBrowser(BrowserConfig{
			Listen: "127.0.0.1:0", PasswordFile: file, Hostname: "plain.example.net",
		}))
	if err != nil {
		t.Fatal(err)
	}
	return server, server.browserHandler()
}

func signIn(t *testing.T, server *Server, handler http.Handler, password, from string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"csrf": {server.csrfToken}, "password": {password}}
	request := httptest.NewRequest(http.MethodPost, "/?p=login", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.RemoteAddr = from
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func sessionCookie(t *testing.T, recorder *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == browserCookie && cookie.Value != "" {
			return cookie
		}
	}
	t.Fatal("no session cookie was set")
	return nil
}

// TestSetPasswordRefusesWhatBcryptCannotKeep: too short is not worth
// having, too long is silently cut by bcrypt, and both are refused.
func TestSetPasswordRefusesWhatBcryptCannotKeep(t *testing.T) {
	file := filepath.Join(t.TempDir(), "password")
	if err := SetPassword(file, "short"); err == nil {
		t.Error("a five-character password was accepted")
	}
	if err := SetPassword(file, strings.Repeat("x", 73)); err == nil {
		t.Error("a 73-byte password was accepted")
	}
	if err := SetPassword(file, "correct horse battery"); err != nil {
		t.Errorf("a sound password was refused: %v", err)
	}
}

// TestTheDoorSendsAStrangerToSignIn: without a session every page is the
// sign-in page, and a background refresh is told rather than redirected.
func TestTheDoorSendsAStrangerToSignIn(t *testing.T) {
	_, handler := newBrowserServer(t, "correct horse battery")

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/?p=accounts", nil))
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "?p=login" {
		t.Errorf("a stranger asking for accounts got %d %q, not a redirect to sign in",
			recorder.Code, recorder.Header().Get("Location"))
	}

	live := httptest.NewRequest(http.MethodGet, "/?p=", nil)
	live.Header.Set("X-Gniza-Live", "1")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, live)
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("a background refresh from a stranger got %d, not 401", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/?p=login", nil))
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, "<!doctype html>") ||
		!strings.Contains(body, `name="password"`) || !strings.Contains(body, "plain.example.net") {
		t.Errorf("the sign-in page is not a document with a password field naming the server:\n%s", body[:min(len(body), 600)])
	}
	if strings.Contains(body, "data-rail") {
		t.Error("the sign-in page shows the rail to somebody not signed in")
	}
}

// TestTheRightPasswordOpensADocument: a sign-in sets a session cookie,
// and with it the pages arrive as whole documents with a sign-out.
func TestTheRightPasswordOpensADocument(t *testing.T) {
	server, handler := newBrowserServer(t, "correct horse battery")

	recorder := signIn(t, server, handler, "correct horse battery", "203.0.113.5:40000")
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "?p=" {
		t.Fatalf("a right password got %d %q", recorder.Code, recorder.Header().Get("Location"))
	}
	cookie := sessionCookie(t, recorder)
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("the session cookie is not HttpOnly and SameSite=Strict: %+v", cookie)
	}
	if cookie.Secure {
		t.Error("a loopback listener marked its cookie Secure, which plain HTTP would drop")
	}

	request := httptest.NewRequest(http.MethodGet, "/?p=settings", nil)
	request.AddCookie(cookie)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK {
		t.Fatalf("settings with a session got %d", recorder.Code)
	}
	for _, want := range []string{
		"<!doctype html>", "<title>Settings · Gniza</title>", `name="viewport"`,
		"</html>", `action="?p=logout"`, "data-rail",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page through the door lacks %q", want)
		}
	}
	if recorder.Header().Get("X-Frame-Options") != "DENY" {
		t.Error("a page on a port can be framed")
	}

	// The socket's own handler is untouched: still a fragment, no door.
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/?p=settings", nil))
	if recorder.Code != http.StatusOK || strings.Contains(recorder.Body.String(), "<html") ||
		strings.Contains(recorder.Body.String(), "?p=logout") {
		t.Error("the socket's page grew a document or a sign-out")
	}

	// Signing out ends the session.
	form := url.Values{"csrf": {server.csrfToken}}
	out := httptest.NewRequest(http.MethodPost, "/?p=logout", strings.NewReader(form.Encode()))
	out.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	out.AddCookie(cookie)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, out)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("sign-out got %d", recorder.Code)
	}
	request = httptest.NewRequest(http.MethodGet, "/?p=settings", nil)
	request.AddCookie(cookie)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther {
		t.Errorf("a signed-out session still opened settings: %d", recorder.Code)
	}
}

// TestWrongPasswordsLockTheAddressOut: the fifth wrong password from one
// address starts a wait, a right one during the wait is refused too, and
// another address is unaffected.
func TestWrongPasswordsLockTheAddressOut(t *testing.T) {
	server, handler := newBrowserServer(t, "correct horse battery")

	for i := 1; i <= lockoutAfter; i++ {
		recorder := signIn(t, server, handler, "not it", "198.51.100.9:1000")
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("wrong password %d got %d", i, recorder.Code)
		}
		if strings.Contains(recorder.Body.String(), "data-rail") {
			t.Fatal("a refused sign-in drew the rail")
		}
	}
	recorder := signIn(t, server, handler, "correct horse battery", "198.51.100.9:1001")
	if recorder.Code != http.StatusTooManyRequests {
		t.Errorf("the right password during a lockout got %d, not 429", recorder.Code)
	}
	if wait := server.browser.lockedFor("198.51.100.9"); wait <= 0 || wait > lockoutBase {
		t.Errorf("the first wait is %v, not up to %v", wait, lockoutBase)
	}

	recorder = signIn(t, server, handler, "correct horse battery", "203.0.113.5:2000")
	if recorder.Code != http.StatusSeeOther {
		t.Errorf("another address was locked out too: %d", recorder.Code)
	}
}

// TestASessionExpiresIdle: a session not used for longer than the idle
// limit is gone, whatever its cookie says.
func TestASessionExpiresIdle(t *testing.T) {
	server, handler := newBrowserServer(t, "correct horse battery")
	cookie := sessionCookie(t, signIn(t, server, handler, "correct horse battery", "203.0.113.5:3000"))

	server.browser.mu.Lock()
	server.browser.sessions[cookie.Value].seen = time.Now().Add(-sessionIdle - time.Minute)
	server.browser.mu.Unlock()

	request := httptest.NewRequest(http.MethodGet, "/?p=", nil)
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther {
		t.Errorf("an idle session still opened the overview: %d", recorder.Code)
	}
}

// TestTheCertificateIsMadeOnceAndKept: with neither file there a
// self-signed pair is written and used; on the next start the same pair
// is loaded rather than replaced, so the fingerprint the operator
// accepted stays good; half a pair is refused.
func TestTheCertificateIsMadeOnceAndKept(t *testing.T) {
	dir := t.TempDir()
	cfg := BrowserConfig{
		CertFile: filepath.Join(dir, "web", "cert.pem"), KeyFile: filepath.Join(dir, "web", "key.pem"),
		Hostname: "plain.example.net",
	}
	first, print1, err := browserCertificate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Certificate) == 0 || len(print1) != 64 {
		t.Fatalf("no certificate or fingerprint came back: %d %q", len(first.Certificate), print1)
	}
	info, err := os.Stat(cfg.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the key is mode %o, not 0600", info.Mode().Perm())
	}
	_, print2, err := browserCertificate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if print2 != print1 {
		t.Error("a second start made a new certificate instead of keeping the first")
	}
	if err := os.Remove(cfg.KeyFile); err != nil {
		t.Fatal(err)
	}
	if _, _, err := browserCertificate(cfg); err == nil {
		t.Error("a certificate without its key was accepted")
	}
}

// TestTheDoorRefusesToOpenWithoutAPassword: a listener with nothing to
// check against does not listen.
func TestTheDoorRefusesToOpenWithoutAPassword(t *testing.T) {
	server, err := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithBrowser(BrowserConfig{
			Listen: "127.0.0.1:0", PasswordFile: filepath.Join(t.TempDir(), "missing"),
		}))
	if err != nil {
		t.Fatal(err)
	}
	err = server.ListenBrowser(t.Context())
	if err == nil || !strings.Contains(err.Error(), "-web-set-password") {
		t.Errorf("listening without a password: %v", err)
	}
}
