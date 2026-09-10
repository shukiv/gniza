package whm_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func read(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// installBinaryHelper is the helper's own text, lifted out of the
// installer so a test can run it without running the installer.
func installBinaryHelper(t *testing.T) string {
	t.Helper()
	script := read(t, "install.sh")
	start := strings.Index(script, "install_binary() {")
	if start < 0 {
		t.Fatal("the installer has no install_binary helper")
	}
	end := strings.Index(script[start:], "\n}\n")
	if end < 0 {
		t.Fatal("the installer's install_binary helper is never closed")
	}
	return script[start : start+end+3]
}

// An installer that writes over a binary where it stands leaves whoever
// reads it looking at a file that is there and not yet complete. On
// 2026-09-09 a live server restarted at 00:02:45 into a
// /usr/local/bin/restic that existed and was not yet executable, and
// crash-looped every five seconds until the download finished at
// 00:06:27. A rename inside the same directory is atomic: a reader sees
// the old file or the new one, and never half of either.
func TestEveryExecutableArrivesByRename(t *testing.T) {
	script := read(t, "install.sh")
	for number, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, "install -m 0755") {
			t.Errorf("line %d puts an executable in place where it stands: %s",
				number+1, trimmed)
		}
	}
	if !strings.Contains(installBinaryHelper(t), "mv -f") {
		t.Error("install_binary does not rename what it wrote")
	}
}

// And the helper has to leave the file it was asked for: the right
// contents, executable, and nothing of its own beside it.
func TestInstallBinaryLeavesTheFileAndNothingElse(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "incoming")
	target := filepath.Join(dir, "gniza-agent")
	if err := os.WriteFile(source, []byte("the new binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("the old binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	driver := "die() { printf 'error: %s\\n' \"$*\" >&2; exit 1; }\n" +
		installBinaryHelper(t) + "\ninstall_binary 0755 \"$1\" \"$2\"\n"
	command := exec.Command("sh", "-c", driver, "sh", source, target)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("install_binary: %v: %s", err, output)
	}

	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "the new binary" {
		t.Errorf("the target holds %q", body)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("the target is mode %v", info.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		var left []string
		for _, entry := range entries {
			left = append(left, entry.Name())
		}
		t.Errorf("the directory holds %v, and should hold the source and the target", left)
	}
}
