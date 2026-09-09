package webui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/node"
	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/pkgacct"
)

// feedEvent is one thing that happened, as the overview reports it.
//
// The unit is the run and not the job: a night of three hundred accounts is
// one event here, because three hundred rows saying the same thing is the
// mistake the running strip already made once.
type feedEvent struct {
	When time.Time
	// Head is what happened, Tag the outcome beside it, and Note the
	// line underneath with the figures in it.
	Head string
	Tag  string
	Note string
	// Tone is "", "warn" or "bad": a colour, and nothing else.
	Tone string
}

// feedOf merges the recent runs, rehearsals and account lifecycle into one
// order, newest first, and stops at limit.
func feedOf(
	runs []runSummary, restores []nodestore.Restore,
	lifecycle []nodestore.LifecycleEvent, limit int,
) []feedEvent {
	var events []feedEvent
	for _, run := range runs {
		event := feedEvent{
			When: run.FinishedAt, Head: "Scheduled backup finished",
			Tag: run.Outcome(), Tone: run.Tone(),
		}
		if !run.Done() {
			event.When, event.Head = run.StartedAt, "Scheduled backup running"
		}
		if event.When.IsZero() {
			event.When = run.QueuedAt
		}
		event.Note = runNote(run)
		events = append(events, event)
	}

	for _, restore := range restores {
		if restore.Kind != node.KindVerify || restore.FinishedAt == nil {
			continue
		}
		event := feedEvent{
			When: *restore.FinishedAt, Head: "Rehearsal passed",
			Tag: restore.Account, Note: restore.Detail,
		}
		if restore.Status != job.StatusSuccess {
			event.Head, event.Tone = "Rehearsal failed", "bad"
			if restore.Error != "" {
				event.Note = restore.Error
			}
		}
		events = append(events, event)
	}

	for _, stored := range lifecycle {
		event := feedEvent{
			When: stored.At, Head: "Account " + lowerFirst(stored.Title()),
			Tag: stored.Account, Note: stored.Detail,
		}
		if !stored.OK {
			event.Tone = "bad"
		}
		events = append(events, event)
	}

	sort.SliceStable(events, func(i, j int) bool { return events[i].When.After(events[j].When) })
	if limit > 0 && len(events) > limit {
		events = events[:limit]
	}
	return events
}

// runNote is the line under a run: the schedule, how long it took on the
// clock, what it cost in new storage, and what did not work.
func runNote(run runSummary) string {
	parts := []string{run.Policy}
	if took := run.Took(); took > 0 {
		parts = append(parts, "took "+humanTook(took))
	}
	if run.BytesAdded > 0 {
		parts = append(parts, humanBytes(run.BytesAdded)+" of new data")
	}
	parts = append(parts, fmt.Sprintf("%d of %d accounts stored", run.Succeeded, run.Accounts))
	if run.Partial > 0 {
		parts = append(parts, fmt.Sprintf("%d reached some destinations only", run.Partial))
	}
	return strings.Join(parts, " · ")
}

// scheduleRow is one schedule as the overview lists it: what it covers,
// when it last ran and when it runs next.
type scheduleRow struct {
	nodestore.Policy
	// Covers says how much of the server this schedule takes in.
	Covers string
	// Last is the run itself when there was one inside the window read
	// back, and LastAt is when it ended -- which is not the same as the
	// schedule's own LastRunAt, because only the timer stamps that and a
	// schedule can be run by hand.
	Last   *runSummary
	LastAt time.Time
	// NextAt is zero for a schedule that is switched off or has nowhere
	// to write: neither one runs on its own.
	NextAt time.Time
	NextIn string
}

// Payload names what this schedule stores, in the words the schedule page
// offers rather than as the identifier it is kept as.
func (r scheduleRow) Payload() string {
	switch pkgacct.Mode(r.PayloadMode) {
	case pkgacct.ModeSplit:
		return "split, deduplicating"
	case pkgacct.ModeMonolithic:
		return "one archive"
	case pkgacct.ModeSystem:
		return "the server itself"
	default:
		return r.PayloadMode
	}
}

func scheduleRows(
	policies []nodestore.Policy, runs []runSummary, accounts int, now time.Time,
) []scheduleRow {
	rows := make([]scheduleRow, 0, len(policies))
	for _, policy := range policies {
		row := scheduleRow{Policy: policy, Covers: covers(policy, accounts)}
		for i, run := range runs {
			if run.PolicyID != policy.ID {
				continue
			}
			// Runs are newest first, so the first match is the one.
			row.Last = &runs[i]
			row.LastAt = run.FinishedAt
			if row.LastAt.IsZero() {
				row.LastAt = run.StartedAt
			}
			break
		}
		if row.Last == nil && policy.LastRunAt != nil {
			row.LastAt = *policy.LastRunAt
		}
		if next, ok := nextFireOf(policy, now); ok {
			row.NextAt, row.NextIn = next, "in "+humanUntil(next.Sub(now))
		}
		rows = append(rows, row)
	}
	return rows
}

// covers says how much of the server a schedule takes in, in accounts
// rather than in the empty list that means "everything".
func covers(policy nodestore.Policy, accounts int) string {
	if len(policy.Accounts) == 0 {
		return fmt.Sprintf("All %d accounts", accounts)
	}
	return fmt.Sprintf("%d of %d accounts", len(policy.Accounts), accounts)
}

// weakest is the accounts that have failed most, worst first.
//
// By count and not by ratio: an account backed up once, unsuccessfully, is
// a hundred per cent failure and would top a ranking by ratio every time,
// which puts the newest account on the server at the head of a list titled
// "weakest". Nothing is returned when nothing has ever failed -- three
// rows saying "never failed" are three rows of filler.
func weakest(accounts []accountView, limit int) []accountView {
	ranked := make([]accountView, 0, len(accounts))
	for _, account := range accounts {
		if account.Runs > 0 {
			ranked = append(ranked, account)
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].Failures() != ranked[j].Failures() {
			return ranked[i].Failures() > ranked[j].Failures()
		}
		return ranked[i].Runs > ranked[j].Runs
	})
	if len(ranked) == 0 || ranked[0].Failures() == 0 {
		return nil
	}
	if limit > 0 && len(ranked) > limit {
		ranked = ranked[:limit]
	}
	return ranked
}
