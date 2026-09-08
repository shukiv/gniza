package webui

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"time"
)

const (
	// daSessionHeader and daKeyHeader carry the session DirectAdmin put
	// in the plugin's environment.
	//
	// They are headers rather than cookies because nothing here is a
	// browser: the plugin script is handed SESSION_ID and SESSION_KEY by
	// DirectAdmin and sets them on a request of its own to this socket.
	// A browser never speaks to this socket and cannot put anything in
	// these -- but that is the plugin's promise to keep, not this end's,
	// so what arrives in them is treated as a claim and put to
	// DirectAdmin before it is worth anything.
	daSessionHeader = "X-Gniza-DA-Session"
	daKeyHeader     = "X-Gniza-DA-Key"
)

// daVerifier is DirectAdmin's answer to whose session this is.
// *daSessionVerifier is the one that asks a real panel; a test answers
// without one.
type daVerifier interface {
	verify(ctx context.Context, sessionID, sessionKey string) (daPrincipal, error)
}

// WithDirectAdmin checks plugin sessions against the panel at panelURL,
// which is DirectAdmin's own address on this server. Without it no
// DirectAdmin socket can be opened, which is the point: there would be
// nothing to check a session against.
func WithDirectAdmin(panelURL string) Option {
	return func(s *Server) { s.daSessions = newDASessionVerifier(panelURL) }
}

// daPrincipalKey carries who DirectAdmin said is asking.
type daPrincipalKey struct{}

// daPrincipalOf is the administrator behind a request on the DirectAdmin
// socket. A zero value means the request did not come through the check,
// which no handler behind it can see.
func daPrincipalOf(r *http.Request) daPrincipal {
	who, _ := r.Context().Value(daPrincipalKey{}).(daPrincipal)
	return who
}

// requireDirectAdminAdmin refuses everything that is not an
// administrator's live DirectAdmin session.
//
// This is the whole of the authorization on that socket. The socket is
// owned by the service account named in plugin.conf's admin_run_as, so
// the uid on the other end says "DirectAdmin's plugin runner" and
// nothing about who reached the page -- it is one account acting for
// whoever DirectAdmin let through, possibly for several people at once.
// So the peer uid is not consulted at all, and the session is put to
// DirectAdmin on every request. See docs/adr/0020.
func (s *Server) requireDirectAdminAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.daSessions == nil {
			// A socket with no way to check a session would otherwise
			// serve every customer's backups to whoever opened it.
			http.Error(w, "this server cannot check DirectAdmin sessions",
				http.StatusServiceUnavailable)
			return
		}
		sessionID := r.Header.Get(daSessionHeader)
		sessionKey := r.Header.Get(daKeyHeader)
		if sessionID == "" || sessionKey == "" {
			http.Error(w, "this page is opened from DirectAdmin", http.StatusUnauthorized)
			return
		}

		who, err := s.daSessions.verify(r.Context(), sessionID, sessionKey)
		if err != nil {
			// The error can name the session it was asked about, so it
			// goes to the log and not to the page.
			s.log.Warn("directadmin plugin: session refused", "error", err)
			if errors.Is(err, errDASessionUnavailable) {
				http.Error(w, "DirectAdmin did not answer, so nothing can say who is asking",
					http.StatusServiceUnavailable)
				return
			}
			http.Error(w, "DirectAdmin did not accept this session; sign in again",
				http.StatusForbidden)
			return
		}
		// A reseller's session is a real DirectAdmin session and is
		// refused here rather than by the page, because the page is
		// account-wide: it reads every destination credential this
		// server holds and can restore over any account on it.
		if who.UserType != "admin" {
			s.log.Warn("directadmin plugin: not an administrator",
				"account", who.Username, "type", who.UserType)
			http.Error(w, "this page is for DirectAdmin administrators", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(
			context.WithValue(r.Context(), daPrincipalKey{}, who)))
	})
}

// ListenDirectAdmin serves the operator's interface to DirectAdmin's
// plugin.
//
// It is a third socket rather than the root-only one widened. That one
// stays root-only: a connection to it means an operator who is already
// root, and that is what makes its mode an authorization. This one is
// owned by the service account DirectAdmin runs the plugin as, which is
// nobody in particular, so its mode only decides which door is used and
// the session decides the rest.
func (s *Server) ListenDirectAdmin(ctx context.Context, socketPath, owner string) error {
	// Before anything is created. A socket that refuses every request is
	// worse than a service that says why it did not start.
	if s.daSessions == nil {
		return fmt.Errorf("webui: %s needs a way to check DirectAdmin sessions", socketPath)
	}
	uid, gid, err := pluginOwner(owner)
	if err != nil {
		return err
	}

	// The directory admits root, who made it, and the plugin's account,
	// which is its group. Nobody else can so much as name what is in it.
	dir := filepath.Dir(socketPath)
	if err := prepareSocketDir(dir, 0o710); err != nil {
		return err
	}
	// The uid is left alone: root made this and root should keep it.
	if err := os.Chown(dir, -1, gid); err != nil {
		return fmt.Errorf("webui: give %s to %s: %w", dir, owner, err)
	}
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("webui: remove stale socket: %w", err)
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("webui: listen on %s: %w", socketPath, err)
	}
	if err := os.Chown(socketPath, uid, gid); err != nil {
		_ = listener.Close()
		return fmt.Errorf("webui: give %s to %s: %w", socketPath, owner, err)
	}
	// Owner alone, and the owner is the plugin's account. The mode is
	// set after the chown: between the listen and here it is root's, and
	// root is the only one who could have reached it anyway.
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = listener.Close()
		return fmt.Errorf("webui: secure socket: %w", err)
	}

	server := &http.Server{
		Handler:           s.requireDirectAdminAdmin(s.Handler()),
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       60 * time.Second,
		// Browsing a snapshot waits on restic, which waits on the
		// network.
		WriteTimeout: 5 * time.Minute,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	s.log.Info("directadmin interface listening", "socket", socketPath, "owner", owner)
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// pluginOwner resolves the account named in plugin.conf's admin_run_as.
//
// root is refused here for the same reason DirectAdmin refuses it there
// -- "Must not be uid=0 and must exist" -- and for one more: an owner of
// root would make this socket a second root-only socket wearing a
// different name, which is not what it is for.
func pluginOwner(owner string) (uid, gid int, err error) {
	if owner == "" {
		return 0, 0, errors.New("webui: the DirectAdmin socket has no account to belong to")
	}
	account, err := user.Lookup(owner)
	if err != nil {
		return 0, 0, fmt.Errorf("webui: %q is not an account on this server: %w", owner, err)
	}
	uid, err = strconv.Atoi(account.Uid)
	if err != nil {
		return 0, 0, fmt.Errorf("webui: %q has no usable uid: %w", owner, err)
	}
	gid, err = strconv.Atoi(account.Gid)
	if err != nil {
		return 0, 0, fmt.Errorf("webui: %q has no usable group: %w", owner, err)
	}
	if uid == 0 {
		return 0, 0, fmt.Errorf("webui: the DirectAdmin socket must not belong to root, and %q is", owner)
	}
	return uid, gid, nil
}
