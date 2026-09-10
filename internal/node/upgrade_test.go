package node_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/agent"
	"github.com/shukiv/gniza/internal/cpanel"
	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/node"
	"github.com/shukiv/gniza/internal/nodestore"
)

// TestStartUpgradeOnlyInstallsTheReleaseItWasToldAbout covers the guards
// in front of the one thing here that fetches something and runs it as
// root. None of these reach the network.
func TestStartUpgradeOnlyInstallsTheReleaseItWasToldAbout(t *testing.T) {
	root := t.TempDir()
	store, err := nodestore.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	settings := nodestore.DefaultSettings()
	settings.StagingRoot = filepath.Join(root, "staging")
	settings.ResticCache = filepath.Join(root, "cache")
	settings.ConfigDir = filepath.Join(root, "config")
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, store, root)

	was := agent.Version
	agent.Version = "v1.2.3"
	t.Cleanup(func() { agent.Version = was })

	if err := store.SaveUpdateState(nodestore.UpdateState{
		CheckedAt: time.Now().UTC(), Version: "v1.3.0",
	}); err != nil {
		t.Fatal(err)
	}

	refused := map[string]string{
		"a version nobody published":  "v9.9.9",
		"a build of somebody's own":   "v1.3.0-2-gabc1234-dirty",
		"not a version at all":        "; rm -rf /",
		"nothing":                     "",
		"the version already running": "v1.2.3",
	}
	for what, version := range refused {
		if err := engine.StartUpgrade(version); err == nil {
			t.Errorf("%s was installed: %q", what, version)
		}
	}
	state, err := store.UpgradeState()
	if err != nil {
		t.Fatal(err)
	}
	if !state.StartedAt.IsZero() {
		t.Errorf("a refused upgrade was recorded as started: %+v", state)
	}
}

// TestStartUpgradeWaitsForABackupThatIsRunning: installing restarts the
// service, and a restart fails whatever was in flight. Better to be told
// to come back than to find tonight's backup marked interrupted.
func TestStartUpgradeWaitsForABackupThatIsRunning(t *testing.T) {
	root := t.TempDir()
	store, err := nodestore.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	settings := nodestore.DefaultSettings()
	settings.StagingRoot = filepath.Join(root, "staging")
	settings.ResticCache = filepath.Join(root, "cache")
	settings.ConfigDir = filepath.Join(root, "config")
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, store, root)

	was := agent.Version
	agent.Version = "v1.2.3"
	t.Cleanup(func() { agent.Version = was })

	if err := store.SaveUpdateState(nodestore.UpdateState{
		CheckedAt: time.Now().UTC(), Version: "v1.3.0",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutJob(nodestore.Job{Account: "customer1", Status: job.StatusRunning}); err != nil {
		t.Fatal(err)
	}

	err = engine.StartUpgrade("v1.3.0")
	if err == nil {
		t.Fatal("an upgrade started while a backup was running")
	}
	if !strings.Contains(err.Error(), "customer1") {
		t.Errorf("the reason does not name what is running: %v", err)
	}
	state, err := store.UpgradeState()
	if err != nil {
		t.Fatal(err)
	}
	if !state.StartedAt.IsZero() {
		t.Errorf("it was recorded as started anyway: %+v", state)
	}
}

// TestUpgradeStatusReadsWhatTheInstallerLeft is how an upgrade is
// reported at all: the process that started one is replaced by it, so
// what happened is read back off disk.
func TestUpgradeStatusReadsWhatTheInstallerLeft(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
		failed bool
		kept   bool
	}{
		{"it worked", "0\n", false, false},
		{"it did not", "1\n", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			store, err := nodestore.Open(filepath.Join(root, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			settings := nodestore.DefaultSettings()
			settings.StagingRoot = filepath.Join(root, "staging")
			settings.ResticCache = filepath.Join(root, "cache")
			settings.ConfigDir = filepath.Join(root, "config")
			if err := store.SaveSettings(settings); err != nil {
				t.Fatal(err)
			}
			engine := newEngine(t, store, root)

			dir := filepath.Join(root, "upgrade", "v1.3.0")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "status"), []byte(tc.status), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "install.log"),
				[]byte("WHM plugin directory: /usr/local/cpanel/whostmgr/docroot/cgi\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := store.SaveUpgradeState(nodestore.UpgradeState{
				Version: "v1.3.0", From: "v1.2.3", Stage: "installing",
				StartedAt: time.Now().UTC().Add(-time.Minute), Dir: dir,
			}); err != nil {
				t.Fatal(err)
			}

			state, err := engine.UpgradeStatus()
			if err != nil {
				t.Fatalf("UpgradeStatus: %v", err)
			}
			if state.FinishedAt.IsZero() {
				t.Fatal("an upgrade that has ended was reported as still running")
			}
			if state.Failed != tc.failed {
				t.Errorf("failed = %v, want %v (%s)", state.Failed, tc.failed, state.Error)
			}
			if state.Stage != "" {
				t.Errorf("stage = %q, want it cleared", state.Stage)
			}
			if !strings.Contains(state.Log, "WHM plugin directory") {
				t.Errorf("the installer's output was not kept: %q", state.Log)
			}
			// It is read once and remembered: the second read must say
			// the same thing after the unpacked copy has been removed.
			again, err := engine.UpgradeStatus()
			if err != nil {
				t.Fatal(err)
			}
			if again.Failed != tc.failed || again.FinishedAt.IsZero() {
				t.Errorf("the second reading disagrees: %+v", again)
			}
			_, err = os.Stat(dir)
			if kept := err == nil; kept != tc.kept {
				t.Errorf("the unpacked release kept = %v, want %v", kept, tc.kept)
			}
		})
	}
}

// TestTheDistChannelIsOrderedByCommit: builds of a branch have no version
// numbers that mean anything, so what says one is later than another is
// the commit each was made from.
func TestTheDistChannelIsOrderedByCommit(t *testing.T) {
	root := t.TempDir()
	store, err := nodestore.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	settings := nodestore.DefaultSettings()
	settings.StagingRoot = filepath.Join(root, "staging")
	settings.ResticCache = filepath.Join(root, "cache")
	settings.ConfigDir = filepath.Join(root, "config")
	settings.UpdateChannel = "dist"
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, store, root)

	wasVersion, wasBuilt := agent.Version, agent.BuiltAt
	agent.Version = "v0.1.0-18-gabc1234"
	agent.BuiltAt = "2026-09-06T10:00:00Z"
	t.Cleanup(func() { agent.Version, agent.BuiltAt = wasVersion, wasBuilt })
	running, _ := agent.Built()

	later := nodestore.UpdateState{
		Channel: "dist", Version: "v0.1.0-19-gdef5678",
		BuiltAt: running.Add(time.Hour), CheckedAt: time.Now().UTC(),
	}
	if !engine.UpdateOffered(later) {
		t.Error("a build from a later commit was not offered")
	}
	// The same commit is not an update, and an earlier one is a way back
	// to a program this server has already left behind.
	for what, state := range map[string]nodestore.UpdateState{
		"the same commit": {Channel: "dist", Version: "v0.1.0-18-gabc1234", BuiltAt: running},
		"an earlier one":  {Channel: "dist", Version: "v0.1.0-9-g0000000", BuiltAt: running.Add(-time.Hour)},
		"one that says nothing about when it was built": {
			Channel: "dist", Version: "v0.1.0-99-gfffffff"},
	} {
		if engine.UpdateOffered(state) {
			t.Errorf("%s was offered as an update", what)
		}
	}

	// A release found before the channel was changed is not installable
	// on this channel: it was read from somewhere else.
	if err := store.SaveUpdateState(nodestore.UpdateState{
		Channel: "releases", Version: "v9.9.9", CheckedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.StartUpgrade("v9.9.9"); err == nil {
		t.Error("a release was installed while following the branch")
	}
}

// TestAVersionThatNamesTheDirectoryAboveIsNotInstalled: the version is
// used as the name of the directory a release is unpacked into, and that
// directory is removed before it is written to. On the dist channel the
// version is whatever the signed manifest says, and ".." would name
// /var/lib/gniza itself -- the state file, the schedules and the history.
func TestAVersionThatNamesTheDirectoryAboveIsNotInstalled(t *testing.T) {
	root := t.TempDir()
	store, err := nodestore.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	settings := nodestore.DefaultSettings()
	settings.StagingRoot = filepath.Join(root, "staging")
	settings.ResticCache = filepath.Join(root, "cache")
	settings.ConfigDir = filepath.Join(root, "config")
	settings.UpdateChannel = "dist"
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, store, root)

	was := agent.Version
	agent.Version = "v1.2.3-4-gabc1234"
	t.Cleanup(func() { agent.Version = was })

	for _, version := range []string{"..", "."} {
		if err := store.SaveUpdateState(nodestore.UpdateState{
			CheckedAt: time.Now().UTC(), Channel: "dist",
			Version: version, BuiltAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
		err := engine.StartUpgrade(version)
		if err == nil {
			t.Fatalf("%q was accepted as a build to install", version)
		}
		if !strings.Contains(err.Error(), "is not a build this server can install") {
			t.Fatalf("%q was refused for the wrong reason: %v", version, err)
		}
	}
}

// unknownPanelFake is the fake provider under the name of a panel no
// release publishes a package for. What the upgrade path needs to know
// about a panel is only what it is called: the name chooses the package,
// and a name with no package is a server that cannot install from here.
type unknownPanelFake struct{ *cpanel.Fake }

func (unknownPanelFake) Name() string { return "Plesk" }

// TestAnUpgradeIsRefusedWhereTheReleaseCarriesNoPackageForThisPanel: a
// release publishes one package per panel, and installing another panel's
// puts a plugin on a machine with nothing to show it while leaving the
// panel that is there without one. Say so instead of running it.
func TestAnUpgradeIsRefusedWhereTheReleaseCarriesNoPackageForThisPanel(t *testing.T) {
	root := t.TempDir()
	store, err := nodestore.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	settings := nodestore.DefaultSettings()
	settings.StagingRoot = filepath.Join(root, "staging")
	settings.ResticCache = filepath.Join(root, "cache")
	settings.ConfigDir = filepath.Join(root, "config")
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	engine := newEngineOnPanel(t, store, root,
		unknownPanelFake{&cpanel.Fake{Root: filepath.Join(root, "cpanel")}})

	was := agent.Version
	agent.Version = "v1.2.3"
	t.Cleanup(func() { agent.Version = was })

	if err := store.SaveUpdateState(nodestore.UpdateState{
		CheckedAt: time.Now().UTC(), Version: "v1.3.0",
	}); err != nil {
		t.Fatal(err)
	}

	err = engine.StartUpgrade("v1.3.0")
	if err == nil {
		t.Fatal("a release with no package for this panel was installed")
	}
	if !strings.Contains(err.Error(), "Plesk") {
		t.Errorf("the refusal does not say which panel this server is on: %v", err)
	}
	state, err := store.UpgradeState()
	if err != nil {
		t.Fatal(err)
	}
	if !state.StartedAt.IsZero() {
		t.Errorf("a refused upgrade was recorded as started: %+v", state)
	}
}

// TestTheWrapperRunsTheInstallerThatWasUnpacked: the transient unit runs
// this script as root and nothing else decides what gets installed. It
// used to name the cPanel tree outright, so on a DirectAdmin server it
// ran an installer that was not there. It now runs whichever package the
// download left beside it -- there is one -- and records what it said.
func TestTheWrapperRunsTheInstallerThatWasUnpacked(t *testing.T) {
	for _, tree := range []string{"cprest-plugin", "gniza-directadmin"} {
		t.Run(tree, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, tree), 0o700); err != nil {
				t.Fatal(err)
			}
			installed := filepath.Join(dir, "installed")
			if err := os.WriteFile(filepath.Join(dir, tree, "install.sh"),
				[]byte("#!/bin/sh\necho ran > "+installed+"\nexit 0\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			wrapper := filepath.Join(dir, "run.sh")
			if err := os.WriteFile(wrapper, []byte(node.InstallerWrapperForTest), 0o700); err != nil {
				t.Fatal(err)
			}

			if out, err := exec.Command("sh", wrapper).CombinedOutput(); err != nil {
				t.Fatalf("running the wrapper: %v: %s", err, out)
			}
			if _, err := os.Stat(installed); err != nil {
				t.Errorf("the installer in %s was not run: %v", tree, err)
			}
			status, err := os.ReadFile(filepath.Join(dir, "status"))
			if err != nil {
				t.Fatalf("the wrapper recorded no status: %v", err)
			}
			if strings.TrimSpace(string(status)) != "0" {
				t.Errorf("the wrapper recorded status %q", status)
			}
		})
	}

	// A directory with no package in it installs nothing and says so,
	// rather than reporting the success of a script that never ran.
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(wrapper, []byte(node.InstallerWrapperForTest), 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sh", wrapper).CombinedOutput(); err != nil {
		t.Fatalf("running the wrapper: %v: %s", err, out)
	}
	status, err := os.ReadFile(filepath.Join(dir, "status"))
	if err != nil {
		t.Fatalf("the wrapper recorded no status: %v", err)
	}
	if strings.TrimSpace(string(status)) == "0" {
		t.Error("a release that unpacked no installer was recorded as installed")
	}
}
