package destination

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// SFTP is a repository reached over SSH. No Gniza software runs on the
// far end; restic drives the system ssh client.
type SFTP struct {
	Host string
	// Port is the SSH port. Zero means the default (22) and produces the
	// short "sftp:user@host:/path" URI form.
	Port int
	User string
	// Root is the absolute remote directory holding the repositories.
	Root string
	// IdentityFile is the path to the private key on the agent. The key is
	// written by the agent from the credential vault at mode 0600 and
	// removed when the job ends.
	IdentityFile string
	// KnownHostsFile pins the server's host key. Empty means the ssh
	// client's default, which Preflight rejects: an unpinned host key
	// turns a DNS or routing compromise into a credential disclosure.
	KnownHostsFile string
	// SSHPath is the ssh client Preflight logs in with; empty means the
	// one on PATH, which is also the one restic drives.
	SSHPath string
}

var _ Destination = (*SFTP)(nil)

func (s *SFTP) Type() Type { return TypeSFTP }

func (s *SFTP) URI(repoPath string) (string, error) {
	if err := s.validate(); err != nil {
		return "", err
	}
	cleaned, err := CleanRepoPath(repoPath)
	if err != nil {
		return "", err
	}
	root := strings.TrimSuffix(s.Root, "/")
	if s.Port == 0 || s.Port == 22 {
		return fmt.Sprintf("sftp:%s@%s:%s/%s", s.User, s.hostForURI(), root, cleaned), nil
	}
	// The URL form needs a double slash to mark the path as absolute:
	// sftp://user@host:2222//srv/restic-repo
	return fmt.Sprintf("sftp://%s@%s:%d/%s/%s",
		s.User, s.hostForURI(), s.Port, root, cleaned), nil
}

// hostForURI bracket-wraps IPv6 literals, which restic's URL form requires.
func (s *SFTP) hostForURI() string {
	if ip := net.ParseIP(s.Host); ip != nil && ip.To4() == nil {
		return "[" + s.Host + "]"
	}
	return s.Host
}

// Env returns no variables. SSH authentication is configured through the
// identity and known-hosts files, which reach ssh via Options.
func (s *SFTP) Env() (map[string]string, error) { return map[string]string{}, nil }

// Options returns restic's sftp.args, which restic injects into the ssh
// command it builds:
//
//	ssh <host> [-p <port>] [-l <user>] <sftp.args...> -s sftp
//
// Without this the agent would fall back to root's ssh configuration:
// the wrong key, an unpinned host key, and an interactive prompt that
// hangs an unattended backup forever.
//
// File paths are not secrets, so unlike credentials they are safe in the
// argument list.
func (s *SFTP) Options() (map[string]string, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	if s.KnownHostsFile == "" {
		return nil, fmt.Errorf("sftp: known-hosts file is required; refusing to trust an unpinned host key")
	}
	args := []string{
		"-i", s.IdentityFile,
		"-o", "UserKnownHostsFile=" + s.KnownHostsFile,
		"-o", "StrictHostKeyChecking=yes",
		// Never prompt: an unattended agent that is asked for a password
		// or a host-key confirmation would block until the job times out.
		"-o", "BatchMode=yes",
		"-o", "IdentitiesOnly=yes",
	}
	return map[string]string{"sftp.args": strings.Join(args, " ")}, nil
}

func (s *SFTP) Preflight(ctx context.Context) error {
	if err := s.validate(); err != nil {
		return err
	}
	if s.KnownHostsFile == "" {
		return fmt.Errorf("sftp: known-hosts file is required; refusing to trust an unpinned host key")
	}
	for name, path := range map[string]string{
		"identity file":    s.IdentityFile,
		"known-hosts file": s.KnownHostsFile,
	} {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("sftp: %s: %w", name, err)
		}
		if info.IsDir() {
			return fmt.Errorf("sftp: %s %q is a directory", name, path)
		}
	}
	if perm := mustStatMode(s.IdentityFile); perm&0o077 != 0 {
		return fmt.Errorf("sftp: identity file %q is group- or world-accessible (mode %04o)",
			s.IdentityFile, perm)
	}

	port := s.Port
	if port == 0 {
		port = 22
	}
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(s.Host, strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("sftp: dial: %w", err)
	}
	if err := conn.Close(); err != nil {
		return err
	}
	return s.login(ctx, port)
}

// login opens the sftp subsystem the way restic will and closes it
// again, so a key the server does not know, or a host key that has
// changed, is found out by Test and said in words rather than by the
// first backup in restic's. The port answering was not proof of either.
func (s *SFTP) login(ctx context.Context, port int) error {
	ssh := s.SSHPath
	if ssh == "" {
		ssh = "ssh"
	}
	options, err := s.Options()
	if err != nil {
		return err
	}
	args := append([]string{"-p", strconv.Itoa(port), "-l", s.User},
		strings.Fields(options["sftp.args"])...)
	args = append(args, "-o", "ConnectTimeout=20", s.Host, "-s", "sftp")
	cmd := exec.CommandContext(ctx, ssh, args...)
	// sftp-server reads its first packet from stdin; at end of file it
	// exits cleanly, which is all that is asked of it.
	cmd.Stdin = strings.NewReader("")
	var said bytes.Buffer
	cmd.Stdout, cmd.Stderr = io.Discard, &said
	if err := cmd.Run(); err != nil {
		return s.explain(said.String(), err)
	}
	return nil
}

// explain turns ssh's complaint into what the operator has to do.
func (s *SFTP) explain(said string, err error) error {
	first := ""
	for _, line := range strings.Split(said, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "Warning: Permanently added") {
			first = line
			break
		}
	}
	switch {
	case strings.Contains(said, "Permission denied (publickey"):
		return fmt.Errorf("sftp: %s does not accept Gniza's key for %s: put the public key shown on this destination's card into %s's ~/.ssh/authorized_keys on that server, then test again",
			s.Host, s.User, s.User)
	case strings.Contains(said, "Host key verification failed"), strings.Contains(said, "REMOTE HOST IDENTIFICATION HAS CHANGED"):
		return fmt.Errorf("sftp: %s's host key is not the one pinned when this destination was added; if that server was reinstalled, edit the destination to pin its new key",
			s.Host)
	case strings.Contains(said, "subsystem request failed"):
		return fmt.Errorf("sftp: %s accepts the key but has no sftp subsystem for %s; restic needs one", s.Host, s.User)
	case first != "":
		return fmt.Errorf("sftp: ssh to %s@%s: %s", s.User, s.Host, first)
	}
	return fmt.Errorf("sftp: ssh to %s@%s: %w", s.User, s.Host, err)
}

func (s *SFTP) validate() error {
	switch {
	case s.Host == "":
		return fmt.Errorf("sftp: host is required")
	case s.User == "":
		return fmt.Errorf("sftp: user is required")
	case !strings.HasPrefix(s.Root, "/"):
		return fmt.Errorf("sftp: root %q must be an absolute path", s.Root)
	case s.Port < 0 || s.Port > 65535:
		return fmt.Errorf("sftp: port %d is out of range", s.Port)
	case s.IdentityFile == "":
		return fmt.Errorf("sftp: identity file is required")
	}
	// A host is a name, not an option. ssh reads anything beginning with
	// a dash as one, and the space probe passes the host on ssh's command
	// line; "--" in front of it is the guard, and this is the second one.
	// Whitespace and control characters have no place in a hostname
	// either, and both reach an argument list from here.
	if strings.HasPrefix(s.Host, "-") {
		return fmt.Errorf("sftp: host %q must not begin with a dash", s.Host)
	}
	if strings.ContainsAny(s.Host, " \t\r\n") || hasControl(s.Host) {
		return fmt.Errorf("sftp: host %q must not contain whitespace or control characters", s.Host)
	}
	// The root is quoted before it reaches the far end's shell, but a
	// newline in it would be a second line of that command whatever the
	// quoting, and restic's sftp URI cannot carry one either.
	if hasControl(s.Root) {
		return fmt.Errorf("sftp: root %q must not contain control characters", s.Root)
	}

	// restic splits sftp.args with shell-like quoting rules, so a path
	// containing whitespace or quotes would be silently torn into several
	// arguments.
	for name, path := range map[string]string{
		"identity file":    s.IdentityFile,
		"known-hosts file": s.KnownHostsFile,
	} {
		if strings.ContainsAny(path, " \t\"'\\") {
			return fmt.Errorf("sftp: %s %q must not contain whitespace, quotes or backslashes", name, path)
		}
	}
	return nil
}

// mustStatMode returns the file's permission bits, or 0 when it cannot be
// stat'ed. Callers stat the file first, so 0 only occurs on a race and is
// treated as "no complaint" rather than a false rejection.
func mustStatMode(path string) os.FileMode {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Mode().Perm()
}

// hasControl reports whether a value carries a character that has no
// business in a hostname or a path: they end up on argument lists, in URIs
// and in a remote shell's input.
func hasControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
