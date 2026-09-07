package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/reassemble"
)

// TestApplyingHandsOverTheAccountDirectory. restorepkg takes the extracted
// account as readily as a tar of it -- its usage lists
// "/path/to/extracted-cpuser-file" -- and copies whatever it is given into
// a temporary directory of its own either way. So a restore that goes
// straight to cPanel must not repack first: the tar would be a second full
// copy of the account on the same disk, which is what put a 48.7 GiB
// account out of reach on a server with 63 GiB free.
func TestApplyingHandsOverTheAccountDirectory(t *testing.T) {
	root := t.TempDir()
	accountDir := filepath.Join(root, "tree", "cpmove-customer1")
	if err := os.MkdirAll(accountDir, 0o700); err != nil {
		t.Fatal(err)
	}

	rebuilt := reassemble.Result{
		TreeDir: filepath.Join(root, "tree"),
		RootDir: accountDir,
	}
	if got := handOverPath(rebuilt); got != accountDir {
		t.Errorf("cPanel would be handed %q, want the account directory %q", got, accountDir)
	}

	// A monolithic snapshot is cPanel's own archive; there is nothing to
	// unpack it into, so the archive is what goes over.
	archive := filepath.Join(root, "cpmove-customer1.tar")
	if got := handOverPath(reassemble.Result{ArchivePath: archive}); got != archive {
		t.Errorf("a monolithic restore would hand over %q, want the archive", got)
	}
	if strings.HasSuffix(handOverPath(rebuilt), ".tar") {
		t.Error("a split restore still hands over a tar")
	}
}
