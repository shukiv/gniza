package store_test

import (
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/store"
)

func TestIncompleteFleetReportsAreNotSuccessfulCopies(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	id, err := f.db.CreateJob(ctx, f.accountID, f.policyID)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := f.db.ClaimNextJob(ctx, f.serverID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	status, err := f.db.ApplyReport(ctx, f.serverID, id, claimed.ClaimToken, []store.TargetReport{
		{RepositoryID: f.repoA.ID, Status: job.TargetSuccess, SnapshotID: "aaaaaaaaaaaaaaaa", Incomplete: true},
		{RepositoryID: f.repoB.ID, Status: job.TargetFailed},
	}, store.JobOutcome{})
	if err != nil {
		t.Fatal(err)
	}
	if status != job.StatusFailed {
		t.Fatalf("a fleet with no complete copies reported %s", status)
	}
	incomplete, err := f.db.IncompleteSnapshotIDs(ctx, f.repoA.ID)
	if err != nil || !incomplete["aaaaaaaaaaaaaaaa"] {
		t.Fatalf("read-error evidence was lost: %v, %v", incomplete, err)
	}
}
