package directadmin

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/shukiv/gniza/internal/panel"
)

// DownloadsDir is the directory under an account's home directory that a
// copy of their backup is written into, where their file manager and FTP
// already look.
const DownloadsDir = "gniza-downloads"

// PutsBack says whether a part of an account can be written back into
// the live account on this panel. Not yet: every put-back here answers
// ErrUnverified (PutHomeDir, PutCrontab, LoadDatabase, CreateDatabase,
// PutDatabaseUsers), so a page that offered one would offer a refusal.
func (r *Real) PutsBack() bool { return false }

// DeliverToHome writes a copy of from into the account's home directory
// as the account.
//
// Root opens the archive, which is in a staging directory the account
// cannot read, and hands the open file to a child running as the
// account, which makes the directory and writes the file. Nothing runs
// as root inside the home directory: the file is owned by the account
// without a chown, and a link planted at the directory's name beforehand
// leads nowhere the account could not already write.
func (r *Real) DeliverToHome(ctx context.Context, account, from, name string) (string, error) {
	if err := usableDownloadName(name); err != nil {
		return "", err
	}
	uid, gid, home, err := r.accountIdentity(account)
	if err != nil {
		return "", err
	}
	source, err := os.Open(from)
	if err != nil {
		return "", fmt.Errorf("directadmin: open the copy for %s: %w", account, err)
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return "", fmt.Errorf("directadmin: open the copy for %s: %w", account, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("directadmin: the copy for %s is not a file", account)
	}
	downloads := filepath.Join(home, DownloadsDir)
	target := filepath.Join(downloads, name)
	// The shell is the account's: umask first, so the directory and the
	// file are the account's alone from the moment they exist; -- so a
	// name is a name. The paths are arguments, not part of the script.
	write := exec.CommandContext(ctx, "/bin/sh", "-c",
		`umask 077 && mkdir -p -- "$1" && cat > "$2"`, "gniza-deliver", downloads, target)
	write.Dir = home
	write.SysProcAttr = &syscall.SysProcAttr{
		// Groups is left empty on purpose: without it the child would
		// keep root's supplementary groups while carrying the account's
		// uid, which is more than the account has.
		Credential: &syscall.Credential{Uid: uid, Gid: gid},
	}
	write.Stdin = source
	var stderr bytes.Buffer
	write.Stderr = &stderr
	if err := write.Run(); err != nil {
		return "", fmt.Errorf("directadmin: write the copy into the home directory of %s: %s: %w",
			account, lastStderrLine(stderr.Bytes()), err)
	}
	return target, nil
}

// RemoveFromHome removes a file DeliverToHome wrote, as the account.
// Only a path under the account's downloads directory is removed, and
// only by the account: root unlinking inside a home directory would
// unlink whatever the account had put at that name.
func (r *Real) RemoveFromHome(ctx context.Context, account, path string) error {
	uid, gid, home, err := r.accountIdentity(account)
	if err != nil {
		return err
	}
	if filepath.Dir(filepath.Clean(path)) != filepath.Join(home, DownloadsDir) {
		return fmt.Errorf("directadmin: %s is not a copy delivered to %s", path, account)
	}
	remove := exec.CommandContext(ctx, "rm", "-f", "--", path)
	remove.Dir = home
	remove.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uid, Gid: gid},
	}
	var stderr bytes.Buffer
	remove.Stderr = &stderr
	if err := remove.Run(); err != nil {
		return fmt.Errorf("directadmin: remove the copy from the home directory of %s: %s: %w",
			account, lastStderrLine(stderr.Bytes()), err)
	}
	return nil
}

// accountIdentity is who the account is and where it lives, from the
// system and not from the request.
func (r *Real) accountIdentity(account string) (uid, gid uint32, home string, err error) {
	if err := usableAccountName(account); err != nil {
		return 0, 0, "", fmt.Errorf("directadmin: %w", err)
	}
	lookup := r.lookupUser
	if lookup == nil {
		lookup = user.Lookup
	}
	found, err := lookup(account)
	if err != nil {
		return 0, 0, "", fmt.Errorf("directadmin: resolve account identity: %w", err)
	}
	id, err := strconv.ParseUint(found.Uid, 10, 32)
	if err != nil {
		return 0, 0, "", fmt.Errorf("directadmin: %s has no numeric uid: %w", account, err)
	}
	group, err := strconv.ParseUint(found.Gid, 10, 32)
	if err != nil {
		return 0, 0, "", fmt.Errorf("directadmin: %s has no numeric gid: %w", account, err)
	}
	// Not a guard against a hostile caller -- one that reached here has
	// root already -- but against a mistake that would write a customer's
	// backup into a system account's files.
	if id == 0 {
		return 0, 0, "", fmt.Errorf("directadmin: %s is not an account", account)
	}
	home = filepath.Clean(found.HomeDir)
	if home == "" || home == "." || home == "/" || filepath.Dir(home) == home {
		return 0, 0, "", fmt.Errorf("directadmin: %s has no home directory of its own", account)
	}
	info, err := os.Stat(home)
	if err != nil {
		return 0, 0, "", fmt.Errorf("directadmin: home directory of %s: %w", account, err)
	}
	if !info.IsDir() {
		return 0, 0, "", fmt.Errorf("directadmin: home directory of %s is not a directory", account)
	}
	return uint32(id), uint32(group), home, nil
}

// usableDownloadName refuses anything that is not one file name: no
// path separators, no leading dot or dash, nothing the shell or a
// terminal would read as other than a name.
func usableDownloadName(name string) error {
	if name == "" || len(name) > 200 || name[0] == '.' || name[0] == '-' || strings.Contains(name, "..") {
		return fmt.Errorf("directadmin: %q is not a file name", name)
	}
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.':
		default:
			return fmt.Errorf("directadmin: %q is not a file name", name)
		}
	}
	return nil
}

// lastStderrLine is the last thing a child said, which is where a shell
// utility puts the reason.
func lastStderrLine(output []byte) string {
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

var _ panel.HomeDeliverer = (*Real)(nil)
var _ panel.PutBackGate = (*Real)(nil)
