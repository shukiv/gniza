package webui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/shukiv/gniza/internal/certs"
)

// The browser interface is the operator's pages on a TCP port, for a
// server that has no panel to put them behind.
//
// The root-only socket's mode is its authorization: a connection means a
// process that is already root. A port carries no such thing, so this
// listener has to bring its own -- a password, a session, a lockout and,
// off the loopback interface, TLS. It serves the same handlers as the
// socket; the only thing it adds in front of them is the door. See
// docs/adr/0024.

// BrowserConfig says where the browser interface listens and what it
// takes to get in.
type BrowserConfig struct {
	// Listen is the host:port to bind. A loopback host is served over
	// plain HTTP, for an ssh tunnel or a reverse proxy on the same
	// machine; anything else is served over TLS.
	Listen string
	// PasswordFile holds one bcrypt hash: the operator's password. It is
	// read on every sign-in, so a new password applies at once.
	PasswordFile string
	// CertFile and KeyFile are the TLS pair. When neither exists a
	// self-signed pair is written there, once.
	CertFile string
	KeyFile  string
	// Hostname is what the self-signed certificate is issued to, and what
	// the sign-in page says it is the door to.
	Hostname string
}

// WithBrowser gives the interface a browser door. Nothing listens until
// ListenBrowser is called.
func WithBrowser(cfg BrowserConfig) Option {
	return func(s *Server) {
		s.browser = &browserDoor{
			cfg:      cfg,
			sessions: map[string]*browserSession{},
			failures: map[string]*loginFailures{},
		}
	}
}

// SetPassword writes the browser interface's password, hashed, where
// the service reads it. It refuses a password too short to be worth
// having and one too long for bcrypt to read all of.
func SetPassword(path, password string) error {
	if len(password) < minPasswordLength {
		return fmt.Errorf("the password needs at least %d characters", minPasswordLength)
	}
	if len(password) > maxPasswordLength {
		return fmt.Errorf("the password can have at most %d bytes", maxPasswordLength)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return fmt.Errorf("hash the password: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	// Written beside its target and renamed into place, so a sign-in that
	// reads the file mid-write sees the old hash or the new one, never a
	// fragment of either.
	temp, err := os.CreateTemp(dir, ".password.*")
	if err != nil {
		return fmt.Errorf("write the password: %w", err)
	}
	defer os.Remove(temp.Name())
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write the password: %w", err)
	}
	if _, err := temp.Write(append(hash, '\n')); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write the password: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("write the password: %w", err)
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		return fmt.Errorf("put the password in place: %w", err)
	}
	return nil
}

const (
	minPasswordLength = 10
	// bcrypt reads the first 72 bytes and no more; a longer password
	// would be accepted with its tail ignored, which is worse than a
	// refusal.
	maxPasswordLength = 72
	bcryptCost        = 12

	browserCookie = "gniza_session"
	// A session ends twelve hours after it began however busy it was,
	// and an hour after it was last used.
	sessionLife = 12 * time.Hour
	sessionIdle = time.Hour
	// After this many wrong passwords from one address the next attempt
	// waits, and the wait doubles with every failure after it.
	lockoutAfter = 5
	lockoutBase  = 30 * time.Second
	lockoutMax   = 15 * time.Minute
)

type browserDoor struct {
	cfg BrowserConfig
	// tls says the listener is serving TLS, which decides whether the
	// session cookie may travel over plain HTTP.
	tls bool

	mu       sync.Mutex
	sessions map[string]*browserSession
	failures map[string]*loginFailures
}

type browserSession struct {
	began time.Time
	seen  time.Time
}

type loginFailures struct {
	count int
	last  time.Time
	until time.Time
}

// documentKey marks a request that arrived through the browser door,
// where no panel wraps the page and the layout has to be a document of
// its own.
type documentKey struct{}

func documentOf(r *http.Request) bool {
	value, _ := r.Context().Value(documentKey{}).(bool)
	return value
}

// ListenBrowser serves the operator's interface on the configured TCP
// address, behind a password.
//
// It returns rather than listens when there is no password to check
// against: a port with no door is not a browser interface, it is the
// socket's contents on the network.
func (s *Server) ListenBrowser(ctx context.Context) error {
	door := s.browser
	if door == nil || door.cfg.Listen == "" {
		return errors.New("webui: the browser interface has no address to listen on")
	}
	if _, err := os.Stat(door.cfg.PasswordFile); err != nil {
		return fmt.Errorf("webui: the browser interface needs a password before it listens; "+
			"set one with gniza-agent -web-set-password: %w", err)
	}
	host, _, err := net.SplitHostPort(door.cfg.Listen)
	if err != nil {
		return fmt.Errorf("webui: -web-listen %q is not host:port: %w", door.cfg.Listen, err)
	}
	door.tls = !isLoopback(host)

	server := &http.Server{
		Handler:           s.browserHandler(),
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       60 * time.Second,
		// Browsing a snapshot waits on restic, which waits on the
		// network.
		WriteTimeout: 5 * time.Minute,
	}
	fingerprint := ""
	if door.tls {
		certificate, print, err := browserCertificate(door.cfg)
		if err != nil {
			return err
		}
		fingerprint = print
		server.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{certificate},
			MinVersion:   tls.VersionTLS12,
		}
	}

	listener, err := net.Listen("tcp", door.cfg.Listen)
	if err != nil {
		return fmt.Errorf("webui: listen on %s: %w", door.cfg.Listen, err)
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	if door.tls {
		s.log.Info("browser interface listening", "address", door.cfg.Listen,
			"tls", true, "certificate_sha256", fingerprint)
		err = server.ServeTLS(listener, "", "")
	} else {
		s.log.Info("browser interface listening", "address", door.cfg.Listen,
			"tls", false, "reach", "ssh -L or a reverse proxy on this machine")
		err = server.Serve(listener)
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// isLoopback says whether a bind host keeps the listener on this machine.
// An empty host binds every address, which is the opposite.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// browserCertificate loads the TLS pair, making a self-signed one the
// first time. Half a pair is an error rather than a fresh pair: the half
// that is there was put there by somebody, on purpose.
func browserCertificate(cfg BrowserConfig) (tls.Certificate, string, error) {
	_, certErr := os.Stat(cfg.CertFile)
	_, keyErr := os.Stat(cfg.KeyFile)
	switch {
	case certErr == nil && keyErr == nil:
	case errors.Is(certErr, os.ErrNotExist) && errors.Is(keyErr, os.ErrNotExist):
		hosts := []string{cfg.Hostname}
		hosts = append(hosts, localAddresses()...)
		pair, err := certs.SelfSigned(cfg.Hostname, hosts, 10*365*24*time.Hour)
		if err != nil {
			return tls.Certificate{}, "", err
		}
		if err := os.MkdirAll(filepath.Dir(cfg.CertFile), 0o700); err != nil {
			return tls.Certificate{}, "", fmt.Errorf("webui: create %s: %w", filepath.Dir(cfg.CertFile), err)
		}
		if err := pair.WriteFiles(cfg.CertFile, cfg.KeyFile); err != nil {
			return tls.Certificate{}, "", err
		}
	default:
		return tls.Certificate{}, "", fmt.Errorf("webui: the TLS pair is incomplete: %s and %s must both exist, or neither",
			cfg.CertFile, cfg.KeyFile)
	}
	certificate, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("webui: load the TLS pair: %w", err)
	}
	pem, err := os.ReadFile(cfg.CertFile)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("webui: read %s: %w", cfg.CertFile, err)
	}
	fingerprint, err := certs.FingerprintPEM(pem)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	return certificate, fingerprint, nil
}

// localAddresses is every address this machine answers on that is not
// loopback, so a self-signed certificate names the address an operator
// will type as well as the hostname.
func localAddresses() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var hosts []string
	for _, addr := range addrs {
		network, ok := addr.(*net.IPNet)
		if !ok || network.IP.IsLoopback() || network.IP.IsLinkLocalUnicast() {
			continue
		}
		hosts = append(hosts, network.IP.String())
	}
	return hosts
}

// browserHandler is the socket's handler with the door in front of it.
func (s *Server) browserHandler() http.Handler {
	return s.recoverPanics(s.markDocument(s.route(s.browserGate(s.operatorMux()))))
}

// markDocument says the page is a document of its own and sets the
// headers a page on a network port wants that a fragment behind a panel
// could not set for itself.
func (s *Server) markDocument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		if s.browser.tls {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), documentKey{}, true)))
	})
}

// browserGate lets a signed-in session through to the operator's pages,
// serves the sign-in page to everybody else, and serves the fonts to
// both, since the sign-in page is set in them.
func (s *Server) browserGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			s.handleLogin(w, r)
			return
		case "/logout":
			s.handleLogout(w, r)
			return
		case "/font":
			next.ServeHTTP(w, r)
			return
		}
		if !s.browser.authenticated(r) {
			// A page refreshing itself in the background should not be
			// answered with the sign-in page: it would swap nothing and
			// try again forever. It is told, and the next click brings
			// the operator to the door.
			if r.Header.Get("X-Gniza-Live") != "" || wantsData(r) {
				http.Error(w, "sign in first", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Location", "?p=login")
			w.WriteHeader(http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// loginView is what the sign-in page shows.
type loginView struct {
	Hostname string
	Error    string
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	door := s.browser
	if r.Method != http.MethodPost {
		if door.authenticated(r) {
			w.Header().Set("Location", "?p=")
			w.WriteHeader(http.StatusSeeOther)
			return
		}
		s.renderLogin(w, r, http.StatusOK, "")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		s.renderLogin(w, r, http.StatusBadRequest, "That form could not be read. Try again.")
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(s.csrfToken)) != 1 {
		s.renderLogin(w, r, http.StatusForbidden, "This page expired when the service restarted. Try again.")
		return
	}

	who := remoteHost(r)
	if wait := door.lockedFor(who); wait > 0 {
		s.log.Warn("browser sign-in refused: locked out", "from", who, "wait", wait)
		s.renderLogin(w, r, http.StatusTooManyRequests,
			fmt.Sprintf("Too many wrong passwords. Try again in %s.", wait.Round(time.Second)))
		return
	}

	ok, err := door.checkPassword(r.PostFormValue("password"))
	if err != nil {
		s.log.Error("browser sign-in: read the password file", "error", err)
		s.renderLogin(w, r, http.StatusInternalServerError,
			"The password on this server could not be read. Set it again with gniza-agent -web-set-password.")
		return
	}
	if !ok {
		door.recordFailure(who)
		s.log.Warn("browser sign-in refused: wrong password", "from", who)
		s.renderLogin(w, r, http.StatusUnauthorized, "That is not the password.")
		return
	}

	token, err := door.begin(who)
	if err != nil {
		s.log.Error("browser sign-in: start a session", "error", err)
		s.renderLogin(w, r, http.StatusInternalServerError, "A session could not be started. Try again.")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: browserCookie, Value: token, Path: "/",
		HttpOnly: true, Secure: door.tls, SameSite: http.SameSiteStrictMode,
		MaxAge: int(sessionLife / time.Second),
	})
	s.log.Info("browser sign-in", "from", who)
	w.Header().Set("Location", "?p=")
	w.WriteHeader(http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		if cookie, err := r.Cookie(browserCookie); err == nil {
			s.browser.end(cookie.Value)
		}
		http.SetCookie(w, &http.Cookie{
			Name: browserCookie, Value: "", Path: "/", MaxAge: -1,
			HttpOnly: true, Secure: s.browser.tls, SameSite: http.SameSiteStrictMode,
		})
	}
	w.Header().Set("Location", "?p=login")
	w.WriteHeader(http.StatusSeeOther)
}

func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, status int, message string) {
	s.renderStatus(w, r, status, "login.html", "Sign in", "",
		loginView{Hostname: s.browser.cfg.Hostname, Error: message}, s.csrfToken)
}

// remoteHost is the address a request came from, without its port, which
// is what a lockout is counted against.
func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// checkPassword compares a password with the hash on disk. The file is
// read every time, so a password set while the service runs applies to
// the next sign-in without a restart.
func (d *browserDoor) checkPassword(password string) (bool, error) {
	stored, err := os.ReadFile(d.cfg.PasswordFile)
	if err != nil {
		return false, err
	}
	hash := strings.TrimSpace(string(stored))
	if hash == "" {
		return false, errors.New("the password file is empty")
	}
	err = bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, bcrypt.ErrMismatchedHashAndPassword):
		return false, nil
	default:
		return false, err
	}
}

// authenticated says the request carries a live session, and counts the
// use against its idle time.
func (d *browserDoor) authenticated(r *http.Request) bool {
	cookie, err := r.Cookie(browserCookie)
	if err != nil || cookie.Value == "" {
		return false
	}
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	session, ok := d.sessions[cookie.Value]
	if !ok {
		return false
	}
	if now.After(session.began.Add(sessionLife)) || now.After(session.seen.Add(sessionIdle)) {
		delete(d.sessions, cookie.Value)
		return false
	}
	session.seen = now
	return true
}

func (d *browserDoor) begin(from string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	// A sign-in is a good moment to forget sessions that have ended and
	// addresses that gave up; nothing else walks these maps.
	for key, session := range d.sessions {
		if now.After(session.began.Add(sessionLife)) || now.After(session.seen.Add(sessionIdle)) {
			delete(d.sessions, key)
		}
	}
	for key, failed := range d.failures {
		if now.After(failed.last.Add(lockoutMax)) && now.After(failed.until) {
			delete(d.failures, key)
		}
	}
	d.sessions[token] = &browserSession{began: now, seen: now}
	delete(d.failures, from)
	return token, nil
}

func (d *browserDoor) end(token string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.sessions, token)
}

// lockedFor is how much longer an address has to wait before it may try
// a password again, or zero.
func (d *browserDoor) lockedFor(from string) time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	failed, ok := d.failures[from]
	if !ok {
		return 0
	}
	if wait := time.Until(failed.until); wait > 0 {
		return wait
	}
	return 0
}

// recordFailure counts a wrong password against an address. From the
// fifth on, each one sets a wait twice as long as the last, up to a
// quarter of an hour; bcrypt's own cost makes the first four slow enough.
func (d *browserDoor) recordFailure(from string) {
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	failed, ok := d.failures[from]
	if !ok {
		failed = &loginFailures{}
		d.failures[from] = failed
	}
	failed.count++
	failed.last = now
	if failed.count >= lockoutAfter {
		wait := lockoutBase << uint(failed.count-lockoutAfter)
		if wait > lockoutMax || wait <= 0 {
			wait = lockoutMax
		}
		failed.until = now.Add(wait)
	}
}
