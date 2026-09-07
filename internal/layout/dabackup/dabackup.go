// Package dabackup is DirectAdmin's account backup, as Gniza has to read
// and rebuild it.
//
// # What is known, and what is not
//
// The names here come from DirectAdmin's own documentation, read in
// September 2026, and not from a running server. What the documentation
// states -- the archive's name, the per-account configuration directory
// and its files, the home directory root, the folders DirectAdmin's own
// backups skip -- is marked as stated. What it does not state is marked
// as assumed, and an assumption here is a restore that silently produces
// an archive DirectAdmin will not read.
//
// ADR 0019 lists what a DirectAdmin host has to answer before any of this
// is run against a customer's server. Until it does, Provisional is what
// this package says about itself, and the agent refuses to select it
// without an operator saying so explicitly.
//
// Like cpmove, it imports nothing of Gniza's but the interface it
// implements: a format cannot depend on the machinery that reads it.
package dabackup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	// ConfigDir is where DirectAdmin keeps everything it knows about one
	// account. Stated: docs.directadmin.com, "Directories and locations".
	ConfigDir = "/usr/local/directadmin/data/users"
	// UserConf, DomainsList and CrontabConf are the files inside it that
	// carry the account's identity, its domains and its cron jobs.
	// Stated, same page.
	UserConf    = "user.conf"
	DomainsList = "domains.list"
	CrontabConf = "crontab.conf"
	// HomeRoot is where accounts live. Stated.
	HomeRoot = "/home"

	// BackupDir is the directory inside the archive holding the account's
	// configuration. Stated: an extracted user backup produces a backup
	// directory and the domains.
	BackupDir = "backup"
	// DomainsDir is where the archive keeps each domain's files.
	// Assumed: the documentation says the domains are in there, not what
	// the directory is called.
	DomainsDir = "domains"
	// DatabaseDirName is where Gniza places the database dumps when it
	// rebuilds an archive. Assumed entirely -- DirectAdmin's own backups
	// carry the dumps, but where is not documented.
	DatabaseDirName = "backup"

	// StagedGrantsFile and StagedRunnableFile are what Gniza names the
	// grants where it stages them, beside the dumps. They are granular's
	// constants spelt out rather than imported, for the same reason as in
	// cpmove.
	StagedGrantsFile   = "_users.sql"
	StagedRunnableFile = "_users-runnable.sql"
)

// Provisional says this layout has never been checked against a running
// DirectAdmin, and names what is at stake if it is wrong.
//
// It is a value rather than a comment because the agent prints it: an
// operator who selects DirectAdmin is told, in the log and on the page,
// that the restore path is unproven here.
const Provisional = "the DirectAdmin layout is taken from documentation and has " +
	"not been checked against a running server: see ADR 0019"

// Layout answers where DirectAdmin keeps the parts of an account.
type Layout struct{}

// Panel names the panel this layout belongs to.
func (Layout) Panel() string { return "directadmin" }

// HomedirDir is where, under the account's own directory in the archive,
// the account's files belong.
//
// Assumed. DirectAdmin does not lay an account out as one home directory
// beside one database directory the way a cpmove tree does; its archive
// carries the domains separately. This is the seam a DirectAdmin host has
// to settle first.
func (Layout) HomedirDir() string { return DomainsDir }

// DatabaseDir is where the dumps belong. Assumed, as above.
func (Layout) DatabaseDir() string { return DatabaseDirName }

// AccountRoot returns the account's own directory inside an extracted
// tree.
//
// A DirectAdmin archive unpacks its contents at the top rather than
// inside one directory named after the account, so the root is the tree
// itself -- but only once something in it identifies the account, which
// is what ValidateArchive is for.
func (Layout) AccountRoot(treeDir, account string) (string, error) {
	if _, err := os.Stat(filepath.Join(treeDir, BackupDir)); err != nil {
		return "", fmt.Errorf("dabackup: no %s directory in the extracted tree: %w", BackupDir, err)
	}
	return treeDir, nil
}

// PlaceDatabaseUsers leaves the grants where Gniza staged them.
//
// DirectAdmin's own restore reads the database users from the archive it
// made itself, and where that is has not been established here. Moving
// the file somewhere invented would be worse than leaving it beside the
// dumps, where whoever restores by hand can find it.
func (Layout) PlaceDatabaseUsers(root string) error { return nil }

// ValidateArchive binds the identity inside a DirectAdmin archive to the
// account it is about to be restored into.
//
// The record it reads is backup/user.conf, whose username= line is
// DirectAdmin's own answer to whose account this is. As in cpmove, a
// restic tag and a filename are not authoritative: the panel's restore
// reads what is inside.
func (Layout) ValidateArchive(ctx context.Context, filename, account string) error {
	if account == "" || strings.ContainsAny(account, "/\\.\x00") {
		return fmt.Errorf("dabackup: invalid expected account %q", account)
	}
	if !nameMatchesArchive(filepath.Base(filename), account) {
		return fmt.Errorf("dabackup: archive filename does not belong to %s", account)
	}
	f, err := os.OpenFile(filename, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("dabackup: account archive is not a regular file")
	}
	var reader io.Reader = f
	if strings.HasSuffix(filename, ".gz") {
		z, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("dabackup: read compressed account archive: %w", err)
		}
		defer z.Close()
		reader = z
	}
	tr := tar.NewReader(reader)
	identity := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("dabackup: read account archive: %w", err)
		}
		name := path.Clean(strings.TrimPrefix(h.Name, "./"))
		if name == "." && h.Typeflag == tar.TypeDir {
			continue
		}
		for _, component := range strings.Split(name, "/") {
			if component == ".." {
				return fmt.Errorf("dabackup: unsafe account archive member %q", h.Name)
			}
		}
		if name != path.Join(BackupDir, UserConf) {
			continue
		}
		if h.Typeflag != tar.TypeReg || h.Size > 1<<20 {
			return fmt.Errorf("dabackup: invalid account identity record %s", h.Name)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		named, found := usernameIn(string(body))
		if !found {
			return fmt.Errorf("dabackup: %s names no account", h.Name)
		}
		if named != account {
			return fmt.Errorf("dabackup: archive's account identity is %s, not %s", named, account)
		}
		identity = true
	}
	if !identity {
		return fmt.Errorf("dabackup: archive has no identity record for %s", account)
	}
	return nil
}

// nameMatchesArchive accepts the shapes DirectAdmin gives a user backup:
// user.<creator>.<account>.tar.gz, the same with a timestamp, and the
// bare account name Gniza uses where it makes one itself.
func nameMatchesArchive(base, account string) bool {
	trimmed := strings.TrimSuffix(strings.TrimSuffix(
		strings.TrimSuffix(base, ".zst"), ".gz"), ".tar")
	if trimmed == account {
		return true
	}
	fields := strings.Split(trimmed, ".")
	if len(fields) < 3 || fields[0] != "user" {
		return false
	}
	// user.<creator>.<account>, optionally followed by a timestamp.
	return fields[2] == account
}

// usernameIn reads the account out of a user.conf.
func usernameIn(body string) (string, bool) {
	for _, line := range strings.Split(body, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "username="); ok {
			return strings.TrimSpace(value), true
		}
	}
	return "", false
}
