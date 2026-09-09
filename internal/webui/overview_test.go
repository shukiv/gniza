package webui

import (
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/node"
	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/panel"
)

func TestTheFeedIsOneLinePerRunAndNotOnePerAccount(t *testing.T) {
	night := time.Date(2026, 9, 8, 2, 0, 0, 0, time.UTC)
	events := feedOf([]runSummary{{
		Policy: "nightly", QueuedAt: night, StartedAt: night,
		FinishedAt: night.Add(42 * time.Minute),
		Accounts:   312, Succeeded: 300, Failed: 12, BytesAdded: 3 << 30,
	}}, nil, nil, 8)
	if len(events) != 1 {
		t.Fatalf("events = %d, want the night as one line", len(events))
	}
	if events[0].Tag != "12 failed" || events[0].Tone != "bad" {
		t.Fatalf("event = %+v, want the failures named", events[0])
	}
	if events[0].Head != "Scheduled backup finished" {
		t.Fatalf("head = %q", events[0].Head)
	}
}

func TestTheFeedPutsEverythingInOneOrder(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	drill := now.Add(-3 * time.Hour)
	run := now.Add(-6 * time.Hour)
	created := now.Add(-time.Hour)
	events := feedOf(
		[]runSummary{{Policy: "nightly", QueuedAt: run, StartedAt: run,
			FinishedAt: run.Add(time.Minute), Accounts: 2, Succeeded: 2}},
		[]nodestore.Restore{{
			Kind: node.KindVerify, Account: "arkady", Status: job.StatusSuccess,
			FinishedAt: &drill, Detail: "17737 files",
		}},
		[]nodestore.LifecycleEvent{{Event: "create", Account: "newsite7", OK: true, At: created}},
		8)
	if len(events) != 3 {
		t.Fatalf("events = %d, want the run, the rehearsal and the account", len(events))
	}
	// The run is placed by when it ended, which is what a reader looking
	// for "when did last night finish" is after.
	if events[0].When != created || events[2].When != run.Add(time.Minute) {
		t.Fatalf("order = %v", []time.Time{events[0].When, events[1].When, events[2].When})
	}
}

func TestTheFeedStopsBeforeItBecomesThePage(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	var lifecycle []nodestore.LifecycleEvent
	for i := range 40 {
		lifecycle = append(lifecycle, nodestore.LifecycleEvent{
			Event: "create", Account: "customer", OK: true,
			At: now.Add(-time.Duration(i) * time.Minute),
		})
	}
	if events := feedOf(nil, nil, lifecycle, 8); len(events) != 8 {
		t.Fatalf("events = %d, want the feed capped", len(events))
	}
}

func TestARunStillGoingIsInTheFeedAsUnfinished(t *testing.T) {
	start := time.Date(2026, 9, 8, 2, 0, 0, 0, time.UTC)
	events := feedOf([]runSummary{{
		Policy: "nightly", QueuedAt: start, StartedAt: start,
		Accounts: 312, Succeeded: 6, Unfinished: 306,
	}}, nil, nil, 8)
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}
	if events[0].Head != "Scheduled backup running" {
		t.Fatalf("head = %q, want a run that has not finished said as one", events[0].Head)
	}
}

func TestAScheduleSaysWhenItLastRanRatherThanWhenItLastFired(t *testing.T) {
	now := time.Date(2026, 9, 8, 15, 0, 0, 0, time.UTC)
	fired := now.Add(-13 * time.Hour)
	byHand := now.Add(-time.Hour)
	rows := scheduleRows(
		[]nodestore.Policy{{
			ID: "nightly", Name: "nightly", Enabled: true, ScheduleCron: "0 2 * * *",
			RepositoryIDs: []string{"a"}, LastRunAt: &fired,
		}},
		[]runSummary{{
			PolicyID: "nightly", Policy: "nightly", QueuedAt: byHand, StartedAt: byHand,
			FinishedAt: byHand.Add(time.Minute), Accounts: 1, Succeeded: 1,
		}},
		19, now)
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	// A schedule run by hand is the last time it ran. Only Schedule()
	// stamps LastRunAt, so reading that alone would report this morning.
	if !rows[0].LastAt.Equal(byHand.Add(time.Minute)) {
		t.Fatalf("last run = %s, want the run an hour ago", rows[0].LastAt)
	}
	if rows[0].Covers != "All 19 accounts" {
		t.Fatalf("covers = %q", rows[0].Covers)
	}
	if !rows[0].NextAt.Equal(time.Date(2026, 9, 9, 2, 0, 0, 0, time.UTC)) {
		t.Fatalf("next run = %s", rows[0].NextAt)
	}
}

func TestAScheduleThatHasNotRunThisWeekStillSaysWhenItLastFired(t *testing.T) {
	now := time.Date(2026, 9, 8, 15, 0, 0, 0, time.UTC)
	fired := now.Add(-30 * 24 * time.Hour)
	rows := scheduleRows([]nodestore.Policy{{
		ID: "monthly", Name: "monthly", Enabled: true, ScheduleCron: "0 4 1 * *",
		RepositoryIDs: []string{"a"}, Accounts: []string{"a", "b"}, LastRunAt: &fired,
	}}, nil, 19, now)
	if len(rows) != 1 || !rows[0].LastAt.Equal(fired) {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].Covers != "2 of 19 accounts" {
		t.Fatalf("covers = %q", rows[0].Covers)
	}
}

func TestADisabledScheduleHasNoNextRun(t *testing.T) {
	now := time.Date(2026, 9, 8, 15, 0, 0, 0, time.UTC)
	rows := scheduleRows([]nodestore.Policy{{
		ID: "off", Name: "off", ScheduleCron: "0 2 * * *", RepositoryIDs: []string{"a"},
	}}, nil, 19, now)
	if len(rows) != 1 || !rows[0].NextAt.IsZero() {
		t.Fatalf("a schedule that is switched off was given a next run: %+v", rows)
	}
}

func TestTheWeakestAccountsAreTheOnesThatFailMostRatherThanTheNewest(t *testing.T) {
	weak := weakest([]accountView{
		{AccountInfo: panel.AccountInfo{User: "steady"}, Runs: 61, Succeeded: 61},
		// One run, one failure is a hundred per cent and would top any
		// ranking by ratio. It is not the account to look after.
		{AccountInfo: panel.AccountInfo{User: "new"}, Runs: 1, Succeeded: 0},
		{AccountInfo: panel.AccountInfo{User: "flaky"}, Runs: 28, Succeeded: 25},
	}, 3)
	if len(weak) != 3 {
		t.Fatalf("weakest = %d", len(weak))
	}
	if weak[0].User != "flaky" {
		t.Fatalf("weakest = %q, want the account that has failed most", weak[0].User)
	}
}

func TestNothingIsCalledWeakWhenNothingHasEverFailed(t *testing.T) {
	if weak := weakest([]accountView{
		{AccountInfo: panel.AccountInfo{User: "a"}, Runs: 9, Succeeded: 9},
		{AccountInfo: panel.AccountInfo{User: "b"}, Runs: 4, Succeeded: 4},
	}, 3); weak != nil {
		t.Fatalf("weakest = %+v, want nothing on a server where nothing has failed", weak)
	}
}
