package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/shukiv/gniza/internal/resticrun"
)

// lockTimeout bounds reading and removing a repository's locks. Each lock
// is a round trip to the destination and there are only ever a few.
const lockTimeout = 5 * time.Minute

// lockSweepEvery is how often the repositories are looked at for locks
// nothing holds. A lock that stops retention costs a night; one left by a
// prune or a check that was killed stops every backup, and must not be
// there when the next schedule fires.
const lockSweepEvery = time.Hour

// sweepLocks removes stale locks from every repository, off the
// scheduler's own goroutine, when the server has nothing running.
func (e *Engine) sweepLocks(ctx context.Context, now time.Time) {
	if now.Sub(e.lastLockSweep) < lockSweepEvery || e.checking.Load() {
		return
	}
	busy, err := e.anyJobRunning()
	if err != nil || busy {
		return
	}
	if !e.sweepingLocks.CompareAndSwap(false, true) {
		return
	}
	e.lastLockSweep = now
	go func() {
		defer e.sweepingLocks.Store(false)
		e.sweepLocksOnce(ctx)
	}()
}

// sweepLocksOnce looks at each repository in turn.
func (e *Engine) sweepLocksOnce(ctx context.Context) {
	repositories, err := e.store.Repositories()
	if err != nil {
		e.log.Error("read repositories", "error", err)
		return
	}
	for _, repo := range repositories {
		if ctx.Err() != nil {
			return
		}
		if repo.InitialisedAt == nil {
			continue
		}
		if _, err := e.ClearStaleLocks(ctx, repo.ID); err != nil {
			e.log.Warn("look for stale locks", "repository_id", repo.ID, "error", err)
		}
	}
}

// ClearStaleLocks removes the locks nothing holds any more from a
// repository, and reports how many went.
//
// A backup that is killed -- by a reboot, by the kernel, by an operator --
// leaves its lock behind, and restic never removes another process's
// lock by itself. Backups go on succeeding past it, which is why it goes
// unnoticed; what stops is everything that needs the repository to
// itself. On the server this was written for, a lock nineteen days old
// had kept retention from running once, and the repository had grown to
// 5908 copies.
//
// Nothing a running process holds is touched. A lock counts as stale by
// restic's own rule, restic is what removes it and applies that rule
// again when it does, and none of it happens while this server has work
// of its own in flight.
func (e *Engine) ClearStaleLocks(ctx context.Context, repositoryID string) (int, error) {
	busy, err := e.anyJobRunning()
	if err != nil {
		return 0, err
	}
	if busy {
		return 0, fmt.Errorf(
			"node: a backup or a restore is running here, so its lock is not a stale one; " +
				"try again when it has finished")
	}
	// Maintenance credentials: removing a lock is a deletion, and an
	// append-only destination refuses one from the credentials a backup
	// runs under.
	repo, err := e.OpenRepository(repositoryID, true)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, lockTimeout)
	defer cancel()

	before, err := e.staleLocks(ctx, repo)
	if err != nil {
		return 0, err
	}
	if len(before) == 0 {
		return 0, nil
	}
	if err := e.runner.Unlock(ctx, repo); err != nil {
		return 0, err
	}
	after, err := e.staleLocks(ctx, repo)
	if err != nil {
		return 0, err
	}
	removed := len(before) - len(after)
	e.log.Info("removed stale locks", "repository_id", repositoryID,
		"removed", removed, "oldest", oldestLock(before).Format(time.RFC3339))
	if len(after) > 0 {
		return removed, fmt.Errorf(
			"node: %d stale locks are still there after restic was asked to remove them",
			len(after))
	}
	return removed, nil
}

// staleLocks is the locks in a repository that nothing holds.
func (e *Engine) staleLocks(ctx context.Context, repo resticrun.Repository) ([]resticrun.Lock, error) {
	locks, err := e.runner.Locks(ctx, repo)
	if err != nil {
		return nil, err
	}
	// restic records the machine's own name in a lock, not the name this
	// server is configured to call itself.
	host, _ := os.Hostname()
	now := time.Now()
	var stale []resticrun.Lock
	for _, lock := range locks {
		if lock.Stale(now, host, processExists) {
			stale = append(stale, lock)
		}
	}
	return stale, nil
}

// clearedLockAfter is what a caller that has just failed asks: was that a
// lock, and has it now been removed, so that trying once more is worth
// it. Any other failure, and a lock that something still holds, answer
// no, and the failure stands as it was.
func (e *Engine) clearedLockAfter(ctx context.Context, repositoryID string, failure error) bool {
	if !errors.Is(failure, resticrun.ErrLocked) {
		return false
	}
	removed, err := e.ClearStaleLocks(ctx, repositoryID)
	if err != nil {
		e.log.Warn("remove stale locks", "repository_id", repositoryID, "error", err)
		return false
	}
	return removed > 0
}

func oldestLock(locks []resticrun.Lock) time.Time {
	var oldest time.Time
	for _, lock := range locks {
		if oldest.IsZero() || lock.Time.Before(oldest) {
			oldest = lock.Time
		}
	}
	return oldest
}

// processExists reports whether a process with this id is running.
// Signal zero delivers nothing and checks only that it could have been
// sent; being refused permission means there is a process to refuse for.
func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
