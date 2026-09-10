//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/agent"
	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/pkgacct"
	"github.com/shukiv/gniza/internal/protocol"
	"github.com/shukiv/gniza/internal/resticrun"
	"github.com/shukiv/gniza/internal/store"
)

// TestBackupEndToEnd drives the whole system: the maintenance runner
// provisions two repositories, the controller queues and dispatches a job,
// the agent stages a synthetic account and uploads it to both destinations,
// and the results come back through the API.
func TestBackupEndToEnd(t *testing.T) {
	h := newHarness(t)
	ctx := h.ctx

	if err := h.worker.Enrol(ctx); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	server, err := h.db.ServerByID(ctx, h.serverID)
	if err != nil {
		t.Fatalf("read server: %v", err)
	}
	if server.Status != "active" {
		t.Errorf("server status = %q, want active after enrolment", server.Status)
	}
	if server.PkgacctFlags["nocompress"] == "" {
		t.Errorf("enrolment did not record probed pkgacct flags: %v", server.PkgacctFlags)
	}

	provisioned, err := h.maintenance.ProvisionPending(ctx)
	if err != nil {
		t.Fatalf("provision repositories: %v", err)
	}
	if provisioned != 2 {
		t.Fatalf("provisioned %d repositories, want 2", provisioned)
	}

	// Both repositories belong to the same server, so the second must have
	// copied the first's chunker parameters. They are fixed at creation, so
	// this is the only moment it can be got right.
	if got, want := chunkerPolynomial(t, h, h.restRepoID), chunkerPolynomial(t, h, h.localRepoID); got != want {
		t.Errorf("chunker polynomial %q on the second repository, want %q from the first", got, want)
	}

	jobID, err := h.db.CreateJob(ctx, h.accountID, h.policyID)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	report := runOneJob(t, h, jobID)
	if report.StagingError != "" {
		t.Fatalf("staging failed: %s", report.StagingError)
	}
	if len(report.Targets) != 2 {
		t.Fatalf("reported %d targets, want 2", len(report.Targets))
	}
	for _, target := range report.Targets {
		if target.Status != string(job.TargetSuccess) {
			t.Errorf("target %s: %s (%s)", target.RepositoryID, target.Status, target.Error)
			continue
		}
		if target.SnapshotID == "" {
			t.Errorf("target %s succeeded without a snapshot id", target.RepositoryID)
		}
		if target.BytesProcessed == 0 {
			t.Errorf("target %s processed no data", target.RepositoryID)
		}
	}

	status, err := h.db.JobStatus(ctx, jobID)
	if err != nil {
		t.Fatalf("read job status: %v", err)
	}
	if status != job.StatusSuccess {
		t.Fatalf("job status = %q, want success", status)
	}

	// The payload really is in both repositories, with the tags the agent
	// attached, verified by restic itself rather than by our own bookkeeping.
	for _, repoID := range []string{h.localRepoID, h.restRepoID} {
		snapshots := listSnapshots(t, h, repoID)
		if len(snapshots) != 1 {
			t.Fatalf("repository %s holds %d snapshots, want 1", repoID, len(snapshots))
		}
		if !hasTag(snapshots[0].Tags, "account:customer1") {
			t.Errorf("snapshot tags = %v, want account:customer1", snapshots[0].Tags)
		}
		if snapshots[0].Hostname != "cp01.example.com" {
			t.Errorf("snapshot hostname = %q", snapshots[0].Hostname)
		}
		// split mode: metadata, home directory and one file per database.
		if len(snapshots[0].Paths) < 3 {
			t.Errorf("snapshot paths = %v, want metadata, homedir and databases",
				snapshots[0].Paths)
		}
	}

	// restic's own integrity check, run the way the maintenance runner does.
	if err := h.maintenance.Check(ctx, h.localRepoID, 100); err != nil {
		t.Errorf("check local repository: %v", err)
	}
	if err := h.maintenance.Check(ctx, h.restRepoID, 100); err != nil {
		t.Errorf("check rest repository: %v", err)
	}
}

// TestSecondBackupDeduplicates is the reason split mode exists: an
// unchanged account must cost almost nothing on the second night.
func TestSecondBackupDeduplicates(t *testing.T) {
	h := newHarness(t)
	ctx := h.ctx

	if _, err := h.maintenance.ProvisionPending(ctx); err != nil {
		t.Fatalf("provision: %v", err)
	}

	first := runJobFor(t, h)
	second := runJobFor(t, h)

	firstAdded := bytesAdded(t, first, h.localRepoID)
	secondAdded := bytesAdded(t, second, h.localRepoID)
	processed := bytesProcessed(t, second, h.localRepoID)

	if firstAdded == 0 {
		t.Fatal("first backup added no data")
	}
	if processed == 0 {
		t.Fatal("second backup processed no data")
	}
	// The account did not change, so the second run should store a tiny
	// fraction of what it read. A compressed pkgacct archive would store
	// close to all of it, which is what this guards against.
	if secondAdded > processed/10 {
		t.Errorf("second backup added %d bytes of %d processed; deduplication is not working",
			secondAdded, processed)
	}
}

// TestAppendOnlyBlocksAgentDeletes proves the design's headline security
// property, and the deployment shape it requires.
//
// rest-server's --append-only is a property of the running process, not of
// a credential. A destination that agents reach through an append-only
// endpoint therefore needs a second, delete-capable endpoint over the same
// data directory for the maintenance runner, or nothing can ever prune it.
func TestAppendOnlyBlocksAgentDeletes(t *testing.T) {
	h := newHarness(t)
	ctx := h.ctx

	if _, err := h.maintenance.ProvisionPending(ctx); err != nil {
		t.Fatalf("provision: %v", err)
	}
	runJobFor(t, h)
	runJobFor(t, h)

	before := listSnapshots(t, h, h.restRepoID)
	if len(before) != 2 {
		t.Fatalf("rest repository holds %d snapshots, want 2", len(before))
	}

	// An attacker holding the agent's credentials, going through the
	// endpoint the agent uses.
	err := h.forgetAsAgent(t, h.restRepoID, resticrun.ForgetSpec{
		KeepLast: 1, GroupBy: "host,tags", Prune: true,
	})
	if err == nil {
		t.Fatal("the append-only endpoint accepted a delete")
	}
	if !strings.Contains(err.Error(), "403") && !strings.Contains(strings.ToLower(err.Error()), "forbidden") {
		t.Errorf("error = %v, want an HTTP 403 from rest-server", err)
	}
	if after := listSnapshots(t, h, h.restRepoID); len(after) != 2 {
		t.Errorf("rest repository holds %d snapshots after the attempted delete, want 2", len(after))
	}

	// The maintenance runner reaches the same storage through the
	// delete-capable endpoint, and must be able to prune.
	if err := h.maintenance.Forget(ctx, h.restRepoID, store.Retention{KeepLast: 1}, true); err != nil {
		t.Fatalf("forget through the maintenance endpoint: %v", err)
	}
	if remaining := listSnapshots(t, h, h.restRepoID); len(remaining) != 1 {
		t.Errorf("rest repository holds %d snapshots after retention, want 1", len(remaining))
	}

	// A destination with no append-only endpoint prunes directly.
	if err := h.maintenance.Forget(ctx, h.localRepoID, store.Retention{KeepLast: 1}, true); err != nil {
		t.Fatalf("forget on a delete-capable destination: %v", err)
	}
	if remaining := listSnapshots(t, h, h.localRepoID); len(remaining) != 1 {
		t.Errorf("local repository holds %d snapshots after retention, want 1", len(remaining))
	}
}

// TestRetentionGroupsByAccount checks the property that makes retention
// work at all: two runs of the same account must land in one restic
// forget group. Staging under the job id instead of the account, or
// tagging a snapshot with its job, would give every run its own group of
// one, and a group of one is never pruned.
func TestRetentionGroupsByAccount(t *testing.T) {
	h := newHarness(t)
	ctx := h.ctx

	if _, err := h.maintenance.ProvisionPending(ctx); err != nil {
		t.Fatalf("provision: %v", err)
	}
	runJobFor(t, h)
	runJobFor(t, h)

	snapshots := listSnapshots(t, h, h.localRepoID)
	if len(snapshots) != 2 {
		t.Fatalf("expected 2 snapshots, got %d", len(snapshots))
	}
	if !equalStrings(snapshots[0].Paths, snapshots[1].Paths) {
		t.Errorf("snapshot paths differ between runs:\n  %v\n  %v",
			snapshots[0].Paths, snapshots[1].Paths)
	}
	if !equalStrings(snapshots[0].Tags, snapshots[1].Tags) {
		t.Errorf("snapshot tags differ between runs:\n  %v\n  %v",
			snapshots[0].Tags, snapshots[1].Tags)
	}
	for _, tag := range snapshots[0].Tags {
		if strings.HasPrefix(tag, "job:") {
			t.Errorf("snapshot carries a per-job tag %q, which would exempt it from retention", tag)
		}
	}

	if err := h.maintenance.Forget(ctx, h.localRepoID, store.Retention{KeepLast: 1}, true); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if remaining := listSnapshots(t, h, h.localRepoID); len(remaining) != 1 {
		t.Errorf("retention kept %d snapshots, want 1", len(remaining))
	}
}

// TestUnregisteredCertificateIsRejected checks that a certificate signed by
// the CA is not sufficient on its own: the fingerprint must be registered.
func TestUnregisteredCertificateIsRejected(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(h.ctx, 15*time.Second)
	defer cancel()

	_, err := h.unauthenticatedClient.NextWork(ctx)
	if err == nil {
		t.Fatal("a client with no certificate was served")
	}
}

// TestPartialFailureKeepsGoodCopies checks that one unreachable destination
// does not invalidate the copy that landed.
func TestPartialFailureKeepsGoodCopies(t *testing.T) {
	h := newHarness(t)
	ctx := h.ctx

	if _, err := h.maintenance.ProvisionPending(ctx); err != nil {
		t.Fatalf("provision: %v", err)
	}
	// Point the rest destination at a port nothing is listening on. The
	// local destination is untouched.
	if _, err := h.db.Pool().Exec(ctx, `
		UPDATE destinations
		   SET config = jsonb_set(config, '{base_url}', '"https://127.0.0.1:1"')
		 WHERE type = 'rest'`); err != nil {
		t.Fatalf("break rest destination: %v", err)
	}

	jobID, err := h.db.CreateJob(ctx, h.accountID, h.policyID)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	report := runOneJob(t, h, jobID)

	var succeeded, failed int
	for _, target := range report.Targets {
		switch target.Status {
		case string(job.TargetSuccess):
			succeeded++
		case string(job.TargetFailed):
			failed++
		}
	}
	if succeeded != 1 || failed != 1 {
		t.Fatalf("got %d successes and %d failures, want one of each", succeeded, failed)
	}

	status, err := h.db.JobStatus(ctx, jobID)
	if err != nil {
		t.Fatalf("read job status: %v", err)
	}
	// Two good copies out of three is a warning, not a lost backup.
	if status != job.StatusPartialSuccess {
		t.Errorf("job status = %q, want partial_success", status)
	}
	if snapshots := listSnapshots(t, h, h.localRepoID); len(snapshots) != 1 {
		t.Errorf("the reachable destination holds %d snapshots, want 1", len(snapshots))
	}
}

// TestStagingPreflightRefusesOversizedAccount checks that a job which
// cannot fit is refused before pkgacct fills the volume.
func TestStagingPreflightRefusesOversizedAccount(t *testing.T) {
	h := newHarness(t)
	ctx := h.ctx

	if _, err := h.maintenance.ProvisionPending(ctx); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := h.db.SetAccountSizeEstimate(ctx, h.accountID, 1<<62); err != nil {
		t.Fatalf("set size estimate: %v", err)
	}

	jobID, err := h.db.CreateJob(ctx, h.accountID, h.policyID)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	report := runOneJob(t, h, jobID)

	if report.StagingError == "" {
		t.Fatal("an account larger than the volume was staged anyway")
	}
	if !strings.Contains(report.StagingError, "not enough room") {
		t.Errorf("staging error = %q, want a space complaint", report.StagingError)
	}

	status, err := h.db.JobStatus(ctx, jobID)
	if err != nil {
		t.Fatalf("read job status: %v", err)
	}
	if status != job.StatusFailed {
		t.Errorf("job status = %q, want failed", status)
	}
	targets, err := h.db.JobTargets(ctx, jobID)
	if err != nil {
		t.Fatalf("read job targets: %v", err)
	}
	for _, target := range targets {
		if target.Status != job.TargetFailed {
			t.Errorf("target %s = %q, want failed", target.RepositoryID, target.Status)
		}
	}
}

// runOneJob polls for the queued job, runs it, and reports the outcome
// through the real API.
func runOneJob(t *testing.T, h *harness, wantJobID string) protocol.JobReport {
	t.Helper()

	ctx, cancel := context.WithTimeout(h.ctx, 3*time.Minute)
	defer cancel()

	work, err := h.agentClient.NextWork(ctx)
	if err != nil {
		t.Fatalf("poll for work: %v", err)
	}
	if work.Kind != protocol.KindBackup {
		t.Fatalf("received %s work, want a backup", work.Kind)
	}
	assignment := *work.Backup
	if wantJobID != "" && assignment.JobID != wantJobID {
		t.Fatalf("received job %s, want %s", assignment.JobID, wantJobID)
	}
	if assignment.CPanelUser != "customer1" {
		t.Fatalf("assignment is for %q", assignment.CPanelUser)
	}

	report := h.worker.RunJob(ctx, assignment)

	// The agent reports on a fresh deadline, so a job that used its whole
	// budget can still say what happened.
	reportCtx, cancelReport := context.WithTimeout(h.ctx, 30*time.Second)
	defer cancelReport()
	if err := h.agentClient.Report(reportCtx, report); err != nil {
		t.Fatalf("report job: %v", err)
	}
	return report
}

// runJobFor queues and runs one backup, returning the report.
func runJobFor(t *testing.T, h *harness) protocol.JobReport {
	t.Helper()
	jobID, err := h.db.CreateJob(h.ctx, h.accountID, h.policyID)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	return runOneJob(t, h, jobID)
}

type snapshot struct {
	ID       string   `json:"id"`
	Tags     []string `json:"tags"`
	Paths    []string `json:"paths"`
	Hostname string   `json:"hostname"`
}

func listSnapshots(t *testing.T, h *harness, repositoryID string) []snapshot {
	t.Helper()
	output := h.resticIn(t, repositoryID, "snapshots", "--json")
	var snapshots []snapshot
	if err := json.Unmarshal(output, &snapshots); err != nil {
		t.Fatalf("decode snapshots: %v\n%s", err, output)
	}
	var payloads []snapshot
	for _, snapshot := range snapshots {
		if !hasTag(snapshot.Tags, resticrun.CompletionReceiptTag) {
			payloads = append(payloads, snapshot)
		}
	}
	return payloads
}

func chunkerPolynomial(t *testing.T, h *harness, repositoryID string) string {
	t.Helper()
	output := h.resticIn(t, repositoryID, "cat", "config")
	// "cat config" prints a banner line before the JSON body.
	start := strings.Index(string(output), "{")
	if start < 0 {
		t.Fatalf("no JSON in restic cat config output: %s", output)
	}
	var config struct {
		Version           int    `json:"version"`
		ChunkerPolynomial string `json:"chunker_polynomial"`
	}
	if err := json.Unmarshal(output[start:], &config); err != nil {
		t.Fatalf("decode repository config: %v\n%s", err, output)
	}
	if config.Version != 2 {
		t.Errorf("repository format version = %d, want 2 for compression support", config.Version)
	}
	return config.ChunkerPolynomial
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func hasTag(tags []string, want string) bool {
	for _, tag := range tags {
		if tag == want {
			return true
		}
	}
	return false
}

func bytesAdded(t *testing.T, report protocol.JobReport, repositoryID string) uint64 {
	t.Helper()
	return findTarget(t, report, repositoryID).BytesAdded
}

func bytesProcessed(t *testing.T, report protocol.JobReport, repositoryID string) uint64 {
	t.Helper()
	return findTarget(t, report, repositoryID).BytesProcessed
}

func findTarget(t *testing.T, report protocol.JobReport, repositoryID string) protocol.TargetReport {
	t.Helper()
	for _, target := range report.Targets {
		if target.RepositoryID == repositoryID {
			if target.Status != string(job.TargetSuccess) {
				t.Fatalf("target %s failed: %s", repositoryID, target.Error)
			}
			return target
		}
	}
	t.Fatalf("no report for repository %s", repositoryID)
	return protocol.TargetReport{}
}

var (
	_ = agent.ErrNoWork
	_ = resticrun.ErrNoSummary
)

// TestWhatTheBackupCouldNotTakeReachesTheFleet is the fleet half of what
// standalone mode has held since it was written. An agent reports what it
// had to leave out and what it took but may not be able to put back; a
// controller that accepts the report and drops both leaves the operator
// with a job marked successful and no record of either.
func TestWhatTheBackupCouldNotTakeReachesTheFleet(t *testing.T) {
	h := newHarness(t)
	ctx := h.ctx

	if _, err := h.maintenance.ProvisionPending(ctx); err != nil {
		t.Fatalf("provision: %v", err)
	}
	h.provider.Missing = []pkgacct.Omission{
		{What: "database customer1_shop", Why: "Lost connection to MySQL server (2013)"},
	}
	h.provider.Warnings = []string{
		"1 file is owned by another account and will stop the restore, " +
			"the first of them public_html/wp-config.php",
	}

	jobID, err := h.db.CreateJob(ctx, h.accountID, h.policyID)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	report := runOneJob(t, h, jobID)
	if len(report.Missing) != 1 || len(report.Warnings) != 1 {
		t.Fatalf("the agent reported missing=%v warnings=%v", report.Missing, report.Warnings)
	}

	notes, err := h.db.JobNotes(ctx, jobID)
	if err != nil {
		t.Fatalf("read job notes: %v", err)
	}
	if len(notes.Missing) != 1 || !strings.Contains(notes.Missing[0], "customer1_shop") {
		t.Errorf("what the backup left out is not on the job: %v", notes.Missing)
	}
	if len(notes.Warnings) != 1 || !strings.Contains(notes.Warnings[0], "wp-config.php") {
		t.Errorf("what the backup may not restore is not on the job: %v", notes.Warnings)
	}
}
