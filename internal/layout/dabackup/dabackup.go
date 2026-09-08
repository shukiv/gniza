// Package dabackup is DirectAdmin's account backup, as Gniza has to read
// and rebuild it.
//
// # What is known, and what is not
//
// The whole-account path is checked against native DirectAdmin 1.709
// archives, including zstd and the backup/user.conf identity. Split and
// granular selectors below are still provisional: native archives have a
// nested backup/home.tar.zst as well as outer domains/ and imap/ trees.
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
	"slices"
	"sort"
	"strings"
	"syscall"

	"github.com/klauspost/compress/zstd"
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
	// Observed on the disposable 1.709 fixture; this is not the whole home.
	DomainsDir = "domains"
	// DatabaseDirName is where Gniza places the database dumps when it
	// rebuilds an archive. Native per-database SQL and .conf files were
	// observed here; rebuilding their authentication metadata is not supported.
	DatabaseDirName = "backup"

	// StagedGrantsFile and StagedRunnableFile are what Gniza names the
	// grants where it stages them, beside the dumps. They are granular's
	// constants spelt out rather than imported, for the same reason as in
	// cpmove.
	StagedGrantsFile   = "_users.sql"
	StagedRunnableFile = "_users-runnable.sql"
)

// Provisional describes the remaining limits of this integration.
//
// It is a value rather than a comment because the agent prints it: an
// operator who selects DirectAdmin is told, in the log and on the page,
// that the restore path is unproven here.
const Provisional = "DirectAdmin whole-account archives were validated on 1.709; " +
	"split/granular restore and the session bridge remain experimental: see ADR 0019"

// Layout answers where DirectAdmin keeps the parts of an account.
type Layout struct{}

// Panel names the panel this layout belongs to.
func (Layout) Panel() string { return "directadmin" }

// HomedirDir is where, under the account's own directory in the archive,
// the account's files belong.
//
// Not suitable for split reassembly. DirectAdmin does not lay an account out as one home directory
// beside one database directory the way a cpmove tree does; its archive
// carries the domains separately. This is the seam a DirectAdmin host has
// to settle first.
func (Layout) HomedirDir() string { return DomainsDir }

// DatabaseDir is where native SQL dumps live.
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
	_, err := inspect(ctx, filename, account)
	return err
}

// ArchiveDatabases names the account's database dumps that the archive
// carries, having checked the archive the way ValidateArchive does.
//
// It is what a finished restore is held to: every database the archive
// names has to be on the account afterwards. The names are read out of
// the archive rather than out of a filename or a tag, and only the dumps
// DirectAdmin's own backup writes are counted -- see databaseIn for what
// that excludes and why.
//
// An archive that names none is not an error. DirectAdmin may carry its
// dumps somewhere this cannot read them, nested inside another
// compressed member among other places, and a check that cannot see them
// has to stay quiet rather than fail every restore of an account that
// has databases. ADR 0019 question 8 says what that leaves open.
func ArchiveDatabases(ctx context.Context, filename, account string) ([]string, error) {
	found, err := inspect(ctx, filename, account)
	if err != nil {
		return nil, err
	}
	sort.Strings(found)
	return slices.Compact(found), nil
}

func inspect(ctx context.Context, filename, account string) ([]string, error) {
	if !validUser(account) {
		return nil, fmt.Errorf("dabackup: invalid expected account %q", account)
	}
	if !nameMatchesArchive(filepath.Base(filename), account) {
		return nil, fmt.Errorf("dabackup: archive filename does not belong to %s", account)
	}
	f, err := os.OpenFile(filename, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("dabackup: account archive is not a regular file")
	}
	var reader io.Reader = f
	if strings.HasSuffix(filename, ".gz") {
		z, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("dabackup: read compressed account archive: %w", err)
		}
		defer z.Close()
		reader = z
	} else if strings.HasSuffix(filename, ".zst") {
		z, err := zstd.NewReader(f, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(128<<20))
		if err != nil {
			return nil, fmt.Errorf("dabackup: read zstd account archive: %w", err)
		}
		defer z.Close()
		reader = z
	}
	reader = contextReader{ctx, reader}
	tr := tar.NewReader(reader)
	identity := false
	var databases []string
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("dabackup: read account archive: %w", err)
		}
		// A member that climbs out of the archive is refused. A member
		// with an awkward name is not: a backslash is an ordinary
		// character in a Linux filename, it reaches a hosting account
		// through Windows FTP clients and through plugins that write
		// their own cache keys, and refusing the archive over one would
		// stop that account being backed up at all. This is the rule
		// cpmove uses.
		if path.IsAbs(h.Name) || strings.ContainsRune(h.Name, 0) {
			return nil, fmt.Errorf("dabackup: unsafe account archive member %q", h.Name)
		}
		// Check before cleaning: cleaning would conceal backup/../user.conf.
		for _, component := range strings.Split(h.Name, "/") {
			if component == ".." {
				return nil, fmt.Errorf("dabackup: unsafe account archive member %q", h.Name)
			}
		}
		name := path.Clean(h.Name)
		if name == "." && h.Typeflag == tar.TypeDir {
			continue
		}
		for _, component := range strings.Split(name, "/") {
			if component == ".." {
				return nil, fmt.Errorf("dabackup: unsafe account archive member %q", h.Name)
			}
		}
		if database, ok := databaseIn(name, account, h.Typeflag); ok {
			databases = append(databases, database)
		}
		if name != path.Join(BackupDir, UserConf) {
			continue
		}
		if identity || h.Typeflag != tar.TypeReg || h.Size > 1<<20 {
			return nil, fmt.Errorf("dabackup: invalid account identity record %s", h.Name)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		named, found := usernameIn(string(body))
		if !found {
			return nil, fmt.Errorf("dabackup: %s names no account", h.Name)
		}
		if named != account {
			return nil, fmt.Errorf("dabackup: archive's account identity is %s, not %s", named, account)
		}
		identity = true
	}
	if !identity {
		return nil, fmt.Errorf("dabackup: archive has no identity record for %s", account)
	}
	// tar.Reader stops at the end marker, before a compressor's checksum.
	// Consume padding and verify the entire compressed stream; refuse a
	// second hidden tar after the one whose identity we just checked.
	buf := make([]byte, 32<<10)
	for {
		n, err := reader.Read(buf)
		for _, b := range buf[:n] {
			if b != 0 {
				return nil, fmt.Errorf("dabackup: non-padding data after account archive")
			}
		}
		if err == io.EOF {
			return databases, nil
		}
		if err != nil {
			return nil, fmt.Errorf("dabackup: finish account archive: %w", err)
		}
	}
}

// databaseIn reads a database dump's name out of an archive member.
//
// Two things have to hold, and the second one is the important one. The
// name has to be DirectAdmin's for one of this account's databases --
// <account>_<something>, the same convention the provider's own listing
// goes by -- and the dump has to be where DirectAdmin's own backup put
// it, directly in the archive's backup directory.
//
// A .sql file anywhere else in the archive belongs to the customer, not
// to DirectAdmin. phpMyAdmin writes one on every export and the migration
// plugins leave one behind, so there is very often one sitting in a web
// root, named after a database that was dropped a year ago. Holding a
// restore to that name would fail a restore that worked, on an account
// that is fine, every time -- and a restore reported as failed is a
// restore somebody runs again.
func databaseIn(name, account string, kind byte) (string, bool) {
	if kind != tar.TypeReg || path.Dir(name) != DatabaseDirName {
		return "", false
	}
	stem, ok := strings.CutSuffix(path.Base(name), ".sql")
	if !ok || !strings.HasPrefix(stem, account+"_") || !validUser(stem) {
		return "", false
	}
	return stem, true
}

// nameMatchesArchive accepts the shapes DirectAdmin gives a user backup:
// user.<creator>.<account>.tar.gz and the same with a timestamp. A name
// without one of the supported tar suffixes is not an account archive.
func nameMatchesArchive(base, account string) bool {
	named, err := ArchiveAccount(base)
	return err == nil && named == account
}

// ArchiveAccount reads a supported native filename, not its authoritative
// identity. Always call ValidateArchive before handing the file to DirectAdmin.
func ArchiveAccount(base string) (string, error) {
	if filepath.Base(base) != base {
		return "", fmt.Errorf("dabackup: expected an archive basename")
	}
	stem := ""
	for _, suffix := range []string{".tar.gz", ".tar.zst", ".tar"} {
		if strings.HasSuffix(base, suffix) {
			stem = strings.TrimSuffix(base, suffix)
			break
		}
	}
	if validUser(stem) {
		return stem, nil
	}
	fields := strings.Split(stem, ".")
	if len(fields) >= 3 && fields[0] == "user" && validUser(fields[1]) && validUser(fields[2]) {
		for _, field := range fields[3:] {
			if !validUser(field) {
				return "", fmt.Errorf("dabackup: invalid archive timestamp")
			}
		}
		return fields[2], nil
	}
	return "", fmt.Errorf("dabackup: unsupported account archive filename %q", base)
}

func validUser(value string) bool {
	if len(value) == 0 || len(value) > 64 || value[0] == '-' {
		return false
	}
	for _, c := range value {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// usernameIn reads the account out of a user.conf.
func usernameIn(body string) (string, bool) {
	var username string
	found := false
	for _, line := range strings.Split(body, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "username="); ok {
			if found {
				return "", false
			}
			username, found = strings.TrimSpace(value), true
		}
	}
	return username, found
}
