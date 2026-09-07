package directadmin_test

import (
	"os"
	"strings"
	"testing"
)

// TestUninstallRetiresWhatItRemoves covers the difference between a
// removal and a deletion.
//
// An uninstaller runs on somebody else's production server, usually
// because something is wrong, and often by somebody who will want the
// old files back an hour later. Deleting the plugin directory outright
// leaves nothing to come back to: not the plugin's own configuration,
// not the binary that was running, not the unit file that started it.
//
// Everything this script takes off the server is moved aside into one
// directory that the script names on its way out, so that removing
// Gniza is undoable and deleting it stays a decision somebody makes on
// purpose.
func TestUninstallRetiresWhatItRemoves(t *testing.T) {
	scriptBytes, err := os.ReadFile("uninstall.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)

	for _, deletion := range []string{"rm -rf", "rm -f", "rmdir"} {
		if strings.Contains(script, deletion) {
			t.Errorf("the uninstaller deletes with %q instead of moving aside", deletion)
		}
	}

	for _, moved := range []string{"$PLUGIN_DIR", "$SERVICE", "gniza-agent", "gniza-hook"} {
		if !strings.Contains(script, moved) {
			t.Errorf("the uninstaller no longer accounts for %s", moved)
		}
	}
	if !strings.Contains(script, `mv -- "$1" "$ATTIC/`) {
		t.Error("the uninstaller does not move what it takes off the server into one place")
	}
	if !strings.Contains(script, `say "$ATTIC"`) && !strings.Contains(script, "$ATTIC") {
		t.Error("the uninstaller does not say where the retired files went")
	}
}
