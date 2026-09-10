// Package panel is the interface between Gniza and the hosting panel a
// server runs.
//
// Everything that knows what an account is, how to package one and how to
// put one back sits behind this interface. The scheduler, the
// repositories, the restore machinery and the web interface do not, and a
// second panel is meant to be another implementation of these methods
// rather than a fork of the program.
//
// A server runs one panel. Which implementation is in use is decided once,
// where the agent starts, and nothing downstream asks again.
//
// The work is behind an interface for a second reason: the agent can be
// exercised end to end on a machine with no panel installed at all, by a
// fake provider that builds a synthetic account tree with the same shape
// the real one produces.
package panel

import (
	"context"

	"github.com/shukiv/gniza/internal/pkgacct"
)

// AccountInfo is what the provider knows about an account.
//
// Accounts fills in only User and HomeDir, because measuring a size means
// walking a home directory and listing databases means a MySQL round trip.
// Account fills in everything, and is called when a backup is about to run.
// ApplyOptions are the choices an operator makes about handing an archive
// to the panel's own restore. Providers must refuse safety options they
// cannot honor, not silently ignore them.
type ApplyOptions struct {
	// Unrestricted turns off cPanel's Restricted Restore.
	//
	// Restricted is the default here, and cPanel's is the opposite. The
	// archive contains a customer's home directory, which the customer
	// controls, and restorepkg runs as root: unrestricted mode will
	// follow what it finds in there. Restricted mode refuses some
	// legitimate content too, which is why this is a choice rather than
	// a rule.
	// DirectAdmin currently requires this explicit acknowledgement for
	// native whole-account overwrite: an equivalent restricted restore has
	// not been established there. False is refused, never silently weakened.
	Unrestricted bool
	// Overwrite restores over an account that is already on this server.
	// Without it restorepkg will not replace what is there, and a restore
	// the interface promised would overwrite the account quietly did not.
	Overwrite bool
	// NewUser asks restorepkg to create the archive under a disposable
	// username. It is used only by live restore certification.
	NewUser string
	// SkipDNS prevents a certification restore from changing production
	// DNS zones.
	SkipDNS bool
}

// Certifier is implemented by a provider that can prove an archive is
// accepted by the panel's own restore on an isolated certification host.
// On cPanel that is restorepkg.
type Certifier interface {
	Certify(ctx context.Context, archivePath, disposableUser string) error
}

type AccountInfo struct {
	User      string
	HomeDir   string
	Databases []string
	// HasPostgreSQL is true when the panel records PostgreSQL data for
	// this account, or when that record cannot reliably rule it out.
	// Split payloads only dump MySQL themselves, so PostgreSQL requires the
	// complete pkgacct archive to avoid a successful backup with missing data.
	HasPostgreSQL bool
	// SizeBytes drives the staging space preflight. Zero means it has not
	// been measured, not that the account is empty.
	SizeBytes uint64
	// LeanBytes is what the panel has to write when the home directory is
	// read where it lies: on DirectAdmin the mail and the database dumps,
	// which is all a backup that leaves out "domain" still carries. It
	// drives the same preflight for that shape, where reserving the whole
	// account refuses backups a disk had room for. Zero is honest -- an
	// account with no mail and no databases writes nothing but its
	// records -- so a caller adds its own floor rather than reading zero
	// as unmeasured.
	LeanBytes uint64
	// PrimaryDomain is shown in listings where it is known.
	PrimaryDomain string
	// Missing says the panel knows about this account but its home
	// directory is not there. Such an account is still listed, because an account
	// left off the page is an account nobody notices is not being backed
	// up; its backup will fail, loudly, which is the point.
	Missing bool
}

// StageRequest asks a provider to materialise one account's payload.
type StageRequest struct {
	Account    AccountInfo
	StagingDir string
	Mode       pkgacct.Mode
	// SkipHomedir and SkipDatabases leave those parts out of the payload
	// entirely, for a schedule that says so.
	SkipHomedir   bool
	SkipDatabases bool
	// SkipEmail leaves the account's mail out. It reaches pkgacct as well
	// as restic: excluding ~/mail keeps the messages out of the file
	// backup, but the mail configuration -- the account names, and the
	// hashes that go with them -- is packed inside pkgacct's own archive,
	// where no restic exclude can reach it.
	SkipEmail bool
}

// Provider produces backup payloads for accounts.
type Provider interface {
	// Name is what this panel is called where an operator reads it:
	// "cPanel", "DirectAdmin". The interface tells operators what is
	// about to run, and the warning above the one irreversible button
	// here named the wrong panel on every DirectAdmin server until this
	// existed.
	Name() string

	// Layout says where this panel keeps the parts of an account, both
	// inside the archive its restore reads and inside the metadata a
	// single-item restore takes things out of.
	Layout() Layout

	// NativeExcludes is what the panel's own backups would leave out of
	// this account -- on cPanel, the server-wide cpbackup-exclude.conf
	// and the account's own. An operator who wrote a path in there has said it
	// must not leave the server.
	NativeExcludes(home string) []string

	// Capabilities reports which pkgacct flags this host supports.
	Capabilities(ctx context.Context) (pkgacct.Capabilities, error)

	// Accounts lists every account on the host. Standalone mode
	// uses it to resolve a policy that covers "all accounts" at run time,
	// so accounts added later are picked up without an edit.
	Accounts(ctx context.Context) ([]AccountInfo, error)

	// Account looks up an account's home directory and databases.
	Account(ctx context.Context, user string) (AccountInfo, error)

	// Stage writes the payload into StagingDir and returns what was
	// produced. The returned payload reports Degraded when the host
	// forced a layout that deduplicates poorly.
	Stage(ctx context.Context, req StageRequest) (pkgacct.Payload, error)

	// StageSystem materialises the server's own configuration: what a
	// replacement machine needs before the accounts restored onto it mean
	// anything.
	StageSystem(ctx context.Context, stagingDir string) (pkgacct.Payload, error)

	// Apply hands a rebuilt account archive to the panel's own restore,
	// overwriting the live account. Callers must only reach this when an operator has
	// explicitly asked for it.
	//
	// It returns what the panel printed. A restore where a module failed
	// still exits zero -- cPanel treats most of them as non-fatal and
	// carries on -- so the transcript is the only account of what was
	// actually put back, and throwing it away on success left nothing to
	// read afterwards.
	Apply(ctx context.Context, archivePath string, options ApplyOptions) (string, error)

	// PutHomeDir copies a restored subtree back into an account's home
	// directory, as that account.
	//
	// The tree comes out of a root-owned staging directory, which the
	// account cannot read; what lands in the home directory is written by
	// the account itself, so it is owned by them and can reach nothing
	// they could not already reach. The caller passes the tree rooted
	// where the home directory was: what is under from/public_html lands
	// in the account's own public_html.
	PutHomeDir(ctx context.Context, user, from string) error

	// CreateDatabase makes a database the account does not have, so a
	// dump has somewhere to go.
	//
	// Somebody who dropped a database and wants it back is the reason
	// granular restores exist, and "create it again first, then come
	// back" is a step this can take for them. It runs as the account, so
	// the panel applies the account's own database quota and name
	// prefix, and writes the panel's record of it -- a database made
	// behind the panel's back is one the customer cannot see or delete.
	CreateDatabase(ctx context.Context, user, database string) error

	// LoadDatabase replaces the contents of one of the account's databases
	// with a dump taken from a backup.
	//
	// The database must already exist and must belong to the account:
	// either it survived, or CreateDatabase made it a moment ago.
	LoadDatabase(ctx context.Context, user, database, dumpPath string) error

	// PutCrontab replaces the account's cron jobs with the ones a backup
	// holds.
	//
	// The whole crontab at once, because that is what the backup holds
	// and what cron reads: an account's jobs are lines in one file, and
	// putting back some of them would leave a file that is neither what
	// is running now nor what was backed up. A job added since the backup
	// was taken goes; a copy of what was replaced is kept on the server.
	PutCrontab(ctx context.Context, user, from string) error

	// PutDatabaseUsers recreates the account's database users, with the
	// passwords they had, and grants them what the backup says they had.
	//
	// A database restored without the user that reads it is a site that
	// still cannot start, which is why this exists beside LoadDatabase.
	// The users are taken as values rather than as a file: what runs here
	// runs as root against the server's MySQL, and the statements are
	// built from checked fields rather than from something a backup
	// happens to contain.
	PutDatabaseUsers(ctx context.Context, user string, users []DatabaseUser) error
}

// DatabaseUser is one database login as a backup recorded it.
type DatabaseUser struct {
	// Name and Host are the two halves of a MySQL account. One name can
	// exist on several hosts with different privileges, and a grant put
	// back against the wrong host is an application that cannot connect.
	Name string
	Host string
	// Plugin authenticates Hash: caching_sha2_password, mysql_native_password.
	Plugin string
	// Hash is the stored authentication string, hex-encoded. It is hex
	// because caching_sha2_password's is binary and would not survive
	// being carried as text.
	Hash string
	// Grants are the privileges the backup recorded, per database.
	Grants []DatabaseGrant
}

// DatabaseGrant is what one user was allowed to do to one database.
type DatabaseGrant struct {
	Database string
	// Privileges are MySQL privilege names as the backup recorded them,
	// or "ALL PRIVILEGES".
	Privileges []string
}
