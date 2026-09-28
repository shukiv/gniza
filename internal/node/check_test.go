package node_test

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/node"
	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/resticrun"
)

// checkedRestic answers "restic check" the way it is told to, and
// remembers what it was asked.
type checkedRestic struct {
	mu     sync.Mutex
	answer resticrun.CommandResult
	ran    []string
}

func (c *checkedRestic) Exec(_ context.Context, cmd resticrun.Command) (resticrun.CommandResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	line := strings.Join(cmd.Args, " ")
	c.ran = append(c.ran, line)
	if strings.Contains(line, "check") {
		return c.answer, nil
	}
	return resticrun.CommandResult{}, nil
}

func (c *checkedRestic) set(answer resticrun.CommandResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.answer = answer
}

func checkedEngine(t *testing.T, restic resticrun.Execer) (*node.Engine, *nodestore.Store, nodestore.Repository) {
	t.Helper()
	root := t.TempDir()
	store, err := nodestore.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	settings := nodestore.DefaultSettings()
	settings.StagingRoot = filepath.Join(root, "staging")
	settings.ResticCache = filepath.Join(root, "cache")
	settings.ConfigDir = filepath.Join(root, "config")
	settings.Hostname = "test.example.com"
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	engine := newEngineWithExec(t, store, root, restic)
	return engine, store, attachedRepository(t, store, engine)
}

// The finding from the fleet check of 2026-09-22: nothing on any server
// had ever asked restic whether a repository could be read back. "Last
// checked" on the page was a login to the destination.
func TestACheckReadsTheRepositoryBackAndRecordsThatItPassed(t *testing.T) {
	restic := &checkedRestic{}
	engine, store, repo := checkedEngine(t, restic)

	engine.CheckForTest(t.Context(), repo.ID)

	after, err := store.Repository(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Check.Known() || !after.Check.Passed || after.Check.SubsetPercent != 10 {
		t.Fatalf("check = %+v", after.Check)
	}
	var asked string
	for _, line := range restic.ran {
		if strings.Contains(line, "check") {
			asked = line
		}
	}
	if !strings.Contains(asked, "--read-data-subset 10%") {
		t.Errorf("the stored data was not read back: %q", asked)
	}
}

// A repository restic finds fault with is recorded as one that did not
// pass, in restic's words, and somebody is told -- once.
func TestADamagedRepositoryIsRecordedAndReportedOnce(t *testing.T) {
	restic := &checkedRestic{}
	engine, store, repo := checkedEngine(t, restic)
	collected := newSink(t)
	if _, err := engine.SaveChannel(nodestore.Channel{
		Name: "Sink", Kind: "webhook", Enabled: true,
		Config: map[string]string{"url": collected.URL},
	}, nil); err != nil {
		t.Fatal(err)
	}
	restic.set(resticrun.CommandResult{ExitCode: 1, Stderr: []byte(
		"pack 3f2a1c9e: not referenced in any index\n" +
			"error for tree 91ab03ff:\n  tree 91ab03ff: file \"wp-config.php\" blob 77aa00 not found in index\n" +
			"Fatal: repository contains errors\n")})

	engine.CheckForTest(t.Context(), repo.ID)
	engine.CheckForTest(t.Context(), repo.ID)

	after, err := store.Repository(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Check.Known() || after.Check.Passed {
		t.Fatalf("a damaged repository was not recorded as one: %+v", after.Check)
	}
	if !strings.Contains(after.Check.Problem, "not found in index") {
		t.Errorf("what restic found is not on record: %q", after.Check.Problem)
	}
	if sent := collected.events("check_failed"); len(sent) != 1 {
		t.Errorf("told %d times, want once", len(sent))
	}

	restic.set(resticrun.CommandResult{})
	engine.CheckForTest(t.Context(), repo.ID)
	if sent := collected.events("check_failed"); len(sent) != 2 {
		t.Errorf("nobody was told that it passes again: %d messages", len(sent))
	}
}

// A destination that does not answer says nothing about the backups in
// it. What the last check found still stands.
func TestACheckThatCouldNotRunLeavesTheLastResultStanding(t *testing.T) {
	restic := &checkedRestic{}
	engine, store, repo := checkedEngine(t, restic)
	engine.CheckForTest(t.Context(), repo.ID)
	passed, err := store.Repository(repo.ID)
	if err != nil {
		t.Fatal(err)
	}

	restic.set(resticrun.CommandResult{ExitCode: 1, Stderr: []byte(
		"Fatal: unable to open repository at sftp:backup:/srv: dial tcp: connection refused\n")})
	engine.CheckForTest(t.Context(), repo.ID)

	after, err := store.Repository(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Check.Passed || !after.Check.CheckedAt.Equal(*passed.Check.CheckedAt) {
		t.Errorf("an unreachable destination overwrote a check that passed: %+v", after.Check)
	}
	if after.Check.AttemptedAt == nil || !strings.Contains(after.Check.LastError, "connection refused") {
		t.Errorf("why it could not be checked is not on record: %+v", after.Check)
	}
}

func TestWhenARepositoryIsCheckedAgain(t *testing.T) {
	now := time.Date(2026, 9, 28, 16, 0, 0, 0, time.UTC)
	at := func(ago time.Duration) *time.Time { stamp := now.Add(-ago); return &stamp }
	day := 24 * time.Hour
	for _, c := range []struct {
		name  string
		repo  nodestore.Repository
		quiet bool
		want  bool
	}{
		{"nothing written there yet", nodestore.Repository{}, true, false},
		{"made an hour ago", nodestore.Repository{InitialisedAt: at(time.Hour)}, true, false},
		{"made two days ago, never checked", nodestore.Repository{InitialisedAt: at(2 * day)}, true, true},
		{"checked yesterday", nodestore.Repository{InitialisedAt: at(30 * day),
			Check: nodestore.RepositoryCheck{CheckedAt: at(day), Passed: true}}, true, false},
		{"checked eight days ago", nodestore.Repository{InitialisedAt: at(30 * day),
			Check: nodestore.RepositoryCheck{CheckedAt: at(8 * day), Passed: true}}, true, true},
		{"due, and a schedule fires in an hour", nodestore.Repository{InitialisedAt: at(30 * day),
			Check: nodestore.RepositoryCheck{CheckedAt: at(8 * day), Passed: true}}, false, false},
		{"three weeks overdue on a server that is never quiet", nodestore.Repository{InitialisedAt: at(90 * day),
			Check: nodestore.RepositoryCheck{CheckedAt: at(22 * day), Passed: true}}, false, true},
		{"could not be run an hour ago", nodestore.Repository{InitialisedAt: at(30 * day),
			Check: nodestore.RepositoryCheck{AttemptedAt: at(time.Hour), LastError: "no route"}}, true, false},
	} {
		if got := node.CheckDueForTest(c.repo, now, c.quiet); got != c.want {
			t.Errorf("%s: due = %v, want %v", c.name, got, c.want)
		}
	}
}

// A check holds the repository to itself, so what is queued waits for it
// rather than failing on its lock.
func TestQueuedWorkWaitsForACheckToFinish(t *testing.T) {
	engine, store, _ := checkedEngine(t, &checkedRestic{})
	queued, err := store.PutJob(nodestore.Job{Account: "alice", Status: job.StatusPending})
	if err != nil {
		t.Fatal(err)
	}

	engine.SetCheckingForTest(true)
	did, err := engine.RunOnce(t.Context())
	if err != nil || did {
		t.Fatalf("work was started under a check: did=%v err=%v", did, err)
	}
	after, err := store.Job(queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != job.StatusPending {
		t.Errorf("the queued backup is now %s", after.Status)
	}
}

// The sweep is what removes a lock a killed prune or check left, which
// would otherwise stop every backup from the next schedule on.
func TestTheHourlySweepRemovesALockNothingHolds(t *testing.T) {
	restic := &lockedRestic{lockedAt: time.Now().Add(-3 * time.Hour)}
	engine, _, _ := lockedEngine(t, restic)

	engine.SweepLocksForTest(t.Context())
	if !restic.ranCommand("unlock") {
		t.Error("a lock three hours old was left on the repository")
	}
}
