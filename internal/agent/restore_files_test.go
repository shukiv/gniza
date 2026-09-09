package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/shukiv/gniza/internal/cpanel"
	"github.com/shukiv/gniza/internal/destination"
	"github.com/shukiv/gniza/internal/protocol"
	"github.com/shukiv/gniza/internal/resticrun"
	"github.com/shukiv/gniza/internal/staging"
)

// TestAFilesRestoreWithNowhereToPutThemKeepsWhatItReports covers the one
// restore kind that is allowed to say where its result goes.
//
// The box on the page is optional, and when it is left empty the files
// are written into the staging directory the restore was given. That
// directory is released when the run ends unless something says to keep
// it, and a files restore said nothing: the run reported success, named
// the path the files were in, and then deleted the path on its way out.
// An operator following the link found nothing there.
func TestAFilesRestoreWithNowhereToPutThemKeepsWhatItReports(t *testing.T) {
	root := t.TempDir()
	stagingRoot := filepath.Join(root, "staging")
	if err := os.MkdirAll(stagingRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	worker := New(Config{
		Provider: &cpanel.Fake{Root: filepath.Join(root, "cpanel")},
		Staging:  &staging.Manager{Root: stagingRoot},
		Runner: resticrun.New(resticrun.Config{RuntimeDir: root},
			archiveRestorer(t, "c1")),
		Log: slog.New(slog.DiscardHandler),
	})

	report := worker.RunRestore(context.Background(), protocol.RestoreAssignment{
		JobID: "restore-files", CPanelUser: "c1", SnapshotID: "aaaaaaaaaaaaaaaa",
		Kind: protocol.RestoreFiles, SizeEstimate: 1024,
		IncludePaths: []string{"/home/c1/public_html/index.php"},
		Source: protocol.Target{Spec: destination.Spec{Type: destination.TypeLocal,
			Config: map[string]string{"root": root}}, RepoPath: "repo",
			RepoPassword: "password"},
	})
	if report.Status != "success" {
		t.Fatalf("the restore did not finish: %+v", report)
	}
	if report.RestoredTo == "" {
		t.Fatal("the restore said success and named no place the files are in")
	}
	if _, err := os.Stat(report.RestoredTo); err != nil {
		t.Fatalf("the restore reported the files are in %s, and they are not: %v",
			report.RestoredTo, err)
	}
	// And what is left is output rather than work in progress: an
	// uncollected result that goes on holding a concurrency slot blocks
	// every other account.
	active, err := (&staging.Manager{Root: stagingRoot}).Active()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Errorf("the finished restore still counts as work in progress: %+v", active)
	}
}
