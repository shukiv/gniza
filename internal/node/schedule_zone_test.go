package node_test

import (
	"context"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/shukiv/gniza/internal/nodestore"
)

// TestAScheduleFiresOnceOnEachDay walks a server's clock over two days and
// counts the firings. A schedule is read on the server's clock, whatever
// zone the last firing was stored in: read in UTC it also comes due at 02:00
// UTC, which on a server two hours ahead is the 04:00 that ran every backup
// a second time.
func TestAScheduleFiresOnceOnEachDay(t *testing.T) {
	jerusalem, err := time.LoadLocation("Asia/Jerusalem")
	if err != nil {
		t.Fatal(err)
	}
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		cron  string
		start time.Time
	}{
		{"one hour ahead of UTC", "0 2 * * *",
			time.Date(2026, 3, 10, 12, 0, 6, 0, time.FixedZone("UTC+1", 3600))},
		{"two hours ahead of UTC", "0 2 * * *",
			time.Date(2026, 3, 10, 12, 0, 6, 0, time.FixedZone("UTC+2", 2*3600))},
		{"three hours ahead of UTC", "0 2 * * *",
			time.Date(2026, 3, 10, 12, 0, 6, 0, time.FixedZone("UTC+3", 3*3600))},
		{"four hours behind UTC", "0 2 * * *",
			time.Date(2026, 3, 10, 12, 0, 6, 0, time.FixedZone("UTC-4", -4*3600))},
		{"UTC", "0 2 * * *",
			time.Date(2026, 3, 10, 12, 0, 6, 0, time.UTC)},
		// The night .10, .143, .148 and .53 ran twice.
		{"Israel, the night the clocks go back", "0 2 * * *",
			time.Date(2026, 10, 24, 12, 0, 6, 0, jerusalem)},
		// The night .163 ran three times: 01:00 happens twice on it.
		{"Israel, an hour that happens twice", "0 1 * * *",
			time.Date(2026, 10, 24, 12, 0, 6, 0, jerusalem)},
		{"New York, an hour that happens twice", "30 1 * * *",
			time.Date(2026, 10, 31, 12, 0, 6, 0, newYork)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine, store, repo := rehearsingEngine(t, "customer1")
			policy, err := store.PutPolicy(nodestore.Policy{
				Name: "Nightly", ScheduleCron: tc.cron, Enabled: true,
				RepositoryIDs: []string{repo.ID},
			})
			if err != nil {
				t.Fatal(err)
			}

			var fired []time.Time
			var seen *time.Time
			end := tc.start.AddDate(0, 0, 2)
			for now := tc.start; now.Before(end); now = now.Add(time.Minute) {
				if _, err := engine.Schedule(context.Background(), now); err != nil {
					t.Fatalf("schedule at %s: %v", now, err)
				}
				stored, err := store.Policy(policy.ID)
				if err != nil {
					t.Fatal(err)
				}
				if last := stored.LastRunAt; last != nil && (seen == nil || !last.Equal(*seen)) {
					fired = append(fired, now)
					seen = last
				}
			}
			if len(fired) != 2 {
				t.Fatalf("fired %d times in two days, want 2: %v", len(fired), fired)
			}
		})
	}
}
