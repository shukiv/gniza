package node_test

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/shukiv/gniza/internal/node"
	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/resticrun"
)

// chunkerFixture is a store and an engine whose restic is a recording
// fake: every backup succeeds, and the commands are kept for the test to
// read back.
func chunkerFixture(t *testing.T) (*nodestore.Store, engineHandle, func() string) {
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

	var mu sync.Mutex
	var commands []string
	engine := newEngineWithExec(t, store, root,
		resticrun.ExecFunc(func(_ context.Context, cmd resticrun.Command) (resticrun.CommandResult, error) {
			mu.Lock()
			commands = append(commands, strings.Join(cmd.Args, " "))
			mu.Unlock()
			for _, arg := range cmd.Args {
				switch arg {
				case "snapshots":
					return resticrun.CommandResult{Stdout: []byte("[]")}, nil
				case "backup":
					return resticrun.CommandResult{Stdout: []byte(
						`{"message_type":"summary","snapshot_id":"aaaaaaaaaaaaaaaa",` +
							`"total_bytes_processed":1024,"data_added":512}`)}, nil
				}
			}
			return resticrun.CommandResult{}, nil
		}))
	ran := func() string {
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(commands, " | ")
	}
	return store, engine, ran
}

type engineHandle = *node.Engine

// backUpTo runs one backup of customer1 against the repository and
// returns the repository as it is stored afterwards.
func backUpTo(t *testing.T, store *nodestore.Store, engine engineHandle, repoID string) nodestore.Repository {
	t.Helper()
	policy, err := store.PutPolicy(nodestore.Policy{
		Name: "Nightly", ScheduleCron: "0 2 * * *", Enabled: true,
		RepositoryIDs: []string{repoID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.QueueBackup(policy.ID, "customer1"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the queued backup: %v", err)
	}
	stored, err := store.Repository(repoID)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

// TestARepositoryWhoseChunkerSourceIsGoneIsStillCreated covers a second
// destination added while the first still existed, and the first removed
// before the second's repository was created.
//
// The second repository copies its chunker parameters from the first, and
// the record naming the first is gone. Seen on a live server: every backup
// failed with "node: open chunker source: nodestore: not found", and the
// page said only that the repository does not exist. There is nothing left
// to copy from, so the repository is created on its own.
func TestARepositoryWhoseChunkerSourceIsGoneIsStillCreated(t *testing.T) {
	store, engine, ran := chunkerFixture(t)

	first, _, err := engine.AddDestination(nodestore.Destination{
		Name: "First", Type: "local", Config: map[string]string{"root": t.TempDir()},
	}, nil, "backups")
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := engine.AddDestination(nodestore.Destination{
		Name: "Second", Type: "local", Config: map[string]string{"root": t.TempDir()},
	}, nil, "backups")
	if err != nil {
		t.Fatal(err)
	}
	if second.ChunkerSourceRepoID == "" {
		t.Fatal("the fixture is wrong: the second repository names no chunker source")
	}
	if err := store.DeleteDestination(first.ID); err != nil {
		t.Fatal(err)
	}

	stored := backUpTo(t, store, engine, second.ID)
	if stored.InitialisedAt == nil {
		t.Errorf("the repository was not created: %s", ran())
	}
	if strings.Contains(ran(), "--from-repo") {
		t.Errorf("restic was pointed at a repository that no longer exists: %s", ran())
	}
	if stored.ChunkerSourceRepoID != "" {
		t.Errorf("the repository still names %s as its chunker source", stored.ChunkerSourceRepoID)
	}
}

// TestARepositoryThatCannotShareAnInvocationWithItsChunkerSourceIsStillCreated
// covers two SFTP destinations with keys of their own.
//
// restic applies -o globally, so two repositories whose sftp.args differ
// cannot be opened in one process, and "init --from-repo" opens both. Seen
// on a live server: a destination added beside the old one, with a key
// prepared for it, and every backup failing with "conflicting backend
// option sftp.args" until the operator gave up. Matching chunker
// parameters make copies between repositories cheaper; they are not worth
// a destination that never gets a repository.
func TestARepositoryThatCannotShareAnInvocationWithItsChunkerSourceIsStillCreated(t *testing.T) {
	store, engine, ran := chunkerFixture(t)

	sftp := func(name, key string) map[string]string {
		return map[string]string{
			"host": "backups.example", "user": "arkady", "root": "/home/arkady/",
			"identity_file":    filepath.Join(t.TempDir(), key),
			"known_hosts_file": filepath.Join(t.TempDir(), name+".known_hosts"),
		}
	}
	_, first, err := engine.AddDestination(nodestore.Destination{
		Name: "Old", Type: "sftp", Config: sftp("old", "old-key"),
	}, nil, "backups")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkRepositoryInitialised(first.ID); err != nil {
		t.Fatal(err)
	}
	_, second, err := engine.AddDestination(nodestore.Destination{
		Name: "New", Type: "sftp", Config: sftp("new", "new-key"),
	}, nil, "backups")
	if err != nil {
		t.Fatal(err)
	}
	if second.ChunkerSourceRepoID != first.ID {
		t.Fatalf("the fixture is wrong: chunker source = %q, want %q", second.ChunkerSourceRepoID, first.ID)
	}

	stored := backUpTo(t, store, engine, second.ID)
	if stored.InitialisedAt == nil {
		t.Errorf("the repository was not created: %s", ran())
	}
	if !strings.Contains(ran(), "init") {
		t.Errorf("restic was never asked to create the repository: %s", ran())
	}
	if stored.ChunkerSourceRepoID != "" {
		t.Errorf("the repository still names %s as its chunker source", stored.ChunkerSourceRepoID)
	}
}
