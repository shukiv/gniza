package whm_test

import (
	"os"
	"strings"
	"testing"
)

// TestUninstallRetiresWhatItRemoves is the rule the DirectAdmin
// uninstaller already follows, for the one that ships in the signed
// release.
//
// An uninstaller runs on somebody else's production server, usually
// because something is wrong, and often at the hands of somebody who will
// want the old files back an hour later. Deleting the plugin outright
// leaves nothing to come back to: not the binary that was running, not
// the hook that was registered, not the account events it had spooled.
//
// Everything this script takes off the server is moved aside into one
// dated directory it names on its way out, so that removing Gniza is
// undoable and deleting it stays a decision somebody makes on purpose.
func TestUninstallRetiresWhatItRemoves(t *testing.T) {
	script := readScript(t, "uninstall.sh")

	if !strings.Contains(script, `mv -- "$1" "$ATTIC/`) {
		t.Fatal("the uninstaller does not move what it takes off the server into one place")
	}
	if !strings.Contains(script, "$ATTIC") {
		t.Fatal("the uninstaller does not say where the retired files went")
	}

	for _, kept := range []string{
		"/usr/local/bin/gniza-agent",
		"/usr/local/cpanel/3rdparty/bin/gniza-hook",
		"/etc/systemd/system/gniza.service",
		"/usr/local/cpanel/Cpanel/API/Gniza.pm",
		"/var/cpanel/perl/Cpanel/Admin/Modules/Gniza",
		"/var/lib/gniza/hooks",
		"/usr/local/share/gniza",
		"$FRONTEND/gniza",
	} {
		if !strings.Contains(script, "retire "+quoteIfNeeded(kept)) {
			t.Errorf("%s is not retired, so removing Gniza cannot be undone", kept)
		}
	}
}

// TestUninstallStillDeletesWhatIsOnlyClutter covers the other half.
//
// Retiring everything would be its own failure. restic's cache is rebuilt
// from the repository on the next backup and is the one thing here that
// reaches gigabytes, so moving it aside would free no disk at all on a
// server somebody is uninstalling to make room. The scratch directory the
// script makes for cPanel's own plugin remover is not the operator's
// data either.
func TestUninstallStillDeletesWhatIsOnlyClutter(t *testing.T) {
	script := readScript(t, "uninstall.sh")

	for _, deleted := range []string{`rm -rf -- /var/cache/gniza`, `rm -rf -- "$PLUGIN_META"`} {
		if !strings.Contains(script, deleted) {
			t.Errorf("the uninstaller no longer deletes %s", deleted)
		}
	}
	// The state and the key are what a reinstall comes back to. Neither
	// is retired, because neither is removed.
	for _, untouched := range []string{"/etc/gniza/master.key", "/var/lib/gniza/state.db"} {
		if strings.Contains(script, "retire "+untouched) {
			t.Errorf("%s is taken off the server; it must be left alone", untouched)
		}
	}
	// An attic under the hooks directory would be moved into itself.
	if strings.Contains(script, "ATTIC=/var/lib/gniza/hooks") {
		t.Error("the attic is inside a directory the script retires")
	}
}

func readScript(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// quoteIfNeeded matches how the script writes a path that needs quoting.
func quoteIfNeeded(path string) string {
	if strings.ContainsAny(path, "$ ") {
		return `"` + path + `"`
	}
	return path
}
