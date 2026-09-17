package node

import (
	"os"
	"time"

	"github.com/shukiv/gniza/internal/cpanel"
	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/nodestore"
)

// LogKinds says which parts of the history to clear, as the Logs page
// divides it.
type LogKinds struct {
	Backups, System, Restores, Lifecycle bool
}

// ClearedLogs is what a clearing came to: what went, by kind, and what
// was held on to.
type ClearedLogs struct {
	Backups, System, Restores, Lifecycle int
	// KeptRuns and KeptRestores count the finished rows the clearing
	// left in the kinds it was asked to clear, because something still
	// reads them.
	KeptRuns, KeptRestores int
}

// Removed is how many rows went in all.
func (c ClearedLogs) Removed() int { return c.Backups + c.System + c.Restores + c.Lifecycle }

// ClearLogs empties the history an operator asked to have emptied, and
// holds on to what is more than a log.
//
// The overview says an account is protected, termination protection lets
// a panel remove one, and a schedule says how its last run went, all out
// of the same rows the Logs page lists. Emptying the bucket would turn
// every account "unprotected" and block every removal until the next
// night. So a clearing keeps, of the finished runs: the last run of each
// account under each schedule, and the last good copy of each account at
// each destination -- plain, complete, and complete under each schedule,
// which are the three questions asked of them. Work still going is never
// touched. Of the restores it keeps the last rehearsal, which the
// overview shows, and any whose archive is still here to be downloaded.
// The snapshots marked as having unreadable files keep their mark
// (nodestore.ClearJobs). The service's own log is the journal's and is
// not this server's to clear.
func (e *Engine) ClearLogs(kinds LogKinds) (ClearedLogs, error) {
	var cleared ClearedLogs
	asOf := time.Now().UTC()

	if kinds.Backups || kinds.System {
		jobs, err := e.store.Jobs(0)
		if err != nil {
			return cleared, err
		}
		evidence := evidenceRuns(jobs)
		asked := func(stored nodestore.Job) bool {
			if stored.Account == cpanel.SystemAccount {
				return kinds.System
			}
			return kinds.Backups
		}
		removed, err := e.store.ClearJobs(func(stored nodestore.Job) bool {
			// A run that finished after the rows were read may be the
			// newest evidence there is, and was not weighed.
			if stored.FinishedAt != nil && stored.FinishedAt.After(asOf) {
				return true
			}
			return !asked(stored) || evidence[stored.ID]
		})
		if err != nil {
			return cleared, err
		}
		for _, stored := range removed {
			if stored.Account == cpanel.SystemAccount {
				cleared.System++
			} else {
				cleared.Backups++
			}
		}
		for _, stored := range jobs {
			if asked(stored) && stored.Status.Terminal() && evidence[stored.ID] {
				cleared.KeptRuns++
			}
		}
	}

	if kinds.Restores {
		restores, err := e.store.Restores(0)
		if err != nil {
			return cleared, err
		}
		held := map[string]bool{}
		rehearsed := false
		for _, stored := range restores { // newest first
			if !stored.Status.Terminal() {
				continue
			}
			if stored.Kind == KindVerify && !rehearsed {
				rehearsed = true
				held[stored.ID] = true
			}
			if stored.Status == job.StatusSuccess && stored.ArchivePath != "" && !stored.Applied {
				if info, err := os.Stat(stored.ArchivePath); err == nil && info.Mode().IsRegular() {
					held[stored.ID] = true
				}
			}
		}
		cleared.KeptRestores = len(held)
		removed, err := e.store.ClearRestores(func(stored nodestore.Restore) bool {
			if stored.FinishedAt != nil && stored.FinishedAt.After(asOf) {
				return true
			}
			return held[stored.ID]
		})
		if err != nil {
			return cleared, err
		}
		cleared.Restores = removed
	}

	if kinds.Lifecycle {
		removed, err := e.store.ClearLifecycle()
		if err != nil {
			return cleared, err
		}
		cleared.Lifecycle = removed
	}

	e.log.Warn("the history was cleared",
		"backups", cleared.Backups, "system_backups", cleared.System,
		"restores", cleared.Restores, "account_events", cleared.Lifecycle,
		"runs_kept", cleared.KeptRuns, "restores_kept", cleared.KeptRestores)
	return cleared, nil
}

// evidenceRuns picks the finished runs that are more than a log, by id.
// jobs come newest first, so the first run seen for a question is the
// one that answers it.
func evidenceRuns(jobs []nodestore.Job) map[string]bool {
	keep := map[string]bool{}
	answered := map[string]bool{}
	first := func(question, id string) {
		if !answered[question] {
			answered[question] = true
			keep[id] = true
		}
	}
	for _, stored := range jobs {
		if !stored.Status.Terminal() {
			continue
		}
		// How the last run of this account under this schedule went,
		// whatever it came to: a failure cleared away would leave the
		// success before it looking like the news.
		first("last|"+stored.Account+"|"+stored.PolicyID, stored.ID)
		for _, target := range stored.Targets {
			if target.Status != job.TargetSuccess || target.Incomplete {
				continue
			}
			where := stored.Account + "|" + target.RepositoryID
			first("copy|"+where, stored.ID)
			if stored.Status == job.StatusSuccess && stored.CompleteAccount {
				first("complete|"+where, stored.ID)
				first("complete|"+where+"|"+stored.PolicyID, stored.ID)
			}
		}
	}
	return keep
}
