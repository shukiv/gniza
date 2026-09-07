package reassemble

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestWhatCPanelIsHandedIsTheAccountItself.
//
// restorepkg takes an extracted directory as well as an archive, and it
// copies whatever it is given into a temporary directory of its own
// either way. So building the tar first buys nothing and costs a second
// full copy of the account on the same disk -- which is what put a
// 48.7 GiB account out of reach on a server with 63 GiB free.
func TestWhatCPanelIsHandedIsTheAccountItself(t *testing.T) {
	restorer, root := buildSplitSnapshot(t)
	workDir := filepath.Join(root, "work")

	result, err := Run(context.Background(), restorer, Request{
		Account: "customer1", SnapshotID: "40dc15203b1cf9aa", WorkDir: workDir,
		TreeOnly: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.RootDir == "" {
		t.Fatal("the rebuild does not say where the account is")
	}
	if filepath.Base(result.RootDir) != "cpmove-customer1" {
		t.Errorf("RootDir = %s, want the cpmove directory", result.RootDir)
	}
	// It is the cpuser file inside that restorepkg reads to know whose
	// account this is, so it has to be there.
	if _, err := os.Stat(filepath.Join(result.RootDir, "meta", "user")); err != nil {
		t.Errorf("the account directory has no cpanel user file: %v", err)
	}
}
