package webui

import (
	"fmt"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/panel"
)

func moment(t time.Time) *time.Time { return &t }

func TestANightOfThreeHundredBackupsIsOneRun(t *testing.T) {
	night := time.Date(2026, 9, 8, 2, 0, 0, 0, time.UTC)
	var jobs []nodestore.Job
	for i := range 300 {
		// A policy queues every account it covers in one loop, so the
		// timestamps are seconds apart rather than identical.
		queued := night.Add(time.Duration(i) * time.Second)
		finished := queued.Add(20 * time.Minute)
		jobs = append(jobs, nodestore.Job{
			PolicyID: "nightly", Account: fmt.Sprintf("customer%d", i),
			Status: job.StatusSuccess, QueuedAt: queued,
			StartedAt: moment(queued), FinishedAt: moment(finished),
		})
	}
	runs := runsOf(jobs, []nodestore.Policy{{ID: "nightly", Name: "nightly"}}, night.Add(-24*time.Hour))
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want the night counted once", len(runs))
	}
	if runs[0].Accounts != 300 || runs[0].Succeeded != 300 {
		t.Fatalf("run = %d accounts, %d succeeded", runs[0].Accounts, runs[0].Succeeded)
	}
	if runs[0].Policy != "nightly" {
		t.Fatalf("policy = %q, want the name the operator gave it", runs[0].Policy)
	}
}

func TestARunThatCrossesMidnightIsStillOneRun(t *testing.T) {
	before := time.Date(2026, 9, 7, 23, 58, 0, 0, time.UTC)
	after := time.Date(2026, 9, 8, 0, 3, 0, 0, time.UTC)
	runs := runsOf([]nodestore.Job{
		{PolicyID: "nightly", Account: "a", Status: job.StatusSuccess,
			QueuedAt: before, StartedAt: moment(before), FinishedAt: moment(after)},
		{PolicyID: "nightly", Account: "b", Status: job.StatusSuccess,
			QueuedAt: after, StartedAt: moment(after), FinishedAt: moment(after.Add(time.Minute))},
	}, nil, before.Add(-24*time.Hour))
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want one run split by nothing but the date", len(runs))
	}
}

func TestABackupAskedForByHandIsItsOwnRun(t *testing.T) {
	night := time.Date(2026, 9, 8, 2, 0, 0, 0, time.UTC)
	afternoon := time.Date(2026, 9, 8, 14, 0, 0, 0, time.UTC)
	runs := runsOf([]nodestore.Job{
		{PolicyID: "nightly", Account: "a", Status: job.StatusSuccess,
			QueuedAt: night, StartedAt: moment(night), FinishedAt: moment(night.Add(time.Minute))},
		{PolicyID: "nightly", Account: "b", Status: job.StatusSuccess,
			QueuedAt: night.Add(2 * time.Second), StartedAt: moment(night), FinishedAt: moment(night.Add(time.Minute))},
		{PolicyID: "nightly", Account: "b", Status: job.StatusFailed,
			QueuedAt: afternoon, StartedAt: moment(afternoon), FinishedAt: moment(afternoon.Add(time.Minute))},
	}, nil, night.Add(-24*time.Hour))
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want the afternoon kept out of last night", len(runs))
	}
	// Newest first, so the one account asked for by hand comes first.
	if runs[0].Accounts != 1 || runs[0].Failed != 1 {
		t.Fatalf("newest run = %d accounts, %d failed", runs[0].Accounts, runs[0].Failed)
	}
	if runs[1].Accounts != 2 {
		t.Fatalf("the night = %d accounts", runs[1].Accounts)
	}
}

func TestARunIsTimedByTheClockAndNotBySummingItsUploads(t *testing.T) {
	start := time.Date(2026, 9, 8, 2, 0, 0, 0, time.UTC)
	runs := runsOf([]nodestore.Job{
		{PolicyID: "nightly", Account: "a", Status: job.StatusSuccess,
			QueuedAt: start, StartedAt: moment(start), FinishedAt: moment(start.Add(20 * time.Minute)),
			Targets: []nodestore.JobTarget{{DurationSecs: 1200}}},
		{PolicyID: "nightly", Account: "b", Status: job.StatusSuccess,
			QueuedAt: start.Add(time.Second), StartedAt: moment(start.Add(5 * time.Minute)),
			FinishedAt: moment(start.Add(25 * time.Minute)),
			Targets:    []nodestore.JobTarget{{DurationSecs: 1200}}},
	}, nil, start.Add(-24*time.Hour))
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	if took := runs[0].Took(); took != 25*time.Minute {
		t.Fatalf("took = %s, want the wall clock rather than 40m of overlapping uploads", took)
	}
}

func TestARunStillGoingHasNoEndTime(t *testing.T) {
	start := time.Date(2026, 9, 8, 2, 0, 0, 0, time.UTC)
	runs := runsOf([]nodestore.Job{
		{PolicyID: "nightly", Account: "a", Status: job.StatusSuccess,
			QueuedAt: start, StartedAt: moment(start), FinishedAt: moment(start.Add(time.Minute))},
		{PolicyID: "nightly", Account: "b", Status: job.StatusRunning,
			QueuedAt: start.Add(time.Second), StartedAt: moment(start)},
	}, nil, start.Add(-24*time.Hour))
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	if runs[0].Done() {
		t.Fatalf("a run with work still in flight was called finished: %+v", runs[0])
	}
	if !runs[0].FinishedAt.IsZero() {
		t.Fatalf("finished at %s while one account is still running", runs[0].FinishedAt)
	}
	if runs[0].Unfinished != 1 {
		t.Fatalf("unfinished = %d, want the one still going", runs[0].Unfinished)
	}
}

func TestARunReportsWhatItStored(t *testing.T) {
	start := time.Date(2026, 9, 8, 2, 0, 0, 0, time.UTC)
	runs := runsOf([]nodestore.Job{{
		PolicyID: "nightly", Account: "a", Status: job.StatusPartialSuccess,
		QueuedAt: start, StartedAt: moment(start), FinishedAt: moment(start.Add(time.Minute)),
		Targets: []nodestore.JobTarget{{BytesAdded: 1 << 20}, {BytesAdded: 3 << 20}},
	}}, nil, start.Add(-24*time.Hour))
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	if runs[0].BytesAdded != 4<<20 {
		t.Fatalf("stored = %d bytes, want every copy counted", runs[0].BytesAdded)
	}
	if runs[0].Partial != 1 {
		t.Fatalf("partial = %d, want the run that reached some destinations counted apart",
			runs[0].Partial)
	}
}

func TestAScheduleSinceRemovedStillNamesItsRun(t *testing.T) {
	start := time.Date(2026, 9, 8, 2, 0, 0, 0, time.UTC)
	runs := runsOf([]nodestore.Job{{
		PolicyID: "gone", Account: "a", Status: job.StatusSuccess,
		QueuedAt: start, StartedAt: moment(start), FinishedAt: moment(start.Add(time.Minute)),
	}}, []nodestore.Policy{{ID: "nightly", Name: "nightly"}}, start.Add(-24*time.Hour))
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	if runs[0].Policy != "a schedule since removed" {
		t.Fatalf("policy = %q", runs[0].Policy)
	}
}

func TestOlderRunsAreLeftOutRatherThanRead(t *testing.T) {
	now := time.Date(2026, 9, 8, 2, 0, 0, 0, time.UTC)
	old := now.Add(-30 * 24 * time.Hour)
	runs := runsOf([]nodestore.Job{{
		PolicyID: "nightly", Account: "a", Status: job.StatusSuccess,
		QueuedAt: old, StartedAt: moment(old), FinishedAt: moment(old.Add(time.Minute)),
	}}, nil, now.Add(-7*24*time.Hour))
	if len(runs) != 0 {
		t.Fatalf("runs = %d, want the month-old night left out", len(runs))
	}
}

func TestWorkStillInFlightIsShownHoweverOldItIs(t *testing.T) {
	now := time.Date(2026, 9, 8, 2, 0, 0, 0, time.UTC)
	old := now.Add(-30 * 24 * time.Hour)
	runs := runsOf([]nodestore.Job{{
		PolicyID: "nightly", Account: "a", Status: job.StatusRunning,
		QueuedAt: old, StartedAt: moment(old),
	}}, nil, now.Add(-7*24*time.Hour))
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want a run that never ended still reported", len(runs))
	}
}

func TestTheOverviewLeadsWithWhetherTheServerIsCovered(t *testing.T) {
	view := dashboardView{
		Accounts: make([]accountView, 19), Protected: 19,
		Destinations: []destinationView{{}},
	}
	if got := view.Verdict(); got != "All 19 accounts are covered." {
		t.Fatalf("verdict = %q", got)
	}
	if got := view.Band(); got != "ok" {
		t.Fatalf("band = %q, want a covered server drawn as covered", got)
	}
}

func TestTheOverviewSaysHowManyAccountsAreNotCovered(t *testing.T) {
	view := dashboardView{
		Accounts: make([]accountView, 312), Protected: 294, Stale: 6, Unprotected: 12,
		Destinations: []destinationView{{}},
	}
	if got := view.Verdict(); got != "18 of 312 accounts are not covered." {
		t.Fatalf("verdict = %q", got)
	}
	if got := view.Band(); got != "bad" {
		t.Fatalf("band = %q, want an account that has never been backed up read as bad", got)
	}
}

func TestAServerWithNowhereToSendABackupSaysThatFirst(t *testing.T) {
	view := dashboardView{Accounts: make([]accountView, 4)}
	if got := view.Verdict(); got != "Nothing is being backed up: this server has no destination." {
		t.Fatalf("verdict = %q", got)
	}
	if got := view.Band(); got != "bad" {
		t.Fatalf("band = %q", got)
	}
}

func TestAnAccountBehindItsScheduleIsAWarningRatherThanAFailure(t *testing.T) {
	view := dashboardView{
		Accounts: make([]accountView, 10), Protected: 9, Stale: 1,
		Destinations: []destinationView{{}},
	}
	if got := view.Band(); got != "warn" {
		t.Fatalf("band = %q, want a stale copy short of bad", got)
	}
	if got := view.Verdict(); got != "1 of 10 accounts is not covered." {
		t.Fatalf("verdict = %q, want it to read as one account", got)
	}
}

func TestTheOverviewMeasuresTheAccountsItIsLookingAfter(t *testing.T) {
	view := dashboardView{}
	addCoverage(&view, []accountView{
		{AccountInfo: panel.AccountInfo{User: "small", SizeBytes: 2 << 30}},
		{AccountInfo: panel.AccountInfo{User: "large", SizeBytes: 5 << 30}},
	})
	if view.Managed != 7<<30 {
		t.Fatalf("managed = %d bytes", view.Managed)
	}
	if view.Largest != 5<<30 || view.LargestAccount != "large" {
		t.Fatalf("largest = %s at %d bytes", view.LargestAccount, view.Largest)
	}
}

func TestARunIsRememberedByWhatItLost(t *testing.T) {
	run := runSummary{Accounts: 312, Succeeded: 294, Failed: 12, Partial: 6}
	if got := run.Outcome(); got != "12 failed" {
		t.Fatalf("outcome = %q, want the failures named first", got)
	}
	if got := run.Tone(); got != "bad" {
		t.Fatalf("tone = %q", got)
	}
	clean := runSummary{Accounts: 19, Succeeded: 19}
	if got := clean.Outcome(); got != "19 backed up" {
		t.Fatalf("outcome = %q", got)
	}
	if got := clean.Tone(); got != "" {
		t.Fatalf("tone = %q, want a clean run drawn as nothing in particular", got)
	}
}

func TestARunStillGoingSaysHowFarItHasGot(t *testing.T) {
	run := runSummary{Accounts: 312, Succeeded: 6, Unfinished: 306}
	if got := run.Outcome(); got != "6 of 312 done" {
		t.Fatalf("outcome = %q", got)
	}
}

func TestAWeekOfBackupsCountsWhatWasStored(t *testing.T) {
	if got := backedUp([]runSummary{
		{Succeeded: 19, Failed: 1}, {Succeeded: 18, Partial: 1},
	}); got != 38 {
		t.Fatalf("backed up = %d, want every account a copy was written for", got)
	}
}

// Queueing a backup must not take the coverage away. Pressing "Back up
// now" on every account queues them all at once; if in-flight work read
// as a gap, the overview would answer "134 of 142 accounts are not
// covered" one second after the operator asked for a backup of all of
// them, on a server whose copies had not changed at all.
func TestAnAccountBeingBackedUpNowIsStillCoveredByTheCopyItAlreadyHas(t *testing.T) {
	fresh := accountView{
		AccountInfo:   panel.AccountInfo{User: "current"},
		LastBackup:    moment(time.Now().Add(-time.Hour)),
		LastStatus:    job.StatusSuccess,
		ExpectedEvery: 24 * time.Hour,
		Running:       true,
	}
	if !fresh.Current() {
		t.Fatalf("an account queued for another backup lost the copy it has")
	}
	view := dashboardView{Accounts: []accountView{fresh}, Destinations: []destinationView{{}}}
	addCoverage(&view, view.Accounts)
	if view.Protected != 1 || view.Stale != 0 || view.Unprotected != 0 {
		t.Fatalf("protected = %d, stale = %d, unprotected = %d",
			view.Protected, view.Stale, view.Unprotected)
	}
	if got := view.Verdict(); got != "All 1 accounts are covered." {
		t.Fatalf("verdict = %q", got)
	}
	// The pill over the row still says what the server is doing.
	if got := fresh.State(); got != StateWorking {
		t.Fatalf("state = %q, want the row to still show the backup running", got)
	}
}

// The other half of that: work in flight is not a backup either. An
// account queued for its first backup has nothing stored yet, and the
// page must not count the queueing as the copy.
func TestAnAccountQueuedForItsFirstBackupIsStillUnprotected(t *testing.T) {
	view := dashboardView{Destinations: []destinationView{{}}}
	view.Accounts = []accountView{{
		AccountInfo:   panel.AccountInfo{User: "new"},
		ExpectedEvery: 24 * time.Hour,
		Running:       true,
	}}
	addCoverage(&view, view.Accounts)
	if view.Unprotected != 1 || view.Protected != 0 {
		t.Fatalf("protected = %d, unprotected = %d", view.Protected, view.Unprotected)
	}
}

// A retry does not undo the failure it is retrying. Until the new backup
// finishes, the last thing this account did was fail, and the page keeps
// saying so.
func TestAnAccountBeingRetriedStillCountsAsFailing(t *testing.T) {
	view := dashboardView{Destinations: []destinationView{{}}}
	view.Accounts = []accountView{{
		AccountInfo:   panel.AccountInfo{User: "retried"},
		LastBackup:    moment(time.Now().Add(-time.Hour)),
		LastStatus:    job.StatusFailed,
		ExpectedEvery: 24 * time.Hour,
		Running:       true,
	}}
	addCoverage(&view, view.Accounts)
	if view.Failed != 1 {
		t.Fatalf("failed = %d, want the failure to survive the retry", view.Failed)
	}
	if got := view.Band(); got != "bad" {
		t.Fatalf("band = %q", got)
	}
}
