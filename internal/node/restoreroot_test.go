package node_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/nodestore"
)

// closedDir is a directory nobody but its owner can write to, all the
// way up. The test's own temporary directory is not one where the
// machine's umask leaves a directory open to its group, so this one is
// made under /tmp, which is sticky.
func closedDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gniza-restore-root-")
	if err != nil {
		t.Skipf("no closed directory to test in: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// The finding from the fleet check of 2026-09-28: on one server staging
// sat on a volume with 7.4 GiB free while another had 38, and nothing
// could be restored or rehearsed that did not fit in the first. Staging
// cannot move, because its path is in every backup. Restores can.
func TestRestoresAreRebuiltWhereTheOperatorSays(t *testing.T) {
	engine, store, _ := checkedEngine(t, &checkedRestic{})
	staging := engine.Settings().StagingRoot
	if engine.RestoreRoot() != staging {
		t.Fatalf("restores start in %s, not in staging", engine.RestoreRoot())
	}

	larger := filepath.Join(closedDir(t), "volume", "gniza-restores")
	if err := engine.SetRestoreRoot(larger); err != nil {
		t.Fatalf("SetRestoreRoot: %v", err)
	}
	if engine.RestoreRoot() != larger {
		t.Errorf("restores are rebuilt in %s", engine.RestoreRoot())
	}
	info, err := os.Stat(larger)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("the directory was not made for root alone: %v %v", info, err)
	}
	saved, err := store.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if saved.RestoreRoot != larger || saved.StagingRoot != staging {
		t.Errorf("saved restore root %q, staging %q", saved.RestoreRoot, saved.StagingRoot)
	}

	// Something finished and waiting there is somebody's download.
	if err := os.MkdirAll(filepath.Join(larger, "keep-restore-alice@1"), 0o700); err != nil {
		t.Fatal(err)
	}
	outputs, err := engine.RetainedOutput()
	if err != nil || len(outputs) != 1 {
		t.Fatalf("what is waiting in the restore directory is not listed: %v %v", outputs, err)
	}
	if err := engine.SetRestoreRoot(""); err == nil {
		t.Error("the directory was left with a download still in it")
	}
	if err := engine.DeleteOutput("restore-alice@1"); err != nil {
		t.Fatal(err)
	}
	if err := engine.SetRestoreRoot(""); err != nil {
		t.Fatalf("going back to staging: %v", err)
	}
	if engine.RestoreRoot() != staging {
		t.Errorf("restores are rebuilt in %s after going back", engine.RestoreRoot())
	}
}

func TestADirectoryAnAccountCanReachIsRefused(t *testing.T) {
	engine, store, _ := checkedEngine(t, &checkedRestic{})
	open := filepath.Join(closedDir(t), "open")
	if err := os.Mkdir(open, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	used := filepath.Join(closedDir(t), "home")
	if err := os.MkdirAll(filepath.Join(used, "alice"), 0o711); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"with accounts in it":             used,
		"inside staging":                  filepath.Join(engine.Settings().StagingRoot, "restores"),
		"not a whole path":                "restores",
		"the top of the filesystem":       "/",
		"under a directory all may write": filepath.Join(open, "restores"),
		"with a line break in it":         "/var/lib/gniza\n/restores",
		"inside Gniza's own settings":     filepath.Join(engine.Settings().ConfigDir, "restores"),
	} {
		if err := engine.SetRestoreRoot(path); err == nil {
			t.Errorf("%s was accepted: %q", name, path)
		}
	}
	if engine.RestoreRoot() != engine.Settings().StagingRoot {
		t.Errorf("a refused path moved the restores to %s", engine.RestoreRoot())
	}

	// And not while something is running: its files are being written to
	// the directory that would be left.
	if _, err := store.PutJob(nodestore.Job{Account: "alice", Status: job.StatusRunning}); err != nil {
		t.Fatal(err)
	}
	err := engine.SetRestoreRoot(filepath.Join(closedDir(t), "restores"))
	if err == nil || !strings.Contains(err.Error(), "running") {
		t.Errorf("the directory was changed under a running job: %v", err)
	}
}
