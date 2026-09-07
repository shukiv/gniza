package reassemble

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/layout/cpmove"
)

// TestARehearsalNeedsNoArchive.
//
// A rehearsal answers one question: can this backup be turned back into an
// account? The tree answers it. Repacking that tree into a tar answers
// nothing further and doubles the disk the rehearsal needs -- which is why
// a 48.7 GiB account could not be rehearsed on a server with 63 GiB free.
func TestARehearsalNeedsNoArchive(t *testing.T) {
	restorer, root := buildSplitSnapshot(t)
	workDir := filepath.Join(root, "work")

	result, err := Run(context.Background(), restorer, Request{
		Layout:  cpmove.Layout{},
		Account: "customer1", SnapshotID: "40dc15203b1cf9aa", WorkDir: workDir,
		TreeOnly: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ArchivePath != "" {
		t.Errorf("a rehearsal repacked the tree anyway: %s", result.ArchivePath)
	}
	entries, err := os.ReadDir(workDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tar") {
			t.Errorf("a tar was written after all: %s", entry.Name())
		}
	}

	// The tree is still whole, which is what is being verified.
	tree := filepath.Join(workDir, "tree", "cpmove-customer1")
	for _, want := range []string{
		filepath.Join(tree, "version"),
		filepath.Join(tree, cpmove.HomedirDir, "public_html", "index.html"),
		filepath.Join(tree, cpmove.DatabaseDir, "customer1_wp.sql"),
	} {
		if _, err := os.Stat(want); err != nil {
			t.Errorf("the tree is incomplete: %v", err)
		}
	}

	// And it verifies, without an archive to point at.
	checks, err := Verify(result)
	if err != nil {
		t.Fatalf("a rehearsal of a good backup failed: %v", err)
	}
	joined := strings.Join(checks, "; ")
	for _, want := range []string{"account tree present", "files in the home directory", "database dumps parse"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the rehearsal does not report %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "archive present") {
		t.Errorf("a rehearsal without an archive claims one: %s", joined)
	}
}
