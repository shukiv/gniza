package webui

import (
	"fmt"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/nodestore"
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
