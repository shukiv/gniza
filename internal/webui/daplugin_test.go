package webui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// stubDASessions answers the way a panel would, without one.
type stubDASessions struct {
	who     daPrincipal
	err     error
	session string
	key     string
	asked   int
}

func (s *stubDASessions) verify(_ context.Context, sessionID, sessionKey string) (daPrincipal, error) {
	s.asked++
	s.session, s.key = sessionID, sessionKey
	return s.who, s.err
}

// served reports whether the request reached the page behind the check,
// and what the check made of the session.
func served(t *testing.T, sessions daVerifier, request *http.Request) (int, bool, daPrincipal) {
	t.Helper()
	var reached bool
	var who daPrincipal
	server := &Server{daSessions: sessions, log: slog.New(slog.DiscardHandler)}
	guarded := server.requireDirectAdminAdmin(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reached = true
		who = daPrincipalOf(r)
	}))
	recorder := httptest.NewRecorder()
	guarded.ServeHTTP(recorder, request)
	return recorder.Code, reached, who
}

func daRequest(session, key string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if session != "" {
		req.Header.Set(daSessionHeader, session)
	}
	if key != "" {
		req.Header.Set(daKeyHeader, key)
	}
	return req
}

// The socket is owned by a service account, so the uid on the other end
// says only "DirectAdmin's plugin runner". What decides whether the page
// runs is DirectAdmin's own answer about the session, and nothing else.
func TestOnlyAnAdministratorsSessionReachesThePage(t *testing.T) {
	for _, test := range []struct {
		name    string
		who     daPrincipal
		err     error
		request *http.Request
		reached bool
		status  int
	}{
		{
			name:    "an administrator",
			who:     daPrincipal{Username: "admin", UserType: "admin"},
			request: daRequest(testDASession, testDAKey),
			reached: true, status: http.StatusOK,
		},
		{
			name:    "a reseller",
			who:     daPrincipal{Username: "hostco", UserType: "reseller"},
			request: daRequest(testDASession, testDAKey),
			status:  http.StatusForbidden,
		},
		{
			name:    "a customer",
			who:     daPrincipal{Username: "studio", UserType: "user"},
			request: daRequest(testDASession, testDAKey),
			status:  http.StatusForbidden,
		},
		{
			name:    "a session DirectAdmin does not know",
			err:     errDASessionDenied,
			request: daRequest(testDASession, testDAKey),
			status:  http.StatusForbidden,
		},
		{
			name:    "DirectAdmin not answering",
			err:     errDASessionUnavailable,
			request: daRequest(testDASession, testDAKey),
			status:  http.StatusServiceUnavailable,
		},
		{
			name:    "no session at all",
			who:     daPrincipal{Username: "admin", UserType: "admin"},
			request: daRequest("", ""),
			status:  http.StatusUnauthorized,
		},
		{
			name:    "a session with no key",
			who:     daPrincipal{Username: "admin", UserType: "admin"},
			request: daRequest(testDASession, ""),
			status:  http.StatusUnauthorized,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, reached, who := served(t,
				&stubDASessions{who: test.who, err: test.err}, test.request)
			if reached != test.reached {
				t.Errorf("the page ran: %v, wanted %v", reached, test.reached)
			}
			if status != test.status {
				t.Errorf("answered %d, wanted %d", status, test.status)
			}
			if test.reached && who.Username != test.who.Username {
				t.Errorf("the page was told %+v", who)
			}
		})
	}
}

// A page that cannot be told who is asking does not run. Starting without
// a way to ask is a configuration mistake, and reading it as "let
// everybody in" would hand every customer's backups to whoever opened the
// socket.
func TestWithNothingToAskThePageDoesNotRun(t *testing.T) {
	status, reached, _ := served(t, nil, daRequest(testDASession, testDAKey))
	if reached {
		t.Error("the page ran with no way to check the session")
	}
	if status != http.StatusServiceUnavailable {
		t.Errorf("answered %d", status)
	}
}

// The session travels to DirectAdmin as the plugin was given it. A check
// that asked about something else would be a check about something else.
func TestTheSessionIsPutToDirectAdminUnchanged(t *testing.T) {
	sessions := &stubDASessions{who: daPrincipal{Username: "admin", UserType: "admin"}}
	if _, reached, _ := served(t, sessions, daRequest(testDASession, testDAKey)); !reached {
		t.Fatal("an administrator was refused")
	}
	if sessions.asked != 1 {
		t.Errorf("DirectAdmin was asked %d times", sessions.asked)
	}
	if sessions.session != testDASession || sessions.key != testDAKey {
		t.Error("the session was changed on the way to DirectAdmin")
	}
}

// Every request is checked. A page held open, or a second request on the
// same connection, does not inherit the first request's answer.
func TestEverySeparateRequestIsChecked(t *testing.T) {
	sessions := &stubDASessions{who: daPrincipal{Username: "admin", UserType: "admin"}}
	for range 3 {
		served(t, sessions, daRequest(testDASession, testDAKey))
	}
	if sessions.asked != 3 {
		t.Errorf("3 requests, %d asked about", sessions.asked)
	}
}

// The refusal a browser sees says what to do about it and carries neither
// the session nor anything DirectAdmin said.
func TestARefusedPageSaysNothingAboutTheSession(t *testing.T) {
	server := &Server{
		daSessions: &stubDASessions{err: fmt.Errorf("%w: %s", errDASessionDenied, testDASession)},
		log:        slog.New(slog.DiscardHandler),
	}
	recorder := httptest.NewRecorder()
	server.requireDirectAdminAdmin(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the page ran")
	})).ServeHTTP(recorder, daRequest(testDASession, testDAKey))

	body := recorder.Body.String()
	for _, leaked := range []string{testDASession, testDAKey} {
		if strings.Contains(body, leaked) {
			t.Errorf("the refusal carries the session: %q", body)
		}
	}
}

// The socket belongs to the account DirectAdmin runs the plugin as, and
// to nobody else: its directory admits that account and root, and the
// socket itself is readable by its owner alone.
func TestTheSocketBelongsToTheAccountDirectAdminRunsThePluginAs(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skipf("no current user: %v", err)
	}
	socketPath := filepath.Join(shortTempDir(t), "directadmin", "plugin.sock")

	ctx, cancel := context.WithCancel(context.Background())
	server := &Server{daSessions: &stubDASessions{}, log: slog.New(slog.DiscardHandler)}
	done := make(chan error, 1)
	go func() { done <- server.ListenDirectAdmin(ctx, socketPath, me.Username) }()

	waitForDASocket(t, socketPath, done)
	info, err := os.Stat(socketPath)
	if err != nil {
		t.Fatalf("the socket is not there: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("the socket is mode %o", mode)
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("no owner on the socket")
	}
	if strconv.FormatUint(uint64(owner.Uid), 10) != me.Uid {
		t.Errorf("the socket is owned by uid %d, not by %s", owner.Uid, me.Uid)
	}
	dir, err := os.Stat(filepath.Dir(socketPath))
	if err != nil {
		t.Fatalf("the directory is not there: %v", err)
	}
	if mode := dir.Mode().Perm(); mode != 0o710 {
		t.Errorf("the directory is mode %o, so it is not the owner's alone", mode)
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("serving ended with %v", err)
	}
}

// An owner that is root defeats the whole arrangement, and DirectAdmin
// refuses it in plugin.conf for its own reasons. An owner that is not an
// account cannot own anything. Neither gets as far as a socket.
func TestASocketIsNotOpenedForAnOwnerThatIsNotOne(t *testing.T) {
	for _, owner := range []string{"", "root", "no-such-account-nobody-made"} {
		socketPath := filepath.Join(shortTempDir(t), "directadmin", "plugin.sock")
		server := &Server{daSessions: &stubDASessions{}, log: slog.New(slog.DiscardHandler)}
		err := server.ListenDirectAdmin(context.Background(), socketPath, owner)
		if err == nil {
			t.Errorf("a socket was opened for owner %q", owner)
			continue
		}
		if _, statErr := os.Stat(socketPath); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("owner %q was refused but the socket is there anyway", owner)
		}
	}
}

// Starting with no way to reach DirectAdmin would open a socket whose
// every request is refused, which is worse than not starting.
func TestTheSocketIsNotOpenedWithNothingToAsk(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skipf("no current user: %v", err)
	}
	socketPath := filepath.Join(shortTempDir(t), "directadmin", "plugin.sock")
	server := &Server{log: slog.New(slog.DiscardHandler)}
	if err := server.ListenDirectAdmin(context.Background(), socketPath, me.Username); err == nil {
		t.Error("a socket was opened with no way to check a session")
	}
}

func waitForDASocket(t *testing.T, socket string, listening <-chan error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-listening:
			t.Fatalf("it stopped before listening: %v", err)
		default:
		}
		if conn, err := net.Dial("unix", socket); err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("it never started listening")
}

// shortTempDir keeps a socket path inside the 108 bytes a unix socket
// address has room for. t.TempDir puts the test's own name in the path,
// and these names are long enough to run past it.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gz")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// accountRequest is a request the account socket attributed to one
// account, with or without the DirectAdmin session the plugin carries.
func accountRequest(account string, withSession bool) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request = request.WithContext(context.WithValue(request.Context(), accountKey{}, account))
	if withSession {
		request.Header.Set(daSessionHeader, testDASession)
		request.Header.Set(daKeyHeader, testDAKey)
	}
	return request
}

// On DirectAdmin the account socket has no capability to consume; it has
// the session the plugin was handed, and DirectAdmin's answer has to
// name the account the socket attributed the request to. Under login-as
// DirectAdmin answers as the customer being impersonated, which is the
// same answer. A process merely running as the account has no session
// to carry, and is refused.
func TestTheAccountSocketOnDirectAdminTakesTheAccountsOwnSession(t *testing.T) {
	studio := daPrincipal{Username: "studio", UserType: "user"}
	cases := []struct {
		name     string
		sessions *stubDASessions
		session  bool
		status   int
		asked    int
	}{
		{"a process running as the account, with no session", &stubDASessions{who: studio}, false, http.StatusForbidden, 0},
		{"the account's own session", &stubDASessions{who: studio}, true, http.StatusOK, 1},
		{"an administrator's own account, at the account level",
			&stubDASessions{who: daPrincipal{Username: "studio", UserType: "admin"}}, true, http.StatusOK, 1},
		{"another account's session", &stubDASessions{who: daPrincipal{Username: "other", UserType: "user"}}, true, http.StatusForbidden, 1},
		{"an administrator's session for somebody else",
			&stubDASessions{who: daPrincipal{Username: "admin", UserType: "admin"}}, true, http.StatusForbidden, 1},
		{"a session DirectAdmin refused", &stubDASessions{err: fmt.Errorf("%w: it reported no such session", errDASessionDenied)}, true, http.StatusForbidden, 1},
		{"a panel that did not answer", &stubDASessions{err: fmt.Errorf("%w: timeout", errDASessionUnavailable)}, true, http.StatusServiceUnavailable, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			server := &Server{daSessions: c.sessions, log: slog.New(slog.DiscardHandler)}
			var reached bool
			var who daPrincipal
			guarded := server.requireUserCapability(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				reached = true
				who = daPrincipalOf(r)
			}))
			response := httptest.NewRecorder()
			guarded.ServeHTTP(response, accountRequest("studio", c.session))
			if response.Code != c.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, c.status, response.Body.String())
			}
			if reached != (c.status == http.StatusOK) {
				t.Errorf("reached the page = %v", reached)
			}
			if c.sessions.asked != c.asked {
				t.Errorf("DirectAdmin was asked %d times, want %d", c.sessions.asked, c.asked)
			}
			if reached && who.Username != "studio" {
				t.Errorf("the page was not told who DirectAdmin said is asking: %+v", who)
			}
			if body := response.Body.String(); strings.Contains(body, testDASession) || strings.Contains(body, testDAKey) {
				t.Errorf("the session reached the page: %q", body)
			}
		})
	}
}

// The capability endpoint is cPanel's: on DirectAdmin nothing issues one,
// and asking for one is asking the account socket for a page it does not
// have.
func TestTheAccountSocketOnDirectAdminIssuesNoCapability(t *testing.T) {
	sessions := &stubDASessions{who: daPrincipal{Username: "studio", UserType: "user"}}
	server := &Server{daSessions: sessions, log: slog.New(slog.DiscardHandler)}
	request := httptest.NewRequest(http.MethodPost, capabilityEndpoint, nil)
	request = request.WithContext(context.WithValue(request.Context(), accountKey{}, "studio"))
	request.Header.Set(daSessionHeader, testDASession)
	request.Header.Set(daKeyHeader, testDAKey)
	response := httptest.NewRecorder()
	server.UserHandler().ServeHTTP(response, request)
	// The session is checked, the page is then not one the socket has:
	// an answer other than 404 would mean the request reached the
	// cPanel endpoint, which has no business on DirectAdmin.
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", response.Code, response.Body.String())
	}
}

// The page is named in the query and reaches the real routes behind the
// real check. A form posted to p=restore lands on the restore route and
// not on the overview, which answers GET only; the route itself then
// refuses for a reason of its own (no engine here), which is further
// than a wrong route gets.
func TestDirectAdminQueryRoutingReachesTheRealRoutes(t *testing.T) {
	server, err := New(nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	server.daSessions = &stubDASessions{who: daPrincipal{Username: "studio", UserType: "user"}}
	handler := server.UserHandler()
	send := func(method, target string) int {
		request := httptest.NewRequest(method, target, strings.NewReader("csrf=x"))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request = request.WithContext(context.WithValue(request.Context(), accountKey{}, "studio"))
		request.Header.Set(daSessionHeader, testDASession)
		request.Header.Set(daKeyHeader, testDAKey)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response.Code
	}
	if code := send(http.MethodPost, "/?p=restore"); code == http.StatusNotFound || code == http.StatusMethodNotAllowed {
		t.Errorf("POST ?p=restore did not reach the restore route: %d", code)
	}
	if code := send(http.MethodGet, "/?p=restore"); code != http.StatusMethodNotAllowed {
		t.Errorf("GET ?p=restore = %d, want 405", code)
	}
	if code := send(http.MethodGet, "/?p=nowhere"); code != http.StatusNotFound {
		t.Errorf("GET ?p=nowhere = %d, want 404", code)
	}
	if code := send(http.MethodGet, "/?p=browse&repository=r"); code == http.StatusNotFound || code == http.StatusMethodNotAllowed {
		t.Errorf("GET ?p=browse did not reach the browse route: %d", code)
	}
}

// A process guessing sessions is refused by DirectAdmin a few times and
// then by this socket alone: DirectAdmin is not asked again for the
// rest of the minute. A session DirectAdmin did accept clears the count,
// and a panel that did not answer counts against nobody.
func TestRepeatedlyRefusedSessionsAreNotPutToDirectAdminAgain(t *testing.T) {
	sessions := &stubDASessions{err: fmt.Errorf("%w: no", errDASessionDenied)}
	now := time.Unix(1_800_000_000, 0)
	server := &Server{daSessions: sessions, log: slog.New(slog.DiscardHandler)}
	server.daRefusals.now = func() time.Time { return now }
	guarded := server.requireUserCapability(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	ask := func() int {
		response := httptest.NewRecorder()
		guarded.ServeHTTP(response, accountRequest("studio", true))
		return response.Code
	}
	for i := 0; i < daRefusalsAllowed; i++ {
		if code := ask(); code != http.StatusForbidden {
			t.Fatalf("refusal %d = %d, want 403", i+1, code)
		}
	}
	if code := ask(); code != http.StatusTooManyRequests {
		t.Fatalf("after %d refusals = %d, want 429", daRefusalsAllowed, code)
	}
	if sessions.asked != daRefusalsAllowed {
		t.Fatalf("DirectAdmin was asked %d times, want %d", sessions.asked, daRefusalsAllowed)
	}
	// Another account is not held to this one's count.
	response := httptest.NewRecorder()
	guarded.ServeHTTP(response, accountRequest("other", true))
	if response.Code != http.StatusForbidden || sessions.asked != daRefusalsAllowed+1 {
		t.Errorf("another account: %d after %d asks", response.Code, sessions.asked)
	}
	// The window passes and DirectAdmin is asked again.
	now = now.Add(daRefusalWindow)
	if code := ask(); code != http.StatusForbidden {
		t.Errorf("after the window = %d, want 403", code)
	}
	// A real session clears the count; a panel that does not answer
	// adds nothing to it.
	sessions.err, sessions.who = nil, daPrincipal{Username: "studio", UserType: "user"}
	if code := ask(); code != http.StatusOK {
		t.Fatalf("real session = %d", code)
	}
	sessions.err = fmt.Errorf("%w: timeout", errDASessionUnavailable)
	for i := 0; i < daRefusalsAllowed+2; i++ {
		if code := ask(); code != http.StatusServiceUnavailable {
			t.Fatalf("unavailable %d = %d, want 503", i+1, code)
		}
	}
}

// A DirectAdmin plugin has one address, and names the page in its query.
func TestDirectAdminNamesTheAccountPageInTheQuery(t *testing.T) {
	cases := []struct {
		target string
		path   string
		query  string
		status int
	}{
		{"/", "/", "", http.StatusOK},
		{"/?p=index&kind=ok&msg=Started", "/", "p=index&kind=ok&msg=Started", http.StatusOK},
		{"/?p=browse&repository=repo&snapshot=abc", "/browse", "p=browse&repository=repo&snapshot=abc", http.StatusOK},
		{"/?p=logs", "/logs", "p=logs", http.StatusOK},
		{"/?p=restore", "/restore", "p=restore", http.StatusOK},
		{"/?p=settings", "", "", http.StatusNotFound},
		{"/browse", "", "", http.StatusNotFound},
	}
	for _, c := range cases {
		var path, query string
		routed := directAdminPages(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			path, query = r.URL.Path, r.URL.RawQuery
		}))
		response := httptest.NewRecorder()
		routed.ServeHTTP(response, httptest.NewRequest(http.MethodGet, c.target, nil))
		if response.Code != c.status {
			t.Errorf("%s: status = %d, want %d", c.target, response.Code, c.status)
		}
		if path != c.path || query != c.query {
			t.Errorf("%s: routed to %q?%q, want %q?%q", c.target, path, query, c.path, c.query)
		}
	}
}
