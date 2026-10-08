package cpanel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCPanelsOwnExclusionsAreObeyed covers a privacy failure rather than a
// data-loss one. An operator who writes a path into cpbackup-exclude.conf
// has said it must not leave the server -- that is the file cPanel
// documents for exactly this. Ignoring it meant the files they excluded
// were the ones being uploaded to a remote destination, and nothing
// anywhere told them.
func TestCPanelsOwnExclusionsAreObeyed(t *testing.T) {
	root := t.TempDir()
	serverConf := filepath.Join(root, "cpbackup-exclude.conf")
	// The real one from a cPanel 136 server, trimmed.
	if err := os.WriteFile(serverConf, []byte(
		"*/core.[0-9]*\n.cpanel/caches\naccess-logs\n\n# a comment\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home", "studio")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ExcludeConfName),
		[]byte("/private-notes\nnode_modules\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	host := &Real{ServerExcludeConf: serverConf}
	excludes := host.NativeExcludes(home)
	joined := strings.Join(excludes, "\n")

	for _, want := range []string{
		// Matched at the top of the home and anywhere below it, which is
		// what cPanel means by a bare name.
		filepath.Join(home, "access-logs"),
		filepath.Join(home, "**", "access-logs"),
		filepath.Join(home, ".cpanel/caches"),
		filepath.Join(home, "**", ".cpanel/caches"),
		// "*/" is cPanel's way of saying "below the top", which matching
		// at any depth already covers.
		filepath.Join(home, "**", "core.[0-9]*"),
		// The account's own list is read too.
		filepath.Join(home, "**", "node_modules"),
		// A leading slash is relative to the home and matches only there.
		filepath.Join(home, "private-notes"),
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("%q is excluded by cPanel and was not passed on:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "a comment") {
		t.Error("a comment was read as a pattern")
	}
	// Anchored under the home, so they cannot also match this program's
	// own staged metadata and database dumps.
	for _, exclude := range excludes {
		if !strings.HasPrefix(exclude, home) {
			t.Errorf("%q is not anchored under the account's home", exclude)
		}
	}
}

// TestAnAccountWithNoExclusionsLeavesOutOnlyTheInstallersArchives covers
// the ordinary case: most accounts have never written the file, and a
// missing file is not a failure. What is left out all the same is the
// archives the one-click installers write under the home, anchored under
// this home and nowhere else -- the operator asked for
// application_backups to be left out everywhere, by default.
func TestAnAccountWithNoExclusionsLeavesOutOnlyTheInstallersArchives(t *testing.T) {
	host := &Real{ServerExcludeConf: filepath.Join(t.TempDir(), "absent")}
	home := filepath.Join(t.TempDir(), "home")
	want := []string{
		filepath.Join(home, "application_backups"),
		filepath.Join(home, "softaculous_backups"),
		filepath.Join(home, "wordpress-backups"),
	}
	got := host.NativeExcludes(home)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("excludes = %v, want exactly %v", got, want)
	}
	if got := host.NativeExcludes(""); got != nil {
		t.Errorf("excludes = %v for an account with no home, want none", got)
	}
}
