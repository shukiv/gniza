package directadmin_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/layout/dabackup"
)

// warningBody is what the installer prints under its warning heading,
// unwrapped back into one sentence.
func warningBody(t *testing.T, script string) string {
	t.Helper()
	// The call, not the definition above it.
	start := strings.Index(script, "\nwarn_block \"")
	if start < 0 {
		t.Fatal("the installer prints no warning block")
	}
	body := script[start:]
	if end := strings.Index(body, "\n\n"); end >= 0 {
		body = body[:end]
	}
	var parts []string
	for _, line := range regexp.MustCompile(`"([^"]*)"`).FindAllStringSubmatch(body, -1) {
		parts = append(parts, line[1])
	}
	if len(parts) < 2 {
		t.Fatalf("the warning block has no body: %q", body)
	}
	// The first quoted string is the heading.
	return strings.Join(strings.Fields(strings.Join(parts[1:], " ")), " ")
}

// The installer used to say the provider "backs up a whole account as one
// archive". That stopped being true when split mode landed, and nobody
// noticed, because the sentence was a second copy of one the agent
// already prints. There is one text now and this is what holds it there.
func TestTheInstallerWarnsInTheSameWordsTheAgentDoes(t *testing.T) {
	got := warningBody(t, read(t, "install.sh"))
	want := strings.Join(strings.Fields(dabackup.Provisional), " ")
	if got != want {
		t.Errorf("the installer's warning has drifted from dabackup.Provisional\n got: %s\nwant: %s", got, want)
	}
}

// A warning nobody sees is not a warning. It is the last thing printed
// after a wall of installer output, and the operator has to be able to
// pick it out.
func TestTheWarningIsHighlightedOnATerminal(t *testing.T) {
	pty, err := exec.LookPath("script")
	if err != nil {
		t.Skip("script(1) is not installed")
	}
	script := read(t, "install.sh")
	program := helperText(t, script, "set_colours") +
		helperText(t, script, "warn_block") +
		"set_colours\nwarn_block Heading Body\n"
	path := filepath.Join(t.TempDir(), "warn.sh")
	if err := os.WriteFile(path, []byte(program), 0o700); err != nil {
		t.Fatal(err)
	}

	onATerminal, err := exec.Command(pty, "-qec", "sh "+path, "/dev/null").CombinedOutput()
	if err != nil {
		t.Fatalf("run under a pty: %v: %s", err, onATerminal)
	}
	if !strings.Contains(string(onATerminal), "\033[") {
		t.Errorf("the warning is not highlighted on a terminal: %q", onATerminal)
	}
	if !strings.Contains(string(onATerminal), "Heading") {
		t.Errorf("the heading is missing: %q", onATerminal)
	}

	// And piped into a file or a log it is text, because escapes there
	// are noise an operator has to read around.
	piped, err := exec.Command("sh", path).CombinedOutput()
	if err != nil {
		t.Fatalf("run with output to a pipe: %v: %s", err, piped)
	}
	if strings.Contains(string(piped), "\033[") {
		t.Errorf("terminal escapes were written to a pipe: %q", piped)
	}
	if !strings.Contains(string(piped), "Heading") || !strings.Contains(string(piped), "Body") {
		t.Errorf("the warning lost its text when not on a terminal: %q", piped)
	}

	// NO_COLOR is a convention an operator sets once and expects honoured.
	plain := exec.Command(pty, "-qec", "sh "+path, "/dev/null")
	plain.Env = append(os.Environ(), "NO_COLOR=1")
	out, err := plain.CombinedOutput()
	if err != nil {
		t.Fatalf("run with NO_COLOR: %v: %s", err, out)
	}
	if strings.Contains(string(out), "\033[") {
		t.Errorf("NO_COLOR was ignored: %q", out)
	}
}
