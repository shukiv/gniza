package cpanel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The client stand-in distinguishes SQL sent as root from SQL sent with a
// separate login. The malicious statement must reach only the latter, and
// the setup/teardown must never contain backup-controlled SQL.
func TestDatabaseDumpUsesASchemaScopedLogin(t *testing.T) {
	r, log := databaseUserRestoreHost(t)
	script := `#!/bin/sh
scope=admin
for arg in "$@"; do
 case "$arg" in
 --print-defaults) echo 'mysql would have been started with the following arguments:'; echo '--socket=/run/mysql.sock --user=root --password=admin-secret'; exit 0;;
 --execute=*) echo localhost; exit 0;;
 --user=cpr_restore_*) scope=isolated;;
 esac
done
printf '%s\n' "$scope" >> ` + shellQuoteForTest(log) + `
cat >> ` + shellQuoteForTest(log) + `
if [ "$scope" = isolated ]; then exit 1; fi
`
	if err := os.WriteFile(r.MysqlPath, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	dump := filepath.Join(t.TempDir(), "dump.sql")
	if err := os.WriteFile(dump, []byte("DROP DATABASE another_customer;\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.LoadDatabase(t.Context(), "customer1", "customer1_wp", dump); err == nil {
		t.Error("privileged SQL from the dump succeeded")
	}
	body, _ := os.ReadFile(log)
	got := string(body)
	if !strings.Contains(got, "isolated\nDROP DATABASE another_customer;") {
		t.Errorf("backup SQL was not isolated: %s", got)
	}
	if !strings.Contains(got, "ON `customer1\\_wp`.*") || strings.Contains(got, "ON *.*") {
		t.Errorf("the temporary login was not granted exactly the selected schema: %s", got)
	}
	if !strings.Contains(got, "DROP USER") {
		t.Errorf("temporary login was not removed after failure: %s", got)
	}
}

// A killed import never runs its own cleanup: the option file stays, with
// a database password in it, and so does the login it was written for.
func TestStartupClearsWhatAKilledImportLeft(t *testing.T) {
	root := t.TempDir()
	left := filepath.Join(root, "gniza-mysql-123456")
	if err := os.Mkdir(left, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(left, "client.cnf"), []byte("[client]\npassword=left-behind\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The temporary directory is everybody's. A link by the right name is
	// not this program's directory, and neither is what it points at.
	elsewhere := filepath.Join(t.TempDir(), "not-ours")
	if err := os.Mkdir(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(elsewhere, "kept")
	if err := os.WriteFile(kept, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(root, "gniza-mysql-link")); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "somebody-elses")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}

	client := filepath.Join(t.TempDir(), "mysql")
	sql := filepath.Join(t.TempDir(), "sql")
	script := "#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in --execute=*) printf 'cpr_restore_0123456789abcdef@localhost\\nroot@localhost\\ncpr_restore_mine@%%\\ncpr_restore_fedcba9876543210@bad`host\\n'; exit 0;; esac; done\ncat >> " + shellQuoteForTest(sql) + "\n"
	if err := os.WriteFile(client, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	r := &Real{MysqlPath: client, SandboxRoot: root}

	cleared, dropped, err := r.ClearInterrupted(t.Context())
	if err != nil {
		t.Fatalf("ClearInterrupted: %v", err)
	}
	if cleared != 1 {
		t.Errorf("cleared = %d, want the one directory left behind", cleared)
	}
	if _, err := os.Lstat(left); !os.IsNotExist(err) {
		t.Error("the password file of a killed import is still on disk")
	}
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("a link in the temporary directory was followed: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("a directory that is not a sandbox was removed: %v", err)
	}
	// Dropped, at the operator's word -- and only a login of exactly the
	// shape this program makes: not root, not an operator's own that
	// shares the prefix, and not one at a host that could not be quoted
	// into a statement.
	if len(dropped) != 1 || dropped[0] != "cpr_restore_0123456789abcdef@localhost" {
		t.Errorf("dropped = %v, want only the one a restore made", dropped)
	}
	ran, _ := os.ReadFile(sql)
	if string(ran) != "DROP USER `cpr_restore_0123456789abcdef`@`localhost`;\n" {
		t.Errorf("SQL sent to the server:\n%s", ran)
	}
}
