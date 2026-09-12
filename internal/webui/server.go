// Package webui serves the standalone node's interface.
//
// It listens on a unix domain socket rather than a TCP port. cPanel servers
// are multi-tenant, the untrusted users are already on the box, and this
// interface can read every destination credential the server holds — a
// loopback port would be reachable by all of them. A WHM plugin CGI, which
// runs as root, proxies to the socket.
// See docs/adr/0007-standalone-mode.md.
package webui

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/shukiv/gniza/internal/node"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// The typefaces the interface is set in travel with the plugin. Asking a
// font host for them would tell a third party whenever a root WHM session
// is open, and would let one serve styling into a privileged page.
//
//go:embed fonts/*.woff2
var fontFS embed.FS

// Server renders the node's interface.
type Server struct {
	engine *node.Engine
	log    *slog.Logger
	// journal reads the service log. It is a field so a test can answer
	// with lines of its own rather than needing a systemd unit.
	journal journalReader
	// templates holds one set per page. Every page defines "content", so
	// parsing them into a single set would leave only the last one.
	templates map[string]*template.Template
	// csrfToken is minted once per process and embedded in every mutating
	// form. There are no sessions here — WHM has already authenticated the
	// operator — but a token still stops another page in the browser
	// posting to this one.
	csrfToken string
	// userCSRFKey derives a different token for every cPanel account. It
	// must be separate from csrfToken: every customer can read the token
	// in their own page, while csrfToken protects root-only WHM actions.
	userCSRFKey []byte
	// reportPreviewKey authenticates reviewed diagnostics carried by the form.
	reportPreviewKey []byte
	// assets are inlined into every page. cpsrvd strips Content-Type from
	// what the plugin proxies back and sets X-Content-Type-Options:
	// nosniff, so a stylesheet fetched as its own request arrives with no
	// type and the browser refuses to apply it. Inlining sidesteps that,
	// and saves two round trips through the CGI besides.
	assets assets
	// userBusy holds each cPanel account to one request at a time on the
	// account-facing socket.
	userBusy inFlight
	// userFeatures enforces cPanel Feature Manager after socket identity
	// has been established, where an account cannot bypass it by replacing
	// or avoiding the PHP frontend.
	userFeatures accountFeatureGate
	// userAuth requires a live cPanel session in addition to the account's
	// Unix uid. Team users share the owner's uid, and arbitrary website
	// processes run under it too, so SO_PEERCRED alone is not enough.
	userAuth *accountSessionAuth
	// daSessions asks DirectAdmin whose session a plugin request carries.
	// On DirectAdmin there is no uid worth reading -- the plugin runs as
	// a service account of Gniza's own -- so this is the whole of the
	// authorization on that socket. Nil on a cPanel server, where no
	// DirectAdmin socket is opened.
	daSessions daVerifier
	// browser is the door on the TCP listener, for a server with no
	// panel: a password, sessions and a lockout. Nil where nothing
	// listens on a port. See browser.go.
	browser *browserDoor
}

// assets is the stylesheet and script, embedded in the page.
type assets struct {
	CSS template.CSS
	JS  template.JS
}

// New builds the interface.
// Option adjusts a server at construction. There is one, and it exists so
// the service log can be answered from somewhere other than journalctl --
// a machine with no systemd unit has no journal to read, and every other
// route on this server can be exercised on one.
type Option func(*Server)

// WithJournal reads the service log with the given command instead of
// journalctl.
func WithJournal(read func(ctx context.Context, args ...string) ([]byte, error)) Option {
	return func(s *Server) { s.journal = read }
}

func New(engine *node.Engine, log *slog.Logger, options ...Option) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	templates, err := parseTemplates()
	if err != nil {
		return nil, err
	}

	css, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		return nil, fmt.Errorf("webui: read stylesheet: %w", err)
	}
	script, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		return nil, fmt.Errorf("webui: read script: %w", err)
	}

	raw := make([]byte, 128)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("webui: generate csrf token: %w", err)
	}
	server := &Server{
		engine: engine, log: log, templates: templates, journal: readJournal,
		csrfToken:        hex.EncodeToString(raw[:32]),
		userCSRFKey:      append([]byte(nil), raw[32:64]...),
		assets:           assets{CSS: template.CSS(css), JS: template.JS(script)},
		userFeatures:     newAccountFeatureGate(),
		userAuth:         newAccountSessionAuth(raw[64:96]),
		reportPreviewKey: append([]byte(nil), raw[96:]...),
	}
	for _, option := range options {
		option(server)
	}
	return server, nil
}

// panelName is what to call the control panel this server runs, for the
// pages that tell an operator what is about to happen.
func (s *Server) panelName() string {
	if s.engine == nil {
		return "your control panel"
	}
	return s.engine.PanelName()
}

// userCSRFToken binds an account-facing form to the Unix peer identity that
// rendered it. Customers neither receive the root WHM token nor share a
// token with another account on the same server.
func (s *Server) userCSRFToken(account string) string {
	mac := hmac.New(sha256.New, s.userCSRFKey)
	_, _ = mac.Write([]byte(account))
	return hex.EncodeToString(mac.Sum(nil))
}

// prepareSocketDir makes a socket's directory at exactly the mode asked
// for, creating any parent along the way as traversable but not writable.
func prepareSocketDir(dir string, mode os.FileMode) error {
	if parent := filepath.Dir(dir); parent != "/" && parent != "." {
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return fmt.Errorf("webui: create %s: %w", parent, err)
		}
		if err := os.Chmod(parent, 0o755); err != nil {
			return fmt.Errorf("webui: set the mode of %s: %w", parent, err)
		}
	}
	if err := os.MkdirAll(dir, mode); err != nil {
		return fmt.Errorf("webui: create %s: %w", dir, err)
	}
	// MkdirAll leaves an existing directory's mode alone, and umask can
	// take bits off a new one, so it is set rather than assumed.
	if err := os.Chmod(dir, mode); err != nil {
		return fmt.Errorf("webui: set the mode of %s: %w", dir, err)
	}
	return nil
}

// parseTemplates builds one template set per page, each containing the
// layout, the shared fragments and that page's own content.
func parseTemplates() (map[string]*template.Template, error) {
	pages, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("webui: list templates: %w", err)
	}

	sets := map[string]*template.Template{}
	for _, page := range pages {
		name := path.Base(page)
		if name == "layout.html" || name == "partials.html" || name == "user_layout.html" {
			continue
		}
		// An account's pages wear their own layout: none of the
		// operator's navigation means anything to a customer. The
		// sign-in page of the browser interface wears none: it is a
		// document of its own, for somebody not yet let in.
		files := []string{"templates/layout.html", "templates/partials.html", page}
		if strings.HasPrefix(name, "user_") {
			files[0] = "templates/user_layout.html"
		}
		if name == "login.html" {
			files = files[1:]
		}
		set, err := template.New(name).Funcs(templateFuncs()).ParseFS(templateFS, files...)
		if err != nil {
			return nil, fmt.Errorf("webui: parse %s: %w", name, err)
		}
		sets[name] = set
	}
	return sets, nil
}

// Handler returns the routed interface, as the sockets serve it.
func (s *Server) Handler() http.Handler {
	return s.recoverPanics(s.route(s.operatorMux()))
}

// operatorMux is every route of the operator's interface, before the
// query-string routing and whatever door a listener puts in front of it.
func (s *Server) operatorMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+adminCapabilityEndpoint, s.issueAdminUserCapability)
	mux.Handle("GET /static/", http.FileServerFS(staticFS))

	mux.HandleFunc("GET /{$}", s.handleDashboard)
	mux.HandleFunc("GET /destinations", s.handleDestinations)
	mux.HandleFunc("POST /destinations/add", s.guard(s.handleAddDestination))
	mux.HandleFunc("POST /destinations/key", s.guard(s.handlePrepareKey))
	mux.HandleFunc("POST /destinations/edit", s.guard(s.handleEditDestination))
	mux.HandleFunc("POST /destinations/test", s.guard(s.handleTestDestination))
	mux.HandleFunc("POST /destinations/delete", s.guard(s.handleDeleteDestination))
	mux.HandleFunc("POST /destinations/recovery", s.guard(s.handleRecoveryKey))
	mux.HandleFunc("POST /destinations/recovery/note", s.guard(s.handleNoteRecovery))
	mux.HandleFunc("POST /destinations/recovery/card", s.guard(s.handleRecoveryCard))
	mux.HandleFunc("POST /destinations/retention/plan", s.guard(s.handlePlanRetention))
	mux.HandleFunc("POST /destinations/retention/approve", s.guard(s.handleApproveRetention))
	mux.HandleFunc("POST /destinations/retention/withdraw", s.guard(s.handleWithdrawRetention))
	mux.HandleFunc("POST /destinations/retention/run", s.guard(s.handleRunRetention))

	mux.HandleFunc("GET /schedule", s.handleSchedule)
	mux.HandleFunc("POST /schedule/save", s.guard(s.handleSaveSchedule))
	mux.HandleFunc("POST /schedule/run", s.guard(s.handleRunSchedule))
	mux.HandleFunc("POST /schedule/delete", s.guard(s.handleDeleteSchedule))

	mux.HandleFunc("GET /accounts", s.handleAccounts)
	mux.HandleFunc("GET /account", s.handleAccount)
	mux.HandleFunc("POST /accounts/backup", s.guard(s.handleBackupNow))
	mux.HandleFunc("POST /accounts/repair", s.guard(s.handleRepairCoverage))
	mux.HandleFunc("POST /accounts/prepare-removal", s.guard(s.handlePrepareRemoval))
	mux.HandleFunc("POST /accounts/download", s.guard(s.handleDownloadRequest))
	mux.HandleFunc("POST /accounts/verify", s.guard(s.handleVerifyRequest))
	mux.HandleFunc("GET /download", s.handleDownload)
	mux.HandleFunc("GET /font", s.handleFont)

	mux.HandleFunc("GET /restore", s.handleRestore)
	mux.HandleFunc("POST /restore/start", s.guard(s.handleStartRestore))
	mux.HandleFunc("POST /restore/items", s.guard(s.handleRestoreItems))
	mux.HandleFunc("POST /restore/forget", s.guard(s.handleForgetAccount))

	mux.HandleFunc("GET /report", s.handleReport)
	mux.HandleFunc("POST /report/send", s.guard(s.handleSendReport))

	mux.HandleFunc("GET /logs", s.handleLogs)
	mux.HandleFunc("GET /logs/download", s.handleLogDownload)
	// The page was called History and lived at /jobs. Somebody's bookmark
	// and somebody's runbook still say so.
	mux.HandleFunc("GET /jobs", s.handleLogs)
	mux.HandleFunc("GET /browse", s.handleBrowse)
	mux.HandleFunc("GET /recover", s.handleRecover)
	mux.HandleFunc("POST /recover/attach", s.guard(s.handleAttach))
	mux.HandleFunc("POST /recover/account", s.guard(s.handleRecoverAccount))
	mux.HandleFunc("POST /recover/accounts", s.guard(s.handleRecoverAccounts))

	mux.HandleFunc("GET /settings", s.handleSettings)
	mux.HandleFunc("POST /settings/save", s.guard(s.handleSaveSettings))
	mux.HandleFunc("POST /settings/output/delete", s.guard(s.handleDeleteOutput))
	mux.HandleFunc("POST /settings/output/clear", s.guard(s.handleClearOutput))
	mux.HandleFunc("POST /settings/update/check", s.guard(s.handleCheckUpdate))
	mux.HandleFunc("POST /settings/update/install", s.guard(s.handleUpgrade))
	mux.HandleFunc("POST /settings/update/dismiss", s.guard(s.handleDismissUpgrade))
	mux.HandleFunc("POST /settings/update/channel", s.guard(s.handleChooseChannel))
	mux.HandleFunc("POST /settings/uninstall", s.guard(s.handleUninstall))
	mux.HandleFunc("POST /settings/channels/save", s.guard(s.handleSaveChannel))
	mux.HandleFunc("POST /settings/channels/test", s.guard(s.handleTestChannel))
	mux.HandleFunc("POST /settings/channels/delete", s.guard(s.handleDeleteChannel))

	return mux
}

// route turns the "p" query parameter into a request path.
//
// cpsrvd will not route anything after a CGI's name — both
// ".../gniza.cgi/" and ".../gniza.cgi/accounts" are 404s, verified
// against cPanel 136 — so every route travels in a query parameter. It is
// translated here rather than in the plugin so the rule is one thing, in
// the language the rest of this package is written in, and can be tested.
// See docs/adr/0008.
func (s *Server) route(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if _, given := query["p"]; !given {
			next.ServeHTTP(w, r)
			return
		}

		route := strings.TrimPrefix(query.Get("p"), "/")
		query.Del("p")
		if !safeRoute(route) {
			s.fail(w, r, http.StatusBadRequest, errors.New("that address is not part of gniza"))
			return
		}

		r.URL.Path = "/" + route
		r.URL.RawQuery = query.Encode()
		next.ServeHTTP(w, r)
	})
}

// safeRoute accepts only the shapes this interface serves, so a crafted "p"
// cannot be used to reach somewhere else.
func safeRoute(route string) bool {
	if route == "" {
		return true
	}
	if strings.Contains(route, "..") || strings.Contains(route, "//") {
		return false
	}
	for _, r := range route {
		isAllowed := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '/' || r == '-' || r == '_' || r == '.'
		if !isAllowed {
			return false
		}
	}
	return true
}

// Listen serves the interface on a unix socket.
//
// The socket sits in an owner-only directory and is itself owner-only, so
// an unprivileged user on a shared server cannot reach it.
func (s *Server) Listen(ctx context.Context, socketPath string) error {
	// Its own directory, at its own mode. The two listeners used to share
	// one and set it to different modes as they started, so which
	// boundary a server ended up with depended on which won the race.
	dir := filepath.Dir(socketPath)
	if err := prepareSocketDir(dir, 0o700); err != nil {
		return err
	}
	// A socket left by a killed process would block the listen.
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("webui: remove stale socket: %w", err)
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("webui: listen on %s: %w", socketPath, err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = listener.Close()
		return fmt.Errorf("webui: secure socket: %w", err)
	}

	server := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Browsing a snapshot or listing a remote repository waits on
		// restic, which waits on the network.
		WriteTimeout: 5 * time.Minute,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	s.log.Info("interface listening", "socket", socketPath)
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// guard rejects a mutating request that does not carry this process's CSRF
// token.
// maxFormBytes is as large as a submitted form is allowed to be.
//
// The biggest form this program has is a list of files to restore, which
// is thousands of paths at worst. A megabyte is far more than that and far
// less than a local process can use to make the service chew through
// memory it never gets back.
const maxFormBytes = 1 << 20

func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return s.guardWithToken(func(*http.Request) string { return s.csrfToken }, s.fail, next)
}

// userGuard uses the account identity attached from SO_PEERCRED. Keeping it
// distinct from guard prevents any cPanel customer from learning the secret
// that authorizes root WHM changes merely by opening their own backup page.
func (s *Server) userGuard(next http.HandlerFunc) http.HandlerFunc {
	return s.guardWithToken(func(r *http.Request) string {
		return s.userCSRFToken(accountOf(r))
	}, s.failUser, next)
}

func (s *Server) guardWithToken(
	expected func(*http.Request) string,
	fail func(http.ResponseWriter, *http.Request, int, error),
	next http.HandlerFunc,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// ParseForm reads the whole body, and without this it reads
		// however much the other end feels like sending, for however
		// long it feels like taking.
		r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
		if err := r.ParseForm(); err != nil {
			fail(w, r, http.StatusBadRequest, fmt.Errorf("unreadable form: %w", err))
			return
		}
		supplied := r.PostFormValue("csrf")
		if subtle.ConstantTimeCompare([]byte(supplied), []byte(expected(r))) != 1 {
			// Usually a stale page after a restart, occasionally something
			// worse. Either way the operator should reload and retry.
			fail(w, r, http.StatusForbidden,
				errors.New("this page expired when the service restarted; reload and try again"))
			return
		}
		next(w, r)
	}
}

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.log.Error("panic serving request",
					"path", r.URL.Path, "panic", recovered)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
