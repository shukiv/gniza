package webui

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/node"
	"github.com/shukiv/gniza/internal/resticrun"
)

// The restore page's two irreversible forms need a destination that has
// been read and a snapshot to restore, which a rendered-page test cannot
// arrange without a repository. This renders the template itself, which
// is where the copy lives.
func renderRestore(t *testing.T, panelName string, cpanel bool) string {
	t.Helper()
	sets, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	set, ok := sets["restore.html"]
	if !ok {
		t.Fatal("there is no restore page")
	}
	var out bytes.Buffer
	if err := set.ExecuteTemplate(&out, "layout", page{
		Title: "Restore", Panel: panelName, Cpanel: cpanel,
		Data: restoreView{
			Tab: "account", Account: "customer1", RepositoryID: "r1",
			Contents:  []storedAccount{{AccountBackups: node.AccountBackups{Account: "customer1"}, State: "live"}},
			Snapshots: []resticrun.Snapshot{{ID: "abc123", Time: time.Now()}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestTheRestorePageNamesOnlyMechanismsThisPanelHas(t *testing.T) {
	da := renderRestore(t, "DirectAdmin", false)
	for _, word := range []string{"restorepkg", "restricted restore", "restricted-restore", "cPanel"} {
		if strings.Contains(strings.ToLower(da), strings.ToLower(word)) {
			t.Errorf("the DirectAdmin restore page says %q", word)
		}
	}
	if !strings.Contains(da, "Whole-account restore") {
		t.Fatal("the whole-account restore form did not render")
	}
	if !strings.Contains(da, "DirectAdmin") {
		t.Error("the DirectAdmin restore page never names the panel it hands the archive to")
	}

	// And cPanel's own mechanisms stay named on cPanel: an operator
	// there is told which safeguard is about to be switched off.
	cp := renderRestore(t, "cPanel", true)
	for _, word := range []string{"restorepkg", "restricted"} {
		if !strings.Contains(strings.ToLower(cp), word) {
			t.Errorf("the cPanel restore page no longer says %q", word)
		}
	}
}
