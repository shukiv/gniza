package node_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/cpanel"
	"github.com/shukiv/gniza/internal/granular"
	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/node"
	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/protocol"
	"github.com/shukiv/gniza/internal/resticrun"
	"github.com/shukiv/gniza/internal/staging"
	"github.com/shukiv/gniza/internal/vault"
)

// TestSourceOperationsStayOnTheAppendOnlyEndpoint protects the boundary
// between the cPanel server and storage. Browsing and initialising add data
// but never need deletion rights; routing either through the maintenance
// URL both exposes that URL to the source server and fails when it is
// correctly isolated on a management network.
func TestSourceOperationsStayOnTheAppendOnlyEndpoint(t *testing.T) {
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

	var repositories []string
	exec := resticrun.ExecFunc(func(_ context.Context, cmd resticrun.Command) (resticrun.CommandResult, error) {
		for _, env := range cmd.Env {
			if strings.HasPrefix(env, "RESTIC_REPOSITORY=") {
				repositories = append(repositories, strings.TrimPrefix(env, "RESTIC_REPOSITORY="))
			}
		}
		for _, arg := range cmd.Args {
			if arg == "snapshots" {
				return resticrun.CommandResult{Stdout: []byte("[]")}, nil
			}
		}
		return resticrun.CommandResult{}, nil
	})
	engine := newEngineWithExec(t, store, root, exec)

	_, repo, err := engine.AddDestination(nodestore.Destination{
		Name: "Append only", Type: "rest",
		Config: map[string]string{
			"base_url":             "https://backup.example.test",
			"maintenance_base_url": "https://delete.example.test",
			"append_only":          "true",
		},
	}, map[string]string{"username": "cp01", "password": "secret"}, "cp01")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.EnsureProvisioned(context.Background()); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if _, err := engine.Snapshots(context.Background(), repo.ID, "customer1"); err != nil {
		t.Fatalf("snapshots: %v", err)
	}

	if len(repositories) < 2 {
		t.Fatalf("captured repositories = %v", repositories)
	}
	for _, repository := range repositories {
		if strings.Contains(repository, "delete.example.test") {
			t.Fatalf("source operation used delete-capable endpoint: %s", repository)
		}
		if !strings.Contains(repository, "backup.example.test") {
			t.Errorf("source operation used unexpected endpoint: %s", repository)
		}
	}
}

// The multipliers are two copies plus a GiB: the account tree, and then
// either the tar repacked from it or the copy restorepkg makes beside
// whatever it is handed. It was three copies, counting a downloaded
// payload that split mode never writes -- restic restores each part
// straight into its slot in the tree.
func TestRestoreStagingUsesTheHistoricalSnapshotSize(t *testing.T) {
	const gib = uint64(1 << 30)
	if got := node.RestoreStagingEstimateForTest("account", gib, 10*gib); got != 21*gib {
		t.Fatalf("whole-account estimate = %d GiB, want 21", got/gib)
	}
	if got := node.RestoreStagingEstimateForTest("account", 12*gib, 10*gib); got != 25*gib {
		t.Fatalf("live-account estimate = %d GiB, want 25", got/gib)
	}
	if got := node.RestoreStagingEstimateForTest("items", 0, 0); got != 0 {
		t.Fatalf("unknown granular estimate = %d, want a refusal", got)
	}
}

func newEngine(t *testing.T, store *nodestore.Store, root string) *node.Engine {
	t.Helper()

	keyHex, err := vault.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(root, "master.key")
	if err := os.WriteFile(keyPath, []byte(keyHex), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := vault.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	v, err := vault.New(key)
	if err != nil {
		t.Fatal(err)
	}

	engine, err := node.New(node.Config{
		Store: store, Vault: v,
		Provider:  &cpanel.Fake{Root: filepath.Join(root, "cpanel")},
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		HookSpool: filepath.Join(root, "hooks"),
	})
	if err != nil {
		t.Fatalf("node.New: %v", err)
	}
	return engine
}

// TestRecoverFromRestartUnwedgesAnAccount covers the failure a standalone
// server has no lease expiry to catch: the process dies mid-backup, the job
// stays "running" forever, and every later backup of that account is
// skipped as busy — silently, every night.
func TestRecoverFromRestartUnwedgesAnAccount(t *testing.T) {
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

	// What a crash leaves: a job that never finished, a restore that never
	// finished, and their staging directories.
	crashed, err := store.PutJob(nodestore.Job{Account: "customer1", Status: job.StatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutRestore(nodestore.Restore{
		Account: "customer2", SnapshotID: "abc", Status: job.StatusRunning,
	}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"stage-customer1", "stage-restore-customer2"} {
		if err := os.MkdirAll(filepath.Join(settings.StagingRoot, key), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	// Something merely queued must survive: a restart should not empty
	// the queue.
	queued, err := store.PutJob(nodestore.Job{Account: "customer3", Status: job.StatusPending})
	if err != nil {
		t.Fatal(err)
	}

	engine := newEngine(t, store, root)

	if survived, err := store.Job(queued.ID); err != nil {
		t.Fatal(err)
	} else if survived.Status != job.StatusPending {
		t.Errorf("a queued job became %q on restart", survived.Status)
	}

	recovered, err := store.Job(crashed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != job.StatusFailed {
		t.Errorf("interrupted job status = %q, want failed", recovered.Status)
	}
	if recovered.StagingErr == "" {
		t.Error("the interrupted job does not say why it failed")
	}
	if recovered.FinishedAt == nil {
		t.Error("the interrupted job has no finish time")
	}

	restores, err := store.Restores(0)
	if err != nil {
		t.Fatal(err)
	}
	if restores[0].Status != job.StatusFailed {
		t.Errorf("interrupted restore status = %q, want failed", restores[0].Status)
	}

	// The debris is gone, so the next attempt can allocate its staging.
	entries, err := os.ReadDir(settings.StagingRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("staging still holds %d directories from the previous run", len(entries))
	}

	// And the account is no longer considered busy.
	if _, err := engine.QueueBackup("policy", "customer1"); err != nil {
		t.Errorf("the account is still wedged after recovery: %v", err)
	}
}

func TestQueueRestoreValidates(t *testing.T) {
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

	if _, err := engine.QueueRestore(nodestore.Restore{Account: "c1"}); err == nil {
		t.Error("a restore with no snapshot should be refused")
	}
	// A files restore with no paths would restore nothing at all.
	if _, err := engine.QueueRestore(nodestore.Restore{
		Account: "c1", SnapshotID: "abc", Kind: "files",
	}); err == nil {
		t.Error("a files restore with no paths should be refused")
	}
	// A files restore leaves the paths where the operator asked for them,
	// which is what "apply" would have meant; there is nothing else for it
	// to do.
	if _, err := engine.QueueRestore(nodestore.Restore{
		Account: "c1", SnapshotID: "abc", Kind: "files",
		IncludePaths: []string{"/home/c1/x"}, Apply: true,
	}); err == nil {
		t.Error("applying a files restore should be refused")
	}
	// Writing part of an account back is allowed only for the parts that
	// can be written back. Everything else is a change the control panel
	// has to make, and a granular restore of it is a copy to hand over.
	for _, kind := range []granular.Kind{
		granular.KindDNS, granular.KindSSL,
		granular.KindFTP, granular.KindDomains,
		granular.KindSettings, granular.KindSystem, granular.Kind("anything"),
	} {
		if _, err := engine.QueueRestore(nodestore.Restore{
			Account: "c1", SnapshotID: "abc", Kind: protocol.RestoreItems,
			ItemKind: string(kind), ItemNames: []string{"x"}, Apply: true,
		}); err == nil {
			t.Errorf("applying a %s restore should be refused", kind)
		}
	}
	// Every part of a basket is checked, not merely the first. Applying
	// the database and quietly leaving out the DNS is not what was asked
	// for, so the whole request is refused.
	if _, err := engine.QueueRestore(nodestore.Restore{
		Account: "c1", SnapshotID: "abc", Kind: protocol.RestoreItems,
		Items: []nodestore.RestoreSelection{
			{Kind: string(granular.KindDatabase), Names: []string{"c1_shop"}},
			{Kind: string(granular.KindDNS)},
		},
		Apply: true,
	}); err == nil {
		t.Error("applying a basket carrying DNS should be refused")
	}
	// A different account, because one account only ever has one piece of
	// work queued and c1's is asserted on below.
	applied, err := engine.QueueRestore(nodestore.Restore{
		Account: "c2", SnapshotID: "abc", Kind: protocol.RestoreItems,
		ItemKind:  string(granular.KindDatabase),
		ItemNames: []string{"c2_shop"}, Apply: true,
	})
	if err != nil {
		t.Fatalf("applying a database restore: %v", err)
	}
	if !applied.Apply {
		t.Error("the queued database restore lost its apply")
	}

	queued, err := engine.QueueRestore(nodestore.Restore{Account: "c1", SnapshotID: "abc"})
	if err != nil {
		t.Fatalf("QueueRestore: %v", err)
	}
	if queued.Status != job.StatusPending || queued.Apply {
		t.Errorf("queued = %+v", queued)
	}

	// One account, one piece of work: a backup and a restore would stage
	// on top of each other.
	if _, err := engine.QueueBackup("policy", "c1"); err == nil {
		t.Error("a backup should not start while a restore of that account is queued")
	}
}

// TestOpenArchiveForDownloadRefusesSymlinkEscapes covers the case a lexical
// containment check misses: the path is inside the staging root by name but
// resolves somewhere else. This handler reads as root on a server whose
// other users are not trusted, so it has to resolve before it decides.
func TestOpenArchiveForDownloadRefusesSymlinkEscapes(t *testing.T) {
	root := t.TempDir()
	store, err := nodestore.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	staging := filepath.Join(root, "staging")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	settings := nodestore.DefaultSettings()
	settings.StagingRoot = staging
	settings.ResticCache = filepath.Join(root, "cache")
	settings.ConfigDir = filepath.Join(root, "config")
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, store, root)

	secret := filepath.Join(root, "secret.tar")
	if err := os.WriteFile(secret, []byte("not yours"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A symlink sitting inside the staging root, pointing out of it.
	escape := filepath.Join(staging, "cpmove-escape.tar")
	if err := os.Symlink(secret, escape); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	// And a sibling directory whose name merely starts with the root's.
	sibling := staging + "-elsewhere"
	if err := os.MkdirAll(sibling, 0o700); err != nil {
		t.Fatal(err)
	}
	siblingArchive := filepath.Join(sibling, "cpmove-c1.tar")
	if err := os.WriteFile(siblingArchive, []byte("also not yours"), 0o600); err != nil {
		t.Fatal(err)
	}

	for name, path := range map[string]string{
		"symlink out of staging": escape,
		"sibling directory":      siblingArchive,
		"outside entirely":       secret,
	} {
		restore, err := store.PutRestore(nodestore.Restore{
			Account: "customer1", SnapshotID: "abc",
			Status: job.StatusSuccess, ArchivePath: path,
		})
		if err != nil {
			t.Fatal(err)
		}
		file, _, _, err := engine.OpenArchiveForDownload(restore.ID)
		if err == nil {
			file.Close()
			t.Errorf("%s was served", name)
		}
	}

	// A real archive in the right place still works.
	good := filepath.Join(staging, "stage-restore-customer1", "cpmove-customer1.tar")
	if err := os.MkdirAll(filepath.Dir(good), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(good, []byte("a real archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	restore, err := store.PutRestore(nodestore.Restore{
		Account: "customer1", SnapshotID: "abc",
		Status: job.StatusSuccess, ArchivePath: good,
	})
	if err != nil {
		t.Fatal(err)
	}
	file, filename, size, err := engine.OpenArchiveForDownload(restore.ID)
	if err != nil {
		t.Fatalf("a legitimate archive was refused: %v", err)
	}
	defer file.Close()
	if filename != "cpmove-customer1.tar" || size != int64(len("a real archive")) {
		t.Errorf("filename=%q size=%d", filename, size)
	}
}

func TestDrillRefusesWhenTheVolumeIsTooFull(t *testing.T) {
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
	settings.MaxConcurrent = 1
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, store, root)

	// A rehearsal writes a full copy of the account into scratch. On a
	// server that is nearly full — which is the normal state of a cPanel
	// box — it has to be refused rather than allowed to fill the volume.
	if _, err := engine.QueueDrill(context.Background(), "customer1"); err == nil {
		t.Error("a drill was queued for an account with no backup")
	}

	// And a drill must not run alongside other work for the same account.
	if _, err := store.PutJob(nodestore.Job{
		Account: "customer1", Status: job.StatusRunning,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.QueueRestore(nodestore.Restore{
		Account: "customer1", SnapshotID: "abc", Kind: node.KindVerify,
	}); err == nil {
		t.Error("a drill was queued while that account was already busy")
	}
}

// Collected output is swept once nobody has come back for it, and work in
// progress is never touched however old it looks.
func TestSweepRemovesOldOutputAndLeavesWorkAlone(t *testing.T) {
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

	manager := &staging.Manager{Root: settings.StagingRoot, MaxConcurrent: 4}
	old, err := manager.Allocate("restore-old", 1<<10)
	if err != nil {
		t.Fatal(err)
	}
	oldOutput, err := manager.Retain(old)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := manager.Allocate("restore-fresh", 1<<10)
	if err != nil {
		t.Fatal(err)
	}
	freshOutput, err := manager.Retain(fresh)
	if err != nil {
		t.Fatal(err)
	}
	working, err := manager.Allocate("customer1", 1<<10)
	if err != nil {
		t.Fatal(err)
	}

	// Only age decides, so the old one is aged rather than waited for.
	longAgo := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(oldOutput.Path, longAgo, longAgo); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(working.Path, longAgo, longAgo); err != nil {
		t.Fatal(err)
	}

	if err := engine.SweepWorkdir(); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if _, err := os.Stat(oldOutput.Path); !os.IsNotExist(err) {
		t.Error("output nobody collected is still there")
	}
	if _, err := os.Stat(freshOutput.Path); err != nil {
		t.Error("output produced today was swept")
	}
	if _, err := os.Stat(working.Path); err != nil {
		t.Error("a directory being worked in was swept")
	}
}

// newEngineWithExec builds an engine whose restic is a stand-in, so the
// paths that only happen when restic fails can be exercised.
func newEngineWithExec(t *testing.T, store *nodestore.Store, root string, exec resticrun.Execer) *node.Engine {
	t.Helper()
	keyHex, err := vault.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(root, "master.key")
	if err := os.WriteFile(keyPath, []byte(keyHex), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := vault.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	v, err := vault.New(key)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := node.New(node.Config{
		Store: store, Vault: v, Exec: exec,
		Provider:  &cpanel.Fake{Root: filepath.Join(root, "cpanel")},
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		HookSpool: filepath.Join(root, "hooks"),
	})
	if err != nil {
		t.Fatalf("node.New: %v", err)
	}
	return engine
}

// attachedRepository is a local destination with a repository on it, which
// is the least a retention test needs to open one.
func attachedRepository(t *testing.T, store *nodestore.Store, engine *node.Engine) nodestore.Repository {
	t.Helper()
	_, repo, err := engine.AddDestination(nodestore.Destination{
		Name: "Local", Type: "local",
		Config: map[string]string{"root": t.TempDir()},
	}, nil, "backups")
	if err != nil {
		t.Fatalf("add destination: %v", err)
	}
	// Retention only looks at repositories that exist on the far end.
	now := time.Now().UTC()
	repo.InitialisedAt = &now
	if _, err := store.PutRepository(repo); err != nil {
		t.Fatal(err)
	}
	return repo
}

// A deleted account's backups are the only ones nothing else ever removes:
// retention thins a series and always keeps something. Without a life they
// accumulate on a destination somebody pays for, forever.
func TestDeletedAccountsAreKeptForTheConfiguredTime(t *testing.T) {
	for _, tc := range []struct {
		days int
		want time.Duration
	}{
		{days: 0, want: nodestore.DefaultDeletedAccountDays * 24 * time.Hour},
		{days: 30, want: 30 * 24 * time.Hour},
		{days: 365, want: 365 * 24 * time.Hour},
		{days: -1, want: 0}, // kept until somebody says otherwise
	} {
		settings := nodestore.Settings{DeletedAccountDays: tc.days}
		if got := settings.KeepDeletedAccountsFor(); got != tc.want {
			t.Errorf("%d days = %v, want %v", tc.days, got, tc.want)
		}
	}
	if nodestore.DefaultDeletedAccountDays != 90 {
		t.Errorf("the default is %d days, and the guides say ninety",
			nodestore.DefaultDeletedAccountDays)
	}
}
