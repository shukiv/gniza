package nodestore_test

import (
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/nodestore"
)

// TestClearingTheHistoryLeavesWhatIsKeptAndWhatIsRunning: the store
// removes the finished runs the caller does not hold on to, never one
// still going, and a snapshot marked as having unreadable files keeps its
// mark once the run that made it is gone.
func TestClearingTheHistoryLeavesWhatIsKeptAndWhatIsRunning(t *testing.T) {
	store := newStore(t)
	put := func(account string, status job.Status, target nodestore.JobTarget) nodestore.Job {
		stored, err := store.PutJob(nodestore.Job{Account: account, Status: status, Targets: []nodestore.JobTarget{target}})
		if err != nil {
			t.Fatal(err)
		}
		return stored
	}
	kept := put("alice", job.StatusSuccess, nodestore.JobTarget{RepositoryID: "repo", Status: job.TargetSuccess, SnapshotID: "aaa"})
	holed := put("alice", job.StatusFailed, nodestore.JobTarget{RepositoryID: "repo", Status: job.TargetFailed, SnapshotID: "bbb", Incomplete: true})
	put("bob", job.StatusFailed, nodestore.JobTarget{RepositoryID: "repo", Status: job.TargetFailed})
	running := put("carol", job.StatusRunning, nodestore.JobTarget{})

	removed, err := store.ClearJobs(func(stored nodestore.Job) bool { return stored.ID == kept.ID })
	if err != nil || len(removed) != 2 {
		t.Fatalf("removed %d, %v; want the two finished runs nobody held on to", len(removed), err)
	}
	left, _ := store.Jobs(0)
	ids := map[string]bool{}
	for _, stored := range left {
		ids[stored.ID] = true
	}
	if len(left) != 2 || !ids[kept.ID] || !ids[running.ID] || ids[holed.ID] {
		t.Errorf("left = %v; want the run held on to and the one still going", ids)
	}
	marks, err := store.IncompleteSnapshotMarks("repo")
	if err != nil || !marks["bbb"] || marks["aaa"] || len(marks) != 1 {
		t.Errorf("marks = %v, %v; want bbb alone", marks, err)
	}
	if other, _ := store.IncompleteSnapshotMarks("rep"); len(other) != 0 {
		t.Errorf("a repository whose id starts the same way was given marks: %v", other)
	}

	// Restores: the finished ones nobody holds on to, and every event.
	done := time.Now().UTC()
	finished, _ := store.PutRestore(nodestore.Restore{Account: "alice", Status: job.StatusSuccess, FinishedAt: &done})
	going, _ := store.PutRestore(nodestore.Restore{Account: "alice", Status: job.StatusRunning})
	if n, err := store.ClearRestores(func(nodestore.Restore) bool { return false }); err != nil || n != 1 {
		t.Errorf("cleared %d restores, %v", n, err)
	}
	if _, err := store.Restore(finished.ID); err == nil {
		t.Error("the finished restore is still there")
	}
	if _, err := store.Restore(going.ID); err != nil {
		t.Errorf("the restore still going was removed: %v", err)
	}
	if _, err := store.PutLifecycleEvent(nodestore.LifecycleEvent{Event: "create", Account: "alice", OK: true}); err != nil {
		t.Fatal(err)
	}
	if n, err := store.ClearLifecycle(); err != nil || n != 1 {
		t.Errorf("cleared %d events, %v", n, err)
	}
	if events, _ := store.LifecycleEvents(0); len(events) != 0 {
		t.Errorf("events left: %v", events)
	}
}
