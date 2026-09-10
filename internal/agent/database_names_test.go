package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// A basket dedups what it holds, so this arrives only from a submission
// that repeats one value or from a caller of the API directly. What it
// used to do was create the database twice: the first CreateDatabase
// succeeded, the second was refused as already existing, and the run
// stopped before the dumps were loaded -- leaving the account holding a
// newly made empty database and a report saying the restore failed.
func TestADatabaseNamedTwiceIsRestoredOnce(t *testing.T) {
	databases := t.TempDir()
	if err := os.WriteFile(filepath.Join(databases, "shop.sql"),
		[]byte("CREATE TABLE orders (id int);\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	dumps, create, hint, err := checkDatabaseDumps(
		[]string{"shop", "shop"}, map[string]bool{}, databases)
	if err != nil {
		t.Fatalf("%v (%s)", err, hint)
	}
	if len(create) != 1 {
		t.Errorf("the database would be created %d times: %v", len(create), create)
	}
	if len(dumps) != 1 {
		t.Errorf("the dump would be loaded %d times: %v", len(dumps), dumps)
	}
}
