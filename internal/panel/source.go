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
// and the databases the operator ticked beside it; a source may have the
// databases and no directory, for a server whose data is all in MySQL.
// A source that came from a container remembers which, so the pages can
// show the container's volumes together and the backup can keep the
// container's own description beside them.
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

// Candidates is what a server offers to be chosen: what is there, less
// what has been chosen already.
type Candidates struct {
	// Roots are the directories whose subdirectories are offered.
	Roots []string
	// Folders are directories under the roots that are not a source yet.
	Folders []string
	// MySQL and PostgreSQL are the databases each client can see. A nil
	// list with an error means the client is not there or cannot
	// connect; a nil list without one means the server has none.
	MySQL           []string
	MySQLError      string
	PostgreSQL      []string
	PostgreSQLError string
	// Containers is every container docker or podman knows about, with
	// the ones already chosen marked.
	Containers []ContainerCandidate
}

// ContainerCandidate is one container that could be backed up.
type ContainerCandidate struct {
	Engine string
	Name   string
	Image  string
	Status string
	// Chosen says a source already came from this container.
	Chosen bool
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
	// taken is refused with an error the operator can read.
	AddSource(ctx context.Context, source Source) error
	// AddContainer makes a source of each of the container's mounts,
	// and one for its description when it has none, and returns them.
	AddContainer(ctx context.Context, engine, name string) ([]Source, error)
	// RemoveSource forgets a choice. The backups taken of it stay.
	RemoveSource(ctx context.Context, name string) error
}
