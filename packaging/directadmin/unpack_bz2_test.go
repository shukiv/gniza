package directadmin_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// helperText lifts one shell function out of an installer so a test can
// run it without running the installer.
func helperText(t *testing.T, script, name string) string {
	t.Helper()
	start := strings.Index(script, name+"() {")
	if start < 0 {
		t.Fatalf("the installer has no %s helper", name)
	}
	end := strings.Index(script[start:], "\n}\n")
	if end < 0 {
		t.Fatalf("the installer's %s helper is never closed", name)
	}
	return script[start : start+end+3]
}

// restic is published as a .bz2 and nothing else, so something on the
// machine has to read one. bunzip2 is the usual answer and is not always
// installed -- a DirectAdmin server on 2026-09-10 stopped at
//
//	error: bunzip2 is needed to install restic; install it and run this again
//
// with everything else in place. python3 is on every panel server and its
// standard library reads bz2, so the installer asks it rather than
// stopping.
func TestResticIsUnpackedWithoutBunzip2(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	script := read(t, "install.sh")
	if strings.Contains(script, "for tool in curl bunzip2 sha256sum") {
		t.Error("bunzip2 is still a hard requirement of the installer")
	}

	dir := t.TempDir()
	// A PATH with python3 on it and no bunzip2 or bzip2 anywhere.
	stub := filepath.Join(dir, "bin")
	if err := os.MkdirAll(stub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"python3", "sh", "cat"} {
		found, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s is not installed", tool)
		}
		if err := os.Symlink(found, filepath.Join(stub, tool)); err != nil {
			t.Fatal(err)
		}
	}

	plain := "restic would be here, and this stands in for it\n"
	compressed := filepath.Join(dir, "restic.bz2")
	make := exec.Command(python, "-c",
		`import bz2,sys; open(sys.argv[1],"wb").write(bz2.compress(sys.argv[2].encode()))`,
		compressed, plain)
	if out, err := make.CombinedOutput(); err != nil {
		t.Fatalf("build the fixture: %v: %s", err, out)
	}

	out := filepath.Join(dir, "restic")
	run := exec.Command("sh", "-c",
		helperText(t, script, "die")+
			helperText(t, script, "install_hint")+
			helperText(t, script, "unpack_bz2")+
			`unpack_bz2 "$1" "$2"`,
		"sh", compressed, out)
	run.Env = []string{"PATH=" + stub}
	if result, err := run.CombinedOutput(); err != nil {
		t.Fatalf("unpacking without bunzip2 failed: %v: %s", err, result)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != plain {
		t.Errorf("unpacked %q, want %q", got, plain)
	}
}

// And when nothing on the machine can read one, the installer says which
// package to install rather than naming a command the operator then has
// to work back to a package from.
func TestAMissingBz2ReaderNamesThePackage(t *testing.T) {
	script := read(t, "install.sh")
	dir := t.TempDir()
	stub := filepath.Join(dir, "bin")
	if err := os.MkdirAll(stub, 0o755); err != nil {
		t.Fatal(err)
	}
	run := exec.Command("sh", "-c",
		helperText(t, script, "die")+
			helperText(t, script, "install_hint")+
			helperText(t, script, "unpack_bz2")+
			`unpack_bz2 /nowhere.bz2 /nowhere`,
		"sh")
	run.Env = []string{"PATH=" + stub}
	out, err := run.CombinedOutput()
	if err == nil {
		t.Fatal("an installer with no way to read a .bz2 carried on")
	}
	if !strings.Contains(string(out), "bzip2") {
		t.Errorf("the message does not name the package to install: %s", out)
	}
}
