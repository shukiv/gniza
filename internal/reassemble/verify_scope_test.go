package reassemble_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/layout/cpmove"
	"github.com/shukiv/gniza/internal/pkgacct"
	"github.com/shukiv/gniza/internal/reassemble"
)

// buildTree writes the shape a rebuilt split account has: an archive, an
// account directory, and whatever parts the caller asks for.
func buildTree(t *testing.T, withHomedir bool, dumps map[string]string) reassemble.Result {
	t.Helper()
	root := t.TempDir()
	archive := filepath.Join(root, "cpmove-webshop.tar")
	if err := os.WriteFile(archive, []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(root, "tree")
	account := filepath.Join(tree, "cpmove-webshop")
	if err := os.MkdirAll(account, 0o700); err != nil {
		t.Fatal(err)
	}
	if withHomedir {
		home := filepath.Join(account, cpmove.HomedirDir)
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "index.php"), []byte("<?php"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if dumps != nil {
		dir := filepath.Join(account, cpmove.DatabaseDir)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for name, body := range dumps {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return reassemble.Result{
		Layout:      cpmove.Layout{},
		ArchivePath: archive, TreeDir: tree, Mode: pkgacct.ModeSplit,
	}
}

// A rehearsal of a backup taken without part of the account has to say so.
// Before this, a schedule that skips databases rehearsed clean every night
// -- "databases are optional" read a deliberate exclusion as an account
// that simply has none -- and the account showed as verified.
func TestARehearsalSaysWhatTheBackupWasTakenWithout(t *testing.T) {
	rebuilt := buildTree(t, true, nil)
	rebuilt.Skipped = []string{"databases"}
	if rebuilt.Complete() {
		t.Fatal("a backup taken without databases reads as complete")
	}

	passed, err := reassemble.Verify(context.Background(), rebuilt)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	said := strings.Join(passed, "; ")
	if !strings.Contains(said, "without databases") {
		t.Errorf("the rehearsal does not say what was left out: %q", said)
	}
	if !strings.Contains(said, "not a backup of the whole account") {
		t.Errorf("the rehearsal reads as a whole account: %q", said)
	}
}

// A backup taken without the home directory rebuilds without one, and
// that is not the empty-homedir failure -- which stays a failure for a
// backup that was supposed to hold it.
func TestAnEmptyHomeDirectoryStillFailsUnlessItWasSkipped(t *testing.T) {
	missing := buildTree(t, false, map[string]string{"shop.sql": "CREATE TABLE t (id int);"})
	if _, err := reassemble.Verify(context.Background(), missing); err == nil {
		t.Fatal("a backup that should hold the home directory rehearsed clean without one")
	}

	skipped := buildTree(t, false, map[string]string{"shop.sql": "CREATE TABLE t (id int);"})
	skipped.Skipped = []string{"homedir"}
	passed, err := reassemble.Verify(context.Background(), skipped)
	if err != nil {
		t.Fatalf("a backup taken without the home directory failed its rehearsal: %v", err)
	}
	if !strings.Contains(strings.Join(passed, "; "), "without homedir") {
		t.Errorf("checks = %v", passed)
	}
}

// And a full backup is checked as it was before: nothing about the scope
// changes what a whole-account rehearsal has to prove.
func TestAFullBackupIsRehearsedAsBefore(t *testing.T) {
	rebuilt := buildTree(t, true, map[string]string{"shop.sql": "CREATE TABLE t (id int);"})
	passed, err := reassemble.Verify(context.Background(), rebuilt)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	said := strings.Join(passed, "; ")
	for _, want := range []string{"archive present", "account tree present",
		"files in the home directory", "1 database dumps parse"} {
		if !strings.Contains(said, want) {
			t.Errorf("checks do not include %q: %q", want, said)
		}
	}
	if strings.Contains(said, "taken without") {
		t.Errorf("a full backup was reported as partial: %q", said)
	}
}
