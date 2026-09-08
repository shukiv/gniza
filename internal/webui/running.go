package webui

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/shukiv/gniza/internal/granular"
	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/node"
	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/protocol"
)

// The waiting forms of what a run is doing. They are constants because
// the strip counts by them as well as printing them.
const (
	waitingToBackUp  = "Waiting to back up"
	waitingToRestore = "Waiting to restore"
)

// runningWork is one backup or restore that is happening now, as the strip
// at the top of every page names it.
//
// A backup takes minutes and a restore can take longer, and until now the
// only way to know one was under way was to be on the page that lists them.
// Somebody who has just asked for a restore and sees nothing asks for it
// again.
type runningWork struct {
	Account string
	// What it is: "Backing up", "Restoring", or the waiting form of
	// either. An account may only have one job in flight, so work that
	// has been asked for and not started is the common case straight
	// after asking -- and showing nothing then is what sends somebody
	// back to ask again.
	Doing string
	// Waiting says this one has not started. It is shown without a
	// spinner and without a bar, because neither would be true.
	Waiting bool
	// Detail is what is being written or read back, where that is
	// something more specific than the account.
	Detail string
	// Percent is restic's own figure, and Known says whether there is
	// one: several stages of a restore cannot be counted at all, and "0%"
	// would read as stuck.
	Percent float64
	Known   bool
}

// Label is the whole line, for a banner that is one sentence per run.
//
// The percentage is in the words as well as on the bar, because the bar is
// a picture and a number is what somebody reads back to a colleague.
func (w runningWork) Label() string {
	line := w.Doing + " " + w.Account
	if w.Detail != "" {
		line += " — " + w.Detail
	}
	if w.Known {
		// Half rounds up, as it does beside the bar on the history
		// page: two places showing the same run must agree.
		line += fmt.Sprintf(" (%.0f%%)", math.Round(w.Percent))
	}
	return line
}

// runningSet is everything in flight, as the strip draws it.
//
// A line each was right when a server had a handful of accounts. A
// DirectAdmin host with three hundred queued one behind the other, and
// the strip -- which is at the top of every page -- became the page: the
// one line that said what was actually being backed up scrolled off the
// top under the ones that said nothing was happening yet. So what is
// running keeps its line and its bar, and what is waiting folds into one
// line with a count that opens.
type runningSet []runningWork

// folds is the number of waiting runs it takes before they are worth
// folding. Below it the count would be longer than the lines it replaced,
// and a customer -- who has one account and so at most one job -- never
// reaches it.
const folds = 3

// Now is the work that has started: one line and one bar each.
func (s runningSet) Now() []runningWork {
	started := make([]runningWork, 0, len(s))
	for _, work := range s {
		if !work.Waiting {
			started = append(started, work)
		}
	}
	return started
}

// Queue is the work that has been asked for and has not started.
func (s runningSet) Queue() []runningWork {
	queued := make([]runningWork, 0, len(s))
	for _, work := range s {
		if work.Waiting {
			queued = append(queued, work)
		}
	}
	return queued
}

// Folded says the queue is long enough to be a count rather than lines.
func (s runningSet) Folded() bool { return len(s.Queue()) > folds }

// QueueSummary is the folded line: what is waiting, counted by what it is
// waiting to do. Both kinds are named when both are there, because "312
// accounts waiting" over the restore somebody is standing by for is the
// wrong thing to read.
func (s runningSet) QueueSummary() string {
	var backups, restores int
	for _, work := range s.Queue() {
		if work.Doing == waitingToRestore {
			restores++
			continue
		}
		backups++
	}
	parts := make([]string, 0, 2)
	if backups > 0 {
		parts = append(parts, fmt.Sprintf("%s waiting to back up", accounts(backups)))
	}
	if restores > 0 {
		parts = append(parts, fmt.Sprintf("%s waiting to restore", accounts(restores)))
	}
	return strings.Join(parts, ", ")
}

// accounts counts them in words that read as a sentence rather than as a
// field: "1 account", not "1 accounts".
func accounts(n int) string {
	if n == 1 {
		return "1 account"
	}
	return fmt.Sprintf("%d accounts", n)
}

// runningWorkFor lists what is in flight, newest first.
//
// account narrows it to one account's own work, which is what a customer is
// shown: another account's restore is not theirs to see. An empty account
// means every account, which is the operator's view.
func runningWorkFor(store *nodestore.Store, account string) (runningSet, error) {
	jobs, err := store.Jobs(0)
	if err != nil {
		return nil, err
	}
	// Which copy is being written is recorded as the repository's
	// identifier, and an identifier is not a name anybody gave anything:
	// the strip read "Backing up clinicco -- 08d0a595086a4a984a2737a6341d3788".
	// The destination has a name, and that is what an operator knows it
	// by. A repository whose destination has since gone is left as it is
	// rather than shown as nothing.
	destinations, err := store.Destinations()
	if err != nil {
		return nil, err
	}
	named := make(map[string]string, len(destinations))
	for _, destination := range destinations {
		named[destination.ID] = destination.Name
	}
	repositories, err := store.Repositories()
	if err != nil {
		return nil, err
	}
	copies := make(map[string]string, len(repositories))
	for _, repository := range repositories {
		if name := named[repository.DestinationID]; name != "" {
			copies[repository.ID] = name
		}
	}
	restores, err := store.Restores(0)
	if err != nil {
		return nil, err
	}

	var running runningSet
	for _, run := range jobs {
		if !stillGoing(run.Status) || (account != "" && run.Account != account) {
			continue
		}
		work := runningWork{
			Account: run.Account, Doing: "Backing up",
			Waiting: run.Status == job.StatusPending,
		}
		if work.Waiting {
			work.Doing = waitingToBackUp
		} else if run.Progress != nil {
			work.Percent, work.Known = run.Progress.Percent, true
			work.Detail = run.Progress.Repository
			if name := copies[work.Detail]; name != "" {
				work.Detail = name
			}
		}
		running = append(running, work)
	}
	for _, run := range restores {
		if !stillGoing(run.Status) || (account != "" && run.Account != account) {
			continue
		}
		work := runningWork{
			Account: run.Account, Doing: "Restoring",
			Waiting: run.Status == job.StatusPending,
			Detail:  askedFor(run),
		}
		if work.Waiting {
			work.Doing = waitingToRestore
		} else if run.Progress != nil {
			// The stage is the detail while it runs: "reading the home
			// directory" says more than the list of parts asked for,
			// which is on the page that asked for them.
			if run.Progress.Stage != "" {
				work.Detail = run.Progress.Stage
			}
			work.Percent, work.Known = run.Progress.Percent, run.Progress.Known
		}
		running = append(running, work)
	}
	// What is running before what is waiting, then a backup before a
	// restore, then by account, so the strip does not reorder itself
	// under somebody reading it.
	sort.SliceStable(running, func(i, j int) bool {
		if running[i].Waiting != running[j].Waiting {
			return !running[i].Waiting
		}
		if running[i].Doing != running[j].Doing {
			return running[i].Doing < running[j].Doing
		}
		return running[i].Account < running[j].Account
	})
	return running, nil
}

// askedFor says what a restore was asked for, in words a sentence can
// carry.
//
// restoreRow.Parts() is written for a table column, where the bare kind
// reads as a value under a heading. "Restoring rtflow — account" does not
// read as anything.
func askedFor(run nodestore.Restore) string {
	if selections := (restoreRow{Restore: run}).Selections(); len(selections) > 0 {
		named := make([]string, 0, len(selections))
		for _, selection := range selections {
			named = append(named, lowerFirst(granular.Kind(selection.Kind).Title()))
		}
		return strings.Join(named, ", ")
	}
	switch run.Kind {
	case protocol.RestoreFiles:
		return "files out of the backup"
	case node.KindVerify:
		return "a rehearsal, which touches nothing live"
	default:
		return "the whole account"
	}
}

// stillGoing reports whether work has been asked for and is not over.
//
// Pending counts. An account may only have one job at a time, so the
// moment straight after asking is usually spent waiting, and a strip that
// showed nothing then was a strip that answered "did that work?" with
// silence.
func stillGoing(status job.Status) bool {
	return status == job.StatusRunning || status == job.StatusPending
}
