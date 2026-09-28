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
	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/node"
	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/resticrun"
	"github.com/shukiv/gniza/internal/vault"
)

const day = 24 * time.Hour

func backedUp(accounts ...string) []nodestore.Job {
	var jobs []nodestore.Job
	for _, account := range accounts {
		jobs = append(jobs, nodestore.Job{Account: account, Status: job.StatusSuccess,
			Targets: []nodestore.JobTarget{{SnapshotID: "aaaaaaaaaaaaaaaa", Status: job.TargetSuccess}}})
	}
	return jobs
}

func rehearsed(account string, ago time.Duration, status job.Status, now time.Time) nodestore.Restore {
	at := now.Add(-ago)
	return nodestore.Restore{Account: account, Kind: node.KindVerify, Status: status,
		QueuedAt: at, FinishedAt: &at}
}

// The fleet check of 2026-09-22 found four servers where no account had
// ever been rehearsed. The rule that picks the next one is what decides
// whether every account has been within the month.
func TestTheAccountThatHasWaitedLongestIsRehearsedNext(t *testing.T) {
	now := time.Date(2026, 9, 28, 23, 0, 0, 0, time.UTC)
	everyone := []string{"carol", "alice", "bob", "dave", "@system"}

	for _, c := range []struct {
		name     string
		jobs     []nodestore.Job
		restores []nodestore.Restore
		want     string
	}{
		{"nobody has a backup", nil, nil, ""},
		{"nobody has been rehearsed", backedUp("carol", "alice", "bob"), nil, "alice"},
		{"the server's own settings are not an account",
			backedUp("@system"), nil, ""},
		{"an account that has left is not rehearsed",
			backedUp("erin"), nil, ""},
		{"one never rehearsed comes before one that is overdue",
			backedUp("alice", "bob"),
			[]nodestore.Restore{rehearsed("alice", 90*day, job.StatusSuccess, now)}, "bob"},
		{"the one rehearsed longest ago",
			backedUp("alice", "bob", "carol"),
			[]nodestore.Restore{
				rehearsed("alice", 40*day, job.StatusSuccess, now),
				rehearsed("bob", 55*day, job.StatusSuccess, now),
				rehearsed("carol", 3*day, job.StatusSuccess, now),
			}, "bob"},
		{"everybody has been within the month",
			backedUp("alice", "bob"),
			[]nodestore.Restore{
				rehearsed("alice", 29*day, job.StatusSuccess, now),
				rehearsed("bob", day, job.StatusSuccess, now),
			}, ""},
		{"one that failed last night is not tried again tonight",
			backedUp("alice"),
			[]nodestore.Restore{rehearsed("alice", day, job.StatusFailed, now)}, ""},
		{"one that failed a week ago is",
			backedUp("alice"),
			[]nodestore.Restore{rehearsed("alice", 8*day, job.StatusFailed, now)}, "alice"},
		{"the newest rehearsal is the one that counts",
			backedUp("alice"),
			[]nodestore.Restore{
				rehearsed("alice", 60*day, job.StatusSuccess, now),
				rehearsed("alice", 2*day, job.StatusSuccess, now),
			}, ""},
		{"a restore is not a rehearsal",
			backedUp("alice"),
			[]nodestore.Restore{{Account: "alice", Kind: "account", Status: job.StatusSuccess,
				QueuedAt: now.Add(-day)}}, "alice"},
	} {
		got, due := node.RehearsalDueForTest(everyone, c.jobs, c.restores, now, 30*day)
		if got != c.want || due != (c.want != "") {
			t.Errorf("%s: %q (due %v), want %q", c.name, got, due, c.want)
		}
	}
}

// snapshotsOf answers restic's snapshots with one backup of each account.
func snapshotsOf(accounts ...string) resticrun.ExecFunc {
	return func(_ context.Context, cmd resticrun.Command) (resticrun.CommandResult, error) {
		if len(cmd.Args) == 0 || cmd.Args[0] != "snapshots" {
			return resticrun.CommandResult{}, nil
		}
		var rows []string
		for _, account := range accounts {
			rows = append(rows, `{"id":"aaaaaaaaaaaaaaaa","time":"2026-09-28T02:00:00Z",`+
				`"tags":["account:`+account+`"]}`)
		}
		return resticrun.CommandResult{Stdout: []byte("[" + strings.Join(rows, ",") + "]")}, nil
	}
}

// rehearsingEngine is a server with these accounts on it, one backup of
// each, and somewhere to keep them.
func rehearsingEngine(t *testing.T, accounts ...string) (*node.Engine, *nodestore.Store, nodestore.Repository) {
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
	settings.Hostname = "test.example.com"
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
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
	sealed, err := vault.New(key)
	if err != nil {
		t.Fatal(err)
	}
	owned := map[string][]string{}
	for _, account := range accounts {
		owned[account] = nil
	}
	engine, err := node.New(node.Config{
		Store: store, Vault: sealed, Exec: snapshotsOf(accounts...),
		Provider:  &cpanel.Fake{Root: filepath.Join(root, "cpanel"), Databases: owned},
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		HookSpool: filepath.Join(root, "hooks"),
	})
	if err != nil {
		t.Fatalf("node.New: %v", err)
	}
	return engine, store, attachedRepository(t, store, engine)
}

func rehearsals(t *testing.T, store *nodestore.Store) []nodestore.Restore {
	t.Helper()
	restores, err := store.Restores(0)
	if err != nil {
		t.Fatal(err)
	}
	var found []nodestore.Restore
	for _, restore := range restores {
		if restore.Kind == node.KindVerify {
			found = append(found, restore)
		}
	}
	return found
}

// A rehearsal is queued at night, when nothing is running and no backup
// is about to start, and one at a time.
func TestARehearsalIsQueuedAtNightWhenTheServerIsIdle(t *testing.T) {
	const account = "customer1"
	engine, store, repo := rehearsingEngine(t, account)
	made := time.Now().Add(-10 * day).UTC()
	repo.InitialisedAt = &made
	if _, err := store.PutRepository(repo); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutJob(nodestore.Job{Account: account, Status: job.StatusSuccess,
		Targets: []nodestore.JobTarget{{RepositoryID: repo.ID, SnapshotID: "aaaaaaaaaaaaaaaa",
			Status: job.TargetSuccess}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutPolicy(nodestore.Policy{
		Name: "Nightly", ScheduleCron: "0 2 * * *", Enabled: true,
		RepositoryIDs: []string{repo.ID},
	}); err != nil {
		t.Fatal(err)
	}

	at := func(hour, minute int) time.Time {
		return time.Date(2026, 9, 29, hour, minute, 0, 0, time.Local)
	}
	for name, when := range map[string]time.Time{
		"in the middle of the day":          at(14, 0),
		"an hour before the backups start":  at(1, 0),
		"a minute before the night is over": at(6, 0),
	} {
		if chosen, err := engine.RehearseOnceForTest(t.Context(), when); err != nil || chosen != "" {
			t.Errorf("%s a rehearsal of %q was queued (%v)", name, chosen, err)
		}
	}
	if found := rehearsals(t, store); len(found) != 0 {
		t.Fatalf("rehearsals were queued outside their hours: %+v", found)
	}

	// While a backup is running, not even at night.
	running, err := store.PutJob(nodestore.Job{Account: "somebody", Status: job.StatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	if chosen, _ := engine.RehearseOnceForTest(t.Context(), at(4, 0)); chosen != "" {
		t.Errorf("a rehearsal of %q was queued beside a running backup", chosen)
	}
	running.Status = job.StatusSuccess
	if _, err := store.PutJob(running); err != nil {
		t.Fatal(err)
	}

	chosen, err := engine.RehearseOnceForTest(t.Context(), at(4, 0))
	if err != nil || chosen != account {
		t.Fatalf("at four in the morning %q was chosen (%v), want %q", chosen, err, account)
	}
	found := rehearsals(t, store)
	if len(found) != 1 || !found[0].Scheduled || found[0].Account != account ||
		found[0].SnapshotID != "aaaaaaaaaaaaaaaa" || found[0].Status.Terminal() {
		t.Fatalf("what was queued: %+v", found)
	}

	// One at a time: the one that is queued is work in flight.
	if chosen, _ := engine.RehearseOnceForTest(t.Context(), at(4, 10)); chosen != "" {
		t.Errorf("a second rehearsal, of %q, was queued behind the first", chosen)
	}

	// And not at all on a server that was told to leave them alone.
	settings, err := store.Settings()
	if err != nil {
		t.Fatal(err)
	}
	settings.RehearseDays = -1
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	done := found[0]
	finished := time.Now().Add(-60 * day).UTC()
	done.Status, done.FinishedAt = job.StatusSuccess, &finished
	if _, err := store.PutRestore(done); err != nil {
		t.Fatal(err)
	}
	if chosen, _ := engine.RehearseOnceForTest(t.Context(), at(4, 20)); chosen != "" {
		t.Errorf("a rehearsal of %q was queued on a server that does not schedule them", chosen)
	}
}

// On 182.54.236.148 staging shares a volume with the one account the
// server holds, and a rehearsal of it takes 50 GiB of the 59.7 that are
// free. Somebody who presses the button has decided that. The schedule
// has not, and leaves it alone with the reason written down.
func TestAScheduledRehearsalLeavesHalfTheDiskAlone(t *testing.T) {
	root := t.TempDir()
	if err := node.RoomToRehearseForTest("alice", 1<<20, root); err != nil {
		t.Errorf("a megabyte was refused: %v", err)
	}
	err := node.RoomToRehearseForTest("alice", 1<<62, root)
	if err == nil {
		t.Fatal("a rehearsal larger than the disk was let through")
	}
	for _, want := range []string{"alice", root, "by hand", "Storage"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
}

func TestRehearsalsAreMonthlyUnlessToldOtherwise(t *testing.T) {
	for days, want := range map[int]time.Duration{
		0: 30 * day, 7: 7 * day, 90: 90 * day, -1: 0,
	} {
		if got := (nodestore.Settings{RehearseDays: days}).RehearseEvery(); got != want {
			t.Errorf("%d days: every %v, want %v", days, got, want)
		}
	}
}
