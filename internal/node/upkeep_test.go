package node_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/resticrun"
)

// resticCalls records which restic subcommands reached the destination, and
// how many jobs were waiting when each one did.
type resticCalls struct {
	mu    sync.Mutex
	store *nodestore.Store
	seen  []string
	jobs  map[string]int
}

func (c *resticCalls) Exec(_ context.Context, command resticrun.Command) (resticrun.CommandResult, error) {
	sub := ""
	for _, arg := range command.Args {
		if slices.Contains([]string{"unlock", "stats", "snapshots", "list", "check", "forget", "prune"}, arg) {
			sub = arg
			break
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, sub)
	// restic lists the locks first, and removes only what it finds stale.
	if c.store != nil && sub == "list" {
		queued, err := c.store.Jobs(0)
		if err != nil {
			return resticrun.CommandResult{}, err
		}
		c.jobs[sub] = len(queued)
	}
	return resticrun.CommandResult{Stdout: []byte(`{"total_size":1,"snapshots_count":1}`)}, nil
}

func (c *resticCalls) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.seen)
}

func (c *resticCalls) saw(sub string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Contains(c.seen, sub)
}

// upkeepServer is a server whose readings are all a long way out of date, so
// that the only thing holding the upkeep back is when it is allowed to run.
func upkeepServer(t *testing.T, cron string, lastFired *time.Time) (*nodestore.Store, nodestore.Repository, nodestore.Policy, *resticCalls, func(time.Time)) {
	t.Helper()
	calls := &resticCalls{jobs: map[string]int{}}
	store, repo, engine := censusEngineWith(t, calls)
	calls.store = store

	long := time.Now().Add(-30 * 24 * time.Hour).UTC()
	repo.Census.MeasuredAt = &long
	stored, err := store.PutRepository(repo)
	if err != nil {
		t.Fatal(err)
	}
	dest, err := store.Destination(repo.DestinationID)
	if err != nil {
		t.Fatal(err)
	}
	dest.LastCheckedAt = &long
	if _, err := store.PutDestination(dest); err != nil {
		t.Fatal(err)
	}
	policy := nodestore.Policy{}
	if cron != "" {
		policy, err = store.PutPolicy(nodestore.Policy{
			Name: "Nightly", ScheduleCron: cron, Enabled: true,
			RepositoryIDs: []string{repo.ID}, Accounts: []string{"customer1"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if lastFired != nil {
			if err := store.SetPolicyLastRun(policy.ID, *lastFired); err != nil {
				t.Fatal(err)
			}
		}
	}
	return store, stored, policy, calls, func(now time.Time) {
		t.Helper()
		if _, err := engine.Schedule(context.Background(), now); err != nil {
			t.Fatal(err)
		}
		// The census and the sweep run beside the scheduler.
		for wait := 0; wait < 200 && (engine.Checking() || engine.CensusingForTest() || engine.SweepingLocksForTest()); wait++ {
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func destinationCheckedSince(t *testing.T, store *nodestore.Store, repo nodestore.Repository, since time.Time) bool {
	t.Helper()
	dest, err := store.Destination(repo.DestinationID)
	if err != nil {
		t.Fatal(err)
	}
	return dest.LastCheckedAt != nil && dest.LastCheckedAt.After(since)
}

func measuredSince(t *testing.T, store *nodestore.Store, repo nodestore.Repository, since time.Time) bool {
	t.Helper()
	after, err := store.Repository(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	return after.Census.MeasuredAt != nil && after.Census.MeasuredAt.After(since)
}

// Between one night's backup and the next, nothing goes to the destination,
// however old the readings are. Every visit is a login on somebody's server.
func TestNothingGoesToADestinationBetweenBackups(t *testing.T) {
	now := time.Date(2026, 11, 1, 14, 0, 0, 0, time.UTC)
	fired := now.Add(-12 * time.Hour)
	before := time.Now().Add(-time.Minute)
	store, repo, _, calls, tick := upkeepServer(t, "0 2 * * *", &fired)

	for hour := 0; hour < 11; hour++ {
		tick(now.Add(time.Duration(hour) * time.Hour))
	}

	if calls.count() != 0 {
		t.Errorf("restic ran between backups: %v", calls.seen)
	}
	if destinationCheckedSince(t, store, repo, before) {
		t.Error("the destination was logged in to between backups")
	}
}

// The lock is looked for before the backups are queued, because a queued backup
// counts as work in flight and locks are not touched while there is any. The
// rest waits for the last backup to finish.
func TestUpkeepComesBeforeAndAfterTheBackups(t *testing.T) {
	now := time.Date(2026, 11, 1, 2, 0, 5, 0, time.UTC)
	fired := now.Add(-24 * time.Hour)
	before := time.Now().Add(-time.Minute)
	store, repo, _, calls, tick := upkeepServer(t, "0 2 * * *", &fired)

	tick(now)

	if !calls.saw("list") {
		t.Fatalf("no look for stale locks before the backups: %v", calls.seen)
	}
	if calls.jobs["list"] != 0 {
		t.Errorf("%d jobs were queued before the look for stale locks", calls.jobs["list"])
	}
	jobs, err := store.Jobs(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("queued %d jobs, want 1", len(jobs))
	}
	if destinationCheckedSince(t, store, repo, before) || measuredSince(t, store, repo, before) {
		t.Fatal("the post-tasks ran while the backup was still to come")
	}

	running := jobs[0]
	running.Status = job.StatusSuccess
	if _, err := store.PutJob(running); err != nil {
		t.Fatal(err)
	}
	tick(now.Add(time.Minute))

	if !destinationCheckedSince(t, store, repo, before) {
		t.Error("the destination was not checked after the backup")
	}
	if !measuredSince(t, store, repo, before) {
		t.Error("the repository was not counted after the backup")
	}
}

// A schedule that fires every hour does not bring the hourly logins back.
func TestAnHourlyScheduleHasItsUpkeepOnceADay(t *testing.T) {
	now := time.Date(2026, 11, 1, 2, 0, 5, 0, time.UTC)
	fired := now.Add(-time.Hour)
	store, repo, _, calls, tick := upkeepServer(t, "0 * * * *", &fired)

	finish := func() {
		t.Helper()
		jobs, err := store.Jobs(0)
		if err != nil {
			t.Fatal(err)
		}
		for _, stored := range jobs {
			if !stored.Status.Terminal() {
				stored.Status = job.StatusSuccess
				if _, err := store.PutJob(stored); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	tick(now)
	finish()
	tick(now.Add(time.Minute))
	first := calls.count()
	if first == 0 {
		t.Fatal("no upkeep after the first backup")
	}
	checked, err := store.Destination(repo.DestinationID)
	if err != nil {
		t.Fatal(err)
	}

	for hour := 1; hour < 10; hour++ {
		tick(now.Add(time.Duration(hour) * time.Hour))
		finish()
		tick(now.Add(time.Duration(hour)*time.Hour + time.Minute))
	}

	if calls.count() != first {
		t.Errorf("restic ran %d more times in ten hours", calls.count()-first)
	}
	again, err := store.Destination(repo.DestinationID)
	if err != nil {
		t.Fatal(err)
	}
	if !again.LastCheckedAt.Equal(*checked.LastCheckedAt) {
		t.Error("the destination was logged in to again inside the day")
	}
}

// A server with no schedule running has nothing to follow, and still has to
// know about its destinations. It looks when it has waited long enough, once.
func TestAServerWithoutBackupsStillLooksOnceADay(t *testing.T) {
	now := time.Now().UTC()
	fired := now.Add(-31 * time.Hour)
	before := now.Add(-time.Minute)
	store, repo, _, calls, tick := upkeepServer(t, "0 2 1 1 *", &fired)

	tick(now)
	if !destinationCheckedSince(t, store, repo, before) {
		t.Fatal("a server with no backups never looked at its destination")
	}
	if !calls.saw("list") {
		t.Error("a server with no pre-task never looked for stale locks")
	}
	count := calls.count()

	tick(now.Add(time.Hour))
	if calls.count() != count {
		t.Errorf("it looked again an hour later: %v", calls.seen[count:])
	}
}
