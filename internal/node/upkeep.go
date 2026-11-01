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
	// upkeepWithoutBackups is how long a server waits for a schedule to
	// fire before it does its upkeep anyway. Longer than a day, so a nightly
	// schedule is always what sets the hour.
	upkeepWithoutBackups = 30 * time.Hour
	// preTaskTimeout bounds what stands between a schedule and its backups.
	// A destination that does not answer in this time is the backup's to
	// report.
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
	e.lastLockSweep = now
	ctx, cancel := context.WithTimeout(ctx, preTaskTimeout)
	defer cancel()
	e.sweepLocksOnce(ctx)
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
// A server whose schedules are not firing still needs telling about its
// destinations, so after upkeepWithoutBackups it stops waiting, and looks for
// locks here because no pre-task will.
func (e *Engine) upkeep(ctx context.Context, now time.Time) {
	busy, err := e.anyJobRunning()
	if err != nil || busy || e.checking.Load() || e.sweepingLocks.Load() {
		return
	}
	if !e.backupsFired.Load() {
		if e.scheduleFiredWithin(now, upkeepWithoutBackups) {
			return
		}
		// Beside the scheduler, and retention and the check wait for it.
		e.sweepLocks(ctx, now)
	}

	e.probeDestinations(ctx, now)

	// sweepRetention takes one repository a pass.
	repositories, err := e.store.Repositories()
	if err != nil {
		e.log.Error("read repositories", "error", err)
		return
	}
	for range repositories {
		if ctx.Err() != nil || !e.sweepRetention(ctx, now) {
			break
		}
	}

	e.censusRepositories(ctx, now)
	if e.censusing.Load() {
		return
	}
	e.checkRepositories(ctx, now)
	e.backupsFired.Store(false)
}

// scheduleFiredWithin reports whether a schedule that can run has fired
// inside the window. A firing is stored, so this survives a restart.
func (e *Engine) scheduleFiredWithin(now time.Time, window time.Duration) bool {
	policies, err := e.store.Policies()
	if err != nil {
		e.log.Error("read schedules", "error", err)
		return true
	}
	for _, policy := range policies {
		if !policy.Enabled || len(policy.RepositoryIDs) == 0 || policy.LastRunAt == nil {
			continue
		}
		if now.Sub(*policy.LastRunAt) < window {
			return true
		}
	}
	return false
}
