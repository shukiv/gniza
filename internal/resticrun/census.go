package resticrun

import (
	"context"
	"encoding/json"
	"fmt"
)

// RepositoryCensus is what a repository holds: how many copies, and what
// they cost the storage they sit on.
type RepositoryCensus struct {
	Snapshots int
	SizeBytes uint64
}

// Census measures a whole repository.
//
// The mode is raw-data, which is what the repository actually occupies at
// its destination after deduplication and compression -- the number an
// operator watching a disk fill up is asking for. restore-size would
// answer a different question: what everything stored would come to if it
// were all written out at once, which is larger than the disk and is not
// what is on it.
//
// It takes no lock. A measurement must never be the reason a backup or a
// prune cannot start, and reading an index that a prune is rewriting is
// worth a stale number, not a failed night.
func (r *Runner) Census(ctx context.Context, repo Repository) (RepositoryCensus, error) {
	result, err := r.run(ctx, repo,
		[]string{"stats", "--json", "--no-lock", "--mode", "raw-data"}, secondary{}, nil)
	if err != nil {
		return RepositoryCensus{}, err
	}
	if err := classifyExit(result.ExitCode, result.Stderr, false); err != nil {
		return RepositoryCensus{}, err
	}
	// Pointers, so that a repository holding nothing is told apart from
	// restic answering something this does not understand. An empty
	// repository is a real reading and one an operator needs to see.
	var stats struct {
		TotalSize *uint64 `json:"total_size"`
		Snapshots *int    `json:"snapshots_count"`
	}
	if result.Truncated || json.Unmarshal(result.Stdout, &stats) != nil ||
		stats.TotalSize == nil || stats.Snapshots == nil {
		return RepositoryCensus{}, fmt.Errorf(
			"resticrun: restic did not say what the repository holds")
	}
	return RepositoryCensus{Snapshots: *stats.Snapshots, SizeBytes: *stats.TotalSize}, nil
}
