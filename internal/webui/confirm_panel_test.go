package webui

import (
	"strings"
	"testing"
)

// TestTheOverwriteWarningNamesThePanelItIsOn was wrong on every
// DirectAdmin server: the page that asks an operator to confirm replacing
// a live account told them the backup goes to cPanel's own restore. The
// warning above the one irreversible button on this interface has to name
// the thing that is actually about to run.
func TestTheOverwriteWarningNamesThePanelItIsOn(t *testing.T) {
	for _, name := range []string{"DirectAdmin", "cPanel"} {
		overwrite := confirmWholeAccount(name, "customer1", "abc123", "?p=restore", false, false)
		create := confirmWholeAccount(name, "customer1", "abc123", "?p=restore", true, false)
		many := confirmManyAccounts(name, []string{"customer1"}, "", "?p=restore", false)
		for what, warning := range map[string]string{
			"overwrite": overwrite.Warning,
			"create":    create.Warning,
			"bulk":      many.Warning,
		} {
			if !strings.Contains(warning, name) {
				t.Errorf("the %s warning on %s does not name it: %q", what, name, warning)
			}
			if name != "cPanel" && strings.Contains(warning, "cPanel") {
				t.Errorf("the %s warning on %s still says cPanel: %q", what, name, warning)
			}
		}
	}
}

// TestTheUnrestrictedNoteIsCPanelsAlone keeps a cPanel mechanism named
// after cPanel. Restricted Restore is cPanel's, and calling it
// DirectAdmin's would be a different wrong answer.
func TestTheUnrestrictedNoteIsCPanelsAlone(t *testing.T) {
	ask := confirmWholeAccount("cPanel", "customer1", "abc123", "?p=restore", false, true)
	joined := strings.Join(ask.Detail, " ")
	if !strings.Contains(joined, "restricted-restore") {
		t.Errorf("the unrestricted run is not described: %q", joined)
	}
}
