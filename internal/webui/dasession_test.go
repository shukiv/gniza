package webui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	testDASession  = "admin:1757260800:aBcDeF"
	testDAKey      = "0123456789abcdef0123456789abcdef"
	testDAPassword = "cGxhaW50ZXh0LXBhc3N3b3Jk"
)

// DirectAdmin is the only thing that knows whose session this is, so it
// is asked, with the session replayed exactly as its own plugins replay
// it: the two values as cookies, and nothing else standing in for them.
func TestDirectAdminIsAskedWhoTheSessionBelongsTo(t *testing.T) {
	var seen *http.Request
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Clone(context.Background())
		_, _ = w.Write([]byte("account=ON&username=studio&usertype=user&password=" + testDAPassword))
	}))
	defer server.Close()
	verifier := newDASessionVerifierOver(server.URL, server.Client())

	who, err := verifier.verify(context.Background(), testDASession, testDAKey)
	if err != nil {
		t.Fatalf("a live session was refused: %v", err)
	}
	if who.Username != "studio" || who.UserType != "user" {
		t.Errorf("DirectAdmin said studio/user, this read %+v", who)
	}
	if seen.URL.Path != "/CMD_API_SHOW_USER_CONFIG" {
		t.Errorf("asked %s", seen.URL.Path)
	}
	if seen.Method != http.MethodGet {
		t.Errorf("asked with %s", seen.Method)
	}
	if seen.URL.RawQuery != "" {
		// Naming a user asks about that user rather than about the
		// session, which is the opposite of what this is for.
		t.Errorf("the request named somebody: %q", seen.URL.RawQuery)
	}
	if got := seen.Header.Get("Cookie"); got != "session="+testDASession+"; key="+testDAKey {
		t.Errorf("the session was not replayed as DirectAdmin's own plugins replay it: %q", got)
	}
}

// The answer carries the session's password. It is not what was asked
// for, it is never wanted, and it must not reach anything that keeps it.
func TestTheSessionPasswordIsNotCarriedOutOfTheAnswer(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("error=0&username=&usertype=user&password=" + testDAPassword))
	}))
	defer server.Close()
	verifier := newDASessionVerifierOver(server.URL, server.Client())

	_, err := verifier.verify(context.Background(), testDASession, testDAKey)
	if err == nil {
		t.Fatal("an answer naming nobody was accepted")
	}
	if strings.Contains(err.Error(), testDAPassword) {
		t.Errorf("the refusal carries the session password: %v", err)
	}
}

// An expired session does not get an answer, it gets the login page --
// as a redirect, or as HTML with a 200 on it. Following the redirect
// would replay the session to wherever it points, so it is refused where
// it stands.
func TestAnExpiredSessionIsRefusedRatherThanFollowed(t *testing.T) {
	var asked int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		// What a real DirectAdmin answers an unauthenticated request
		// with: 302 to /evo/login, carrying the path back.
		if r.URL.Path == "/CMD_API_SHOW_USER_CONFIG" {
			http.Redirect(w, r,
				"/evo/login?return-to=%2FCMD_API_SHOW_USER_CONFIG", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("error=0&username=somebody&usertype=admin"))
	}))
	defer server.Close()
	verifier := newDASessionVerifierOver(server.URL, server.Client())

	if _, err := verifier.verify(context.Background(), testDASession, testDAKey); err == nil {
		t.Fatal("a session that was redirected to the login page was accepted")
	}
	if asked != 1 {
		t.Errorf("the session was replayed to the redirect target: %d requests", asked)
	}
}

// Everything DirectAdmin can answer that is not one live session
// belonging to one nameable account.
func TestOnlyAnAnswerNamingOneAccountIsAccepted(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{"DirectAdmin said no", 200, "error=1&text=Unable%20to%20find%20session"},
		{"the login page", 200, "<html><body>Please log in</body></html>"},
		{"an error page", 500, "error=0&username=studio&usertype=user"},
		{"nobody named", 200, "error=0&username=&usertype=user"},
		{"no type", 200, "error=0&username=studio&usertype="},
		{"a type nobody has", 200, "error=0&username=studio&usertype=superuser"},
		{"a name that is a path", 200, "error=0&username=../../etc&usertype=user"},
		{"two accounts named", 200, "error=0&username=studio&username=another&usertype=user"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			verifier := newDASessionVerifierOver(server.URL, server.Client())

			who, err := verifier.verify(context.Background(), testDASession, testDAKey)
			if err == nil {
				t.Fatalf("accepted, as %+v", who)
			}
		})
	}
}

// DirectAdmin's documentation describes error=1 as how it says no. It
// does not promise that a live session says error=0, and an answer that
// names one account is an answer whether or not the field is there.
// Refusing it would refuse every real login on a version that omits it.
func TestAnAnswerWithNoErrorFieldStillNamesTheAccount(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("username=studio&usertype=user"))
	}))
	defer server.Close()
	verifier := newDASessionVerifierOver(server.URL, server.Client())

	who, err := verifier.verify(context.Background(), testDASession, testDAKey)
	if err != nil {
		t.Fatalf("an answer naming the account was refused for saying nothing about error: %v", err)
	}
	if who.Username != "studio" {
		t.Errorf("read %+v", who)
	}
}

// An account type DirectAdmin does have is read as itself, because what
// a page may do depends on which one it is.
func TestEachAccountTypeIsReadAsItself(t *testing.T) {
	for _, want := range []string{"admin", "reseller", "user"} {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("error=0&username=studio&usertype=" + want))
		}))
		verifier := newDASessionVerifierOver(server.URL, server.Client())
		who, err := verifier.verify(context.Background(), testDASession, testDAKey)
		server.Close()
		if err != nil {
			t.Fatalf("%s was refused: %v", want, err)
		}
		if who.UserType != want {
			t.Errorf("%s was read as %s", want, who.UserType)
		}
	}
}

// A session value that is not one is refused before it is sent, because
// a newline in a cookie header is a second header.
func TestASessionThatIsNotOneIsNeverSent(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("error=0&username=studio&usertype=admin"))
	}))
	defer server.Close()
	verifier := newDASessionVerifierOver(server.URL, server.Client())

	for _, test := range []struct{ session, key string }{
		{"", testDAKey},
		{testDASession, ""},
		{"admin\r\nX-Real-IP: 1.2.3.4", testDAKey},
		{testDASession, "key\nwith a newline"},
		{strings.Repeat("a", 4097), testDAKey},
	} {
		if _, err := verifier.verify(context.Background(), test.session, test.key); err == nil {
			t.Errorf("session %q key %q was sent", test.session, test.key)
		}
	}
}

// The verifier Gniza builds for a real server is the same one these
// tests exercise: it stops at the first answer and it does not wait
// forever for one.
func TestTheVerifierBuiltForARealPanelStopsAtTheFirstAnswer(t *testing.T) {
	verifier := newDASessionVerifier("https://localhost:2222/")
	if verifier.baseURL != "https://localhost:2222" {
		t.Errorf("base URL is %q", verifier.baseURL)
	}
	if verifier.client.CheckRedirect == nil {
		t.Error("it would follow a redirect to the login page")
	}
	if verifier.client.Timeout == 0 {
		t.Error("it would wait forever for a panel that does not answer")
	}
}

// A refusal says what happened without quoting what DirectAdmin sent
// back. The usual thing that arrives instead of an answer is the login
// page; the thing that must never travel is the session's password.
func TestARefusalQuotesNothingDirectAdminSent(t *testing.T) {
	const secret = "s3cr3t-cookie-value"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html><body>" + secret + "</body></html>"))
	}))
	defer server.Close()
	verifier := newDASessionVerifierOver(server.URL, server.Client())

	_, err := verifier.verify(context.Background(), testDASession, testDAKey)
	if err == nil {
		t.Fatal("the login page was accepted as an answer")
	}
	for _, leaked := range []string{secret, testDASession, testDAKey} {
		if strings.Contains(err.Error(), leaked) {
			t.Errorf("the refusal carries %q: %v", leaked, err)
		}
	}
	if !errors.Is(err, errDASessionDenied) {
		t.Errorf("the refusal is not a refusal: %v", err)
	}
}
