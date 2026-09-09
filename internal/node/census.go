package node

import (
	"context"
	"time"

	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/resticrun"
)

const (
	// censusEvery is how often each repository is asked what it holds.
	// The answer changes by one night's backups, and getting it costs a
	// walk of the repository index across the network.
	censusEvery = 6 * time.Hour
	// censusRetryAfter keeps a repository that cannot be measured from
	// being tried on every tick. A destination that is down is down for
	// hours.
	censusRetryAfter = time.Hour
	// censusTimeout bounds one repository. The first reading of a large
	// repository over SFTP downloads the whole index; later ones are
	// served from the local restic cache and take seconds.
	censusTimeout = 20 * time.Minute
)

// censusRepositories measures what each repository holds, off the
// scheduler's own goroutine.
//
// Nothing here may delay a backup: the tick this runs from is the one
// that fires the schedules, and a repository that takes ten minutes to
// answer would hold the 02:00 run behind it. So the work is handed to a
// goroutine, and a tick arriving while the last one is still going does
// nothing at all.
func (e *Engine) censusRepositories(ctx context.Context, now time.Time) {
	if !e.censusDue(now) {
		return
	}
	if !e.censusing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer e.censusing.Store(false)
		e.takeCensus(ctx, now)
	}()
}

// censusDue is the cheap check on the scheduler's own goroutine: is any
// repository old enough to be worth waking a goroutine for.
func (e *Engine) censusDue(now time.Time) bool {
	repositories, err := e.store.Repositories()
	if err != nil {
		e.log.Error("read repositories", "error", err)
		return false
	}
	for _, repo := range repositories {
		if censusDueFor(repo, now) {
			return true
		}
	}
	return false
}

// censusDueFor says whether this repository is worth measuring now: one
// that has just been measured is not, and neither is one that has just
// failed to be.
func censusDueFor(repo nodestore.Repository, now time.Time) bool {
	if repo.InitialisedAt == nil {
		// Nothing has ever been written here, so there is nothing on the
		// far end to measure.
		return false
	}
	if repo.Census.AttemptedAt != nil && now.Sub(*repo.Census.AttemptedAt) < censusRetryAfter {
		return false
	}
	if repo.Census.MeasuredAt == nil {
		return true
	}
	return now.Sub(*repo.Census.MeasuredAt) >= censusEvery
}

// takeCensus measures every repository that is due, one at a time. They
// share one network link and one restic cache, and measuring them at once
// would compete with whatever backup is running.
func (e *Engine) takeCensus(ctx context.Context, now time.Time) {
	repositories, err := e.store.Repositories()
	if err != nil {
		e.log.Error("read repositories", "error", err)
		return
	}
	for _, repo := range repositories {
		if ctx.Err() != nil {
			return
		}
		if !censusDueFor(repo, now) {
			continue
		}
		e.censusOne(ctx, repo.ID)
	}
}

// censusOne measures one repository and records what it found.
//
// A reading that fails leaves the last good one standing: a destination
// that cannot be reached tonight has not become empty, and an operator
// reading "94 GiB, as of yesterday" with the reason underneath knows
// more than one reading a blank.
func (e *Engine) censusOne(ctx context.Context, repositoryID string) {
	attempted := time.Now().UTC()
	measured, err := e.measure(ctx, repositoryID)
	if err == nil {
		e.recordCensus(repositoryID, func(state *nodestore.RepositoryCensus) {
			at := time.Now().UTC()
			state.Snapshots, state.SizeBytes = measured.Snapshots, measured.SizeBytes
			state.MeasuredAt = &at
			state.AttemptedAt, state.LastError = nil, ""
		})
		return
	}
	e.log.Warn("measure what a repository holds", "repository_id", repositoryID, "error", err)
	e.recordCensus(repositoryID, func(state *nodestore.RepositoryCensus) {
		state.AttemptedAt, state.LastError = &attempted, err.Error()
	})
}

// measure asks restic what one repository holds, under a timeout of its
// own so that one unreachable destination does not stop the rest being
// measured.
func (e *Engine) measure(ctx context.Context, repositoryID string) (resticrun.RepositoryCensus, error) {
	handle, err := e.OpenRepository(repositoryID, false)
	if err != nil {
		return resticrun.RepositoryCensus{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, censusTimeout)
	defer cancel()
	return e.runner.Census(ctx, handle)
}

// recordCensus writes only the census back.
//
// Measuring takes minutes, and the record read at the start of it is not
// the record on disk at the end: retention approvals and destination
// edits land on the same repository. So it is read again here and only
// this field is touched.
func (e *Engine) recordCensus(repositoryID string, set func(*nodestore.RepositoryCensus)) {
	stored, err := e.store.Repository(repositoryID)
	if err != nil {
		e.log.Error("read a repository to record what it holds",
			"repository_id", repositoryID, "error", err)
		return
	}
	set(&stored.Census)
	if _, err := e.store.PutRepository(stored); err != nil {
		e.log.Error("record what a repository holds",
			"repository_id", repositoryID, "error", err)
	}
}
