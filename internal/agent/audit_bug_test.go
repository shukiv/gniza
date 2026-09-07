package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/cpanel"
	"github.com/shukiv/gniza/internal/layout/cpmove"
	"github.com/shukiv/gniza/internal/pkgacct"
	"github.com/shukiv/gniza/internal/reassemble"
)

// TestARestoreThatDidNotPutTheDatabasesBackIsNotASuccess covers what
// cPanel's exit code does not say.
//
// Only two of cPanel's restore modules treat their own failure as fatal.
// Every other one -- the databases among them -- is recorded as a skipped
// item and the restore carries on and exits zero, saying "Account
// Restored". Restoring over an account that already exists, which is what
// a restore into a live account is, the only check afterwards was that
// the account was still there. It always was. So a restore that put none
// of the databases back was reported as a success, and nobody looked
// again.
//
// The databases the archive holds are now looked for on the account
// afterwards.
func TestARestoreThatDidNotPutTheDatabasesBackIsNotASuccess(t *testing.T) {
	rebuilt := rebuiltWithDatabases(t, "c1", "c1_shop", "c1_wp")

	// cPanel said yes and put neither database back.
	silent := quietAgent(&cpanel.Fake{Root: t.TempDir()})
	err := silent.confirmRestored(context.Background(), silent.log, "c1", rebuilt)
	if err == nil {
		t.Fatal("a restore that put none of the databases back was reported as a success")
	}
	for _, name := range []string{"c1_shop", "c1_wp"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the error does not name %s: %v", name, err)
		}
	}

	// One of the two, which is the shape a single failed module leaves.
	half := quietAgent(&cpanel.Fake{
		Root:      t.TempDir(),
		Databases: map[string][]string{"c1": {"c1_shop"}},
	})
	err = half.confirmRestored(context.Background(), half.log, "c1", rebuilt)
	if err == nil {
		t.Fatal("a restore missing one of two databases was reported as a success")
	}
	if strings.Contains(err.Error(), "c1_shop") || !strings.Contains(err.Error(), "c1_wp") {
		t.Errorf("the error should name only the database that is missing: %v", err)
	}

	// And the restore that worked passes.
	whole := quietAgent(&cpanel.Fake{
		Root:      t.TempDir(),
		Databases: map[string][]string{"c1": {"c1_shop", "c1_wp", "c1_extra"}},
	})
	if err := whole.confirmRestored(context.Background(), whole.log, "c1", rebuilt); err != nil {
		t.Errorf("a restore that put the databases back was reported as failed: %v", err)
	}
}

// A monolithic snapshot keeps its databases inside cPanel's own archive,
// where nothing here can see them without unpacking it. There is nothing
// to check against, and inventing a check would fail every restore of one.
func TestAMonolithicRestoreIsNotFailedForDatabasesItCannotList(t *testing.T) {
	rebuilt := rebuiltWithDatabases(t, "c1", "c1_shop")
	rebuilt.Mode = pkgacct.ModeMonolithic

	agent := quietAgent(&cpanel.Fake{Root: t.TempDir()})
	if err := agent.confirmRestored(context.Background(), agent.log, "c1", rebuilt); err != nil {
		t.Errorf("a monolithic restore was failed over databases it cannot list: %v", err)
	}
}

// rebuiltWithDatabases lays out a split rebuild the way reassemble leaves
// one: a single top-level directory with the dumps beside the rest.
func rebuiltWithDatabases(t *testing.T, account string, databases ...string) reassemble.Result {
	t.Helper()
	tree := t.TempDir()
	dumps := filepath.Join(tree, "cpmove-"+account, cpmove.DatabaseDir)
	if err := os.MkdirAll(dumps, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range databases {
		if err := os.WriteFile(filepath.Join(dumps, name+".sql"),
			[]byte("-- dump\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		// cPanel writes a create statement beside each dump. It is not a
		// database and must not be counted as one.
		if err := os.WriteFile(filepath.Join(dumps, name+".create"),
			[]byte("CREATE DATABASE\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return reassemble.Result{
		Account: account, TreeDir: tree,
		Layout: cpmove.Layout{}, Mode: pkgacct.ModeSplit,
	}
}
