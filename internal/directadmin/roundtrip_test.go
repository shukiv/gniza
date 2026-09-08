package directadmin

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/shukiv/gniza/internal/destination"
	"github.com/shukiv/gniza/internal/panel"
	"github.com/shukiv/gniza/internal/pkgacct"
	"github.com/shukiv/gniza/internal/reassemble"
	"github.com/shukiv/gniza/internal/resticrun"
)

// Real restic plus a native-command fixture: protects the integration between
// the provider's compressed output, snapshot classification and reassembly.
func TestNativeArchiveRepositoryRoundTrip(t *testing.T) {
	path, err := exec.LookPath("restic")
	if err != nil {
		t.Skip("restic is not installed")
	}
	r := nativeHost(t)
	ctx := t.Context()
	staging := privateStaging(t)
	payload, err := r.Stage(ctx, panel.StageRequest{Account: panel.AccountInfo{User: "studio"}, StagingDir: staging, Mode: pkgacct.ModeMonolithic})
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(payload.Parts[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	runner := resticrun.New(resticrun.Config{Binary: path, RuntimeDir: privateStaging(t), CacheDir: privateStaging(t)}, nil)
	repo := resticrun.Repository{Dest: &destination.Local{Root: privateStaging(t)}, Path: "fixture", Password: "local-test-only-password"}
	if err := runner.Init(ctx, repo, nil); err != nil {
		t.Fatal(err)
	}
	backup, err := runner.Backup(ctx, repo, resticrun.BackupSpec{Paths: payload.Paths(), Tags: []string{"account:studio"}, RecordCompletion: true})
	if err != nil || backup.Incomplete {
		t.Fatalf("backup incomplete: %+v %v", backup, err)
	}
	if err := runner.Check(ctx, repo, resticrun.CheckSpec{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(payload.Parts[0].Path, filepath.Join(staging, "held-original")); err != nil {
		t.Fatal(err)
	}
	result, err := reassemble.Run(ctx, runner, reassemble.Request{Account: "studio", SnapshotID: backup.Summary.SnapshotID, Layout: r.Layout(), WorkDir: privateStaging(t), Repo: repo})
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := os.ReadFile(result.ArchivePath)
	if err != nil || string(original) != string(rebuilt) {
		t.Fatalf("restored archive differs: %v", err)
	}
	if _, err := r.Apply(ctx, result.ArchivePath, panel.ApplyOptions{Overwrite: true, Unrestricted: true}); err != nil {
		t.Fatal(err)
	}
}
