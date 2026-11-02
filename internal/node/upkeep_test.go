package node_test

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/node"
	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/resticrun"
)

// resticCalls records which restic subcommands reached the destination, and
// how many jobs were waiting when each one did.
type resticCalls struct {
	mu       sync.Mutex
	store    *nodestore.Store
	seen     []string
	jobs     map[string]int
	failList bool
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
	if sub == "list" {
		if c.failList {
			return resticrun.CommandResult{ExitCode: 1, Stderr: []byte("Fatal: no answer")}, nil
		}
		if c.store != nil {
			queued, err := c.store.Jobs(0)
			if err != nil {
				return resticrun.CommandResult{}, err
			}
			c.jobs[sub] = len(queued)
		}
		return resticrun.CommandResult{}, nil
	}
	return resticrun.CommandResult{Stdout: []byte(`{"total_size":1,"snapshots_count":1}`)}, nil
}

func (c *resticCalls) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.seen)
}

func (c *resticCalls) times(sub string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, seen := range c.seen {
		if seen == sub {
			n++
		}
	}
	return n
}

// upkeepServer is a server whose readings are all as old as asked, on a
// schedule that last fired when asked.
type upkeepServer struct {
	t      *testing.T
	store  *nodestore.Store
	repo   nodestore.Repository
	calls  *resticCalls
	engine *node.Engine
}

func newUpkeepServer(t *testing.T, cron string, lastFired time.Time, readingsAged time.Duration) *upkeepServer {
	t.Helper()
	calls := &resticCalls{jobs: map[string]int{}}
	store, repo, engine := censusEngineWith(t, calls)
	calls.store = store

	read := time.Now().Add(-readingsAged).UTC()
	repo.Census.MeasuredAt = &read
	stored, err := store.PutRepository(repo)
	if err != nil {
		t.Fatal(err)
	}
	dest, err := store.Destination(repo.DestinationID)
	if err != nil {
		t.Fatal(err)
	}
	dest.LastCheckedAt = &read
	if _, err := store.PutDestination(dest); err != nil {
		t.Fatal(err)
	}
	policy, err := store.PutPolicy(nodestore.Policy{
		Name: "Nightly", ScheduleCron: cron, Enabled: true,
		RepositoryIDs: []string{repo.ID}, Accounts: []string{"customer1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetPolicyLastRun(policy.ID, lastFired); err != nil {
		t.Fatal(err)
	}
	return &upkeepServer{t, store, stored, calls, engine}
}

// tick runs the scheduler once, and waits for what it started beside itself.
func (s *upkeepServer) tick(now time.Time) {
	s.t.Helper()
	if _, err := s.engine.Schedule(context.Background(), now); err != nil {
		s.t.Fatal(err)
	}
	for wait := 0; wait < 200 && (s.engine.Checking() || s.engine.CensusingForTest() || s.engine.SweepingLocksForTest()); wait++ {
		time.Sleep(5 * time.Millisecond)
	}
}

// finishBackups marks everything queued as having succeeded.
func (s *upkeepServer) finishBackups() int {
	s.t.Helper()
	jobs, err := s.store.Jobs(0)
	if err != nil {
		s.t.Fatal(err)
	}
	finished := 0
	for _, stored := range jobs {
		if stored.Status.Terminal() {
			continue
		}
		stored.Status = job.StatusSuccess
		if _, err := s.store.PutJob(stored); err != nil {
			s.t.Fatal(err)
		}
		finished++
	}
	return finished
}

func (s *upkeepServer) lastChecked() time.Time {
	s.t.Helper()
	dest, err := s.store.Destination(s.repo.DestinationID)
	if err != nil {
		s.t.Fatal(err)
	}
	return *dest.LastCheckedAt
}

func (s *upkeepServer) lastMeasured() time.Time {
	s.t.Helper()
	repo, err := s.store.Repository(s.repo.ID)
	if err != nil {
		s.t.Fatal(err)
	}
	return *repo.Census.MeasuredAt
}

// onTheHour is the start of the current hour, plus the few seconds a tick
// usually lands after it.
func onTheHour() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 5, 0, time.UTC)
}

// neverFiresSoon is a schedule that will not come due during a test unless it
// is run on New Year's Day.
const neverFiresSoon = "0 2 1 1 *"

// Between one night's backup and the next, nothing goes to the destination.
// Every visit is a login on somebody's server.
func TestNothingGoesToADestinationBetweenBackups(t *testing.T) {
	now := time.Now().UTC()
	server := newUpkeepServer(t, neverFiresSoon, now.Add(-12*time.Hour), 12*time.Hour)
	checked := server.lastChecked()

	for hour := 0; hour < 11; hour++ {
		server.tick(now.Add(time.Duration(hour) * time.Hour))
	}

	if server.calls.count() != 0 {
		t.Errorf("restic ran between backups: %v", server.calls.seen)
	}
	if !server.lastChecked().Equal(checked) {
		t.Error("the destination was logged in to between backups")
	}
}

// The lock is looked for before the backups are queued, because a queued backup
// counts as work in flight and locks are not touched while there is any. The
// rest waits for the last backup to finish.
func TestUpkeepComesBeforeAndAfterTheBackups(t *testing.T) {
	now := onTheHour()
	cron := fmt.Sprintf("0 %d * * *", now.Hour())
	server := newUpkeepServer(t, cron, now.Add(-24*time.Hour), 24*time.Hour)
	checked, measured := server.lastChecked(), server.lastMeasured()

	server.tick(now)

	if server.calls.times("list") == 0 {
		t.Fatalf("no look for stale locks before the backups: %v", server.calls.seen)
	}
	if server.calls.jobs["list"] != 0 {
		t.Errorf("%d jobs were queued before the look for stale locks", server.calls.jobs["list"])
	}
	if !server.lastChecked().Equal(checked) || !server.lastMeasured().Equal(measured) {
		t.Fatal("the post-tasks ran while the backup was still to come")
	}

	if finished := server.finishBackups(); finished != 1 {
		t.Fatalf("finished %d backups, want 1", finished)
	}
	for minute := 1; minute < 4; minute++ {
		server.tick(now.Add(time.Duration(minute) * time.Minute))
	}

	if !server.lastChecked().After(checked) {
		t.Error("the destination was not checked after the backup")
	}
	if !server.lastMeasured().After(measured) {
		t.Error("the repository was not counted after the backup")
	}
}

// A schedule that fires every hour does not bring the hourly logins back.
func TestAnHourlyScheduleHasItsUpkeepOnceADay(t *testing.T) {
	now := onTheHour()
	server := newUpkeepServer(t, "0 * * * *", now.Add(-time.Hour), 24*time.Hour)
	checked := server.lastChecked()

	server.tick(now)
	server.finishBackups()
	for minute := 1; minute < 4; minute++ {
		server.tick(now.Add(time.Duration(minute) * time.Minute))
	}
	first := server.calls.count()
	if first == 0 || !server.lastChecked().After(checked) {
		t.Fatal("no upkeep after the first backup")
	}
	checked = server.lastChecked()

	for hour := 1; hour < 10; hour++ {
		at := now.Add(time.Duration(hour) * time.Hour)
		server.tick(at)
		server.finishBackups()
		server.tick(at.Add(time.Minute))
	}

	if server.calls.count() != first {
		t.Errorf("restic ran %d more times in ten hours", server.calls.count()-first)
	}
	if !server.lastChecked().Equal(checked) {
		t.Error("the destination was logged in to again inside the day")
	}
}

// A server with no schedule running has nothing to follow, and still has to
// know about its destinations. It looks when it has waited long enough, once.
func TestAServerWithoutBackupsStillLooksOnceADay(t *testing.T) {
	now := time.Now().UTC()
	server := newUpkeepServer(t, neverFiresSoon, now.Add(-31*time.Hour), 31*time.Hour)
	checked := server.lastChecked()

	server.tick(now)
	if !server.lastChecked().After(checked) {
		t.Fatal("a server with no backups never looked at its destination")
	}
	if server.calls.times("list") == 0 {
		t.Error("a server with no pre-task never looked for stale locks")
	}
	for minute := 1; minute < 4; minute++ {
		server.tick(now.Add(time.Duration(minute) * time.Minute))
	}
	count := server.calls.count()

	server.tick(now.Add(time.Hour))
	if server.calls.count() != count {
		t.Errorf("it looked again an hour later: %v", server.calls.seen[count:])
	}
}

// A schedule can fire every night and still give the upkeep nothing to follow:
// it found no accounts, or the agent restarted during the run. The firing must
// not be taken for upkeep having happened.
func TestAFiringWithNothingToFollowDoesNotStopTheUpkeep(t *testing.T) {
	now := time.Now().UTC()
	server := newUpkeepServer(t, neverFiresSoon, now.Add(-time.Hour), 31*time.Hour)
	checked := server.lastChecked()

	server.tick(now)

	if !server.lastChecked().After(checked) {
		t.Error("a schedule that keeps firing stopped the destination being checked")
	}
}

// A job that never finishes holds the repository's upkeep back, and must not
// also stop anyone finding out that the destination has gone.
func TestAWedgedJobDoesNotStopTheDestinationCheck(t *testing.T) {
	now := time.Now().UTC()
	server := newUpkeepServer(t, neverFiresSoon, now.Add(-time.Hour), 31*time.Hour)
	checked := server.lastChecked()
	if _, err := server.store.PutJob(nodestore.Job{Account: "wedged", Status: job.StatusRunning}); err != nil {
		t.Fatal(err)
	}

	server.tick(now)

	if !server.lastChecked().After(checked) {
		t.Error("a wedged job stopped the destination being checked")
	}
	if server.calls.count() != 0 {
		t.Errorf("restic ran beside a job: %v", server.calls.seen)
	}
}

// A look for locks that did not get an answer has not happened. The next
// schedule to fire asks again rather than waiting out the day.
func TestALockLookThatFailedIsTriedAgainAtTheNextFiring(t *testing.T) {
	now := onTheHour()
	server := newUpkeepServer(t, "0 * * * *", now.Add(-time.Hour), 12*time.Hour)
	server.calls.failList = true

	server.tick(now)
	first := server.calls.times("list")
	if first == 0 {
		t.Fatal("no look for stale locks")
	}
	server.finishBackups()
	server.tick(now.Add(time.Hour))

	if server.calls.times("list") <= first {
		t.Error("a look that failed was not tried again at the next firing")
	}
}
