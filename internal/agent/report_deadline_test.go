package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/cpanel"
	"github.com/shukiv/gniza/internal/destination"
	"github.com/shukiv/gniza/internal/protocol"
	"github.com/shukiv/gniza/internal/resticrun"
	"github.com/shukiv/gniza/internal/staging"
)

// TestWorkThatOutLastsTheReportBudgetStillReportsIt.
//
// The deadline a finished job gets to say what happened used to be taken
// out before the work started. A backup of a real account runs for
// minutes or hours, so by the time there was anything to report the
// deadline had passed and the report was never sent: the lease expired,
// the controller queued the job again, and the agent ran the whole thing
// once more. For a restore that had already written into a live account,
// it wrote into it again.
func TestWorkThatOutLastsTheReportBudgetStillReportsIt(t *testing.T) {
	root := t.TempDir()
	stagingRoot := filepath.Join(root, "staging")
	if err := os.MkdirAll(stagingRoot, 0o700); err != nil {
		t.Fatal(err)
	}

	var reported atomic.Bool
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == protocol.PathRestoreReport {
			reported.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{})
	}))
	defer controller.Close()

	was := reportBudget
	reportBudget = 200 * time.Millisecond
	t.Cleanup(func() { reportBudget = was })

	worker := New(Config{
		Client:   NewClientWithHTTP(controller.URL, controller.Client()),
		Provider: &cpanel.Fake{Root: filepath.Join(root, "cpanel")},
		Staging:  &staging.Manager{Root: stagingRoot},
		Runner: resticrun.New(resticrun.Config{RuntimeDir: root},
			slowRestorer(t, "c1", 500*time.Millisecond)),
		Log: slog.New(slog.DiscardHandler),
	})
	// Long enough that the heartbeat never fires: what is under test is
	// the report, not the renewal.
	worker.LeaseRenewEvery = time.Hour

	worker.execute(context.Background(), protocol.Assignment{
		Kind: protocol.KindRestore,
		Restore: &protocol.RestoreAssignment{
			JobID: "restore-slow", CPanelUser: "c1", SnapshotID: "aaaaaaaaaaaaaaaa",
			Kind: protocol.RestoreAccount, SizeEstimate: 1024,
			Source: protocol.Target{Spec: destination.Spec{Type: destination.TypeLocal,
				Config: map[string]string{"root": root}}, RepoPath: "repo",
				RepoPassword: "password"},
		},
	})

	if !reported.Load() {
		t.Fatal("the restore finished and the controller was never told, " +
			"so the lease expires and the whole restore is run again")
	}
}

// slowRestorer answers like archiveRestorer, taking longer over it than
// the report budget allows.
func slowRestorer(t *testing.T, account string, takes time.Duration) resticrun.Execer {
	t.Helper()
	inner := archiveRestorer(t, account)
	return resticrun.ExecFunc(func(ctx context.Context, cmd resticrun.Command) (resticrun.CommandResult, error) {
		time.Sleep(takes)
		return inner.Exec(ctx, cmd)
	})
}
