package resticrun

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// ErrLocked means restic would not start because the repository is
// locked. It is told apart from every other failure because it is the
// one a caller can do something about: a lock left by a process that was
// killed is never released by anybody, and everything that needs the
// repository to itself -- a retention plan, a prune, a check -- fails on
// it every night until somebody removes it.
var ErrLocked = errors.New("resticrun: the repository is locked")

// exitLocked is what restic exits with when it could not take its lock.
const exitLocked = 11

// StaleLockAfter is how old a lock has to be before restic itself calls
// it stale. A process that holds a lock writes a fresh one every five
// minutes, so one that is half an hour old belongs to nothing.
const StaleLockAfter = 30 * time.Minute

// maxLocks bounds how many locks are read. A repository carries one per
// process working in it; hundreds would mean something else is wrong, and
// reading each costs a round trip to the destination.
const maxLocks = 64

// Lock is one lock file in a repository.
type Lock struct {
	ID        string    `json:"-"`
	Time      time.Time `json:"time"`
	Exclusive bool      `json:"exclusive"`
	Hostname  string    `json:"hostname"`
	Username  string    `json:"username"`
	PID       int       `json:"pid"`
}

// Stale reports whether nothing holds this lock any more, by the rule
// restic applies: it has not been refreshed for half an hour, or it was
// taken on this host by a process that no longer exists. A lock taken on
// another host is only ever stale by its age, since nothing here can ask
// that host about its processes.
func (l Lock) Stale(now time.Time, host string, alive func(pid int) bool) bool {
	if now.Sub(l.Time) > StaleLockAfter {
		return true
	}
	if host == "" || l.Hostname != host || alive == nil {
		return false
	}
	return !alive(l.PID)
}

var lockID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Locks reads the locks a repository carries.
//
// It takes no lock of its own: this is what is asked when something else
// could not take one, and a reader that queued behind the lock it was
// sent to look at would never answer.
func (r *Runner) Locks(ctx context.Context, repo Repository) ([]Lock, error) {
	listed, err := r.run(ctx, repo, []string{"list", "locks", "--no-lock"}, secondary{}, nil)
	if err != nil {
		return nil, err
	}
	if err := classifyExit(listed.ExitCode, listed.Stderr, false); err != nil {
		return nil, err
	}
	var locks []Lock
	scanner := bufio.NewScanner(bytes.NewReader(listed.Stdout))
	for scanner.Scan() {
		id := string(bytes.TrimSpace(scanner.Bytes()))
		if id == "" {
			continue
		}
		if !lockID.MatchString(id) {
			return nil, fmt.Errorf("resticrun: restic listed a lock this cannot read")
		}
		if len(locks) == maxLocks {
			return nil, fmt.Errorf("resticrun: the repository carries more than %d locks", maxLocks)
		}
		lock, err := r.lock(ctx, repo, id)
		if err != nil {
			return nil, err
		}
		locks = append(locks, lock)
	}
	return locks, scanner.Err()
}

// lock reads one lock file.
func (r *Runner) lock(ctx context.Context, repo Repository, id string) (Lock, error) {
	result, err := r.run(ctx, repo, []string{"cat", "lock", id, "--no-lock"}, secondary{}, nil)
	if err != nil {
		return Lock{}, err
	}
	if err := classifyExit(result.ExitCode, result.Stderr, false); err != nil {
		return Lock{}, err
	}
	var lock Lock
	if err := json.Unmarshal(result.Stdout, &lock); err != nil || lock.Time.IsZero() {
		return Lock{}, fmt.Errorf("resticrun: lock %s could not be read", id[:12])
	}
	lock.ID = id
	return lock, nil
}

// Unlock removes the locks restic finds stale, and only those: a lock a
// running process is refreshing is left where it is.
func (r *Runner) Unlock(ctx context.Context, repo Repository) error {
	result, err := r.run(ctx, repo, []string{"unlock"}, secondary{}, nil)
	if err != nil {
		return err
	}
	if err := classifyExit(result.ExitCode, result.Stderr, false); err != nil {
		return err
	}
	return refusedRemoval(result.Stderr)
}
