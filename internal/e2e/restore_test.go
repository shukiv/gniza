//go:build e2e

package e2e_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/layout/cpmove"
	"github.com/shukiv/gniza/internal/maintenance"
	"github.com/shukiv/gniza/internal/protocol"
	"github.com/shukiv/gniza/internal/store"
)

// TestRestoreRoundTrip is the question a backup product exists to answer:
// after backing an account up, can its bytes be got back?
func TestRestoreRoundTrip(t *testing.T) {
	h := newHarness(t)
	ctx := h.ctx

	if _, err := h.maintenance.ProvisionPending(ctx); err != nil {
		t.Fatalf("provision: %v", err)
	}
	runJobFor(t, h)

	original := readTree(t, filepath.Join(h.provider.Root, "home", "customer1"))
	if len(original) == 0 {
		t.Fatal("the fake account has no files to compare against")
	}

	snapshots := listSnapshots(t, h, h.localRepoID)
	if len(snapshots) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(snapshots))
	}

	restoreID, err := h.db.CreateRestore(ctx, store.RestoreRequest{
		AccountID:    h.accountID,
		RepositoryID: h.localRepoID,
		SnapshotID:   snapshots[0].ID,
	})
	if err != nil {
		t.Fatalf("CreateRestore: %v", err)
	}

	report := runOneRestore(t, h, restoreID)
	if report.Status != string(job.StatusSuccess) {
		t.Fatalf("restore failed: %s", report.Error)
	}
	if report.Applied {
		// apply defaults to off; a restore must not touch a live account
		// unless an operator asked.
		t.Error("the restore was applied without being asked to")
	}
	if len(h.provider.Applied) != 0 {
		t.Errorf("restorepkg was invoked: %v", h.provider.Applied)
	}
	if report.ArchivePath == "" {
		t.Fatal("no archive path reported; an operator would not know where the restore went")
	}

	// The rebuilt tree sits beside the archive. Every file the account had
	// must be back, byte for byte.
	tree := filepath.Join(filepath.Dir(report.ArchivePath), "tree")
	root, err := soleDir(tree)
	if err != nil {
		t.Fatalf("restored tree: %v", err)
	}
	restored := readTree(t, filepath.Join(root, cpmove.HomedirDir))
	compareTrees(t, original, restored)

	// The database dumps came back too.
	for _, database := range []string{"customer1_wp", "customer1_shop"} {
		path := filepath.Join(root, cpmove.DatabaseDir, database+".sql")
		body, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read restored dump %s: %v", database, err)
			continue
		}
		if !strings.Contains(string(body), "CREATE TABLE") {
			t.Errorf("dump %s does not look like SQL: %q", database, body)
		}
	}

	// And the account configuration from the metadata archive.
	if body, err := os.ReadFile(filepath.Join(root, "meta", "user")); err != nil {
		t.Errorf("read restored metadata: %v", err)
	} else if strings.TrimSpace(string(body)) != "customer1" {
		t.Errorf("restored metadata names %q", body)
	}

	stored, err := h.db.RestoreByID(ctx, restoreID)
	if err != nil {
		t.Fatalf("RestoreByID: %v", err)
	}
	if stored.Status != job.StatusSuccess || stored.BytesRestored == 0 {
		t.Errorf("recorded restore = %+v", stored)
	}
}

// TestRepeatedRestoreSupersedesTheLastArchive checks that a second restore
// of an account is not wedged by the archive the first one deliberately
// left behind.
func TestRepeatedRestoreSupersedesTheLastArchive(t *testing.T) {
	h := newHarness(t)
	ctx := h.ctx

	if _, err := h.maintenance.ProvisionPending(ctx); err != nil {
		t.Fatalf("provision: %v", err)
	}
	runJobFor(t, h)
	snapshots := listSnapshots(t, h, h.localRepoID)

	queue := func() protocol.RestoreReport {
		t.Helper()
		id, err := h.db.CreateRestore(ctx, store.RestoreRequest{
			AccountID: h.accountID, RepositoryID: h.localRepoID,
			SnapshotID: snapshots[0].ID,
		})
		if err != nil {
			t.Fatalf("CreateRestore: %v", err)
		}
		return runOneRestore(t, h, id)
	}

	first := queue()
	if first.Status != string(job.StatusSuccess) {
		t.Fatalf("first restore failed: %s", first.Error)
	}
	if _, err := os.Stat(first.ArchivePath); err != nil {
		t.Fatalf("the first restore's archive is not where it said: %v", err)
	}

	second := queue()
	if second.Status != string(job.StatusSuccess) {
		t.Fatalf("second restore failed: %s", second.Error)
	}
	if second.ArchivePath == first.ArchivePath {
		t.Error("a new restore reused the older restore's download path")
	}
	if _, err := os.Stat(first.ArchivePath); !os.IsNotExist(err) {
		t.Errorf("the superseded restore's archive still exists: %v", err)
	}
	if _, err := os.Stat(second.ArchivePath); err != nil {
		t.Errorf("the second restore's archive is missing: %v", err)
	}
}

// TestRestoreSingleFile covers the request operators actually make most
// often: one file back, without touching the rest of the account.
func TestRestoreSingleFile(t *testing.T) {
	h := newHarness(t)
	ctx := h.ctx

	if _, err := h.maintenance.ProvisionPending(ctx); err != nil {
		t.Fatalf("provision: %v", err)
	}
	runJobFor(t, h)

	home := filepath.Join(h.provider.Root, "home", "customer1")
	wanted := filepath.Join(home, "public_html", "page-03.html")
	original, err := os.ReadFile(wanted)
	if err != nil {
		t.Fatalf("read the file to be restored: %v", err)
	}

	snapshots := listSnapshots(t, h, h.localRepoID)
	target := filepath.Join(t.TempDir(), "recovered")

	restoreID, err := h.db.CreateRestore(ctx, store.RestoreRequest{
		AccountID:    h.accountID,
		RepositoryID: h.localRepoID,
		SnapshotID:   snapshots[0].ID,
		Kind:         protocol.RestoreFiles,
		IncludePaths: []string{wanted},
		TargetDir:    target,
	})
	if err != nil {
		t.Fatalf("CreateRestore: %v", err)
	}

	report := runOneRestore(t, h, restoreID)
	if report.Status != string(job.StatusSuccess) {
		t.Fatalf("restore failed: %s", report.Error)
	}
	if report.RestoredTo != target {
		t.Errorf("restored to %q, want %q", report.RestoredTo, target)
	}

	// A files restore keeps the original path, so the operator can see
	// where it came from.
	recovered := filepath.Join(target, strings.TrimPrefix(wanted, "/"))
	body, err := os.ReadFile(recovered)
	if err != nil {
		t.Fatalf("read recovered file: %v", err)
	}
	if string(body) != string(original) {
		t.Error("the recovered file does not match the original")
	}

	// And nothing else came with it.
	var count int
	_ = filepath.WalkDir(target, func(_ string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			count++
		}
		return nil
	})
	if count != 1 {
		t.Errorf("restored %d files, want only the one requested", count)
	}
}

// TestRestoreAppliesWhenAsked checks the opt-in path reaches cPanel.
func TestRestoreAppliesWhenAsked(t *testing.T) {
	h := newHarness(t)
	ctx := h.ctx

	if _, err := h.maintenance.ProvisionPending(ctx); err != nil {
		t.Fatalf("provision: %v", err)
	}
	runJobFor(t, h)

	snapshots := listSnapshots(t, h, h.localRepoID)
	restoreID, err := h.db.CreateRestore(ctx, store.RestoreRequest{
		AccountID:    h.accountID,
		RepositoryID: h.localRepoID,
		SnapshotID:   snapshots[0].ID,
		Apply:        true,
	})
	if err != nil {
		t.Fatalf("CreateRestore: %v", err)
	}

	report := runOneRestore(t, h, restoreID)
	if report.Status != string(job.StatusSuccess) {
		t.Fatalf("restore failed: %s", report.Error)
	}
	if !report.Applied {
		t.Error("the report does not record that the restore was applied")
	}
	if len(h.provider.Applied) != 1 {
		t.Fatalf("restorepkg was invoked %d times, want once", len(h.provider.Applied))
	}
	// The account directory, not a tar of it. restorepkg takes either and
	// copies whatever it is given into a temporary directory of its own,
	// so repacking first would put a second full copy of the account on
	// the same disk for nothing.
	handed := h.provider.Applied[0]
	if filepath.Base(handed) != "cpmove-customer1" {
		t.Errorf("restorepkg was handed %q, want the extracted account directory", handed)
	}
	// Asked at hand-over time, because staging is swept the moment the
	// restore finishes and by now the path is gone whatever it was.
	if !h.provider.AppliedDirectory[0] {
		t.Errorf("restorepkg was handed %q, which was not a directory", handed)
	}
}

// TestRestoreFromAppendOnlyEndpoint closes the loop on the append-only
// design: writes are restricted, reads never were.
func TestRestoreFromAppendOnlyEndpoint(t *testing.T) {
	h := newHarness(t)
	ctx := h.ctx

	if _, err := h.maintenance.ProvisionPending(ctx); err != nil {
		t.Fatalf("provision: %v", err)
	}
	runJobFor(t, h)

	snapshots := listSnapshots(t, h, h.restRepoID)
	restoreID, err := h.db.CreateRestore(ctx, store.RestoreRequest{
		AccountID:    h.accountID,
		RepositoryID: h.restRepoID,
		SnapshotID:   snapshots[0].ID,
	})
	if err != nil {
		t.Fatalf("CreateRestore: %v", err)
	}

	report := runOneRestore(t, h, restoreID)
	if report.Status != string(job.StatusSuccess) {
		t.Fatalf("restore from the append-only endpoint failed: %s", report.Error)
	}
	if report.BytesRestored == 0 {
		t.Error("nothing was restored")
	}
}

// TestRestoreDrill checks the rehearsal the maintenance runner performs, and
// that it leaves a record.
func TestRestoreDrill(t *testing.T) {
	h := newHarness(t)
	ctx := h.ctx

	if _, err := h.maintenance.ProvisionPending(ctx); err != nil {
		t.Fatalf("provision: %v", err)
	}
	runJobFor(t, h)

	result, err := h.maintenance.Drill(ctx, maintenance.DrillRequest{
		RepositoryID: h.localRepoID,
		Account:      "customer1",
		WorkDir:      filepath.Join(t.TempDir(), "drill"),
	})
	if err != nil {
		t.Fatalf("Drill: %v", err)
	}
	if result.Account != "customer1" || result.SnapshotID == "" {
		t.Errorf("result = %+v", result)
	}
	if len(result.Checks) < 3 {
		t.Errorf("drill made only %d checks: %v", len(result.Checks), result.Checks)
	}

	// The rehearsal must be recorded, or nobody learns that restores work.
	var (
		status string
		output string
	)
	if err := h.db.Pool().QueryRow(ctx, `
		SELECT status::text, coalesce(output, '')
		  FROM maintenance_runs
		 WHERE repository_id = $1 AND kind = 'drill'
		 ORDER BY started_at DESC LIMIT 1`, h.localRepoID).Scan(&status, &output); err != nil {
		t.Fatalf("read the drill record: %v", err)
	}
	if status != string(job.StatusSuccess) {
		t.Errorf("recorded drill status = %q", status)
	}
	if !strings.Contains(output, "home directory") {
		t.Errorf("recorded drill says %q, want the checks it made", output)
	}

	// Scratch space is not a deliverable.
	if _, err := os.Stat(filepath.Join(t.TempDir(), "drill")); !os.IsNotExist(err) {
		t.Error("the drill left its scratch directory behind")
	}
}

// runOneRestore polls for the queued restore, runs it, and reports through
// the real API.
func runOneRestore(t *testing.T, h *harness, wantJobID string) protocol.RestoreReport {
	t.Helper()

	ctx, cancel := context.WithTimeout(h.ctx, 3*time.Minute)
	defer cancel()

	work, err := h.agentClient.NextWork(ctx)
	if err != nil {
		t.Fatalf("poll for work: %v", err)
	}
	if work.Kind != protocol.KindRestore {
		t.Fatalf("received %s work, want a restore", work.Kind)
	}
	if work.Restore.JobID != wantJobID {
		t.Fatalf("received restore %s, want %s", work.Restore.JobID, wantJobID)
	}

	report := h.worker.RunRestore(ctx, *work.Restore)

	reportCtx, cancelReport := context.WithTimeout(h.ctx, 30*time.Second)
	defer cancelReport()
	if err := h.agentClient.ReportRestore(reportCtx, report); err != nil {
		t.Fatalf("report restore: %v", err)
	}
	return report
}

// readTree reads every file under root, keyed by path relative to root.
func readTree(t *testing.T, root string) map[string]string {
	t.Helper()
	contents := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		contents[relative] = string(body)
		return nil
	})
	if err != nil {
		t.Fatalf("read tree %s: %v", root, err)
	}
	return contents
}

func compareTrees(t *testing.T, original, restored map[string]string) {
	t.Helper()
	for path, want := range original {
		got, present := restored[path]
		if !present {
			t.Errorf("%s is missing from the restore", path)
			continue
		}
		if got != want {
			t.Errorf("%s differs after restore (%d bytes vs %d)", path, len(got), len(want))
		}
	}
	for path := range restored {
		if _, expected := original[path]; !expected {
			t.Errorf("the restore produced an unexpected file %s", path)
		}
	}
}

func soleDir(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(dir, entry.Name()), nil
		}
	}
	return "", os.ErrNotExist
}
