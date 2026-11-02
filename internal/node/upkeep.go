package node

import (
	"context"
	"time"
)

const (
	// upkeepEvery is the most often the upkeep goes to a destination. Every
	// visit is a login on somebody's server, and a schedule that fires
	// every hour is no reason to make the visits hourly. It is short of a
	// day so that a night which finishes earlier than the last still has
	// its upkeep.
	upkeepEvery = 20 * time.Hour
	// upkeepWithoutBackups is how old a reading gets before the server stops
	// waiting for a schedule and does its upkeep anyway. Longer than a day,
	// so a nightly schedule is what sets the hour.
	upkeepWithoutBackups = 30 * time.Hour
	// preTaskTimeout bounds what each repository can put between a schedule
	// and its backups. A destination that does not answer in this time is
	// the backup's to report.
	preTaskTimeout = 2 * time.Minute
)

// preTasks is what has to be done before a schedule queues its backups: a lock
// left behind by a prune or a check that was killed would stop every one of
// them. It comes before the queueing because a queued backup counts as work in
// flight, and locks are not touched while there is any. The caller is the
// scheduler, so this is bounded and the backups go ahead whatever it finds.
func (e *Engine) preTasks(ctx context.Context, now time.Time) {
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
	defer e.sweepingLocks.Store(false)
	// Only a look that reached everything counts: one that did not is tried
	// again before the next schedule fires, not a day later.
	if e.sweepLocksWithin(ctx, preTaskTimeout) {
		e.lastLockSweep = now
	}
}

// upkeep runs from every tick and does its work once the backups a schedule
// queued have all finished, rather than on timers of its own: the repository
// is free then, the night's changes are there to be counted, and the logins all
// come at the hour the destination is already expecting them.
//
// Each step decides for itself, from what is stored, whether it is due. So a
// tick that comes back while the census is still running repeats nothing, and
// neither does a restart. The order matters: retention before the census, so
// the count is what is left, and the integrity check last because it wants the
// repository to itself.
//
// Following the backups is when this normally happens, and it is not what
// decides whether it happens. A server whose schedules are not firing, whose
// firing queued nothing, or which restarted during the run has nothing to
// follow, so a reading older than upkeepWithoutBackups starts the upkeep by
// itself, and looks for locks here because no pre-task did. A job that never
// finishes would hold all of this back, so an overdue destination is still
// reached for beside it: that does not touch the repository.
func (e *Engine) upkeep(ctx context.Context, now time.Time) {
	busy, err := e.anyJobRunning()
	if err != nil {
		return
	}
	overdue := e.upkeepOverdue(now)
	if busy {
		if overdue {
			e.probeDestinations(ctx, now)
		}
		return
	}
	if e.checking.Load() || e.sweepingLocks.Load() {
		return
	}
	if !e.backupsFired.Load() {
		if !overdue {
			return
		}
		// Beside the scheduler, and retention and the check wait for it.
		e.sweepLocks(ctx, now)
	}

	e.probeDestinations(ctx, now)

	// One repository a tick, so that whatever is queued meanwhile runs
	// between them. The next tick comes back here for the next one.
	if e.sweepRetention(ctx, now) {
		return
	}

	e.censusRepositories(ctx, now)
	if e.censusing.Load() {
		return
	}
	e.checkRepositories(ctx, now)
	e.backupsFired.Store(false)
}

// upkeepOverdue reports whether a destination or a repository has gone longer
// than upkeepWithoutBackups without a reading, or has never had one. A reading
// is stored, so this survives a restart.
func (e *Engine) upkeepOverdue(now time.Time) bool {
	destinations, err := e.store.Destinations()
	if err != nil {
		e.log.Error("read destinations", "error", err)
		return false
	}
	for _, dest := range destinations {
		if dest.LastCheckedAt == nil || now.Sub(*dest.LastCheckedAt) >= upkeepWithoutBackups {
			return true
		}
	}
	repositories, err := e.store.Repositories()
	if err != nil {
		e.log.Error("read repositories", "error", err)
		return false
	}
	for _, repo := range repositories {
		if repo.InitialisedAt == nil {
			continue
		}
		if repo.Census.MeasuredAt == nil || now.Sub(*repo.Census.MeasuredAt) >= upkeepWithoutBackups {
			return true
		}
	}
	return false
}
