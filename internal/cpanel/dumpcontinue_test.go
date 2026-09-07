package cpanel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/panel"
	"github.com/shukiv/gniza/internal/pkgacct"
)

// selectiveMysqldump fails for one named database and works for every
// other, which is what a single corrupt table looks like from here.
func selectiveMysqldump(t *testing.T, bad, stderr string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mysqldump")
	script := `#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = ` + shellQuote(bad) + ` ]; then
    printf '%s\n' ` + shellQuote(stderr) + ` >&2
    exit 2
  fi
done
echo "-- dump of $*"
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestOneBadDatabaseDoesNotCostTheAccountItsBackup.
//
// An account on a live server had 47 databases and one corrupt table in
// one of them. Staging returned on the first failed dump, so that account
// had no backup at all for a week -- not its home directory, not the other
// 46 databases. One table is not a reason to store nothing.
//
// What must not happen instead is a quiet hole: the dump that failed is
// removed rather than left as an empty file that would restore as a
// database with no tables, and the payload says what is missing.
func TestOneBadDatabaseDoesNotCostTheAccountItsBackup(t *testing.T) {
	const said = "mysqldump: Couldn't execute 'SELECT * FROM `qssq_sbi_feed_caches`': " +
		"Lost connection to MySQL server during query (2013)"
	host := newFakeHost(t, "customer1")
	host.MysqldumpPath = selectiveMysqldump(t, "customer1_broken", said)
	host.MysqlPath = fakeMysqldump(t, "", 0)

	dir := t.TempDir()
	databases := filepath.Join(dir, "databases")
	payload := pkgacct.Payload{DumpPaths: map[string]string{
		"customer1_wp":     filepath.Join(databases, "customer1_wp.sql"),
		"customer1_broken": filepath.Join(databases, "customer1_broken.sql"),
		"customer1_shop":   filepath.Join(databases, "customer1_shop.sql"),
	}}

	missing, err := host.dumpDatabases(context.Background(),
		panel.StageRequest{StagingDir: dir, Account: panel.AccountInfo{User: "customer1"}}, payload)
	if err != nil {
		t.Fatalf("one bad database failed the whole account: %v", err)
	}

	for _, good := range []string{"customer1_wp.sql", "customer1_shop.sql"} {
		info, statErr := os.Stat(filepath.Join(databases, good))
		if statErr != nil || info.Size() == 0 {
			t.Errorf("%s was not dumped: %v", good, statErr)
		}
	}
	if _, statErr := os.Stat(filepath.Join(databases, "customer1_broken.sql")); !os.IsNotExist(statErr) {
		t.Error("the failed dump was left behind, where it would restore as an empty database")
	}

	if len(missing) != 1 {
		t.Fatalf("the payload reports %d omissions, want 1: %v", len(missing), missing)
	}
	if !strings.Contains(missing[0].What, "customer1_broken") {
		t.Errorf("the omission does not name the database: %q", missing[0].What)
	}
	if !strings.Contains(missing[0].Why, "2013") {
		t.Errorf("the omission does not say what mysqldump said: %q", missing[0].Why)
	}
}
