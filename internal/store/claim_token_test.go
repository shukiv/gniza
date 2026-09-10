package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/store"
)

// A job whose lease ran out goes back on the queue and is claimed again.
// Both attempts belong to the same server and name the same job, so the
// only thing that tells them apart is the token the claim generated. The
// late one must not be able to close out the one that is running.

func TestALateBackupReportCannotFinishTheNextAttempt(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	jobID, err := f.db.CreateJob(ctx, f.accountID, f.policyID)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	abandoned, err := f.db.ClaimNextJob(ctx, f.serverID, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	reclaimed, err := f.db.ReclaimExpiredLeases(ctx)
	if err != nil {
		t.Fatalf("ReclaimExpiredLeases: %v", err)
	}
	if reclaimed != 1 {
		t.Fatalf("reclaimed %d jobs, want the one whose lease expired", reclaimed)
	}

	running, err := f.db.ClaimNextJob(ctx, f.serverID, time.Minute)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if running.ClaimToken == "" || running.ClaimToken == abandoned.ClaimToken {
		t.Fatalf("the second attempt reuses the first attempt's token %q", running.ClaimToken)
	}

	// The abandoned agent finishes late and says the backup failed.
	if _, err := f.db.ApplyReport(ctx, f.serverID, jobID, abandoned.ClaimToken,
		[]store.TargetReport{
			{RepositoryID: f.repoA.ID, Status: job.TargetFailed, Error: "connection reset"},
			{RepositoryID: f.repoB.ID, Status: job.TargetFailed, Error: "connection reset"},
		}, store.JobOutcome{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the abandoned attempt reported and got %v, want ErrNotFound", err)
	}
	status, err := f.db.JobStatus(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if status != job.StatusRunning {
		t.Errorf("the job is %q, want it still running for the attempt that holds it", status)
	}

	// And the attempt that holds the job can still report its own result.
	status, err = f.db.ApplyReport(ctx, f.serverID, jobID, running.ClaimToken,
		[]store.TargetReport{
			{RepositoryID: f.repoA.ID, Status: job.TargetSuccess, SnapshotID: "aaa"},
			{RepositoryID: f.repoB.ID, Status: job.TargetSuccess, SnapshotID: "bbb"},
		}, store.JobOutcome{})
	if err != nil {
		t.Fatalf("the running attempt could not report: %v", err)
	}
	if status != job.StatusSuccess {
		t.Errorf("status = %q, want success", status)
	}
}

func TestALateRestoreReportCannotFinishTheNextAttempt(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if err := f.db.MarkRepositoryInitialised(ctx, f.repoA.ID); err != nil {
		t.Fatalf("MarkRepositoryInitialised: %v", err)
	}
	// A restore that only produces a copy, because that is the one a lost
	// lease still puts back on the queue: a restore writing into a live
	// account is stopped instead of retried, which is
	// TestARestoreThatWritesIntoTheAccountIsNotRequeuedWhenItsLeaseRunsOut.
	// Two attempts at the same restore are what this is about either way.
	jobID, err := f.db.CreateRestore(ctx, store.RestoreRequest{
		AccountID: f.accountID, RepositoryID: f.repoA.ID,
		SnapshotID: "40dc15203b1cf9",
	})
	if err != nil {
		t.Fatalf("CreateRestore: %v", err)
	}

	abandoned, err := f.db.ClaimNextRestore(ctx, f.serverID, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := f.db.ReclaimExpiredRestoreLeases(ctx); err != nil {
		t.Fatalf("ReclaimExpiredRestoreLeases: %v", err)
	}
	running, err := f.db.ClaimNextRestore(ctx, f.serverID, time.Minute)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if running.ClaimToken == "" || running.ClaimToken == abandoned.ClaimToken {
		t.Fatalf("the second attempt reuses the first attempt's token %q", running.ClaimToken)
	}

	// This is the one that matters: a restore reported successful stops
	// being queued, and the second attempt is still running.
	if err := f.db.ApplyRestoreReport(ctx, f.serverID, jobID, abandoned.ClaimToken,
		store.RestoreOutcome{Status: job.StatusSuccess, BytesRestored: 4096},
	); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the abandoned attempt reported and got %v, want ErrNotFound", err)
	}
	stored, err := f.db.RestoreByID(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != job.StatusRunning {
		t.Errorf("the restore is %q, want it still running", stored.Status)
	}

	if err := f.db.ApplyRestoreReport(ctx, f.serverID, jobID, running.ClaimToken,
		store.RestoreOutcome{Status: job.StatusSuccess, BytesRestored: 8192},
	); err != nil {
		t.Fatalf("the running attempt could not report: %v", err)
	}
	stored, err = f.db.RestoreByID(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.BytesRestored != 8192 {
		t.Errorf("restored %d bytes, want the running attempt's 8192", stored.BytesRestored)
	}
}

// A report that carries no token, or one that is not a token at all, is
// refused by the controller before it reaches SQL: a uuid cast on a
// arbitrary string is a database error rather than an answer.
func TestAReportWithoutATokenIsRefused(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	jobID, err := f.db.CreateJob(ctx, f.accountID, f.policyID)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if _, err := f.db.ClaimNextJob(ctx, f.serverID, time.Minute); err != nil {
		t.Fatalf("ClaimNextJob: %v", err)
	}
	for _, token := range []string{"", "not-a-token", "40dc1520"} {
		if _, err := f.db.ApplyReport(ctx, f.serverID, jobID, token, nil,
			store.JobOutcome{StagingError: "staging: need 8192 bytes free, have 512"}); err == nil {
			t.Errorf("token %q was accepted", token)
		}
	}
	status, err := f.db.JobStatus(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if status != job.StatusRunning {
		t.Errorf("the job is %q, want it untouched", status)
	}
}
