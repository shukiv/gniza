package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/store"
)

// TestARetrysAccountOfThePayloadReplacesTheLastOnes checks that the
// attempt which just reported is the one on the job. A retry that found
// the database it lost last night must not leave the account still
// listed as missing it: what these lines are for is telling an operator
// what is wrong now, not what was wrong once.
func TestARetrysAccountOfThePayloadReplacesTheLastOnes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	jobID, err := f.db.CreateJob(ctx, f.accountID, f.policyID)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	first, err := f.db.ClaimNextJob(ctx, f.serverID, time.Minute)
	if err != nil {
		t.Fatalf("ClaimNextJob: %v", err)
	}
	if _, err := f.db.ApplyReport(ctx, f.serverID, jobID, first.ClaimToken,
		[]store.TargetReport{
			{RepositoryID: f.repoA.ID, Status: job.TargetSuccess, SnapshotID: "aaa"},
			{RepositoryID: f.repoB.ID, Status: job.TargetFailed, Error: "timeout"},
		}, store.JobOutcome{
			Missing:  []string{"database customer1_shop -- Lost connection (2013)"},
			Warnings: []string{"1 file is owned by another account"},
		}); err != nil {
		t.Fatalf("ApplyReport: %v", err)
	}

	notes, err := f.db.JobNotes(ctx, jobID)
	if err != nil {
		t.Fatalf("JobNotes: %v", err)
	}
	if len(notes.Missing) != 1 || len(notes.Warnings) != 1 {
		t.Fatalf("the first attempt recorded missing=%v warnings=%v", notes.Missing, notes.Warnings)
	}

	// The second attempt found everything.
	if _, err := f.db.Pool().Exec(ctx,
		`UPDATE backup_jobs SET status = 'pending' WHERE id = $1`, jobID); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	second, err := f.db.ClaimNextJob(ctx, f.serverID, time.Minute)
	if err != nil {
		t.Fatalf("ClaimNextJob: %v", err)
	}
	if _, err := f.db.ApplyReport(ctx, f.serverID, jobID, second.ClaimToken,
		[]store.TargetReport{
			{RepositoryID: f.repoB.ID, Status: job.TargetSuccess, SnapshotID: "bbb"},
		}, store.JobOutcome{}); err != nil {
		t.Fatalf("second ApplyReport: %v", err)
	}

	notes, err = f.db.JobNotes(ctx, jobID)
	if err != nil {
		t.Fatalf("JobNotes: %v", err)
	}
	if len(notes.Missing) != 0 || len(notes.Warnings) != 0 {
		t.Errorf("a complete retry still reports missing=%v warnings=%v",
			notes.Missing, notes.Warnings)
	}
}
