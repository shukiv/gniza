package node_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/resticrun"
)

// TestRetentionRefusesToRunWithoutApproval is the gate the operator asked
// for: nothing is deleted from a repository until somebody has looked at
// a plan and said go.
func TestRetentionRefusesToRunWithoutApproval(t *testing.T) {
	root := t.TempDir()
	store, err := nodestore.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	settings := nodestore.DefaultSettings()
	settings.StagingRoot = filepath.Join(root, "staging")
	settings.ResticCache = filepath.Join(root, "cache")
	settings.ConfigDir = filepath.Join(root, "config")
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, store, root)

	repo, err := store.PutRepository(nodestore.Repository{Path: "repo", DestinationID: "d1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutPolicy(nodestore.Policy{
		Name: "Nightly", ScheduleCron: "0 2 * * *", Enabled: true,
		RepositoryIDs: []string{repo.ID},
		Retention:     nodestore.Retention{KeepDaily: 7},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := engine.ApplyRetention(context.Background(), repo.ID); err == nil {
		t.Fatal("backups were deleted from a repository nobody had approved")
	}

	// And approving is itself refused until there is a plan to approve:
	// an operator cannot agree to something they have not been shown.
	if err := engine.ApproveRetention(repo.ID); err == nil {
		t.Fatal("retention was approved for a repository with no plan")
	}
}

// TestAKeepPolicyOfNothingIsRefused covers the argument list that would
// empty a repository. restic deletes every snapshot when told to keep
// none, so a schedule that says nothing must not be read as "keep
// nothing" — and must not quietly get a default nobody chose either.
func TestAKeepPolicyOfNothingIsRefused(t *testing.T) {
	root := t.TempDir()
	store, err := nodestore.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	settings := nodestore.DefaultSettings()
	settings.StagingRoot = filepath.Join(root, "staging")
	settings.ResticCache = filepath.Join(root, "cache")
	settings.ConfigDir = filepath.Join(root, "config")
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, store, root)

	repo, err := store.PutRepository(nodestore.Repository{Path: "repo", DestinationID: "d1"})
	if err != nil {
		t.Fatal(err)
	}
	// A schedule that writes here but keeps nothing, and one that keeps
	// something but writes elsewhere.
	if _, err := store.PutPolicy(nodestore.Policy{
		Name: "No keeps", ScheduleCron: "0 2 * * *", Enabled: true,
		RepositoryIDs: []string{repo.ID},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutPolicy(nodestore.Policy{
		Name: "Elsewhere", ScheduleCron: "0 3 * * *", Enabled: true,
		RepositoryIDs: []string{"other"},
		Retention:     nodestore.Retention{KeepDaily: 7},
	}); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	repo.RetentionApprovedAt = &now
	if _, err := store.PutRepository(repo); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ApplyRetention(context.Background(), repo.ID); err == nil {
		t.Fatal("a repository with no keep policy was handed to restic forget")
	}
}

// TestARetentionFailureIsReportedAndBacksOff covers what happens when
// restic will not run — a stale lock is the everyday case, and one turned
// up on the live server during development.
//
// Two things must hold, and neither did. The error has to reach the
// caller: recording it and returning nil told an operator who had just
// hit a locked repository that there was nothing to remove. And the
// attempt has to count towards the throttle, or the sweep retries the
// same locked repository on every fifteen-second tick, silently, for
// ever.
func TestARetentionFailureIsReportedAndBacksOff(t *testing.T) {
	root := t.TempDir()
	store, err := nodestore.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	settings := nodestore.DefaultSettings()
	settings.StagingRoot = filepath.Join(root, "staging")
	settings.ResticCache = filepath.Join(root, "cache")
	settings.ConfigDir = filepath.Join(root, "config")
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}

	// A restic that answers the way a locked repository does.
	locked := resticrun.ExecFunc(func(context.Context, resticrun.Command) (resticrun.CommandResult, error) {
		return resticrun.CommandResult{
			ExitCode: 11,
			Stdout: []byte(`{"message_type":"exit_error","code":11,` +
				`"message":"unable to create lock in backend: repository is already locked"}`),
		}, nil
	})
	engine := newEngineWithExec(t, store, root, locked)

	repo := attachedRepository(t, store, engine)
	if _, err := store.PutPolicy(nodestore.Policy{
		Name: "Nightly", ScheduleCron: "0 2 * * *", Enabled: true,
		RepositoryIDs: []string{repo.ID},
		Retention:     nodestore.Retention{KeepDaily: 7},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := engine.PlanRetention(context.Background(), repo.ID); err == nil {
		t.Fatal("a locked repository was reported as having nothing to remove")
	}

	after, err := store.Repository(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Retention.LastError == "" {
		t.Error("the reason was not recorded where the page can show it")
	}
	if after.Retention.AttemptedAt == nil {
		t.Fatal("a failed attempt left no timestamp, so the sweep would retry it every tick")
	}
	if !engine.RetentionIsThrottledForTest(after) {
		t.Error("a repository that has just failed is due again immediately")
	}
}

// TestApprovalDoesNotSurviveAPolicyChange is the finding from the second
// security review: approval was one timestamp, so it outlived the policy
// it was given for. Approve a plan that keeps thirty daily backups, edit
// the same schedule down to one, and the next run would delete twenty-nine
// days of backups under a policy nobody had read.
func TestApprovalDoesNotSurviveAPolicyChange(t *testing.T) {
	root := t.TempDir()
	store, err := nodestore.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	settings := nodestore.DefaultSettings()
	settings.StagingRoot = filepath.Join(root, "staging")
	settings.ResticCache = filepath.Join(root, "cache")
	settings.ConfigDir = filepath.Join(root, "config")
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}

	// A restic that answers every forget with a plan, and records what it
	// was asked to keep and whether the call was a dry run.
	var forgets []string
	recording := resticrun.ExecFunc(func(_ context.Context, cmd resticrun.Command) (resticrun.CommandResult, error) {
		if len(cmd.Args) > 0 && cmd.Args[0] == "forget" {
			forgets = append(forgets, strings.Join(cmd.Args, " "))
		}
		return resticrun.CommandResult{Stdout: []byte(
			`[{"host":"cp01","keep":[{"id":"aaaaaaaa"}],"remove":[{"id":"bbbbbbbb"}]}]`)}, nil
	})
	engine := newEngineWithExec(t, store, root, recording)

	repo := attachedRepository(t, store, engine)
	policy, err := store.PutPolicy(nodestore.Policy{
		Name: "Nightly", ScheduleCron: "0 2 * * *", Enabled: true,
		RepositoryIDs: []string{repo.ID},
		Retention:     nodestore.Retention{KeepDaily: 30},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if _, err := engine.PlanRetention(ctx, repo.ID); err != nil {
		t.Fatalf("PlanRetention: %v", err)
	}
	if err := engine.ApproveRetention(repo.ID); err != nil {
		t.Fatalf("ApproveRetention: %v", err)
	}

	// The operator edits the schedule afterwards -- a typo, or a change
	// meant for somewhere else.
	policy.Retention = nodestore.Retention{KeepDaily: 1}
	if _, err := store.PutPolicy(policy); err != nil {
		t.Fatal(err)
	}

	before := len(forgets)
	if _, err := engine.ApplyRetention(ctx, repo.ID); err == nil {
		t.Fatal("backups were deleted under a policy the operator never approved")
	}
	for _, forget := range forgets[before:] {
		if !strings.Contains(forget, "--dry-run") {
			t.Errorf("a destructive forget ran anyway: restic %s", forget)
		}
	}

	// The interface says so rather than only refusing when the button is
	// pressed, and the sweep leaves it alone until it is approved again.
	stored, err := store.Repository(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if engine.RetentionApprovalCovers(stored, nodestore.Retention{KeepDaily: 1}) {
		t.Error("the changed policy still counts as approved")
	}
	// And it does not offer to approve the plan on record, which was
	// taken under the policy from before the edit. Offering a button
	// whose only outcome is a refusal is how an operator concludes the
	// approval is broken and stops trusting the page.
	if engine.PlanApprovable(stored, nodestore.Retention{KeepDaily: 1}) {
		t.Error("the stale plan is still offered for approval")
	}

	// Reading the new plan and approving it puts the repository back in
	// service, under the policy that was actually read this time.
	if _, err := engine.PlanRetention(ctx, repo.ID); err != nil {
		t.Fatalf("PlanRetention: %v", err)
	}
	if err := engine.ApproveRetention(repo.ID); err != nil {
		t.Fatalf("ApproveRetention after a fresh plan: %v", err)
	}
	if _, err := engine.ApplyRetention(ctx, repo.ID); err != nil {
		t.Fatalf("ApplyRetention after a fresh approval: %v", err)
	}
}

// TestARepositoryThatCannotBeOpenedStopsBeingTriedEveryTick: the sweep
// looks at one repository per pass and then returns, and it skips a
// repository it has looked at recently. "Recently" is read off the
// timestamps a plan or an apply leaves behind, so a failure that leaves
// none is a failure the sweep repeats on every tick -- and because it
// returns afterwards, every other repository on the server is never
// reached. A destination that has gone is the ordinary way in.
func TestARepositoryThatCannotBeOpenedStopsBeingTriedEveryTick(t *testing.T) {
	root := t.TempDir()
	store, err := nodestore.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	settings := nodestore.DefaultSettings()
	settings.StagingRoot = filepath.Join(root, "staging")
	settings.ResticCache = filepath.Join(root, "cache")
	settings.ConfigDir = filepath.Join(root, "config")
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, store, root)

	repo, err := store.PutRepository(nodestore.Repository{Path: "repo", DestinationID: "gone"})
	if err != nil {
		t.Fatal(err)
	}
	keeps := nodestore.Retention{KeepDaily: 7}
	if _, err := store.PutPolicy(nodestore.Policy{
		Name: "Nightly", ScheduleCron: "0 2 * * *", Enabled: true,
		RepositoryIDs: []string{repo.ID}, Retention: keeps,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := engine.PlanRetention(context.Background(), repo.ID); err == nil {
		t.Fatal("a repository whose destination is gone was planned")
	}
	stored, err := store.Repository(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Retention.AttemptedAt == nil {
		t.Error("a plan that could not open the repository left no timestamp, " +
			"so the sweep tries it on every tick and reaches nothing else")
	}
	if stored.Retention.LastError == "" {
		t.Error("a plan that could not open the repository said nothing about why")
	}

	now := time.Now().UTC()
	stored.RetentionApprovedAt = &now
	stored.RetentionApprovedKeeps = keeps
	stored.Retention.AttemptedAt = nil
	stored.Retention.LastError = ""
	if _, err := store.PutRepository(stored); err != nil {
		t.Fatal(err)
	}

	if _, err := engine.ApplyRetention(context.Background(), repo.ID); err == nil {
		t.Fatal("a repository whose destination is gone was applied")
	}
	stored, err = store.Repository(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Retention.AttemptedAt == nil {
		t.Error("an apply that could not open the repository left no timestamp, " +
			"so the sweep tries it on every tick and reaches nothing else")
	}
}

// Withdrawing an approval is the one control that stops retention
// deleting anything, and the sweep writes to the same record on its own
// tick. Both read the repository, change their own field and write the
// whole record back, so a sweep that read the record before the
// withdrawal puts the approval back when it records what it found -- and
// the next apply deletes snapshots under an approval nobody holds.
func TestAWithdrawnApprovalIsNotPutBackByTheSweep(t *testing.T) {
	root := t.TempDir()
	store, err := nodestore.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	settings := nodestore.DefaultSettings()
	settings.StagingRoot = filepath.Join(root, "staging")
	settings.ResticCache = filepath.Join(root, "cache")
	settings.ConfigDir = filepath.Join(root, "config")
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, store, root)

	keeps := nodestore.Retention{KeepDaily: 7}
	approved := time.Now().UTC()
	repo, err := store.PutRepository(nodestore.Repository{
		Path: "repo", DestinationID: "gone",
		RetentionApprovedAt: &approved, RetentionApprovedKeeps: keeps,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutPolicy(nodestore.Policy{
		Name: "Nightly", ScheduleCron: "0 2 * * *", Enabled: true,
		RepositoryIDs: []string{repo.ID}, Retention: keeps,
	}); err != nil {
		t.Fatal(err)
	}

	// The destination is gone, so every plan fails and records why --
	// which is a write to the same record the withdrawal writes to.
	stop, swept, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		first := true
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = engine.PlanRetention(context.Background(), repo.ID)
			if first {
				close(swept)
				first = false
			}
		}
	}()

	<-swept
	time.Sleep(5 * time.Millisecond)
	if err := engine.WithdrawRetention(repo.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	close(stop)
	<-done

	after, err := store.Repository(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.RetentionApprovedAt != nil {
		t.Error("the approval was withdrawn and the sweep put it back")
	}
	if after.RetentionApprovedKeeps != (nodestore.Retention{}) {
		t.Errorf("the approved keeps came back as %+v", after.RetentionApprovedKeeps)
	}
}
