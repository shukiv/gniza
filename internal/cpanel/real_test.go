package cpanel

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/panel"
	"github.com/shukiv/gniza/internal/pkgacct"
)

// newFakeHost lays out the parts of a cPanel server the real provider reads.
func newFakeHost(t *testing.T, users ...string) *Real {
	t.Helper()
	root := t.TempDir()
	usersDir := filepath.Join(root, "var", "cpanel", "users")
	homeRoot := filepath.Join(root, "home")
	if err := os.MkdirAll(usersDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, user := range users {
		if err := os.WriteFile(filepath.Join(usersDir, user), []byte("USER="+user+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(homeRoot, user, "public_html"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &Real{UsersDir: usersDir, HomeRoot: homeRoot}
}

// TestAccountsDoesNotDependOnMySQL is the bug this test exists for: listing
// used to fetch each account's databases, so on a server where the MySQL
// credentials were not reachable every account vanished from the interface
// and the operator was told they had none.
func TestAccountsDoesNotDependOnMySQL(t *testing.T) {
	host := newFakeHost(t, "customer1", "customer2")
	// A mysql that always fails, the way it does when HOME is unset and
	// root's ~/.my.cnf cannot be found.
	host.MysqldumpPath = "/nonexistent/mysqldump"

	accounts, err := host.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("listed %d accounts, want 2: %+v", len(accounts), accounts)
	}
	for _, account := range accounts {
		if account.HomeDir == "" {
			t.Errorf("account %s has no home directory", account.User)
		}
		// A listing is names and homes only; measuring is for backup time.
		if account.SizeBytes != 0 || len(account.Databases) != 0 {
			t.Errorf("account %s was measured during a listing: %+v", account.User, account)
		}
	}
}

func TestAccountsIsCheap(t *testing.T) {
	host := newFakeHost(t, "customer1")
	// Something big enough that walking it would show up.
	big := filepath.Join(host.HomeRoot, "customer1", "public_html")
	for i := range 200 {
		name := filepath.Join(big, "file"+string(rune('a'+i%26))+string(rune('a'+i/26)))
		if err := os.WriteFile(name, make([]byte, 4096), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	started := time.Now()
	if _, err := host.Accounts(context.Background()); err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	// Listing twenty accounts on a real server took nineteen seconds when
	// it walked their home directories. It must not walk anything.
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("listing took %s; it is walking the filesystem again", elapsed)
	}
}

func TestAccountsSkipsEntriesThatAreNotAccounts(t *testing.T) {
	host := newFakeHost(t, "customer1")
	usersDir := host.UsersDir
	for _, name := range []string{"system", ".cache", "bad name"} {
		if err := os.WriteFile(filepath.Join(usersDir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// An account cPanel knows about whose home directory is not there is
	// still listed, and marked. Leaving it off the page is how an account
	// stops being backed up without anybody finding out: nothing fails,
	// because nothing is attempted.
	if err := os.WriteFile(filepath.Join(usersDir, "homeless"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	accounts, err := host.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	found := map[string]bool{}
	for _, account := range accounts {
		found[account.User] = account.Missing
	}
	if len(found) != 2 {
		t.Fatalf("accounts = %+v, want customer1 and homeless only", accounts)
	}
	if missing, listed := found["customer1"]; !listed || missing {
		t.Errorf("customer1 should be listed and present, got missing=%v listed=%v", missing, listed)
	}
	if missing, listed := found["homeless"]; !listed {
		t.Error("an account with no home directory was hidden rather than reported")
	} else if !missing {
		t.Error("an account with no home directory was not marked as such")
	}
}

func TestAccountFailsLoudlyWhenDatabasesCannotBeListed(t *testing.T) {
	host := newFakeHost(t, "customer1")
	// Account is called when a backup is about to run. Returning an empty
	// database list here would produce a backup that looks fine and
	// restores a site with no content, so it must fail instead.
	t.Setenv("PATH", t.TempDir())

	if _, err := host.Account(context.Background(), "customer1"); err == nil {
		t.Error("Account succeeded with no way to list databases")
	}
}

func TestPostgreSQLCannotFallThroughTheMySQLOnlySplitPath(t *testing.T) {
	request := panel.StageRequest{
		Account: panel.AccountInfo{User: "customer1", HasPostgreSQL: true},
		Mode:    pkgacct.ModeSplit,
	}
	mode, reason, err := safeDatabaseMode(request)
	if err != nil {
		t.Fatalf("safeDatabaseMode: %v", err)
	}
	if mode != pkgacct.ModeMonolithic || reason == "" {
		t.Fatalf("mode = %q, reason = %q; want explained monolithic fallback", mode, reason)
	}

	request.SkipHomedir = true
	if _, _, err := safeDatabaseMode(request); err == nil {
		t.Fatal("a databases-only split backup silently accepted PostgreSQL")
	}

	request.SkipDatabases = true
	if mode, _, err := safeDatabaseMode(request); err != nil || mode != pkgacct.ModeSplit {
		t.Fatalf("an intentional database exclusion should remain split: mode=%q err=%v", mode, err)
	}
}
