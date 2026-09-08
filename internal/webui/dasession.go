package webui

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	// daSessionEndpoint is DirectAdmin's own answer to whose session a
	// pair of session values belongs to.
	//
	// Asked with no parameters, it answers about the session doing the
	// asking, which is the question here. Naming a user would ask about
	// that user instead, and an administrator's session may name anyone.
	//
	// CMD_API_GET_SESSION is the endpoint whose name says it answers
	// this, and it is not the one used. Two reasons, both from a real
	// 1.709 host: it refuses a GET -- "The requested command requires
	// POST but GET was used" -- and its answer carries the session's
	// password. This one answers a GET and carries no password at all,
	// so nothing here ever holds one.
	daSessionEndpoint = "/CMD_API_SHOW_USER_CONFIG"
	// daCACertPath is the certificate DirectAdmin serves its own panel
	// with. A server whose panel has a certificate from a public
	// authority verifies through the system pool without it; one still
	// on the self-signed certificate DirectAdmin installs verifies
	// through this. Neither is a reason to stop verifying.
	daCACertPath = "/usr/local/directadmin/conf/cacert.pem"
	// maxDASessionValue is longer than any session identifier or key
	// DirectAdmin issues, and short enough that nothing large is ever
	// put in a header.
	maxDASessionValue = 4096
	// maxDASessionAnswer bounds the reply. A login page is the usual
	// thing that arrives instead of an answer, and it is not large.
	maxDASessionAnswer = 64 << 10
	daSessionTimeout   = 10 * time.Second
)

var (
	errDASessionDenied      = errors.New("DirectAdmin did not accept the session")
	errDASessionUnavailable = errors.New("DirectAdmin session validation is unavailable")
)

// daPrincipal is who DirectAdmin says a session belongs to.
//
// It carries the two fields the answer is asked for and none of the
// others. DirectAdmin also returns the session's password, base64
// encoded, which is not wanted here and must not travel any further than
// the parse that discards it.
type daPrincipal struct {
	// Username is the account the session is logged in as. Under
	// DirectAdmin's login-as, that is the customer being impersonated
	// rather than whoever is at the keyboard: it is the right answer to
	// "what may this session do" and not an answer to "who is doing it".
	Username string
	// UserType is admin, reseller or user.
	UserType string
}

// daSessionVerifier turns a DirectAdmin panel session into the account it
// belongs to by asking DirectAdmin.
//
// A plugin script is handed SESSION_ID and SESSION_KEY in its
// environment and cannot itself prove anything about them: a process
// merely running as an account has neither value, and a request that
// carries them is only worth what DirectAdmin says it is. So they are
// replayed to DirectAdmin's own API, exactly as DirectAdmin's own
// plugins replay them, and only DirectAdmin's answer is believed. See
// ADR 0019.
//
// Both values are bearer credentials for as long as the session lives.
// They are never logged, never persisted and never put in an error.
type daSessionVerifier struct {
	baseURL string
	client  *http.Client
}

// newDASessionVerifier talks to the panel at baseURL, which is
// DirectAdmin's own address on this server -- https://<host>:2222.
//
// The host has to be the name DirectAdmin's certificate carries, not an
// address that reaches the same machine: the certificate is checked, and
// a panel with a real certificate for its hostname does not answer to
// 127.0.0.1. That name is DirectAdmin's own to say, so it is read from
// its configuration rather than assumed here.
func newDASessionVerifier(baseURL string) *daSessionVerifier {
	return newDASessionVerifierOver(baseURL, &http.Client{
		// Deliberately not http.DefaultTransport, and deliberately
		// without a Proxy: DirectAdmin is on this machine, and an
		// HTTPS_PROXY in the environment would send a live session
		// credential through whatever it names.
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
				RootCAs:    daRoots(),
			},
		},
	})
}

// newDASessionVerifierOver is the same over a client somebody else built,
// which is how a test reaches a panel that is not on this machine.
//
// The two things that make the request safe rather than merely correct
// are set here rather than left to the caller: a session is replayed
// once and never followed to a second address, and a panel that does not
// answer does not hold the page open.
func newDASessionVerifierOver(baseURL string, client *http.Client) *daSessionVerifier {
	client.CheckRedirect = refuseRedirect
	client.Timeout = daSessionTimeout
	return &daSessionVerifier{baseURL: strings.TrimSuffix(baseURL, "/"), client: client}
}

// daRoots is the system pool with DirectAdmin's own certificate added.
//
// Adding it is what lets a server still on the certificate DirectAdmin
// installs for itself be verified rather than skipped. A pool that
// cannot be read is returned as nil, which means the system pool alone:
// fewer certificates trusted, never more.
func daRoots() *x509.CertPool {
	pem, err := os.ReadFile(daCACertPath)
	if err != nil {
		return nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	pool.AppendCertsFromPEM(pem)
	return pool
}

// refuseRedirect stops at the first answer.
//
// An expired session is answered with a redirect to the login page.
// Following it would replay the session values to wherever the redirect
// points, and would turn a refusal into a slower refusal at best.
func refuseRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// verify asks DirectAdmin whose session this is.
func (v *daSessionVerifier) verify(ctx context.Context, sessionID, sessionKey string) (daPrincipal, error) {
	if err := usableDASessionValue(sessionID); err != nil {
		return daPrincipal{}, err
	}
	if err := usableDASessionValue(sessionKey); err != nil {
		return daPrincipal{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.baseURL+daSessionEndpoint, nil)
	if err != nil {
		return daPrincipal{}, fmt.Errorf("%w: %v", errDASessionUnavailable, err)
	}
	// The two values as one Cookie header, which is how DirectAdmin's own
	// plugins hand a session back to it.
	req.Header.Set("Cookie", "session="+sessionID+"; key="+sessionKey)
	resp, err := v.client.Do(req)
	if err != nil {
		// A transport error can quote the request URL but never the
		// header, so nothing here carries the session.
		return daPrincipal{}, fmt.Errorf("%w: %v", errDASessionUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return daPrincipal{}, fmt.Errorf("%w: it answered %s", errDASessionDenied, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDASessionAnswer+1))
	if err != nil {
		return daPrincipal{}, fmt.Errorf("%w: %v", errDASessionUnavailable, err)
	}
	if len(body) > maxDASessionAnswer {
		return daPrincipal{}, fmt.Errorf("%w: its answer is too long to be one", errDASessionDenied)
	}
	return readDASession(string(body))
}

// readDASession reads DirectAdmin's answer.
//
// The answer is a url-encoded set of fields. Nothing from it reaches an
// error message: an answer that is not one is usually the login page,
// and one that is carries the session's password.
func readDASession(body string) (daPrincipal, error) {
	fields, err := url.ParseQuery(body)
	if err != nil {
		return daPrincipal{}, fmt.Errorf("%w: its answer is not one", errDASessionDenied)
	}
	// First, before anything reads the rest. This endpoint does not
	// answer with a password, and the deletion is here anyway: it costs
	// one line, and the day somebody points this at an endpoint that
	// does is the day it matters.
	delete(fields, "password")

	// DirectAdmin says no with error=1, and says it that way for a
	// refused session and for a malformed request alike. A real answer
	// from this endpoint carries no error field at all, so an absent one
	// is read as an answer rather than as a refusal. Nothing is lost:
	// what keeps a refusal and the login page out is that neither names
	// an account, below.
	if reported, present := fields["error"]; present && (len(reported) != 1 || reported[0] != "0") {
		return daPrincipal{}, fmt.Errorf("%w: it reported no such session", errDASessionDenied)
	}
	// Exactly one of each. A repeated field is an answer that names two
	// accounts, and picking either of them is picking one at random.
	if len(fields["username"]) != 1 || len(fields["usertype"]) != 1 {
		return daPrincipal{}, fmt.Errorf("%w: its answer does not name one account", errDASessionDenied)
	}
	who := daPrincipal{Username: fields.Get("username"), UserType: fields.Get("usertype")}
	if err := usableDAAccountName(who.Username); err != nil {
		return daPrincipal{}, fmt.Errorf("%w: it named no account", errDASessionDenied)
	}
	switch who.UserType {
	case "admin", "reseller", "user":
	default:
		// Not quoted: an answer that is not one of the three is an
		// answer that is not DirectAdmin's.
		return daPrincipal{}, fmt.Errorf("%w: it named an account type that is not one", errDASessionDenied)
	}
	return who, nil
}

// usableDASessionValue refuses anything that is not one session value.
//
// It becomes part of a Cookie header, so a newline in it is a second
// header and a control character in it is a request that is not the one
// this meant to make. The value itself never appears in the refusal.
func usableDASessionValue(value string) error {
	if value == "" {
		return fmt.Errorf("%w: it was asked about no session", errDASessionDenied)
	}
	if len(value) > maxDASessionValue {
		return fmt.Errorf("%w: the session value is too long to be one", errDASessionDenied)
	}
	// A cookie's value is printable ASCII without space, and without the
	// three characters that end one or start another. Real DirectAdmin
	// session identifiers are narrower still -- letters, digits and
	// underscore -- but this is the boundary the header itself draws,
	// and drawing it wider than DirectAdmin does costs nothing here.
	for _, char := range value {
		if char <= 0x20 || char > 0x7e || char == ';' || char == ',' || char == '"' {
			return fmt.Errorf("%w: the session value is not one", errDASessionDenied)
		}
	}
	return nil
}

// usableDAAccountName refuses anything that is not a DirectAdmin
// username, because the name becomes a path and a database prefix.
func usableDAAccountName(user string) error {
	if user == "" || len(user) > 64 || user[0] == '-' {
		return fmt.Errorf("%q is not an account name", user)
	}
	for _, char := range user {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z',
			char >= '0' && char <= '9', char == '-', char == '_':
		default:
			return fmt.Errorf("%q is not an account name", user)
		}
	}
	return nil
}
