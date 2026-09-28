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

const staleLockID = "553a8a7a00000000000000000000000000000000000000000000000000000001"

// lockedRestic stands in for a repository that carries one lock left by a
// backup that was killed, and refuses a retention plan until that lock
// has been removed.
type lockedRestic struct {
	mu       sync.Mutex
	lockedAt time.Time
	unlocked bool
	ran      []string
}

func (l *lockedRestic) Exec(_ context.Context, cmd resticrun.Command) (resticrun.CommandResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	line := strings.Join(cmd.Args, " ")
	l.ran = append(l.ran, line)
	switch {
	case strings.Contains(line, "list locks"):
		if l.unlocked {
			return resticrun.CommandResult{}, nil
		}
		return resticrun.CommandResult{Stdout: []byte(staleLockID + "\n")}, nil
	case strings.Contains(line, "cat lock"):
		return resticrun.CommandResult{Stdout: []byte(`{"time":"` +
			l.lockedAt.Format(time.RFC3339) + `","exclusive":false,` +
			`"hostname":"uscp","username":"root","pid":338155}`)}, nil
	case strings.HasSuffix(line, "unlock"):
		l.unlocked = true
		return resticrun.CommandResult{}, nil
	case strings.Contains(line, "forget"):
		if !l.unlocked {
			return resticrun.CommandResult{ExitCode: 11, Stderr: []byte(
				`{"message_type":"exit_error","code":11,"message":"unable to create lock ` +
					`in backend: repository is already locked by PID 338155 on uscp"}`)}, nil
		}
		return resticrun.CommandResult{Stdout: []byte("[]")}, nil
	}
	return resticrun.CommandResult{}, nil
}

func (l *lockedRestic) ranCommand(part string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.ran {
		if strings.Contains(line, part) {
			return true
		}
	}
	return false
}

func lockedEngine(t *testing.T, restic *lockedRestic) (*node.Engine, *nodestore.Store, nodestore.Repository) {
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
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	engine := newEngineWithExec(t, store, root, restic)
	repo := attachedRepository(t, store, engine)
	if _, err := store.PutPolicy(nodestore.Policy{
		Name: "Nightly", ScheduleCron: "0 2 * * *", Enabled: true,
		RepositoryIDs: []string{repo.ID},
		Retention:     nodestore.Retention{KeepDaily: 7},
	}); err != nil {
		t.Fatal(err)
	}
	return engine, store, repo
}

// The finding from the fleet check of 2026-09-22: a backup killed on
// 2026-09-09 left its lock on a repository, and retention failed on it
// every night for nineteen days with nobody the wiser.
func TestALockLeftByAKilledBackupIsRemovedAndThePlanIsTaken(t *testing.T) {
	restic := &lockedRestic{lockedAt: time.Now().Add(-19 * 24 * time.Hour)}
	engine, store, repo := lockedEngine(t, restic)

	if _, err := engine.PlanRetention(t.Context(), repo.ID); err != nil {
		t.Fatalf("the plan still failed on a lock nothing holds: %v", err)
	}
	if !restic.ranCommand("unlock") {
		t.Error("restic was never asked to remove the stale lock")
	}
	after, err := store.Repository(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Retention.PlannedAt == nil || after.Retention.LastError != "" {
		t.Errorf("retention = %+v", after.Retention)
	}
}

// A lock that is minutes old and was taken on another machine may be
// somebody working. It stays, and the failure is reported as it was.
func TestALockSomethingMayStillHoldIsLeftAlone(t *testing.T) {
	restic := &lockedRestic{lockedAt: time.Now().Add(-2 * time.Minute)}
	engine, store, repo := lockedEngine(t, restic)

	if _, err := engine.PlanRetention(t.Context(), repo.ID); err == nil {
		t.Fatal("a plan was taken past a lock that may be held")
	}
	if restic.ranCommand("unlock") {
		t.Error("a lock that is two minutes old was removed")
	}
	after, err := store.Repository(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(after.Retention.LastError, "locked") {
		t.Errorf("the page would not say why: %q", after.Retention.LastError)
	}
}

// While this server is backing something up there is a lock that is its
// own, and nothing is removed until that has finished.
func TestNoLockIsRemovedWhileABackupIsRunning(t *testing.T) {
	restic := &lockedRestic{lockedAt: time.Now().Add(-19 * 24 * time.Hour)}
	engine, store, repo := lockedEngine(t, restic)
	if _, err := store.PutJob(nodestore.Job{Account: "alice", Status: job.StatusRunning}); err != nil {
		t.Fatal(err)
	}

	if _, err := engine.ClearStaleLocks(t.Context(), repo.ID); err == nil {
		t.Fatal("locks were removed while a backup was running")
	}
	if restic.ranCommand("unlock") {
		t.Error("restic was asked to unlock under a running backup")
	}
}

// A plan taken before the repository stopped answering describes a
// repository that has since had every night added to it.
func TestAPlanOlderThanTheLastFailureIsNotOfferedForApproval(t *testing.T) {
	restic := &lockedRestic{lockedAt: time.Now().Add(-2 * time.Minute)}
	engine, _, repo := lockedEngine(t, restic)
	keeps := nodestore.Retention{KeepDaily: 7}

	planned := time.Now().Add(-20 * 24 * time.Hour)
	failed := time.Now().Add(-time.Hour)
	repo.Retention = nodestore.RetentionState{PlannedAt: &planned, PlannedKeeps: keeps}
	if !engine.PlanApprovable(repo, keeps) {
		t.Fatal("a plan with no failure after it was not approvable")
	}
	repo.Retention.AttemptedAt, repo.Retention.LastError = &failed, "the repository is locked"
	if engine.PlanApprovable(repo, keeps) {
		t.Error("a plan three weeks older than the last failure was offered for approval")
	}
}

// What happened on 182.54.236.10 the evening the fix was installed: the
// hourly look removed three locks, and the page went on saying the
// repository was locked, because that was the last thing retention had
// said and it would not be asked again until the next night.
func TestThePlanIsTakenAgainOnceTheSweepHasRemovedTheLocks(t *testing.T) {
	for name, gone := range map[string]bool{
		"the sweep removes the lock":        false,
		"the lock had been removed already": true,
	} {
		restic := &lockedRestic{lockedAt: time.Now().Add(-19 * 24 * time.Hour), unlocked: gone}
		engine, store, repo := lockedEngine(t, restic)
		made := time.Now().Add(-30 * 24 * time.Hour).UTC()
		failed := time.Now().Add(-time.Hour).UTC()
		repo.InitialisedAt = &made
		repo.Retention = nodestore.RetentionState{AttemptedAt: &failed,
			LastError: "resticrun: the repository is locked: restic exited 11"}
		if _, err := store.PutRepository(repo); err != nil {
			t.Fatal(err)
		}

		engine.SweepLocksForTest(t.Context())

		after, err := store.Repository(repo.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.Retention.LastError != "" || after.Retention.PlannedAt == nil ||
			!after.Retention.PlannedAt.After(failed) {
			t.Errorf("%s, and the page still says: %+v", name, after.Retention)
		}
	}
}
