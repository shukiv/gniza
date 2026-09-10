package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/store"
	"github.com/shukiv/gniza/internal/testsupport"
)

// fixture is a fully wired controller database: one server, one account,
// one policy and two repositories in two destinations.
type fixture struct {
	db        *store.Store
	serverID  string
	accountID string
	policyID  string
	repoA     store.Repository
	repoB     store.Repository
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()

	db, err := store.Open(ctx, testsupport.PostgresDSN(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(db.Close)
	if _, err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	serverID, err := db.CreateServer(ctx, "cp01.example.com", "fingerprint-cp01")
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	accountID, err := db.CreateAccount(ctx, store.Account{
		ServerID: serverID, CPanelUser: "customer1",
		PrimaryDomain: "customer1.example", SizeEstimate: 4096,
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	newRepo := func(name, destType, config, path string) store.Repository {
		t.Helper()
		credentialID, err := db.CreateSecret(ctx, store.SecretBackendCredentials,
			[]byte("sealed-credentials"), "key1")
		if err != nil {
			t.Fatalf("CreateSecret: %v", err)
		}
		destID, err := db.CreateDestination(ctx, store.Destination{
			Name: name, Type: destType, Config: []byte(config),
			CredentialsSecretID: credentialID,
		})
		if err != nil {
			t.Fatalf("CreateDestination: %v", err)
		}
		passwordID, err := db.CreateSecret(ctx, store.SecretRepositoryPassword,
			[]byte("sealed-password"), "key1")
		if err != nil {
			t.Fatalf("CreateSecret: %v", err)
		}
		repo, err := db.CreateRepository(ctx, store.Repository{
			DestinationID: destID, ServerID: serverID,
			Path: path, PasswordSecretID: passwordID,
		})
		if err != nil {
			t.Fatalf("CreateRepository: %v", err)
		}
		return repo
	}

	repoA := newRepo("Local NAS", "local", `{"root":"/srv/backups"}`, "cp01")
	repoB := newRepo("Wasabi Miami", "s3", `{"bucket":"cp-backups"}`, "cp01")

	policyID, err := db.CreatePolicy(ctx, store.Policy{
		Name: "nightly", ScheduleCron: "0 2 * * *", PayloadMode: "split",
		Retention: store.Retention{KeepDaily: 7, KeepMonthly: 6},
	})
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
	for _, repo := range []store.Repository{repoA, repoB} {
		if err := db.AttachRepositoryToPolicy(ctx, policyID, repo.ID); err != nil {
			t.Fatalf("AttachRepositoryToPolicy: %v", err)
		}
	}
	if err := db.AttachPolicyToAccount(ctx, accountID, policyID); err != nil {
		t.Fatalf("AttachPolicyToAccount: %v", err)
	}

	return &fixture{db: db, serverID: serverID, accountID: accountID,
		policyID: policyID, repoA: repoA, repoB: repoB}
}

func TestCreateRepositoryFillsChunkerSource(t *testing.T) {
	f := newFixture(t)

	// The first repository for a server is its own chunker source.
	if f.repoA.ChunkerSourceRepoID != "" {
		t.Errorf("first repository has chunker source %q, want none", f.repoA.ChunkerSourceRepoID)
	}
	// Every later one must copy that repository's parameters: they are
	// fixed at init and can never be changed.
	if f.repoB.ChunkerSourceRepoID != f.repoA.ID {
		t.Errorf("second repository chunker source = %q, want %q",
			f.repoB.ChunkerSourceRepoID, f.repoA.ID)
	}
}

func TestServerLookupAndEnrolment(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	server, err := f.db.ServerByFingerprint(ctx, "fingerprint-cp01")
	if err != nil {
		t.Fatalf("ServerByFingerprint: %v", err)
	}
	if server.Hostname != "cp01.example.com" || server.Status != "pending" {
		t.Errorf("server = %+v", server)
	}

	if _, err := f.db.ServerByFingerprint(ctx, "unknown"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown fingerprint gave %v, want ErrNotFound", err)
	}

	flags := map[string]string{"nocompress": "--nocompress", "skiphomedir": "--skiphomedir"}
	if err := f.db.RecordEnrolment(ctx, f.serverID, flags, "/data/staging"); err != nil {
		t.Fatalf("RecordEnrolment: %v", err)
	}
	server, err = f.db.ServerByID(ctx, f.serverID)
	if err != nil {
		t.Fatalf("ServerByID: %v", err)
	}
	if server.Status != "active" {
		t.Errorf("status = %q, want active", server.Status)
	}
	if server.PkgacctFlags["nocompress"] != "--nocompress" {
		t.Errorf("pkgacct flags = %v", server.PkgacctFlags)
	}
	if server.StagingRoot != "/data/staging" {
		t.Errorf("staging root = %q", server.StagingRoot)
	}
}

func TestJobLifecycle(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	jobID, err := f.db.CreateJob(ctx, f.accountID, f.policyID)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	// A second job for the same account and policy must not queue behind
	// the first: a slow nightly run should not be joined by the next tick.
	if _, err := f.db.CreateJob(ctx, f.accountID, f.policyID); !errors.Is(err, store.ErrNoWork) {
		t.Errorf("duplicate CreateJob gave %v, want ErrNoWork", err)
	}

	claimed, err := f.db.ClaimNextJob(ctx, f.serverID, time.Minute)
	if err != nil {
		t.Fatalf("ClaimNextJob: %v", err)
	}
	if claimed.JobID != jobID {
		t.Errorf("claimed %q, want %q", claimed.JobID, jobID)
	}
	if len(claimed.Targets) != 2 {
		t.Fatalf("claimed %d targets, want 2", len(claimed.Targets))
	}
	if claimed.Account.CPanelUser != "customer1" {
		t.Errorf("account = %+v", claimed.Account)
	}
	if claimed.Policy.Retention.KeepDaily != 7 {
		t.Errorf("retention = %+v", claimed.Policy.Retention)
	}
	for _, target := range claimed.Targets {
		if len(target.RepoPasswordSealed) == 0 {
			t.Error("target is missing its sealed repository password")
		}
	}

	// The job is leased, so a second claim finds nothing.
	if _, err := f.db.ClaimNextJob(ctx, f.serverID, time.Minute); !errors.Is(err, store.ErrNoWork) {
		t.Errorf("second claim gave %v, want ErrNoWork", err)
	}

	// One destination is unreachable; the other holds a good copy.
	status, err := f.db.ApplyReport(ctx, f.serverID, jobID, claimed.ClaimToken,
		[]store.TargetReport{
			{RepositoryID: f.repoA.ID, Status: job.TargetSuccess,
				SnapshotID: "40dc1520", BytesAdded: 1024, BytesProcessed: 4096, DurationSecs: 1.5},
			{RepositoryID: f.repoB.ID, Status: job.TargetFailed, Error: "connection timeout"},
		}, store.JobOutcome{})
	if err != nil {
		t.Fatalf("ApplyReport: %v", err)
	}
	if status != job.StatusPartialSuccess {
		t.Errorf("status = %q, want partial_success", status)
	}

	stored, err := f.db.JobStatus(ctx, jobID)
	if err != nil {
		t.Fatalf("JobStatus: %v", err)
	}
	if stored != job.StatusPartialSuccess {
		t.Errorf("stored status = %q", stored)
	}

	targets, err := f.db.JobTargets(ctx, jobID)
	if err != nil {
		t.Fatalf("JobTargets: %v", err)
	}
	var succeeded int
	for _, target := range targets {
		if target.Status == job.TargetSuccess {
			succeeded++
			if target.SnapshotID != "40dc1520" || target.BytesAdded != 1024 {
				t.Errorf("target = %+v", target)
			}
		}
		if target.Attempt != 1 {
			t.Errorf("attempt = %d, want 1", target.Attempt)
		}
	}
	if succeeded != 1 {
		t.Errorf("%d targets succeeded, want 1", succeeded)
	}
}

func TestClaimRetriesOnlyFailedTargets(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	jobID, err := f.db.CreateJob(ctx, f.accountID, f.policyID)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	first, err := f.db.ClaimNextJob(ctx, f.serverID, time.Millisecond)
	if err != nil {
		t.Fatalf("ClaimNextJob: %v", err)
	}
	if _, err := f.db.ApplyReport(ctx, f.serverID, jobID, first.ClaimToken,
		[]store.TargetReport{
			{RepositoryID: f.repoA.ID, Status: job.TargetSuccess, SnapshotID: "aaa"},
			{RepositoryID: f.repoB.ID, Status: job.TargetFailed, Error: "timeout"},
		}, store.JobOutcome{}); err != nil {
		t.Fatalf("ApplyReport: %v", err)
	}

	// Re-queue the job the way a retry would, then confirm the claim only
	// carries the target that still needs work: re-uploading a good copy
	// would waste the whole payload again.
	if _, err := f.db.Pool().Exec(ctx,
		`UPDATE backup_jobs SET status = 'pending' WHERE id = $1`, jobID); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	claimed, err := f.db.ClaimNextJob(ctx, f.serverID, time.Minute)
	if err != nil {
		t.Fatalf("ClaimNextJob: %v", err)
	}
	if len(claimed.Targets) != 1 {
		t.Fatalf("claimed %d targets, want only the failed one", len(claimed.Targets))
	}
	if claimed.Targets[0].RepositoryID != f.repoB.ID {
		t.Errorf("claimed repository %q, want %q", claimed.Targets[0].RepositoryID, f.repoB.ID)
	}
	if claimed.Targets[0].Attempt != 1 {
		t.Errorf("attempt = %d, want 1", claimed.Targets[0].Attempt)
	}
}

func TestStagingErrorFailsEveryTarget(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	jobID, err := f.db.CreateJob(ctx, f.accountID, f.policyID)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	claimed, err := f.db.ClaimNextJob(ctx, f.serverID, time.Minute)
	if err != nil {
		t.Fatalf("ClaimNextJob: %v", err)
	}

	status, err := f.db.ApplyReport(ctx, f.serverID, jobID, claimed.ClaimToken, nil,
		store.JobOutcome{StagingError: "staging: need 8192 bytes free, have 512"})
	if err != nil {
		t.Fatalf("ApplyReport: %v", err)
	}
	if status != job.StatusFailed {
		t.Errorf("status = %q, want failed", status)
	}
	targets, err := f.db.JobTargets(ctx, jobID)
	if err != nil {
		t.Fatalf("JobTargets: %v", err)
	}
	for _, target := range targets {
		if target.Status != job.TargetFailed {
			t.Errorf("target %s = %q, want failed", target.RepositoryID, target.Status)
		}
		if target.Err == "" {
			t.Error("target should carry the staging error")
		}
	}
}

func TestReclaimExpiredLeases(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, err := f.db.CreateJob(ctx, f.accountID, f.policyID); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	// A lease that has already expired stands in for an agent that died
	// mid-job.
	if _, err := f.db.ClaimNextJob(ctx, f.serverID, -time.Minute); err != nil {
		t.Fatalf("ClaimNextJob: %v", err)
	}
	if _, err := f.db.ClaimNextJob(ctx, f.serverID, time.Minute); !errors.Is(err, store.ErrNoWork) {
		t.Fatalf("job should be leased, got %v", err)
	}

	reclaimed, err := f.db.ReclaimExpiredLeases(ctx)
	if err != nil {
		t.Fatalf("ReclaimExpiredLeases: %v", err)
	}
	if reclaimed != 1 {
		t.Fatalf("reclaimed %d, want 1", reclaimed)
	}
	if _, err := f.db.ClaimNextJob(ctx, f.serverID, time.Minute); err != nil {
		t.Errorf("reclaimed job should be claimable again: %v", err)
	}
}

func TestConcurrencyCapBlocksSecondClaim(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	secondAccount, err := f.db.CreateAccount(ctx, store.Account{
		ServerID: f.serverID, CPanelUser: "customer2", SizeEstimate: 2048,
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if err := f.db.AttachPolicyToAccount(ctx, secondAccount, f.policyID); err != nil {
		t.Fatalf("AttachPolicyToAccount: %v", err)
	}
	for _, accountID := range []string{f.accountID, secondAccount} {
		if _, err := f.db.CreateJob(ctx, accountID, f.policyID); err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
	}

	// max_concurrency defaults to 1: staging two full accounts at once
	// would double the disk the operator sized for.
	if _, err := f.db.ClaimNextJob(ctx, f.serverID, time.Minute); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if _, err := f.db.ClaimNextJob(ctx, f.serverID, time.Minute); !errors.Is(err, store.ErrNoWork) {
		t.Errorf("second claim gave %v, want ErrNoWork", err)
	}
}

func TestCreateJobRefusesPolicyWithNoRepositories(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	emptyPolicy, err := f.db.CreatePolicy(ctx, store.Policy{
		Name: "misconfigured", ScheduleCron: "0 3 * * *",
	})
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
	if err := f.db.AttachPolicyToAccount(ctx, f.accountID, emptyPolicy); err != nil {
		t.Fatalf("AttachPolicyToAccount: %v", err)
	}
	// A job with no targets could only ever roll up to failed. Refusing at
	// creation surfaces the misconfiguration instead of burying it in job
	// history.
	if _, err := f.db.CreateJob(ctx, f.accountID, emptyPolicy); err == nil {
		t.Error("a policy with no repositories should be refused")
	}
}

func TestOnlyOneRunningJobPerAccount(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Two policies covering the same account, and room to run both at once.
	if _, err := f.db.Pool().Exec(ctx,
		`UPDATE servers SET max_concurrency = 4 WHERE id = $1`, f.serverID); err != nil {
		t.Fatalf("raise concurrency: %v", err)
	}
	secondPolicy, err := f.db.CreatePolicy(ctx, store.Policy{
		Name: "weekly-offsite", ScheduleCron: "0 3 * * 0",
	})
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
	if err := f.db.AttachRepositoryToPolicy(ctx, secondPolicy, f.repoA.ID); err != nil {
		t.Fatalf("AttachRepositoryToPolicy: %v", err)
	}
	if err := f.db.AttachPolicyToAccount(ctx, f.accountID, secondPolicy); err != nil {
		t.Fatalf("AttachPolicyToAccount: %v", err)
	}
	for _, policyID := range []string{f.policyID, secondPolicy} {
		if _, err := f.db.CreateJob(ctx, f.accountID, policyID); err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
	}

	// The agent stages an account under its own name, so two concurrent
	// jobs for one account would collide on the same staging directory.
	first, err := f.db.ClaimNextJob(ctx, f.serverID, time.Minute)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if _, err := f.db.ClaimNextJob(ctx, f.serverID, time.Minute); !errors.Is(err, store.ErrNoWork) {
		t.Fatalf("second claim for the same account gave %v, want ErrNoWork", err)
	}

	// Once the first finishes, the second policy's job runs.
	if _, err := f.db.ApplyReport(ctx, f.serverID, first.JobID, first.ClaimToken,
		[]store.TargetReport{
			{RepositoryID: f.repoA.ID, Status: job.TargetSuccess, SnapshotID: "aaa"},
			{RepositoryID: f.repoB.ID, Status: job.TargetSuccess, SnapshotID: "bbb"},
		}, store.JobOutcome{}); err != nil {
		t.Fatalf("ApplyReport: %v", err)
	}
	if _, err := f.db.ClaimNextJob(ctx, f.serverID, time.Minute); err != nil {
		t.Errorf("the queued job should run once the first finishes: %v", err)
	}
}

func TestRestoreLifecycle(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// A restore needs a provisioned repository to read from.
	if err := f.db.MarkRepositoryInitialised(ctx, f.repoA.ID); err != nil {
		t.Fatalf("MarkRepositoryInitialised: %v", err)
	}

	jobID, err := f.db.CreateRestore(ctx, store.RestoreRequest{
		AccountID:  f.accountID,
		SnapshotID: "40dc15203b1cf9",
	})
	if err != nil {
		t.Fatalf("CreateRestore: %v", err)
	}

	claimed, err := f.db.ClaimNextRestore(ctx, f.serverID, time.Minute)
	if err != nil {
		t.Fatalf("ClaimNextRestore: %v", err)
	}
	if claimed.JobID != jobID {
		t.Errorf("claimed %q, want %q", claimed.JobID, jobID)
	}
	if claimed.Account.CPanelUser != "customer1" {
		t.Errorf("account = %+v", claimed.Account)
	}
	// The controller picks the source; the agent is told which repository.
	if claimed.Source.RepositoryID != f.repoA.ID {
		t.Errorf("source = %q, want the provisioned repository %q",
			claimed.Source.RepositoryID, f.repoA.ID)
	}
	if len(claimed.Source.RepoPasswordSealed) == 0 {
		t.Error("source is missing its sealed repository password")
	}
	if claimed.Apply {
		t.Error("apply should default to false: a restore must not overwrite an account unasked")
	}

	if _, err := f.db.ClaimNextRestore(ctx, f.serverID, time.Minute); !errors.Is(err, store.ErrNoWork) {
		t.Errorf("second claim gave %v, want ErrNoWork", err)
	}

	if err := f.db.ApplyRestoreReport(ctx, f.serverID, jobID, claimed.ClaimToken,
		store.RestoreOutcome{
			Status: job.StatusSuccess, BytesRestored: 4096,
			ArchivePath: "/var/lib/gniza/staging/stage-restore-customer1/cpmove-customer1.tar",
		}); err != nil {
		t.Fatalf("ApplyRestoreReport: %v", err)
	}
	restore, err := f.db.RestoreByID(ctx, jobID)
	if err != nil {
		t.Fatalf("RestoreByID: %v", err)
	}
	if restore.Status != job.StatusSuccess || restore.BytesRestored != 4096 {
		t.Errorf("restore = %+v", restore)
	}
	if restore.ArchivePath == "" {
		t.Error("the archive path should be recorded so an operator can find it")
	}
}

func TestRestoreWaitsForARunningBackup(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if err := f.db.MarkRepositoryInitialised(ctx, f.repoA.ID); err != nil {
		t.Fatalf("MarkRepositoryInitialised: %v", err)
	}
	backupID, err := f.db.CreateJob(ctx, f.accountID, f.policyID)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	claimed, err := f.db.ClaimNextJob(ctx, f.serverID, time.Minute)
	if err != nil {
		t.Fatalf("ClaimNextJob: %v", err)
	}
	if _, err := f.db.CreateRestore(ctx, store.RestoreRequest{
		AccountID: f.accountID, SnapshotID: "40dc15203b1cf9",
	}); err != nil {
		t.Fatalf("CreateRestore: %v", err)
	}

	// Backup and restore both stage under the account's name, so running
	// them at once would have them writing over each other.
	if _, err := f.db.ClaimNextRestore(ctx, f.serverID, time.Minute); !errors.Is(err, store.ErrNoWork) {
		t.Fatalf("restore claim during a backup gave %v, want ErrNoWork", err)
	}

	if _, err := f.db.ApplyReport(ctx, f.serverID, backupID, claimed.ClaimToken,
		[]store.TargetReport{
			{RepositoryID: f.repoA.ID, Status: job.TargetSuccess, SnapshotID: "aaa"},
			{RepositoryID: f.repoB.ID, Status: job.TargetSuccess, SnapshotID: "bbb"},
		}, store.JobOutcome{}); err != nil {
		t.Fatalf("ApplyReport: %v", err)
	}
	if _, err := f.db.ClaimNextRestore(ctx, f.serverID, time.Minute); err != nil {
		t.Errorf("restore should run once the backup finishes: %v", err)
	}
}

func TestCreateRestoreRejectsFilesWithoutPaths(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if err := f.db.MarkRepositoryInitialised(ctx, f.repoA.ID); err != nil {
		t.Fatalf("MarkRepositoryInitialised: %v", err)
	}
	// A files restore with no paths would silently restore nothing.
	if _, err := f.db.CreateRestore(ctx, store.RestoreRequest{
		AccountID: f.accountID, SnapshotID: "40dc15203b1cf9", Kind: "files",
	}); err == nil {
		t.Error("a files restore with no paths should be refused")
	}
	// restorepkg takes a whole account archive, so applying a partial
	// restore is meaningless.
	if _, err := f.db.CreateRestore(ctx, store.RestoreRequest{
		AccountID: f.accountID, SnapshotID: "40dc15203b1cf9", Kind: "files",
		IncludePaths: []string{"/home/customer1/public_html/index.html"}, Apply: true,
	}); err == nil {
		t.Error("applying a files restore should be refused")
	}
}

func TestReclaimExpiredRestoreLeases(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if err := f.db.MarkRepositoryInitialised(ctx, f.repoA.ID); err != nil {
		t.Fatalf("MarkRepositoryInitialised: %v", err)
	}
	if _, err := f.db.CreateRestore(ctx, store.RestoreRequest{
		AccountID: f.accountID, SnapshotID: "40dc15203b1cf9",
	}); err != nil {
		t.Fatalf("CreateRestore: %v", err)
	}
	if _, err := f.db.ClaimNextRestore(ctx, f.serverID, -time.Minute); err != nil {
		t.Fatalf("ClaimNextRestore: %v", err)
	}

	reclaimed, err := f.db.ReclaimExpiredRestoreLeases(ctx)
	if err != nil {
		t.Fatalf("ReclaimExpiredRestoreLeases: %v", err)
	}
	if reclaimed != 1 {
		t.Fatalf("reclaimed %d, want 1", reclaimed)
	}
	claimed, err := f.db.ClaimNextRestore(ctx, f.serverID, time.Minute)
	if err != nil {
		t.Fatalf("reclaimed restore should be claimable: %v", err)
	}
	if claimed.JobID == "" {
		t.Error("no job id on the reclaimed restore")
	}
}
