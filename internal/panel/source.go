package panel

import (
	"context"
	"time"
)

// Source is one thing an operator chose to back up on a server that has
// no panel to say what an account is (ADR 0025).
//
// A source is backed up as an account of its own, under its name, so the
// scheduler, the snapshots' tags, retention and the restore machinery
// see nothing new. What it carries is one directory, read where it lies,
// or one database, dumped beside a note; a source may still carry a
// directory and databases together, which is what the first shape of
// the form made. A source that came from a container remembers which,
// so the pages can show the container's volumes together and the backup
// can keep the container's own description beside them.
type Source struct {
	Name string `json:"name"`
	// Path is the directory backed up, absolute, or empty for a source
	// of databases only. One directory per source: a snapshot has one
	// home directory part (internal/reassemble), so a second folder is
	// a second source.
	Path string `json:"path,omitempty"`
	// MySQL and PostgreSQL are the databases dumped beside the files,
	// by name, as the operator chose them.
	MySQL      []string `json:"mysql,omitempty"`
	PostgreSQL []string `json:"postgresql,omitempty"`
	// MySQLUsers are the accounts, as user@host, whose password hash
	// and grants are kept with the dumps so a restore brings back the
	// login that opens the database. Each has rights on one of MySQL.
	MySQLUsers []string `json:"mysql_users,omitempty"`
	// Container is set on a source made from a container: Path is then
	// one of its mounts on the host.
	Container *ContainerRef `json:"container,omitempty"`
	AddedAt   time.Time     `json:"added_at"`
}

// ContainerRef names the container a source came from.
type ContainerRef struct {
	// Engine is docker or podman.
	Engine string `json:"engine"`
	Name   string `json:"name"`
	// Mount is where, inside the container, the source's path is
	// mounted. Empty for a source that holds only the container's
	// description, made for a container with no mounts.
	Mount string `json:"mount,omitempty"`
}

// Candidates is what a server offers to be chosen, by kind, with what
// has been chosen already marked so the pages can show it greyed rather
// than offer it twice.
type Candidates struct {
	// Roots are the directories whose subdirectories are offered.
	Roots []string
	// Folders are the directories under the roots.
	Folders []FolderCandidate
	// MySQL and PostgreSQL are the databases each client can see. A nil
	// list with an error means the client is not there or cannot
	// connect; a nil list without one means the server has none.
	MySQL           []DatabaseCandidate
	MySQLError      string
	PostgreSQL      []DatabaseCandidate
	PostgreSQLError string
	// MySQLUsers is every account the MySQL server has, with what each
	// can reach, so the operator sees who opens which database and can
	// choose the accounts to keep with the dumps.
	MySQLUsers []DatabaseUserCandidate
	// Containers is every container docker or podman knows about.
	Containers []ContainerCandidate
	// Stacks are the compose projects the containers belong to, with
	// the directory their compose files live in, which is the stack's
	// configuration and is offered as a folder.
	Stacks []StackCandidate
	// Engines are the container engines looked for, present or not, and
	// the directory each keeps its own configuration in.
	Engines []EngineCandidate
}

// FolderCandidate is one directory that could be backed up.
type FolderCandidate struct {
	Path string
	// ChosenAs names the source that already backs this directory up,
	// or is empty.
	ChosenAs string
}

// DatabaseCandidate is one database that could be dumped.
type DatabaseCandidate struct {
	Name string
	// Size is what the server says it holds, in bytes; zero when it
	// could not say.
	Size uint64
	// Users are the accounts with rights on it: MySQL's grantees at the
	// schema level, PostgreSQL's owner. They are listed so the operator
	// knows whose data it is.
	Users []string
	// Rights says what each of those accounts can do on it (MySQL).
	Rights []DatabaseRight
	// ChosenAs names the source that already dumps this database, or is
	// empty.
	ChosenAs string
}

// DatabaseRight is one account's privileges on one database.
type DatabaseRight struct {
	User string
	Host string
	// Database is the one the privileges are on, when the right is read
	// from the account's side.
	Database   string
	Privileges []string
}

// Who is the account as user@host.
func (r DatabaseRight) Who() string { return r.User + "@" + r.Host }

// DatabaseUserCandidate is one account of the MySQL server: who it is,
// what authenticates it, and what it can reach.
type DatabaseUserCandidate struct {
	User string
	Host string
	// Plugin is the authentication plugin; the hash is never shown.
	Plugin string
	// System says the account is the server's own or the operator's
	// (root, mysql, mariadb.sys ...): shown, not offered.
	System bool
	// Global are the privileges held on every database, USAGE left out.
	Global []string
	// Rights are the privileges held on single databases.
	Rights []DatabaseRight
	// AttachedTo names the sources that keep this account with their
	// dumps, comma joined, or is empty.
	AttachedTo string
}

// Who is the account as user@host.
func (u DatabaseUserCandidate) Who() string { return u.User + "@" + u.Host }

// ContainerCandidate is one container that could be backed up.
type ContainerCandidate struct {
	Engine string
	Name   string
	Image  string
	Status string
	// Stack is the compose project the container belongs to, from its
	// labels, or empty.
	Stack string
	// Mounts counts the volumes and bind mounts it has on the host: what
	// choosing it backs up.
	Mounts int
	// Chosen says a source already came from this container.
	Chosen bool
}

// StackCandidate is one compose project: the containers under one
// compose file, and the directory that file lives in.
type StackCandidate struct {
	Engine string
	Name   string
	// Dir is the compose working directory, holding the compose file
	// and usually the .env beside it: the stack's configuration.
	Dir        string
	Containers []string
	// ChosenAs names the source that already backs the directory up, or
	// is empty.
	ChosenAs string
}

// EngineCandidate is one container engine looked for on the server.
type EngineCandidate struct {
	// Name is docker or podman.
	Name string
	// Present says the engine's command answered.
	Present bool
	// ConfigDir is where the engine keeps its own configuration
	// (/etc/docker, /etc/containers), when that directory exists.
	ConfigDir string
	// ChosenAs names the source that already backs ConfigDir up, or is
	// empty.
	ChosenAs string
	// Error says why the engine listed nothing, when it is present but
	// did not answer.
	Error string
}

// Listing is one directory of the server, for the folder browser: what
// is under it, folders first, with the ones chosen already marked.
type Listing struct {
	Dir string
	// Parent is the directory above, or empty at /.
	Parent string
	// Within names the source whose folder is Dir or holds it, or is
	// empty: everything under Dir is backed up already as part of it.
	Within  string
	Entries []FolderEntry
	// More counts the files left out once the first ones are shown; a
	// folder is never left out.
	More int
}

// FolderEntry is one folder or file in a Listing.
type FolderEntry struct {
	Name string
	Path string
	// Kind is "folder" or "file". A file is shown so the folder can be
	// recognised by what it holds; only a folder is chosen.
	Kind string
	// Size is the file's, in bytes; zero for a folder.
	Size int64
	// Link says the entry is a symbolic link, and Kind is its target's.
	Link bool
	// ChosenAs names the source that already backs this folder up, or
	// is empty.
	ChosenAs string
}

// Browser is implemented by a Chooser that can show the server's
// directories one at a time, so a folder can be found rather than
// typed.
type Browser interface {
	// Browse lists the directories under dir, which is absolute. A path
	// that is not a directory is refused with an error the operator can
	// read.
	Browse(ctx context.Context, dir string) (Listing, error)
}

// Chooser is implemented by a provider whose accounts are chosen by the
// operator rather than declared by a panel. The pages ask for it and
// show the choosing when it is there; on cPanel and DirectAdmin it is
// not, and the accounts page is the panel's list as before.
type Chooser interface {
	// Sources is what has been chosen, by name.
	Sources(ctx context.Context) ([]Source, error)
	// Candidates is what could be chosen now.
	Candidates(ctx context.Context) (Candidates, error)
	// AddSource records a choice. A source with neither a path nor a
	// database, a path that is not a directory, or a name already
	// taken is refused with an error the operator can read. A source
	// left unnamed is named after its folder, or mysql-<database> or
	// pg-<database>.
	AddSource(ctx context.Context, source Source) error
	// AddContainer makes a source of each of the container's mounts,
	// and one for its description when it has none, and returns them.
	AddContainer(ctx context.Context, engine, name string) ([]Source, error)
	// AttachMySQLUser keeps an account, as user@host, with every source
	// that dumps a database it has rights on, and returns those sources'
	// names. An account with rights on no database chosen, or that the
	// server does not have, is refused with an error the operator can
	// read.
	AttachMySQLUser(ctx context.Context, who string) ([]string, error)
	// RemoveSource forgets a choice. The backups taken of it stay.
	RemoveSource(ctx context.Context, name string) error
}
