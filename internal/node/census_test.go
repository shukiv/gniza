package node_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/node"
	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/resticrun"
)

func censusEngineWith(t *testing.T, exec resticrun.Execer) (*nodestore.Store, nodestore.Repository, *node.Engine) {
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
	engine := newEngineWithExec(t, store, root, exec)
	return store, attachedRepository(t, store, engine), engine
}

// What a repository holds is a question restic answers slowly and over
// the network, so the page reads it from here rather than asking.
func TestWhatARepositoryHoldsIsMeasuredAndKept(t *testing.T) {
	store, repo, engine := censusEngineWith(t, resticrun.ExecFunc(
		func(_ context.Context, _ resticrun.Command) (resticrun.CommandResult, error) {
			return resticrun.CommandResult{Stdout: []byte(
				`{"total_size":103079215104,"snapshots_count":294}`)}, nil
		}))

	engine.TakeCensusForTest(context.Background(), time.Now())

	after, err := store.Repository(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Census.Snapshots != 294 || after.Census.SizeBytes != 103079215104 {
		t.Fatalf("census = %+v", after.Census)
	}
	if after.Census.MeasuredAt == nil {
		t.Fatal("nothing recorded when the reading was taken, so the page cannot say")
	}
	if after.Census.LastError != "" {
		t.Errorf("a measurement that worked left an error: %q", after.Census.LastError)
	}
}

// A destination that cannot be reached tonight does not make yesterday's
// figure untrue. Keeping it, with the reason beside it, is the point of
// storing the reading at all.
func TestAFailedMeasurementKeepsTheLastGoodOne(t *testing.T) {
	failing := resticrun.ExecFunc(func(_ context.Context, _ resticrun.Command) (resticrun.CommandResult, error) {
		return resticrun.CommandResult{ExitCode: 1,
			Stderr: []byte("Fatal: unable to open config file")}, nil
	})
	store, repo, engine := censusEngineWith(t, failing)

	// A reading from long enough ago that the census is due again.
	measured := time.Now().Add(-30 * 24 * time.Hour).UTC()
	repo.Census = nodestore.RepositoryCensus{
		Snapshots: 294, SizeBytes: 103079215104, MeasuredAt: &measured,
	}
	if _, err := store.PutRepository(repo); err != nil {
		t.Fatal(err)
	}

	engine.TakeCensusForTest(context.Background(), time.Now())

	after, err := store.Repository(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Census.Snapshots != 294 || after.Census.SizeBytes != 103079215104 {
		t.Fatalf("a failed reading discarded the last good one: %+v", after.Census)
	}
	if after.Census.MeasuredAt == nil || !after.Census.MeasuredAt.Equal(measured) {
		t.Fatalf("the reading was redated by a measurement that did not happen: %+v", after.Census)
	}
	if after.Census.LastError == "" || after.Census.AttemptedAt == nil {
		t.Fatalf("nothing says why the figure is old: %+v", after.Census)
	}
}

// The census reads a repository record, spends minutes on the network,
// and writes it back. Anything else that wrote to that record in between
// -- retention approving a plan, most of all -- must survive.
func TestAMeasurementDoesNotOverwriteWhatHappenedWhileItRan(t *testing.T) {
	var store *nodestore.Store
	var repoID string
	slow := resticrun.ExecFunc(func(_ context.Context, _ resticrun.Command) (resticrun.CommandResult, error) {
		// Stand in for the operator approving retention while restic is
		// still walking the index.
		stored, err := store.Repository(repoID)
		if err != nil {
			return resticrun.CommandResult{}, err
		}
		approved := time.Now().UTC()
		stored.RetentionApprovedAt = &approved
		if _, err := store.PutRepository(stored); err != nil {
			return resticrun.CommandResult{}, err
		}
		return resticrun.CommandResult{Stdout: []byte(
			`{"total_size":103079215104,"snapshots_count":294}`)}, nil
	})
	var repo nodestore.Repository
	var engine *node.Engine
	store, repo, engine = censusEngineWith(t, slow)
	repoID = repo.ID

	engine.TakeCensusForTest(context.Background(), time.Now())

	after, err := store.Repository(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.RetentionApprovedAt == nil {
		t.Fatal("the measurement wrote back a record it had read minutes earlier, undoing an approval")
	}
	if after.Census.Snapshots != 294 {
		t.Fatalf("the measurement was lost: %+v", after.Census)
	}
}

// Measuring is expensive, so a repository measured recently is left
// alone: the census runs on every scheduler tick and must cost nothing
// on almost all of them.
func TestARepositoryMeasuredRecentlyIsNotMeasuredAgain(t *testing.T) {
	calls := 0
	counting := resticrun.ExecFunc(func(_ context.Context, _ resticrun.Command) (resticrun.CommandResult, error) {
		calls++
		return resticrun.CommandResult{Stdout: []byte(
			`{"total_size":1,"snapshots_count":1}`)}, nil
	})
	_, _, engine := censusEngineWith(t, counting)

	now := time.Now()
	engine.TakeCensusForTest(context.Background(), now)
	engine.TakeCensusForTest(context.Background(), now.Add(time.Minute))
	if calls != 1 {
		t.Fatalf("restic was run %d times for one repository within the hour", calls)
	}
}

// How much room a destination has left is worth knowing whether or not
// anybody has set up somewhere to be told about it. The probe used to sit
// inside the notification watch, which returns early when no channel is
// enabled -- so on a server with no email configured, the space column
// stayed empty forever and nobody could see the disk filling up.
func TestDestinationSpaceIsMeasuredWithoutANotificationChannel(t *testing.T) {
	store, repo, engine := censusEngineWith(t, resticrun.ExecFunc(
		func(_ context.Context, _ resticrun.Command) (resticrun.CommandResult, error) {
			return resticrun.CommandResult{Stdout: []byte(
				`{"total_size":1,"snapshots_count":1}`)}, nil
		}))
	channels, err := store.Channels()
	if err != nil {
		t.Fatal(err)
	}
	for _, channel := range channels {
		if channel.Enabled {
			t.Fatalf("this server was meant to have nothing listening: %s", channel.Name)
		}
	}

	if _, err := engine.Schedule(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}

	dest, err := store.Destination(repo.DestinationID)
	if err != nil {
		t.Fatal(err)
	}
	if dest.LastCheckedAt == nil {
		t.Fatal("the destination was never reached for")
	}
	if dest.Space.MeasuredAt == nil {
		t.Fatalf("nothing measured the room left on it: %+v", dest.Space)
	}
}
