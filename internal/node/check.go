package node

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/notify"
	"github.com/shukiv/gniza/internal/resticrun"
)

const (
	// checkEvery is how often each repository is asked whether it is
	// whole.
	checkEvery = 7 * 24 * time.Hour
	// checkRetryAfter keeps a repository whose check could not be run
	// from being tried on every tick.
	checkRetryAfter = 6 * time.Hour
	// checkSubsetPercent is how much of the stored data a check reads
	// back, on top of the whole of the structure. Reading everything
	// costs what the backups cost to send; a different tenth each week
	// has read most of a repository in a season.
	checkSubsetPercent = 10
	// checkQuietBefore is how long before the next schedule fires a check
	// will still start. It holds the repository to itself while it runs,
	// so a backup that came due in the middle would wait for it.
	checkQuietBefore = 4 * time.Hour
	// checkTimeout stops one that is still going when that quiet is
	// nearly used up.
	checkTimeout = 3*time.Hour + 30*time.Minute
	// checkOverdue is when a repository is checked whatever is due: a
	// server whose schedules fire every hour is never quiet for four.
	checkOverdue = 3 * checkEvery
)

// checkRepositories asks restic whether each repository is whole, one
// repository at a time and off the scheduler's own goroutine.
//
// A check takes the repository's exclusive lock, which a backup cannot
// share. So it starts only when nothing is running and nothing is due
// for hours, and while it runs the worker leaves queued work where it is.
func (e *Engine) checkRepositories(ctx context.Context, now time.Time) {
	repositoryID, due := e.checkDue(now)
	if !due {
		return
	}
	busy, err := e.anyJobRunning()
	if err != nil || busy || e.sweepingLocks.Load() {
		// The sweep for stale locks is removing them now. A check that
		// started beside it would find the lock, try to remove it too,
		// and one of the two would be told it had gone.
		return
	}
	if !e.checking.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer e.checking.Store(false)
		e.checkOne(ctx, repositoryID, checkSubsetPercent)
	}()
}

// CheckNow checks one repository because somebody asked, without waiting
// for it to come due. It answers as soon as the check has started.
func (e *Engine) CheckNow(ctx context.Context, repositoryID string) error {
	repo, err := e.store.Repository(repositoryID)
	if err != nil {
		return err
	}
	if repo.InitialisedAt == nil {
		return fmt.Errorf("node: nothing has been written to this destination yet, " +
			"so there is nothing to check")
	}
	busy, err := e.anyJobRunning()
	if err != nil {
		return err
	}
	if busy {
		return fmt.Errorf("node: a backup or a restore is running, and a check needs " +
			"the repository to itself; try again when it has finished")
	}
	if !e.checking.CompareAndSwap(false, true) {
		return fmt.Errorf("node: a check is already running")
	}
	// The request that asked for it ends long before the check does.
	background := context.WithoutCancel(ctx)
	go func() {
		defer e.checking.Store(false)
		e.checkOne(background, repositoryID, checkSubsetPercent)
	}()
	return nil
}

// Checking reports whether a check is running now.
func (e *Engine) Checking() bool { return e.checking.Load() }

// checkDue picks the repository most in need of a check, if any is.
func (e *Engine) checkDue(now time.Time) (string, bool) {
	repositories, err := e.store.Repositories()
	if err != nil {
		e.log.Error("read repositories", "error", err)
		return "", false
	}
	quiet := e.quietFor(now, checkQuietBefore)
	var (
		chosen string
		oldest time.Time
		found  bool
	)
	for _, repo := range repositories {
		if !checkDueFor(repo, now, quiet) {
			continue
		}
		last := time.Time{}
		if repo.Check.CheckedAt != nil {
			last = *repo.Check.CheckedAt
		}
		if !found || last.Before(oldest) {
			chosen, oldest, found = repo.ID, last, true
		}
	}
	return chosen, found
}

// checkDueFor says whether this repository is worth checking now.
func checkDueFor(repo nodestore.Repository, now time.Time, quiet bool) bool {
	if repo.InitialisedAt == nil {
		return false
	}
	check := repo.Check
	if check.AttemptedAt != nil && now.Sub(*check.AttemptedAt) < checkRetryAfter {
		return false
	}
	// A repository is first checked a day after it was made rather than
	// the moment it was: there is nothing in it yet, and the first night
	// is the one that must not be held up.
	since := repo.InitialisedAt.Add(24*time.Hour - checkEvery)
	if check.CheckedAt != nil {
		since = *check.CheckedAt
	}
	age := now.Sub(since)
	switch {
	case age < checkEvery:
		return false
	case age >= checkOverdue:
		return true
	default:
		return quiet
	}
}

// quietFor reports whether no enabled schedule fires within the window.
// A schedule that cannot be read is no reason to think the server is
// busy; the scheduler has said so already.
func (e *Engine) quietFor(now time.Time, window time.Duration) bool {
	policies, err := e.store.Policies()
	if err != nil {
		return false
	}
	for _, policy := range policies {
		if !policy.Enabled || len(policy.RepositoryIDs) == 0 {
			continue
		}
		schedule, err := cron.ParseStandard(policy.ScheduleCron)
		if err != nil {
			continue
		}
		if !schedule.Next(now).After(now.Add(window)) {
			return false
		}
	}
	return true
}

// checkOne checks one repository and records what came of it.
func (e *Engine) checkOne(ctx context.Context, repositoryID string, subset int) {
	started := time.Now().UTC()
	err := e.check(ctx, repositoryID, subset)
	finished := time.Now().UTC()

	var before nodestore.RepositoryCheck
	_, recordErr := e.store.ChangeRepository(repositoryID, func(repo *nodestore.Repository) {
		before = repo.Check
		switch {
		case err == nil:
			repo.Check = nodestore.RepositoryCheck{
				CheckedAt: &finished, Passed: true, SubsetPercent: subset,
				Seconds: finished.Sub(started).Seconds(),
			}
		case errors.Is(err, resticrun.ErrDamaged):
			repo.Check = nodestore.RepositoryCheck{
				CheckedAt: &finished, Passed: false, Problem: err.Error(),
				SubsetPercent: subset, Seconds: finished.Sub(started).Seconds(),
			}
		default:
			// It could not be run. What the last one found still stands.
			repo.Check.AttemptedAt, repo.Check.LastError = &finished, err.Error()
		}
	})
	if recordErr != nil {
		e.log.Error("record a repository check", "repository_id", repositoryID, "error", recordErr)
	}

	switch {
	case err == nil:
		e.log.Info("repository checked", "repository_id", repositoryID,
			"read_back_percent", subset, "seconds", int(finished.Sub(started).Seconds()))
		if before.Known() && !before.Passed {
			e.Notify(ctx, notify.Message{
				Event: notify.EventCheckFailed, Level: notify.SeverityInfo,
				Subject: fmt.Sprintf("%s passes its check again", e.destinationName(repositoryID)),
				Body:    "restic read the repository and found nothing wrong with it.",
			})
		}
	case errors.Is(err, resticrun.ErrDamaged):
		e.log.Error("a repository did not pass its check", "repository_id", repositoryID, "error", err)
		if !before.Known() || before.Passed {
			e.Notify(ctx, notify.Message{
				Event:   notify.EventCheckFailed,
				Subject: fmt.Sprintf("%s did not pass its integrity check", e.destinationName(repositoryID)),
				Body: err.Error() + "\n\nSome of what is stored there may not restore. Backups " +
					"carry on. Rehearse a restore of the accounts that matter most, and read " +
					"the troubleshooting guide before repairing anything.",
			})
		}
	default:
		e.log.Warn("a repository could not be checked", "repository_id", repositoryID, "error", err)
	}
}

// check runs restic's check under a timeout of its own, removing a stale
// lock first if that is what stops it.
func (e *Engine) check(ctx context.Context, repositoryID string, subset int) error {
	repo, err := e.OpenRepository(repositoryID, true)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	spec := resticrun.CheckSpec{ReadDataSubsetPercent: subset}
	err = e.runner.Check(ctx, repo, spec)
	if e.clearedLockAfter(ctx, repositoryID, err) {
		err = e.runner.Check(ctx, repo, spec)
	}
	return err
}

// destinationName is what an operator calls the place a repository is
// kept, for a message about it.
func (e *Engine) destinationName(repositoryID string) string {
	repo, err := e.store.Repository(repositoryID)
	if err != nil {
		return "A destination"
	}
	dest, err := e.store.Destination(repo.DestinationID)
	if err != nil || dest.Name == "" {
		return "A destination"
	}
	return dest.Name
}
