package webui_test

import (
	"strings"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/agent"
	"github.com/shukiv/gniza/internal/node"
	"github.com/shukiv/gniza/internal/nodestore"
)

// seedRepository gives the pages something to render their forms
// against: a schedule form appears only once there is somewhere to write,
// and the restore page only once there is a repository to read.
func seedRepository(t *testing.T, engine *node.Engine) string {
	t.Helper()
	dest, err := engine.Store().PutDestination(nodestore.Destination{
		Name: "Backup disk", Type: "local", Config: map[string]string{"root": t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := engine.Store().PutRepository(nodestore.Repository{
		DestinationID: dest.ID, Path: "cp01",
	})
	if err != nil {
		t.Fatal(err)
	}
	return repo.ID
}

// A DirectAdmin server has no pkgacct, no restorepkg, no Restricted
// Restore, no WHM plugin and no cPanel hooks. Naming them tells an
// operator to go and look for something that is not on their machine,
// and -- worse on the restore page -- to expect a safeguard that is not
// running.
func TestNoPageNamesAMechanismThisPanelDoesNotHave(t *testing.T) {
	client, _, engine := newUIOnDirectAdmin(t)
	repository := seedRepository(t, engine)
	pages := []string{
		"/schedule", "/restore", "/accounts", "/jobs",
		"/restore?account=customer1&repository=" + repository,
		"/settings?tab=backups", "/settings?tab=version",
		"/settings?tab=storage", "/settings?tab=alerts",
	}
	absent := []string{"pkgacct", "restorepkg", "cpmove", "WHM plugin", "cPanel hook", "restricted-restore", "restricted restore"}
	for _, path := range pages {
		status, page := get(t, client, path)
		if status != 200 {
			t.Fatalf("GET %s = %d", path, status)
		}
		for _, word := range absent {
			if strings.Contains(strings.ToLower(page), strings.ToLower(word)) {
				t.Errorf("%s says %q on a DirectAdmin server", path, word)
			}
		}
	}
}

// And the same pages on cPanel keep saying it: these are cPanel's own
// mechanisms, and an operator there is told which one is about to run.
func TestTheCPanelPagesStillNameCPanelsOwnMechanisms(t *testing.T) {
	client, _, engine := newUI(t)
	seedRepository(t, engine)
	for path, want := range map[string]string{
		"/schedule":             "pkgacct",
		"/settings?tab=version": "WHM plugin",
		"/settings?tab=backups": "cPanel",
	} {
		status, page := get(t, client, path)
		if status != 200 {
			t.Fatalf("GET %s = %d", path, status)
		}
		if !strings.Contains(strings.ToLower(page), strings.ToLower(want)) {
			t.Errorf("%s on cPanel no longer says %q", path, want)
		}
	}
}

// TestTheVersionCardOffersTheInstallOnEitherPanel: a release publishes a
// package per panel, so a DirectAdmin server installs from here exactly
// as a cPanel one does. This is the page half of that -- a card that
// names a release and offers no way to install it leaves an operator
// looking for a command nobody wrote down.
func TestTheVersionCardOffersTheInstallOnEitherPanel(t *testing.T) {
	client, _, engine := newUIOnDirectAdmin(t)
	// The card offers an install only over a released build, so this is
	// one: with a development version running, nothing is ever offered
	// and the page would be silent whatever the panel.
	was := agent.Version
	agent.Version = "v1.2.3"
	t.Cleanup(func() { agent.Version = was })

	if err := engine.Store().SaveUpdateState(nodestore.UpdateState{
		CheckedAt: time.Now().UTC(), Version: "v99.0.0",
		URL: "https://github.com/shukiv/gniza/releases/tag/v99.0.0",
	}); err != nil {
		t.Fatal(err)
	}

	status, page := get(t, client, "/settings?tab=version")
	if status != 200 {
		t.Fatalf("GET /settings?tab=version = %d", status)
	}
	if !strings.Contains(page, "v99.0.0") {
		t.Fatal("the card does not say what has been released, so this proves nothing")
	}
	if !strings.Contains(page, "settings/update/install") {
		t.Error("a DirectAdmin server is not offered the release it could install")
	}
}
