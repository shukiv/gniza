package node

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/shukiv/gniza/internal/human"
	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/notify"
	"github.com/shukiv/gniza/internal/resticrun"
	"github.com/shukiv/gniza/internal/staging"
)

const (
	// rehearseSweepEvery is how often the server looks for an account
	// that is due. Looking means listing the accounts and the history.
	rehearseSweepEvery = 10 * time.Minute
	// rehearseRetryAfter is how long an account whose rehearsal failed,
	// or did not fit, waits before it is tried again. Sooner than that
	// and one account that cannot be rehearsed is rehearsed every night
	// in place of the ones that can.
	rehearseRetryAfter = 7 * 24 * time.Hour
	// rehearseQuietBefore is how long before a schedule fires a rehearsal
	// will still start. It reads a whole account back from the
	// destination, and tonight's backup is not to wait for it.
	rehearseQuietBefore = 2 * time.Hour
	// rehearseFromHour and rehearseUntilHour are the hours of the night,
	// by the server's own clock, in which one may start.
	rehearseFromHour  = 22
	rehearseUntilHour = 6
	// rehearseClearBefore is how long before a schedule fires a rehearsal
	// the server started is stopped. The scheduler and the worker take
	// turns, so one still running when the backups are due would hold
	// them up until it had finished.
	rehearseClearBefore = 10 * time.Minute
	// rehearseShare is how much of the free space a rehearsal nobody
	// asked for may take: one part in this many. Somebody who asks for
	// one has looked at the disk. Nobody has looked when the schedule
	// asks, and the disk is often the one the accounts are on.
	rehearseShare = 2
)

// NoRoomToRehearse is a scheduled rehearsal that was not started because
// of what it would have taken.
type NoRoomToRehearse struct {
	Account    string
	Need, Free uint64
	Root       string
}

func (e *NoRoomToRehearse) Error() string {
	return fmt.Sprintf(
		"node: rehearsing %s takes %s, and a rehearsal nobody asked for takes no more "+
			"than one part in %d of the %s free in %s. Rehearse it by hand when the "+
			"server can spare the room, or name a larger directory under Settings, Storage",
		e.Account, human.Bytes(e.Need), rehearseShare, human.Bytes(e.Free), e.Root)
}

// OutOfTimeToRehearse is a scheduled rehearsal that was stopped because
// the backups were about to start.
type OutOfTimeToRehearse struct {
	Account string
	Until   time.Time
}

func (e *OutOfTimeToRehearse) Error() string {
	return fmt.Sprintf(
		"node: the rehearsal of %s was not finished by %s, when it was stopped to "+
			"leave the server to its backups. Rehearse it by hand at a time of "+
			"day that has room for it",
		e.Account, e.Until.Local().Format("15:04"))
}

// KindNotRehearsed is what a rehearsal the server asked for becomes when
// it was not run to the end for a reason that is the server's and not
// the backup's: there was not the room, or not the time. It is not a
// rehearsal that failed, and nothing that reads rehearsals reads it as
// one.
const KindNotRehearsed = "not-rehearsed"

// notRehearsed reports whether a scheduled rehearsal ended for a reason
// that says nothing about the backup.
func notRehearsed(err error) bool {
	var noRoom *NoRoomToRehearse
	var full *staging.ErrInsufficientSpace
	var late *OutOfTimeToRehearse
	return errors.As(err, &noRoom) || errors.As(err, &full) || errors.As(err, &late)
}

// nextScheduleFire is when the next enabled schedule starts.
func (e *Engine) nextScheduleFire(now time.Time) (time.Time, bool) {
	policies, err := e.store.Policies()
	if err != nil {
		return time.Time{}, false
	}
	var (
		next  time.Time
		found bool
	)
	for _, policy := range policies {
		if !policy.Enabled || len(policy.RepositoryIDs) == 0 {
			continue
		}
		schedule, err := cron.ParseStandard(policy.ScheduleCron)
		if err != nil {
			continue
		}
		if fires := schedule.Next(now); !found || fires.Before(next) {
			next, found = fires, true
		}
	}
	return next, found
}

// rehearseAccounts queues a rehearsal of the account that has waited
// longest for one, when the server has nothing else to do.
//
// A backup nobody has rebuilt is a backup nobody knows can be rebuilt.
// The fleet check of 2026-09-22 found four servers on which no account
// had ever been rehearsed, because the only thing that started one was a
// button.
func (e *Engine) rehearseAccounts(ctx context.Context, now time.Time) {
	if now.Sub(e.lastRehearsalSweep) < rehearseSweepEvery {
		return
	}
	e.lastRehearsalSweep = now
	account, err := e.rehearseOnce(ctx, now.Local())
	if err != nil {
		e.log.Warn("queue a scheduled rehearsal", "account", account, "error", err)
	}
}

// rehearseOnce queues one rehearsal if one is due and this is the time
// for it, and reports the account it chose.
func (e *Engine) rehearseOnce(ctx context.Context, now time.Time) (string, error) {
	settings, err := e.store.Settings()
	if err != nil {
		return "", err
	}
	every := settings.RehearseEvery()
	if every <= 0 || !rehearsalHour(now) || e.checking.Load() || e.sweepingLocks.Load() {
		return "", nil
	}
	if !e.quietFor(now, rehearseQuietBefore) {
		return "", nil
	}
	// One at a time, and never beside a backup: a rehearsal that is
	// queued or running is work in flight like any other.
	busy, err := e.anyJobRunning()
	if err != nil || busy {
		return "", err
	}
	accounts, err := e.provider.Accounts(ctx)
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(accounts))
	for _, account := range accounts {
		names = append(names, account.User)
	}
	jobs, err := e.store.Jobs(0)
	if err != nil {
		return "", err
	}
	restores, err := e.store.Restores(0)
	if err != nil {
		return "", err
	}
	account, due := rehearsalDue(names, jobs, restores, now, every)
	if !due {
		return "", nil
	}
	if _, err := e.queueDrill(ctx, account, true); err != nil {
		return account, err
	}
	e.log.Info("queued a scheduled rehearsal", "account", account)
	return account, nil
}

// rehearsalHour reports whether a rehearsal may start at this hour.
func rehearsalHour(now time.Time) bool {
	hour := now.Hour()
	return hour >= rehearseFromHour || hour < rehearseUntilHour
}

// rehearsalDue picks the account that has waited longest for a
// rehearsal, from those on the server that have a backup.
//
// One never rehearsed comes before any that has been. One whose last
// rehearsal failed is tried again after a week rather than a month, and
// is not tried again the next night.
func rehearsalDue(accounts []string, jobs []nodestore.Job, restores []nodestore.Restore,
	now time.Time, every time.Duration) (string, bool) {

	backedUp := map[string]bool{}
	for _, stored := range jobs {
		for _, target := range stored.Targets {
			if target.SnapshotID != "" {
				backedUp[stored.Account] = true
			}
		}
	}
	type rehearsal struct {
		at     time.Time
		passed bool
	}
	last := map[string]rehearsal{}
	for _, stored := range restores {
		if stored.Kind != KindVerify && stored.Kind != KindNotRehearsed {
			continue
		}
		at := stored.QueuedAt
		if stored.FinishedAt != nil {
			at = *stored.FinishedAt
		}
		if previous, seen := last[stored.Account]; seen && !at.After(previous.at) {
			continue
		}
		last[stored.Account] = rehearsal{at: at,
			passed: stored.Kind == KindVerify && stored.Status == job.StatusSuccess}
	}

	var (
		chosen string
		oldest time.Time
		found  bool
	)
	sorted := append([]string(nil), accounts...)
	sort.Strings(sorted)
	for _, account := range sorted {
		if !backedUp[account] || strings.HasPrefix(account, "@") {
			continue
		}
		previous, rehearsed := last[account]
		if rehearsed {
			wait := every
			if !previous.passed && rehearseRetryAfter < wait {
				wait = rehearseRetryAfter
			}
			if now.Sub(previous.at) < wait {
				continue
			}
		}
		if !found || previous.at.Before(oldest) {
			chosen, oldest, found = account, previous.at, true
		}
	}
	return chosen, found
}

// queueDrill queues a rehearsal of an account's newest backup, and
// records whether anybody asked for it.
func (e *Engine) queueDrill(ctx context.Context, account string, scheduled bool) (nodestore.Restore, error) {
	repositories, err := e.store.Repositories()
	if err != nil {
		return nodestore.Restore{}, err
	}

	var (
		newest     resticrun.Snapshot
		newestRepo string
	)
	for _, repository := range repositories {
		if repository.InitialisedAt == nil {
			continue
		}
		snapshots, err := e.Snapshots(ctx, repository.ID, account)
		if err != nil {
			continue
		}
		for _, snapshot := range snapshots {
			if snapshot.Time.After(newest.Time) {
				newest, newestRepo = snapshot, repository.ID
			}
		}
	}
	if newestRepo == "" {
		return nodestore.Restore{}, fmt.Errorf("node: %s has no backup to rehearse", account)
	}

	return e.QueueRestore(nodestore.Restore{
		Account:      account,
		RepositoryID: newestRepo,
		SnapshotID:   newest.ID,
		Kind:         KindVerify,
		Scheduled:    scheduled,
	})
}

// roomToRehearse refuses a scheduled rehearsal that would take more of
// the disk than one nobody is watching should.
func roomToRehearse(account string, need uint64, restores *staging.Manager) error {
	free, err := staging.AvailableBytes(restores.Root)
	if err != nil {
		// Allocate asks the same question and says what is wrong.
		return nil
	}
	if need > free/rehearseShare {
		return &NoRoomToRehearse{Account: account, Need: need, Free: free, Root: restores.Root}
	}
	return nil
}

// tellOfFailedRehearsal says that a backup did not rebuild, when the
// schedule found it and nobody was looking at the page.
func (e *Engine) tellOfFailedRehearsal(ctx context.Context, stored nodestore.Restore, err error) {
	if !stored.Scheduled || notRehearsed(err) {
		// Not having the room says nothing about the backup, and is on
		// the page for whoever wants to know why it was not rehearsed.
		return
	}
	e.Notify(ctx, notify.Message{
		Event:   notify.EventRehearsalFailed,
		Subject: fmt.Sprintf("The newest backup of %s did not rebuild", stored.Account),
		Body: err.Error() + "\n\nThis was a rehearsal: nothing was restored and the " +
			"account was not touched. Until one passes, take it that this account " +
			"cannot be restored from its newest backup. Run a backup of it and " +
			"rehearse it again from the Accounts page.",
	})
}
