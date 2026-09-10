package update_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestTheInstallerReadsTheVersionOutOfTheChecksums covers the one line of
// get.sh that decides whether an install happens at all.
//
// The build writes "# cprest <version> <commit time>" at the top of
// SHA256SUMS, and get.sh refuses to install checksums that do not say
// which release they are for. Its pattern was anchored straight after the
// version, from before the commit time was added, so every release built
// since carried a header the installer would not read:
//
//	error: the published checksums do not say which release they are for
//
// Signature verified, checksum verified, and nothing installed. The
// pattern is exercised here as the shell actually runs it, so it cannot
// drift away from the header the Makefile writes again.
func TestTheInstallerReadsTheVersionOutOfTheChecksums(t *testing.T) {
	extract := versionExpression(t)

	for _, header := range []struct {
		name, line, want string
	}{
		{"a release as the build writes it today",
			"# cprest v0.1.2 2026-09-06T05:42:56+03:00", "v0.1.2"},
		{"a release before the commit time was added",
			"# cprest v0.1.0", "v0.1.0"},
		{"a branch build, which is not a release and must not read as one",
			"# cprest v0.1.2-18-gabc1234 2026-09-06T05:42:56+03:00", ""},
		{"a line with something else where the version goes",
			"# cprest not-a-version", ""},
	} {
		t.Run(header.name, func(t *testing.T) {
			work := t.TempDir()
			sums := header.line + "\n" +
				"0000000000000000000000000000000000000000000000000000000000000000  " +
				"cprest-plugin-amd64.tar.gz\n"
			if err := os.WriteFile(filepath.Join(work, "SHA256SUMS"),
				[]byte(sums), 0o600); err != nil {
				t.Fatal(err)
			}

			script := "set -eu\nwork=\"$1\"\n" + extract + "\nprintf '%s' \"$signed_for\"\n"
			out, err := exec.Command("sh", "-c", script, "sh", work).CombinedOutput()
			if err != nil {
				t.Fatalf("running the installer's own expression: %v: %s", err, out)
			}
			if got := string(out); got != header.want {
				t.Errorf("the installer reads %q out of %q, and wants %q",
					got, header.line, header.want)
			}
		})
	}
}

// versionExpression is the assignment out of get.sh itself, so this test
// checks the installer that ships rather than a copy of it.
func versionExpression(t *testing.T) string {
	t.Helper()
	script, err := os.ReadFile(filepath.Join("..", "..", "packaging", "whm", "get.sh"))
	if err != nil {
		t.Fatal(err)
	}
	found := regexp.MustCompile(`(?s)\n( *signed_for=\$\(.*?\n.*?\)\n)`).
		FindSubmatch(script)
	if found == nil {
		t.Fatal("get.sh no longer assigns signed_for in a form this test can find")
	}
	return strings.TrimSpace(string(found[1]))
}

// TestTheInstallerPicksThePackageForThePanelOnTheServer covers the other
// line of get.sh that decides what happens: which of the two published
// packages this machine takes. Installing the wrong one puts a plugin on
// a server with no panel to show it, and leaves the machine's own panel
// without one -- so the choice is made from what is on the disk, and a
// server with neither panel is told so instead of being installed onto.
func TestTheInstallerPicksThePackageForThePanelOnTheServer(t *testing.T) {
	block := panelExpression(t)

	for _, server := range []struct {
		name    string
		dirs    []string
		tarball string
		tree    string
	}{
		{"cPanel", []string{"cpanel"}, "cprest-plugin-amd64.tar.gz", "cprest-plugin"},
		{"DirectAdmin", []string{"directadmin"}, "gniza-directadmin-amd64.tar.gz", "gniza-directadmin"},
		{"both, which keeps the package it has always had",
			[]string{"cpanel", "directadmin"}, "cprest-plugin-amd64.tar.gz", "cprest-plugin"},
		{"neither", nil, "", ""},
	} {
		t.Run(server.name, func(t *testing.T) {
			root := t.TempDir()
			for _, dir := range server.dirs {
				if err := os.MkdirAll(filepath.Join(root, "usr", "local", dir), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			// The block as it ships, reading the temporary machine
			// instead of this one.
			script := "set -eu\narch=amd64\n" +
				"die() { printf 'error: %s\\n' \"$*\" >&2; exit 1; }\nsay() { :; }\n" +
				strings.ReplaceAll(block, "/usr/local/", root+"/usr/local/") +
				"\nprintf '%s %s' \"$tarball\" \"$tree\"\n"
			out, err := exec.Command("sh", "-c", script).CombinedOutput()
			if server.tarball == "" {
				if err == nil {
					t.Fatalf("a server with no panel was installed onto: %s", out)
				}
				if !strings.Contains(string(out), "DirectAdmin") {
					t.Errorf("the refusal does not say what it looked for: %s", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("running the installer's own choice: %v: %s", err, out)
			}
			if got, want := string(out), server.tarball+" "+server.tree; got != want {
				t.Errorf("the installer takes %q, and wants %q", got, want)
			}
		})
	}
}

// panelExpression is the choice out of get.sh itself, so this test checks
// the installer that ships rather than a copy of it.
func panelExpression(t *testing.T) string {
	t.Helper()
	script, err := os.ReadFile(filepath.Join("..", "..", "packaging", "whm", "get.sh"))
	if err != nil {
		t.Fatal(err)
	}
	found := regexp.MustCompile(`(?s)\n( *if \[ -d /usr/local/cpanel \].*?\n *fi\n)`).
		FindSubmatch(script)
	if found == nil {
		t.Fatal("get.sh no longer chooses a package in a form this test can find")
	}
	return string(found[1])
}
