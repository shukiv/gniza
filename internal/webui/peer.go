package webui

import (
	"fmt"
	"net"
	"os/user"
	"strconv"
	"strings"

	"github.com/shukiv/gniza/internal/panel"
	"golang.org/x/sys/unix"
)

// peerAccount is the account on the other end of a unix socket.
//
// The account-facing interface runs as the account it belongs to -- cpsrvd
// executes a cPanel user's CGI as that user, and DirectAdmin runs an
// account's plugin page as the account (ADR 0020) -- so the kernel
// already knows who is asking. Reading it from the socket means the
// browser never gets to say who it is, and a request cannot be pointed at
// somebody else's backups by changing a parameter. Whether that uid is a
// customer in good standing is the panel's to say.
func (s *Server) peerAccount(conn net.Conn) (string, error) {
	creds, err := peerCredentials(conn)
	if err != nil {
		return "", err
	}
	return accountFromCredentials(s.holder, creds.Uid)
}

// accountFromCredentials is the account behind a uid, by the panel's
// word. Root is never an account; neither is a uid the panel does not
// hold, nor one it has suspended.
func accountFromCredentials(holder panel.AccountHolder, uid uint32) (string, error) {
	if holder == nil {
		return "", fmt.Errorf("webui: this panel has no account-facing interface")
	}
	// uid 0 could open the socket, but root has no business being served
	// as "an account": there is no single account to confine it to.
	if uid == 0 {
		return "", fmt.Errorf("webui: this interface is for accounts, not for root")
	}
	account, err := user.LookupId(strconv.FormatUint(uint64(uid), 10))
	if err != nil {
		return "", fmt.Errorf("webui: uid %d is not an account on this server", uid)
	}
	// A uid the operating system knows is not necessarily a customer:
	// system users have uids too. The panel's own registry decides.
	if !holder.HoldsAccount(account.Username) {
		return "", fmt.Errorf("webui: %q is not an account of this panel", account.Username)
	}
	// A suspended account's files are still backed up; the person behind
	// it does not go on driving the service.
	if holder.Suspended(account.Username) {
		return "", fmt.Errorf("webui: %q is suspended", account.Username)
	}
	return account.Username, nil
}

func peerCredentials(conn net.Conn) (*unix.Ucred, error) {
	if budgeted, ok := conn.(*budgetedConn); ok {
		conn = budgeted.Conn
	}
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return nil, fmt.Errorf("webui: connection is not a unix socket")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return nil, fmt.Errorf("webui: read the connection: %w", err)
	}

	var creds *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		creds, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return nil, fmt.Errorf("webui: read the peer's credentials: %w", err)
	}
	if credErr != nil {
		return nil, fmt.Errorf("webui: read the peer's credentials: %w", credErr)
	}
	return creds, nil
}

// plainName keeps a name that is about to be joined onto a path to
// something that cannot climb out of it.
func plainName(name string) bool {
	return name != "" && !strings.ContainsAny(name, "/.")
}
