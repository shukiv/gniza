package panel

import (
	"context"
	"fmt"
	"strings"
)

// ArchiveLayout is where a panel keeps the parts of an account inside the
// archive its own restore reads.
//
// Gniza stages an account as parts -- its settings, its home directory, a
// dump per database -- and puts them back together into whatever shape the
// panel's restore expects. That shape is the panel's to define: cPanel's
// cpmove tree keeps the home directory in homedir/ and one dump per file
// in mysql/, and another panel keeps them somewhere else entirely.
type ArchiveLayout interface {
	// Panel names the panel whose archives this describes, for messages
	// and for the record kept against a restore.
	Panel() string

	// HomedirDir and DatabaseDir are where, under the account's own
	// directory inside the archive, the home directory and the database
	// dumps belong.
	HomedirDir() string
	DatabaseDir() string

	// AccountRoot is the account's own directory inside an extracted
	// tree. It is discovered rather than assumed: the name is the panel's
	// to choose, and a tree that does not hold one account is a tree that
	// must not be restored.
	AccountRoot(treeDir, account string) (string, error)

	// PlaceDatabaseUsers moves the grants Gniza staged beside the dumps to
	// wherever this panel's own restore reads them.
	//
	// A database restored without the user that opens it is a site that
	// still cannot start, and the panels do not agree on where that file
	// lives.
	PlaceDatabaseUsers(root string) error

	// ValidateArchive binds the identity inside an archive to the account
	// it is about to be restored into.
	//
	// A restic tag and a filename are not authoritative: both can say one
	// customer while the archive's own records say another, and the
	// panel's restore reads the records. Restoring one customer's data
	// into another's account is about the worst thing this program could
	// do, so every panel has to answer this for its own format.
	ValidateArchive(ctx context.Context, filename, account string) error
}

// ItemLayout is where a panel keeps one kind of thing, for a restore of
// that thing on its own.
//
// Members are prefixes inside the account's metadata archive. Paths are
// relative to the home directory. Both are the panel's names, and a
// snapshot from an older version of it may not carry all of them -- a plan
// asks for what it wants and the extraction reports what it found.
type ItemLayout interface {
	// SettingsMembers is the panel's own record of the account: who it
	// is, what it is allowed, which package it is on.
	SettingsMembers() []string
	// CronMembers is the account's scheduled jobs.
	CronMembers() []string
	// FTPMembers is its FTP logins.
	FTPMembers() []string
	// MailMembers is the mail configuration that is not maildir:
	// forwarders, filters, and the domains' mail settings.
	MailMembers() []string

	// DNSMembers, SSLMembers and DomainMembers are the zones, the
	// certificates and the web server configuration -- of the whole
	// account when no name is given, or of the domains named.
	DNSMembers(names []string) ([]string, error)
	SSLMembers(names []string) ([]string, error)
	DomainMembers(names []string) ([]string, error)

	// WebsitePaths is the document root, relative to the home directory.
	WebsitePaths() []string
	// MailboxPaths is where the messages of the named mailboxes live,
	// relative to the home directory.
	MailboxPaths(names []string) []string
}

// Layout is everything a panel has to say about where things are.
type Layout interface {
	ArchiveLayout
	ItemLayout
}

// ArchiveDrill is what a panel can prove about a whole-account archive
// without unpacking it.
//
// A rehearsal of a split snapshot walks the rebuilt tree: it counts the
// files in the home directory and reads every dump, because a dump that
// came back empty restores an empty database and that is worse than an
// obvious failure. A whole-account snapshot is one file the panel's own
// restore reads, so there is no tree to walk and none of those checks
// have anywhere to run.
//
// A panel whose archive Gniza can read inside implements this and makes
// those same checks against the archive: how much of the account it
// carries, and whether each dump would restore anything. One whose
// archive Gniza cannot read -- cPanel's, so far -- does not implement
// it, and the rehearsal reports what it actually checked rather than
// more.
//
// This is a rehearsal's question and not a restore's. It reads the
// bodies of what it finds, which is work every backup would otherwise
// pay for on every run, so it is deliberately not part of
// ValidateArchive.
type ArchiveDrill interface {
	// DrillArchive returns the checks the archive passed, or the first
	// one it failed.
	DrillArchive(ctx context.Context, filename, account string) ([]string, error)
}

// ArchivePacker is a panel whose account archive Gniza takes apart
// itself, because the panel will not produce the parts.
//
// cPanel is asked for them: pkgacct has --skiphomedir and the home
// directory is backed up in place. DirectAdmin has nothing of the kind --
// admin-backup writes one compressed archive and its documentation says
// nothing about telling it to leave anything out -- and restic cannot
// deduplicate a compressed archive, so a nightly backup of one stores
// close to a full copy every night (docs/DESIGN.md §4). The parts are
// taken out of the archive it does write, and put back into it before
// its own restore sees them.
//
// A layout implements this only if it can do both halves. The pair is the
// point: a backup that could be taken apart and not put back together is
// a backup nothing can restore, which is worse than one that was never
// split.
type ArchivePacker interface {
	// UnpackArchive takes an account archive apart into dir, which
	// becomes the parts a split payload hands restic.
	UnpackArchive(ctx context.Context, archivePath, account, dir string) error
	// PackArchive puts it back together into outDir, under the name the
	// panel's own restore expects, and reports where it put it.
	PackArchive(ctx context.Context, dir, account, outDir string) (string, error)
}

// UsableDomainName refuses anything that is not a domain name.
//
// A name reaches this from an operator's choice in the interface and is
// then used to build paths inside an archive, so it is checked here rather
// than trusted: the shapes that matter are a slash, a leading dot and
// anything that could climb out of the directory it is meant to name.
func UsableDomainName(domain string) error {
	refuse := fmt.Errorf("%q is not a domain name", domain)
	if domain == "" || len(domain) > 253 {
		return refuse
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return refuse
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 ||
			label[0] == '-' || label[len(label)-1] == '-' {
			return refuse
		}
		for _, char := range label {
			switch {
			case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z',
				char >= '0' && char <= '9', char == '-', char == '_':
			default:
				return refuse
			}
		}
	}
	return nil
}
