package webui

import (
	"sort"
	"time"

	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/nodestore"
)

// A run is one firing of a schedule: the accounts it queued together and
// what became of them.
//
// Nothing stamps a job with the firing it belongs to, so a run is
// reconstructed here. Grouping by the date would be wrong twice over: a
// schedule that fires at 23:30 would be split in half at midnight, and a
// backup asked for by hand at two in the afternoon would be counted as
// part of last night. What actually separates two runs is a gap: a
// schedule queues every account it covers in one loop, seconds apart, and
// anything queued long after that is something else.
const runGap = 10 * time.Minute

// runSummary is one run, in the terms the overview reports it: how many
// accounts, how they ended, how long the whole thing took on the clock,
// and how much new data it cost.
type runSummary struct {
	PolicyID string
	// Policy is the name the operator gave the schedule, or a phrase
	// saying it is gone. A run outlives the schedule that made it.
	Policy   string
	QueuedAt time.Time
	// StartedAt is when the first account of the run started, and
	// FinishedAt when the last one finished. FinishedAt is zero while
	// anything is still in flight.
	StartedAt  time.Time
	FinishedAt time.Time

	Accounts   int
	Succeeded  int
	Partial    int
	Failed     int
	Cancelled  int
	Unfinished int

	// BytesAdded is what the run cost in new storage across every copy it
	// wrote, which is restic's own figure rather than the size of what
	// was read.
	BytesAdded uint64
}

// Took is how long the run lasted on the clock.
//
// Not the sum of its uploads: accounts overlap, so adding each target's
// own duration reports a night of twenty parallel minutes as several
// hours. Zero while the run is still going.
func (r runSummary) Took() time.Duration {
	if r.StartedAt.IsZero() || r.FinishedAt.IsZero() {
		return 0
	}
	return r.FinishedAt.Sub(r.StartedAt)
}

// Done says every account in the run has reached an end.
func (r runSummary) Done() bool { return r.Unfinished == 0 }

// Clean says the run finished with nothing to answer for.
func (r runSummary) Clean() bool { return r.Done() && r.Failed == 0 && r.Partial == 0 }

// runsOf reconstructs the runs behind a set of jobs, newest first.
//
// since bounds the work: the job bucket holds every backup this server has
// ever made, and the overview reports the recent past. Work that has not
// finished is reported however old it is -- a run that never ended is
// exactly what somebody needs to see.
func runsOf(jobs []nodestore.Job, policies []nodestore.Policy, since time.Time) []runSummary {
	named := make(map[string]string, len(policies))
	for _, policy := range policies {
		named[policy.ID] = policy.Name
	}

	// By schedule, then in the order they were queued, so a gap between
	// two of them is a gap in one schedule's own history rather than in
	// the server's.
	byPolicy := make(map[string][]nodestore.Job)
	for _, stored := range jobs {
		if stored.QueuedAt.Before(since) && stored.Status.Terminal() {
			continue
		}
		byPolicy[stored.PolicyID] = append(byPolicy[stored.PolicyID], stored)
	}

	var runs []runSummary
	for policyID, queued := range byPolicy {
		sort.SliceStable(queued, func(i, j int) bool {
			return queued[i].QueuedAt.Before(queued[j].QueuedAt)
		})
		var current *runSummary
		for _, stored := range queued {
			if current == nil || stored.QueuedAt.Sub(current.QueuedAt) > runGap {
				runs = append(runs, runSummary{
					PolicyID: policyID,
					Policy:   fallback(named[policyID], "a schedule since removed"),
					QueuedAt: stored.QueuedAt,
				})
				current = &runs[len(runs)-1]
			}
			addJob(current, stored)
		}
	}

	sort.SliceStable(runs, func(i, j int) bool {
		if !runs[i].QueuedAt.Equal(runs[j].QueuedAt) {
			return runs[i].QueuedAt.After(runs[j].QueuedAt)
		}
		return runs[i].Policy < runs[j].Policy
	})
	return runs
}

// addJob folds one account's job into the run it belongs to.
func addJob(run *runSummary, stored nodestore.Job) {
	run.Accounts++
	switch stored.Status {
	case job.StatusSuccess:
		run.Succeeded++
	case job.StatusPartialSuccess:
		run.Partial++
	case job.StatusFailed:
		run.Failed++
	case job.StatusCancelled:
		run.Cancelled++
	default:
		run.Unfinished++
	}
	for _, target := range stored.Targets {
		run.BytesAdded += target.BytesAdded
	}

	if stored.StartedAt != nil && (run.StartedAt.IsZero() || stored.StartedAt.Before(run.StartedAt)) {
		run.StartedAt = *stored.StartedAt
	}
	// One account still in flight leaves the whole run without an end,
	// which is the truth: the run is not over.
	if !stored.Status.Terminal() {
		run.FinishedAt = time.Time{}
		return
	}
	if run.Unfinished > 0 {
		return
	}
	if stored.FinishedAt != nil && stored.FinishedAt.After(run.FinishedAt) {
		run.FinishedAt = *stored.FinishedAt
	}
}
