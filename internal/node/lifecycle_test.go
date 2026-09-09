package node

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/cpanel"
	"github.com/shukiv/gniza/internal/hookspool"
	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/vault"
)

func lifecycleEngine(t *testing.T, users map[string][]string, uids map[string]int) (*Engine, *nodestore.Store) {
	t.Helper()
	root := t.TempDir()
	store, err := nodestore.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	settings := nodestore.DefaultSettings()
	settings.StagingRoot = filepath.Join(root, "staging")
	settings.ResticCache = filepath.Join(root, "cache")
	settings.ConfigDir = filepath.Join(root, "config")
	done := time.Now().UTC()
	settings.IdentitiesBackfilledAt = &done
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	v, err := vault.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	engine, err := New(Config{
		Store: store, Vault: v,
		Provider: &cpanel.Fake{Root: filepath.Join(root, "cpanel"), Databases: users},
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		AccountUID: func(user string) (int, error) {
			return uids[user], nil
		},
		HookSpool: filepath.Join(root, "hooks"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return engine, store
}

func TestModifyHookCarriesNamedPolicyAcrossRename(t *testing.T) {
	engine, store := lifecycleEngine(t,
		map[string][]string{"newname": nil}, map[string]int{"newname": 1042})
	since := time.Now().Add(-24 * time.Hour).UTC()
	if _, err := store.PutIdentity(nodestore.AccountIdentity{
		Account: "oldname", UID: 1042, SinceAt: since,
	}); err != nil {
		t.Fatal(err)
	}
	policy, err := store.PutPolicy(nodestore.Policy{
		Name: "selected", Accounts: []string{"oldname"}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := engine.ReconcileAccountRenames(context.Background()); err != nil {
		t.Fatal(err)
	}
	policy, _ = store.Policy(policy.ID)
	if len(policy.Accounts) != 1 || policy.Accounts[0] != "newname" {
		t.Fatalf("policy accounts = %v, want renamed account", policy.Accounts)
	}
	identity, err := store.Identity("newname")
	if err != nil || identity.UID != 1042 || !identity.SinceAt.Equal(since) {
		t.Fatalf("renamed identity = %+v, %v", identity, err)
	}
}

func TestCreateReconciliationDoesNotMistakeReusedUIDForRename(t *testing.T) {
	engine, store := lifecycleEngine(t,
		map[string][]string{"newowner": nil}, map[string]int{"newowner": 1042})
	if _, err := store.PutIdentity(nodestore.AccountIdentity{
		Account: "departed", UID: 1042, SinceAt: time.Now().Add(-30 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	policy, _ := store.PutPolicy(nodestore.Policy{
		Name: "selected", Accounts: []string{"departed"}, Enabled: true,
	})

	if err := engine.ReconcileAccounts(context.Background()); err != nil {
		t.Fatal(err)
	}
	policy, _ = store.Policy(policy.ID)
	if policy.Accounts[0] != "departed" {
		t.Fatalf("a reused uid was treated as a rename: %v", policy.Accounts)
	}
	identity, err := store.Identity("newowner")
	if err != nil || !identity.Recycled {
		t.Fatalf("new owner has no privacy boundary: %+v, %v", identity, err)
	}
}

func TestCreateHookSeparatesReusedNameEvenWhenUIDWasAlsoReused(t *testing.T) {
	engine, store := lifecycleEngine(t,
		map[string][]string{"webshop": nil}, map[string]int{"webshop": 1042})
	oldSince := time.Now().Add(-30 * 24 * time.Hour).UTC()
	previous, err := store.PutIdentity(nodestore.AccountIdentity{
		Account: "webshop", UID: 1042, SinceAt: oldSince,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.AccountCreated("webshop", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	current, err := store.Identity("webshop")
	if err != nil {
		t.Fatal(err)
	}
	if !current.Recycled || !current.SinceAt.After(oldSince) {
		t.Fatalf("reused name and uid inherited the old boundary: %+v", current)
	}
	if !current.CreatedAt.Equal(previous.CreatedAt) {
		t.Fatal("identity history was replaced rather than advanced")
	}
}

func TestNewAccountGetsImmediateBackupFromWidestAllAccountPolicy(t *testing.T) {
	engine, store := lifecycleEngine(t,
		map[string][]string{"newsite": nil}, map[string]int{"newsite": 1500})
	_, _ = store.PutPolicy(nodestore.Policy{
		Name: "named only", Accounts: []string{"someoneelse"}, Enabled: true,
		RepositoryIDs: []string{"named-repo"},
	})
	_, _ = store.PutPolicy(nodestore.Policy{
		Name: "one copy", Enabled: true, RepositoryIDs: []string{"repo-1"},
	})
	widest, _ := store.PutPolicy(nodestore.Policy{
		Name: "two copies", Enabled: true,
		RepositoryIDs: []string{"repo-1", "repo-2"},
	})

	queued, err := engine.QueueInitialBackup("newsite")
	if err != nil || !queued {
		t.Fatalf("initial backup = %v, %v", queued, err)
	}
	jobs, err := store.Jobs(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].PolicyID != widest.ID || jobs[0].Account != "newsite" {
		t.Fatalf("initial jobs = %+v", jobs)
	}
}

func TestInitialBackupPrefersCompletePolicyOverWiderPartialPolicy(t *testing.T) {
	engine, store := lifecycleEngine(t,
		map[string][]string{"newsite": nil}, map[string]int{"newsite": 1500})
	_, _ = store.PutPolicy(nodestore.Policy{
		Name: "partial", Enabled: true, SkipEmail: true,
		RepositoryIDs: []string{"repo-1", "repo-2"},
	})
	complete, _ := store.PutPolicy(nodestore.Policy{
		Name: "complete", Enabled: true, RepositoryIDs: []string{"repo-1"},
	})
	queued, err := engine.QueueInitialBackup("newsite")
	if err != nil || !queued {
		t.Fatalf("initial backup = %v, %v", queued, err)
	}
	jobs, _ := store.Jobs(0)
	if len(jobs) != 1 || jobs[0].PolicyID != complete.ID {
		t.Fatalf("initial backup used a partial payload: %+v", jobs)
	}
}

func TestSuspensionBackupIsOptInAndCoversAllFullDestinations(t *testing.T) {
	engine, store := lifecycleEngine(t,
		map[string][]string{"customer1": nil}, map[string]int{"customer1": 1500})
	result, err := engine.QueueSuspensionBackup("customer1")
	if err != nil || result.Enabled || result.Queued {
		t.Fatalf("default suspension backup = %+v, %v", result, err)
	}
	settings, _ := store.Settings()
	settings.BackupOnSuspension = true
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	_, _ = store.PutPolicy(nodestore.Policy{
		Name: "partial", Enabled: true, SkipEmail: true,
		RepositoryIDs: []string{"repo-1", "repo-2"},
	})
	_, _ = store.PutPolicy(nodestore.Policy{
		Name: "local full", Enabled: true, RepositoryIDs: []string{"repo-1"},
	})
	_, _ = store.PutPolicy(nodestore.Policy{
		Name: "remote full", Enabled: true, Accounts: []string{"customer1"},
		RepositoryIDs: []string{"repo-2"},
	})

	result, err = engine.QueueSuspensionBackup("customer1")
	if err != nil || !result.Enabled || !result.Queued || len(result.Policies) != 2 {
		t.Fatalf("suspension backup = %+v, %v", result, err)
	}
	jobs, _ := store.Jobs(0)
	if len(jobs) != 2 {
		t.Fatalf("suspension jobs = %+v", jobs)
	}
	result, err = engine.QueueSuspensionBackup("customer1")
	if err != nil || !result.Busy || result.Queued {
		t.Fatalf("duplicate suspension backup = %+v, %v", result, err)
	}
}

func TestCoverageRepairRejectsPolicyThatDoesNotCoverAccount(t *testing.T) {
	engine, store := lifecycleEngine(t,
		map[string][]string{"customer1": nil}, map[string]int{"customer1": 1500})
	wrong, _ := store.PutPolicy(nodestore.Policy{
		Name: "someone else", Accounts: []string{"customer2"}, Enabled: true,
		RepositoryIDs: []string{"repo-1"},
	})
	if _, err := engine.QueueCoverageRepair(wrong.ID, "customer1"); err == nil {
		t.Fatal("unrelated policy was accepted for coverage repair")
	}
	right, _ := store.PutPolicy(nodestore.Policy{
		Name: "customer one", Accounts: []string{"customer1"}, Enabled: true,
		RepositoryIDs: []string{"repo-1"},
	})
	if _, err := engine.QueueCoverageRepair(right.ID, "customer1"); err != nil {
		t.Fatalf("covered policy was refused: %v", err)
	}
}

func TestRemovingOnlyNamedAccountDisablesRatherThanWidensPolicy(t *testing.T) {
	engine, store := lifecycleEngine(t, map[string][]string{}, map[string]int{})
	policy, _ := store.PutPolicy(nodestore.Policy{
		Name: "one customer", Accounts: []string{"departed"}, Enabled: true,
	})
	if err := engine.AccountRemoved("departed"); err != nil {
		t.Fatal(err)
	}
	policy, _ = store.Policy(policy.ID)
	if policy.Enabled || len(policy.Accounts) != 0 {
		t.Fatalf("terminated account widened policy: %+v", policy)
	}
}

// TestARemovedAccountIsNeverTreatedAsARenameSource covers the way the
// ownership boundary can be handed to the wrong customer.
//
// A rename is inferred from an old name that vanished while its uid stayed.
// Linux reuses a deleted account's uid, so a new account that reached the
// server without a create hook -- restored by restorepkg, or created while
// this service was stopped -- looks exactly like a rename of the departed
// account whose uid it inherited. Taking it as one hands the new customer
// the previous one's SinceAt, and their backups with it.
//
// cPanel told us that account was removed. A removal is not a rename.
func TestARemovedAccountIsNeverTreatedAsARenameSource(t *testing.T) {
	engine, store := lifecycleEngine(t,
		map[string][]string{"newowner": nil}, map[string]int{"newowner": 1042})

	departedSince := time.Now().Add(-90 * 24 * time.Hour).UTC()
	retired := time.Now().Add(-24 * time.Hour).UTC()
	if _, err := store.PutIdentity(nodestore.AccountIdentity{
		Account: "departed", UID: 1042, SinceAt: departedSince, RetiredAt: &retired,
	}); err != nil {
		t.Fatal(err)
	}
	policy, err := store.PutPolicy(nodestore.Policy{
		Name: "selected", Accounts: []string{"departed"}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The modify hook is the one path allowed to infer a rename at all.
	if err := engine.ReconcileAccountRenames(context.Background()); err != nil {
		t.Fatal(err)
	}

	identity, err := store.Identity("newowner")
	if err != nil {
		t.Fatalf("the new account has no identity at all: %v", err)
	}
	if !identity.SinceAt.After(departedSince) {
		t.Fatalf("the new owner inherited the departed customer's boundary: %+v", identity)
	}
	if !identity.Recycled {
		t.Fatal("the new owner was recorded without a privacy boundary")
	}
	policy, _ = store.Policy(policy.ID)
	if len(policy.Accounts) != 1 || policy.Accounts[0] != "departed" {
		t.Fatalf("a removal was carried across as a rename: %v", policy.Accounts)
	}
}

// TestAnUndeliveredCreateStillSeparatesTheNewOwner is the finding from the
// second security review: the hook fails open when this service is not
// running, and polling cannot recover what it dropped.
//
// A customer leaves, their username is deleted and given to somebody else,
// and the operating system hands the new account the same uid. If both
// events happened while the service was down, the account list afterwards
// is identical to the one before -- same name, same uid -- so reconciling
// against it treats two customers as one, and the new one can list and
// restore the old one's backups.
//
// The hook writes those two events down instead of losing them, and they
// are replayed before anything reads the account list.
func TestAnUndeliveredCreateStillSeparatesTheNewOwner(t *testing.T) {
	engine, store := lifecycleEngine(t,
		map[string][]string{"webshop": nil}, map[string]int{"webshop": 1042})

	// The customer who was here before, and something they restored.
	firstOwnerSince := time.Now().Add(-30 * 24 * time.Hour).UTC()
	if _, err := store.PutIdentity(nodestore.AccountIdentity{
		Account: "webshop", UID: 1042, SinceAt: firstOwnerSince,
	}); err != nil {
		t.Fatal(err)
	}
	theirs := nodestore.Restore{
		Account: "webshop", SnapshotID: "40dc1520",
		QueuedAt: time.Now().Add(-20 * 24 * time.Hour).UTC(), AccountSince: firstOwnerSince,
	}
	if !engine.BelongsToCurrentHolder(theirs) {
		t.Fatal("the account's own restore was hidden from it before anything changed")
	}

	// The service is down. cPanel removes the account and creates it
	// again for somebody else, onto the same name and the same uid, and
	// both hooks write what they were told to the spool.
	removedAt := time.Now().Add(-2 * time.Hour).UTC()
	createdAt := time.Now().Add(-1 * time.Hour).UTC()
	for _, event := range []hookspool.Event{
		{At: removedAt, Event: "remove", Account: "webshop",
			Payload: []byte(`{"data":{"user":"webshop"}}`)},
		{At: createdAt, Event: "create", Account: "webshop",
			Payload: []byte(`{"data":{"user":"webshop"}}`)},
	} {
		if _, err := hookspool.Write(engine.hookSpool, event); err != nil {
			t.Fatalf("spool %s: %v", event.Event, err)
		}
	}

	// The service comes back and looks at the accounts, which is where
	// the boundary used to be lost.
	if err := engine.ReconcileAccounts(context.Background()); err != nil {
		t.Fatal(err)
	}

	current, err := store.Identity("webshop")
	if err != nil {
		t.Fatal(err)
	}
	if !current.Recycled {
		t.Fatal("the name changed hands and nothing recorded it, so the new owner " +
			"can read the last customer's backups")
	}
	if !current.SinceAt.Equal(createdAt) {
		t.Errorf("the boundary is %v, want the moment cPanel made the account, %v",
			current.SinceAt, createdAt)
	}
	if engine.BelongsToCurrentHolder(theirs) {
		t.Error("the previous customer's restore is offered to the new one")
	}

	// Replayed events are cleared, and replaying is harmless anyway: a
	// hook that timed out after the service had already recorded the
	// event leaves a file for something that is done.
	pending, problems := hookspool.Pending(engine.hookSpool)
	if len(pending) != 0 || len(problems) != 0 {
		t.Errorf("spool still holds %d events and %d unreadable files", len(pending), len(problems))
	}
	if err := engine.AccountCreated("webshop", createdAt); err != nil {
		t.Fatal(err)
	}
	again, err := store.Identity("webshop")
	if err != nil {
		t.Fatal(err)
	}
	if !again.SinceAt.Equal(createdAt) {
		t.Errorf("replaying the same event moved the boundary to %v, hiding the "+
			"present owner's own backups from them", again.SinceAt)
	}
}

// A create spooled for a name that no longer exists has nothing left to
// record, and keeping it makes every reconciliation from now on fail on
// the same file.
//
// The sequence is ordinary: the service is down, an account is made and
// removed again before it comes back. There is no current holder whose
// backups the boundary would protect, and a name created later brings its
// own hook with it. A lookup that fails for any other reason is a
// different thing and the event is kept.
func TestAReplayedCreateForAVanishedAccountIsDiscarded(t *testing.T) {
	engine, _ := lifecycleEngine(t, nil, nil)
	gone := errors.New("boom")
	engine.accountUID = func(name string) (int, error) {
		if name == "webshop" {
			return 0, fmt.Errorf("node: %s is %w", name, ErrNoSuchAccount)
		}
		return 0, fmt.Errorf("node: cannot read the account list: %w", gone)
	}

	for _, event := range []hookspool.Event{
		{At: time.Now().Add(-2 * time.Hour).UTC(), Event: "create", Account: "webshop",
			Payload: []byte(`{"data":{"user":"webshop"}}`)},
		{At: time.Now().Add(-1 * time.Hour).UTC(), Event: "create", Account: "unreadable",
			Payload: []byte(`{"data":{"user":"unreadable"}}`)},
	} {
		if _, err := hookspool.Write(engine.hookSpool, event); err != nil {
			t.Fatalf("spool %s: %v", event.Account, err)
		}
	}

	if err := engine.ReplayHookSpool(); err != nil {
		t.Fatal(err)
	}
	pending, problems := hookspool.Pending(engine.hookSpool)
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if len(pending) != 1 {
		t.Fatalf("the spool holds %d events, want only the one whose account "+
			"could not be read", len(pending))
	}
	if pending[0].Event.Account != "unreadable" {
		t.Errorf("the spool kept %q; the event for the account that is gone should "+
			"have been discarded and the unreadable one kept",
			pending[0].Event.Account)
	}

	// And it stays discarded: a file that can never succeed must not come
	// back on the next sweep.
	if err := engine.ReplayHookSpool(); err != nil {
		t.Fatal(err)
	}
	pending, _ = hookspool.Pending(engine.hookSpool)
	if len(pending) != 1 {
		t.Errorf("a second replay left %d events", len(pending))
	}

	// The whole sequence the discard exists for: made and removed again
	// while the service was down. The remove is for a name this store has
	// never recorded, and must not stick either -- there is nothing to
	// retire, which is the answer, not a failure.
	for _, event := range []hookspool.Event{
		{At: time.Now().Add(-4 * time.Hour).UTC(), Event: "create", Account: "shortlived",
			Payload: []byte(`{"data":{"user":"shortlived"}}`)},
		{At: time.Now().Add(-3 * time.Hour).UTC(), Event: "remove", Account: "shortlived",
			Payload: []byte(`{"data":{"user":"shortlived"}}`)},
	} {
		if _, err := hookspool.Write(engine.hookSpool, event); err != nil {
			t.Fatalf("spool %s: %v", event.Event, err)
		}
	}
	engine.accountUID = func(name string) (int, error) {
		return 0, fmt.Errorf("node: %s is %w", name, ErrNoSuchAccount)
	}
	if err := engine.ReplayHookSpool(); err != nil {
		t.Fatal(err)
	}
	pending, _ = hookspool.Pending(engine.hookSpool)
	for _, entry := range pending {
		if entry.Event.Account == "shortlived" {
			t.Errorf("the %s for an account that was made and removed while the "+
				"service was down is still in the spool", entry.Event.Event)
		}
	}
}

// TestAReplayedCreateQueuesTheInitialBackup covers the half of the create
// hook that the spool replay was leaving out. A new account is given a
// baseline straight away rather than waiting for the next nightly run,
// and an account made while this service was down is the one that most
// needs it -- nothing at all ran for it. Recording the ownership boundary
// and stopping there left the account with no backup and nothing saying
// so.
func TestAReplayedCreateQueuesTheInitialBackup(t *testing.T) {
	engine, store := lifecycleEngine(t,
		map[string][]string{"latecomer": nil}, map[string]int{"latecomer": 1600})
	policy, err := store.PutPolicy(nodestore.Policy{
		Name: "Nightly", ScheduleCron: "0 2 * * *", Enabled: true,
		RepositoryIDs: []string{"repo-1"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := hookspool.Write(engine.hookSpool, hookspool.Event{
		At: time.Now().Add(-90 * time.Minute).UTC(), Event: "create", Account: "latecomer",
		Payload: []byte(`{"data":{"user":"latecomer"}}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.ReplayHookSpool(); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Identity("latecomer"); err != nil {
		t.Fatalf("the ownership boundary was not recorded: %v", err)
	}
	jobs, err := store.Jobs(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Account != "latecomer" || jobs[0].PolicyID != policy.ID {
		t.Fatalf("a replayed create left the account with no backup queued: %+v", jobs)
	}

	// And only once: the event is cleared, so a second sweep must not
	// queue the same baseline again.
	if err := engine.ReplayHookSpool(); err != nil {
		t.Fatal(err)
	}
	if jobs, _ := store.Jobs(0); len(jobs) != 1 {
		t.Errorf("a second replay queued the baseline again: %+v", jobs)
	}
}

// TestStartupClearsAPasswordFileAnInterruptedRunLeft is the wiring for
// resticrun.SweepPasswordDirs. A server whose service was restarted
// mid-backup was found holding two of these, each with nothing in it but
// the repository password, and nothing removed them on the way back up.
func TestStartupClearsAPasswordFileAnInterruptedRunLeft(t *testing.T) {
	root := t.TempDir()
	staging := filepath.Join(root, "staging")
	stale := filepath.Join(staging, "gniza-run-4038542850")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "repo.pass"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := nodestore.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	settings := nodestore.DefaultSettings()
	settings.StagingRoot = staging
	settings.ResticCache = filepath.Join(root, "cache")
	settings.ConfigDir = filepath.Join(root, "config")
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	v, err := vault.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{
		Store: store, Vault: v,
		Provider:  &cpanel.Fake{Root: filepath.Join(root, "cpanel")},
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		HookSpool: filepath.Join(root, "hooks"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the password file survived startup: %v", err)
	}
}
