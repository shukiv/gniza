package directadmin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/shukiv/gniza/internal/pkgacct"
)

// systemAccount is the name a server's own configuration is backed up
// under, on every panel. No account can be called it: "@" is not a
// character a unix user name may carry.
const systemAccount = "@system"

// systemPaths is the server-level configuration worth carrying to a
// replacement machine: DirectAdmin's own, and that of the services it
// configures. An account's own data is backed up as that account.
//
// A path that is not there is skipped and written down as absent. One
// list serves Apache, nginx and OpenLiteSpeed, ProFTPD and Pure-FTPd,
// and every version of DirectAdmin this runs on. An entry with a star
// in it is a pattern.
var systemPaths = []string{
	// DirectAdmin itself: directadmin.conf, the licence, the certificate
	// the panel serves, and the password it reaches MySQL with.
	"/usr/local/directadmin/conf",
	// The administrator's side of it: packages, the addresses the server
	// owns, reseller limits, login keys, brute-force settings.
	"/usr/local/directadmin/data/admin",
	// Templates and skins an administrator has changed. The ones
	// DirectAdmin ships are installed with it.
	"/usr/local/directadmin/data/templates/custom",
	"/usr/local/directadmin/data/tickets",
	// Hooks an administrator wrote.
	"/usr/local/directadmin/scripts/custom",
	// What CustomBuild was told to build, and what was put in front of
	// its own configuration. With these a replacement server builds the
	// same web server, the same PHP versions and the same mail stack.
	"/usr/local/directadmin/custombuild/options.conf",
	"/usr/local/directadmin/custombuild/php_extensions.conf",
	"/usr/local/directadmin/custombuild/custom",
	"/usr/local/directadmin/custombuild/versions.txt",

	// Mail: which domains are delivered here and to whom, and exim and
	// dovecot as they are configured.
	"/etc/virtual",
	"/etc/exim.conf",
	"/etc/exim.*.conf",
	"/etc/exim.*.conf.custom",
	"/etc/exim.pl",
	"/etc/system_filter.exim",
	"/etc/exim.key",
	"/etc/exim.cert",
	"/etc/dovecot",

	// DNS. The zones are each account's, and travel with it too; the
	// signing keys beside them are the one thing here that cannot be
	// made again. A zone whose DNSSEC keys are lost has to be unsigned
	// at the registrar before it resolves.
	"/etc/named.conf",
	"/etc/named",
	"/etc/rndc.key",
	"/var/named",

	// The web server, whichever it is, and PHP as each version is set.
	"/etc/httpd/conf",
	"/etc/nginx",
	"/usr/local/lsws/conf",
	"/usr/local/php*/lib/php.ini",
	"/usr/local/php*/lib/php.conf.d",
	"/usr/local/lib/php.ini",

	"/etc/my.cnf",
	"/etc/my.cnf.d",
	"/etc/mysql",

	"/etc/proftpd.conf",
	"/etc/proftpd.passwd",
	"/etc/pure-ftpd.conf",
	"/etc/pure-ftpd",

	"/etc/csf",
	"/etc/hosts",

	// Who exists on the machine, and the uids the restored homes are
	// owned by.
	"/etc/passwd",
	"/etc/group",
	"/etc/shadow",

	// Scheduled work that belongs to no account.
	"/var/spool/cron",
	"/etc/crontab",
	"/etc/cron.d",
}

// SystemNotCarried is what a system backup of a DirectAdmin server leaves
// out on purpose, so nobody takes the archive for a whole machine.
var SystemNotCarried = []string{
	"Anything that belongs to an account: its home directory, databases, mail and " +
		"settings all travel in that account's own backup.",
	"DirectAdmin's own installed files, its skins and its plugins. A replacement " +
		"server installs DirectAdmin and the plugins first; this is what was " +
		"configured on top of them.",
	"What CustomBuild compiled. options.conf and custom/ are carried, and " +
		"CustomBuild builds the same software from them.",
	"Gniza's own configuration, including the key that decrypts these backups. " +
		"A backup that contained the key to itself would protect nothing.",
}

// StageSystem copies the server's own configuration into the staging
// directory.
//
// It is a copy rather than an archive so restic sees files it can
// deduplicate: a server's configuration changes a little at a time, and
// most nights it stores almost nothing.
//
// What this does not do is put any of it back. A replacement server is
// set up by an administrator reading these files, and which of them can
// be written over a running DirectAdmin is a question for a host to
// answer, not this list.
func (r *Real) StageSystem(ctx context.Context, stagingDir string) (pkgacct.Payload, error) {
	root := filepath.Join(stagingDir, "system")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return pkgacct.Payload{}, fmt.Errorf("directadmin: create %s: %w", root, err)
	}

	var copied, absent []string
	for _, listed := range systemPaths {
		found, err := r.systemMatches(listed)
		if err != nil {
			return pkgacct.Payload{}, err
		}
		if len(found) == 0 {
			absent = append(absent, listed)
			continue
		}
		for _, path := range found {
			if err := ctx.Err(); err != nil {
				return pkgacct.Payload{}, err
			}
			source := filepath.Join(r.systemRoot(), path)
			info, err := os.Lstat(source)
			if err != nil {
				continue
			}
			target := filepath.Join(root, "files", strings.TrimPrefix(path, "/"))
			if err := copySystemPath(source, target, info); err != nil {
				return pkgacct.Payload{}, err
			}
			copied = append(copied, path)
		}
	}
	if len(copied) == 0 {
		// Every path absent is not a DirectAdmin server with nothing
		// configured; it is this looking in the wrong place. An empty
		// backup that succeeded would be found out at the worst time.
		return pkgacct.Payload{}, fmt.Errorf(
			"directadmin: none of the server's configuration was found under %s",
			r.systemRoot())
	}
	if err := r.writeSystemManifest(ctx, root, copied, absent); err != nil {
		return pkgacct.Payload{}, err
	}

	payload := pkgacct.Payload{
		Mode:    pkgacct.ModeSystem,
		Account: systemAccount,
		Parts:   []pkgacct.Part{{Kind: pkgacct.PartSystem, Path: root}},
	}
	return payload, payload.Verify()
}

// systemRoot is where the paths are looked for. It is "/" except in a
// test.
func (r *Real) systemRoot() string {
	if r.SystemRoot != "" {
		return r.SystemRoot
	}
	return "/"
}

// systemMatches is the paths one entry of the list names on this server,
// as they are written in the list: absolute, and without the root a test
// puts in front of them.
func (r *Real) systemMatches(listed string) ([]string, error) {
	under := filepath.Join(r.systemRoot(), listed)
	if !strings.ContainsAny(listed, "*?[") {
		if _, err := os.Lstat(under); err != nil {
			return nil, nil
		}
		return []string{listed}, nil
	}
	matches, err := filepath.Glob(under)
	if err != nil {
		return nil, fmt.Errorf("directadmin: %s is not a pattern: %w", listed, err)
	}
	sort.Strings(matches)
	found := make([]string, 0, len(matches))
	for _, match := range matches {
		rel, err := filepath.Rel(r.systemRoot(), match)
		if err != nil {
			return nil, err
		}
		found = append(found, "/"+rel)
	}
	return found, nil
}

// writeSystemManifest records what was taken and what this server was, so
// whoever sets up its replacement can judge rather than guess.
func (r *Real) writeSystemManifest(ctx context.Context, root string, copied, absent []string) error {
	var manifest strings.Builder
	manifest.WriteString("# Gniza system backup\n")
	manifest.WriteString("panel\tDirectAdmin\n")
	fmt.Fprintf(&manifest, "paths_copied\t%d\n", len(copied))
	if out, err := exec.CommandContext(ctx, r.binary(), "version").Output(); err == nil {
		fmt.Fprintf(&manifest, "directadmin_version\t%s\n", firstLineOf(out))
	}
	for _, path := range copied {
		fmt.Fprintf(&manifest, "copied\t%s\n", path)
	}
	for _, path := range absent {
		fmt.Fprintf(&manifest, "absent\t%s\n", path)
	}
	return os.WriteFile(filepath.Join(root, "manifest.txt"), []byte(manifest.String()), 0o600)
}

func firstLineOf(out []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(line)
}

// copySystemPath copies a file or a tree, keeping modes and symlinks and
// following nothing.
//
// It copies a server that is running. A file that was there when the
// directory was listed and gone when it was opened -- a session, a lock,
// a queue entry -- is not a failure of the backup.
func copySystemPath(source, target string, info os.FileInfo) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("directadmin: create %s: %w", filepath.Dir(target), err)
	}
	if !info.IsDir() {
		return copySystemEntry(source, target, info)
	}
	return filepath.Walk(source, func(path string, walked os.FileInfo, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("directadmin: read %s: %w", path, err)
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		into := filepath.Join(target, rel)
		if walked.IsDir() {
			return os.MkdirAll(into, 0o700)
		}
		return copySystemEntry(path, into, walked)
	})
}

// copySystemEntry copies one thing that is not a directory. A socket, a
// device and a pipe are nothing a backup can carry.
func copySystemEntry(source, target string, info os.FileInfo) error {
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		// The name is worth keeping; the target is not followed, since it
		// points at something this copy does not own.
		link, err := os.Readlink(source)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("directadmin: read %s: %w", source, err)
		}
		_ = os.Remove(target)
		return os.Symlink(link, target)
	case !info.Mode().IsRegular():
		return nil
	}
	in, err := os.Open(source)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("directadmin: read %s: %w", source, err)
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("directadmin: write %s: %w", target, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("directadmin: copy %s: %w", source, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("directadmin: write %s: %w", target, err)
	}
	// These are configuration files whose modes matter.
	return os.Chmod(target, info.Mode().Perm())
}
