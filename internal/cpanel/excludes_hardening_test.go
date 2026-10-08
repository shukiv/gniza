package cpanel

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestExcludesRefuseASymlink: the account writes this file and root reads
// it. A link to a file root can read and the account cannot would put that
// file's contents into restic's command line.
func TestExcludesRefuseASymlink(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home", "customer1")
	if err := os.MkdirAll(home, 0o711); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(root, "root-only.cnf")
	if err := os.WriteFile(secret, []byte("password=hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(home, ExcludeConfName)); err != nil {
		t.Fatal(err)
	}

	real := &Real{ServerExcludeConf: filepath.Join(root, "no-such-server-list")}
	for _, pattern := range real.NativeExcludes(home) {
		if strings.Contains(pattern, "hunter2") {
			t.Fatalf("a private file was read through a symlink: %q", pattern)
		}
	}
}

// TestExcludesDoNotBlockOnAFIFO: the engine backs up one account at a
// time, so an open that never returns is every backup on the server
// stopping.
func TestExcludesDoNotBlockOnAFIFO(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home", "customer1")
	if err := os.MkdirAll(home, 0o711); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(home, ExcludeConfName), 0o600); err != nil {
		t.Skipf("this filesystem has no fifos: %v", err)
	}

	real := &Real{ServerExcludeConf: filepath.Join(root, "no-such-server-list")}
	done := make(chan []string, 1)
	go func() { done <- real.NativeExcludes(home) }()
	select {
	case excludes := <-done:
		// The built-in list is there whatever the file is; nothing
		// beyond it may come from a pipe.
		if len(excludes) != len(gnizaSkips) {
			t.Errorf("a pipe was read as a list of exclusions: %v", excludes)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading an account's exclusions blocked on a pipe nobody is writing to")
	}
}

// TestExcludesRefuseAnotherOwnersFile covers the hard link: same
// directory, same name, somebody else's contents.
func TestExcludesRefuseAnotherOwnersFile(t *testing.T) {
	if os.Getuid() != 0 {
		// Only root can hand a file to another owner, so only root can
		// set this up. The check itself runs for everybody.
		t.Skip("this needs root to change a file's owner")
	}
	root := t.TempDir()
	home := filepath.Join(root, "home", "customer1")
	if err := os.MkdirAll(home, 0o711); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(home, 1001, 1001); err != nil {
		t.Fatal(err)
	}
	list := filepath.Join(home, ExcludeConfName)
	if err := os.WriteFile(list, []byte("public_html/cache\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(list, 1002, 1002); err != nil {
		t.Fatal(err)
	}

	real := &Real{ServerExcludeConf: filepath.Join(root, "no-such-server-list")}
	if excludes := real.NativeExcludes(home); len(excludes) != 0 {
		t.Errorf("another account's file was obeyed: %v", excludes)
	}
}

// TestExcludesStillReadTheOrdinaryFile: the hardening must not stop the
// thing working. An operator who excluded a directory expects it excluded.
func TestExcludesStillReadTheOrdinaryFile(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home", "customer1")
	if err := os.MkdirAll(home, 0o711); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ExcludeConfName),
		[]byte("# theirs\npublic_html/cache\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := filepath.Join(root, "cpbackup-exclude.conf")
	if err := os.WriteFile(server, []byte("/tmp\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	real := &Real{ServerExcludeConf: server}
	excludes := real.NativeExcludes(home)
	if len(excludes) == 0 {
		t.Fatal("nothing was excluded")
	}
	joined := strings.Join(excludes, " ")
	if !strings.Contains(joined, "public_html/cache") || !strings.Contains(joined, "/tmp") {
		t.Errorf("the lists were not both read: %v", excludes)
	}
}

// TestExcludesAreBounded: a file of a million lines is not a list of
// exclusions, and every one of them would become a restic argument.
func TestExcludesAreBounded(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home", "customer1")
	if err := os.MkdirAll(home, 0o711); err != nil {
		t.Fatal(err)
	}
	var huge strings.Builder
	for i := 0; i < maxExcludeLines*3; i++ {
		huge.WriteString("public_html/many\n")
	}
	if err := os.WriteFile(filepath.Join(home, ExcludeConfName), []byte(huge.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	real := &Real{ServerExcludeConf: filepath.Join(root, "no-such-server-list")}
	if excludes := real.NativeExcludes(home); len(excludes) > maxExcludeLines*2 {
		t.Errorf("an unbounded file produced %d patterns", len(excludes))
	}
}
