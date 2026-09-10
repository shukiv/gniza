// Package dabackup is DirectAdmin's account backup, as Gniza has to read
// and rebuild it.
//
// # What is known, and what is not
//
// The whole-account path is checked against native DirectAdmin 1.709
// archives, including zstd and the backup/user.conf identity. Split mode
// is here too, in split.go: DirectAdmin will not produce the parts, so
// the archive it does produce is taken apart into them and put back
// together before its own restore sees it, and every member of a real
// 1.709 archive comes back header for header. What has not happened is
// an account restored from an archive Gniza rebuilt. The granular
// selectors below remain provisional.
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

	// NestedHomeArchive is the second compressed archive DirectAdmin
	// writes inside the first, holding the rest of the home directory.
	// Observed on the 1.709 fixture; testdata lists what is in it.
	NestedHomeArchive = "home.tar.zst"

	// MailDir is where the archive keeps the messages of each domain's
	// mailboxes, beside the websites in DomainsDir. Observed on the same
	// fixture.
	MailDir = "imap"

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
const Provisional = "DirectAdmin archives were validated on 1.709, split mode " +
	"rebuilds one header for header, and an account has been restored from an " +
	"archive Gniza rebuilt on one server (ADR 0021); granular restore and the " +
	"session bridge remain experimental: see ADR 0019"

// Layout answers where DirectAdmin keeps the parts of an account.
type Layout struct{}

// Panel names the panel this layout belongs to.
func (Layout) Panel() string { return "directadmin" }

// HomedirDir is where, under the account's own directory in the tree,
// the account's files belong.
//
// DirectAdmin's archive does not lay an account out as one home directory
// the way a cpmove tree does: it carries domains/ and imap/ separately
// and the rest of the home directory in a second archive inside the
// first. Those are three views of one directory, and taking the archive
// apart puts them back into it -- so this names that directory, and not
// domains/, which is a third of it. See split.go.
func (Layout) HomedirDir() string { return HomeTreeDir }

// DatabaseDir is where native SQL dumps live.
func (Layout) DatabaseDir() string { return DatabaseDirName }

// AccountRoot returns the account's own records inside a tree.
//
// A tree arrives in one of two shapes. An archive extracted whole unpacks
// its contents at the top rather than inside one directory named after
// the account, so the root is the tree itself. A backup taken apart into
// parts keeps those records in the metadata part, beside the home
// directory rather than above it, so the root is that part.
//
// Neither shape says whose account it is. That is what ValidateArchive is
// for, and it reads the archive rather than the tree.
func (Layout) AccountRoot(treeDir, account string) (string, error) {
	// The parts first: a split tree has a metadata part with the records
	// in it, and an extracted archive has no metadata directory at all.
	if _, err := os.Stat(filepath.Join(treeDir, MetadataTreeDir, BackupDir)); err == nil {
		return filepath.Join(treeDir, MetadataTreeDir), nil
	}
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
	_, err := inspect(ctx, filename, account, false)
	return err
}

// DrillArchive rehearses this archive: it checks everything
// ValidateArchive checks, and reads the body of every database dump it
// carries.
//
// A dump that came back empty restores an empty database, and a dump
// that came back truncated does the same thing less obviously. On
// cPanel those are caught in the rebuilt tree; a DirectAdmin snapshot
// has no tree, so without this a rehearsal of one proved the archive
// arrived and nothing about what is in it.
//
// Reading the bodies is why this is separate from ValidateArchive: a
// dump is the largest thing in the archive after the home directory,
// and a backup must not pay for a rehearsal's work on every run.
func (Layout) DrillArchive(ctx context.Context, filename, account string) ([]string, error) {
	found, err := inspect(ctx, filename, account, true)
	if err != nil {
		return nil, err
	}
	// The split path refuses a rebuilt tree whose home directory came
	// back empty. An archive carrying the account's identity and none of
	// its files is that same backup arriving as one file, and it
	// restores an account with nothing in it.
	if found.accountFiles == 0 && !found.nestedHome {
		return nil, fmt.Errorf(
			"dabackup: the archive carries %s's identity and none of its files -- "+
				"restoring it would put back an empty account", account)
	}

	var passed []string
	if found.accountFiles > 0 {
		passed = append(passed, fmt.Sprintf("%d account files in the archive", found.accountFiles))
	}
	if found.nestedHome {
		passed = append(passed, "the rest of the home directory is in "+
			path.Join(BackupDir, NestedHomeArchive))
	}
	names := slices.Compact(slices.Sorted(slices.Values(found.databases)))
	if len(names) > 0 {
		// An archive that names none is not a failure: the account may
		// have none, and DirectAdmin may carry them somewhere this
		// cannot read. It is not a check that can be claimed either.
		noun := "database dumps parse"
		if len(names) == 1 {
			noun = "database dump parses"
		}
		passed = append(passed, fmt.Sprintf("%d %s", len(names), noun))
	}
	return passed, nil
}

// archiveContents is what one walk of an archive found.
type archiveContents struct {
	// databases are the account's dumps, by name, in the order met.
	databases []string
	// accountFiles is how many of the account's own files the archive
	// carries loose -- its websites under domains/ and its messages
	// under imap/ -- as opposed to DirectAdmin's records of it.
	accountFiles int
	// nestedHome is whether the rest of the home directory is here, in
	// the second compressed archive DirectAdmin writes inside the first.
	nestedHome bool
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
	found, err := inspect(ctx, filename, account, false)
	if err != nil {
		return nil, err
	}
	return slices.Compact(slices.Sorted(slices.Values(found.databases))), nil
}

// inspect walks the archive once. It always checks the identity record;
// readDumps says whether to read the body of each dump as well, which is
// a rehearsal's work and not a restore's.
func inspect(ctx context.Context, filename, account string, readDumps bool) (archiveContents, error) {
	if !validUser(account) {
		return archiveContents{}, fmt.Errorf("dabackup: invalid expected account %q", account)
	}
	if !nameMatchesArchive(filepath.Base(filename), account) {
		return archiveContents{}, fmt.Errorf(
			"dabackup: archive filename %q does not belong to %s",
			filepath.Base(filename), account)
	}
	f, err := os.OpenFile(filename, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return archiveContents{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return archiveContents{}, fmt.Errorf("dabackup: account archive is not a regular file")
	}
	var reader io.Reader = f
	if strings.HasSuffix(filename, ".gz") {
		z, err := gzip.NewReader(f)
		if err != nil {
			return archiveContents{}, fmt.Errorf("dabackup: read compressed account archive: %w", err)
		}
		defer z.Close()
		reader = z
	} else if strings.HasSuffix(filename, ".zst") {
		z, err := zstd.NewReader(f, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(128<<20))
		if err != nil {
			return archiveContents{}, fmt.Errorf("dabackup: read zstd account archive: %w", err)
		}
		defer z.Close()
		reader = z
	}
	reader = contextReader{ctx, reader}
	tr := tar.NewReader(reader)
	identity := false
	found := archiveContents{}
	for {
		if err := ctx.Err(); err != nil {
			return archiveContents{}, err
		}
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return archiveContents{}, fmt.Errorf("dabackup: read account archive: %w", err)
		}
		// A member that climbs out of the archive is refused. A member
		// with an awkward name is not: a backslash is an ordinary
		// character in a Linux filename, it reaches a hosting account
		// through Windows FTP clients and through plugins that write
		// their own cache keys, and refusing the archive over one would
		// stop that account being backed up at all. This is the rule
		// cpmove uses.
		if path.IsAbs(h.Name) || strings.ContainsRune(h.Name, 0) {
			return archiveContents{}, fmt.Errorf("dabackup: unsafe account archive member %q", h.Name)
		}
		// Check before cleaning: cleaning would conceal backup/../user.conf.
		for _, component := range strings.Split(h.Name, "/") {
			if component == ".." {
				return archiveContents{}, fmt.Errorf("dabackup: unsafe account archive member %q", h.Name)
			}
		}
		name := path.Clean(h.Name)
		if name == "." && h.Typeflag == tar.TypeDir {
			continue
		}
		for _, component := range strings.Split(name, "/") {
			if component == ".." {
				return archiveContents{}, fmt.Errorf("dabackup: unsafe account archive member %q", h.Name)
			}
		}
		if h.Typeflag == tar.TypeReg {
			switch {
			case name == path.Join(BackupDir, NestedHomeArchive):
				found.nestedHome = true
			case accountFileIn(name):
				found.accountFiles++
			}
		}
		if database, ok := databaseIn(name, account, h.Typeflag); ok {
			found.databases = append(found.databases, database)
			if readDumps {
				restores, err := dumpRestoresSomething(tr)
				if err != nil {
					return archiveContents{}, fmt.Errorf("dabackup: read dump %s: %w", name, err)
				}
				if !restores {
					return archiveContents{}, fmt.Errorf(
						"dabackup: the dump %s carries nothing to restore -- a database "+
							"put back from it would come back empty", name)
				}
			}
		}
		if name != path.Join(BackupDir, UserConf) {
			continue
		}
		if identity || h.Typeflag != tar.TypeReg || h.Size > 1<<20 {
			return archiveContents{}, fmt.Errorf("dabackup: invalid account identity record %s", h.Name)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return archiveContents{}, err
		}
		named, found := usernameIn(string(body))
		if !found {
			return archiveContents{}, fmt.Errorf("dabackup: %s names no account", h.Name)
		}
		if named != account {
			return archiveContents{}, fmt.Errorf("dabackup: archive's account identity is %s, not %s", named, account)
		}
		identity = true
	}
	if !identity {
		return archiveContents{}, fmt.Errorf("dabackup: archive has no identity record for %s", account)
	}
	// tar.Reader stops at the end marker, before a compressor's checksum.
	// Consume padding and verify the entire compressed stream; refuse a
	// second hidden tar after the one whose identity we just checked.
	buf := make([]byte, 32<<10)
	for {
		n, err := reader.Read(buf)
		for _, b := range buf[:n] {
			if b != 0 {
				return archiveContents{}, fmt.Errorf("dabackup: non-padding data after account archive")
			}
		}
		if err == io.EOF {
			return found, nil
		}
		if err != nil {
			return archiveContents{}, fmt.Errorf("dabackup: finish account archive: %w", err)
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
// tarRegular is tar.TypeReg, named so the fixture test can ask about a
// listing that has no headers to hand.
const tarRegular = tar.TypeReg

// accountFileIn says whether an archive member is one of the account's
// own files rather than one of DirectAdmin's records of the account.
//
// The websites under domains/ and the messages under imap/ are what a
// customer would recognise as theirs. backup/ is DirectAdmin's own
// record -- the identity, the dumps, the password hash -- and an archive
// with that and nothing else restores an empty account.
func accountFileIn(name string) bool {
	root, _, nested := strings.Cut(name, "/")
	return nested && (root == DomainsDir || root == MailDir)
}

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
	if len(fields) >= 3 && isAccountType(fields[0]) && validUser(fields[1]) && validUser(fields[2]) {
		for _, field := range fields[3:] {
			if !validUser(field) {
				return "", fmt.Errorf("dabackup: invalid archive timestamp")
			}
		}
		return fields[2], nil
	}
	return "", fmt.Errorf("dabackup: unsupported account archive filename %q", base)
}

// dumpRestoresSomething says whether a database dump would put anything
// back, reading it as a stream.
//
// A dump is the largest member of the archive after the home directory,
// and a rehearsal that read one into memory would fail on exactly the
// accounts most worth rehearsing. So it is scanned in fixed-size pieces,
// carrying the tail of each one forward so a CREATE split across two of
// them is still found.
//
// Empty is the case that matters most: an empty dump restores an empty
// database, which is worse than an obvious failure. A dump with a header
// and no CREATE in it is the truncated version of the same thing. This
// is the check the cpmove path makes against the rebuilt tree, made
// against the archive instead.
func dumpRestoresSomething(r io.Reader) (bool, error) {
	const want = "CREATE"
	buf := make([]byte, 64<<10)
	carry := ""
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := carry + strings.ToUpper(string(buf[:n]))
			if strings.Contains(chunk, want) {
				return true, nil
			}
			if len(chunk) > len(want)-1 {
				chunk = chunk[len(chunk)-(len(want)-1):]
			}
			carry = chunk
		}
		if err == io.EOF {
			// An empty dump reaches here having found nothing, which is
			// the answer it should give.
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
}

// isAccountType is the first field of a DirectAdmin backup filename,
// which its own restore refuses in any other shape: "The file must be of
// the form: type.creator.username.tar.gz". The type is what the account
// is, so a server's resellers and its administrator are named
// "reseller.*" and "admin.*" -- reading only "user.*" failed every one of
// them, and on a server where one account in seven is a reseller that is
// one account in seven never being backed up.
func isAccountType(value string) bool {
	return value == "user" || value == "reseller" || value == "admin"
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
