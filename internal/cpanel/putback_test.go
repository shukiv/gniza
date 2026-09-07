package cpanel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/panel"
)

// A set-user-ID bit that was on a file when the backup was taken must not
// come back with it. The account could set one on its own file again, so
// this is not what stops them: it stops a restore from quietly undoing the
// removal of one, which is a thing done after a compromise.
func TestSetIDBitsDoNotSurviveAStagedTree(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "bin")
	if err := os.MkdirAll(nested, 0o2755); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(nested, "helper")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(binary, 0o4755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(nested, os.FileMode(0o755)|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}

	if err := dropSetIDBits(root); err != nil {
		t.Fatalf("dropSetIDBits: %v", err)
	}

	for _, path := range []string{binary, nested} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
			t.Errorf("%s kept mode %v", path, info.Mode())
		}
	}
	// The ordinary permissions are what the account is getting back, so
	// they have to survive.
	info, err := os.Stat(binary)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("the executable bit was lost with the setuid bit: %v", info.Mode())
	}
}

func TestADatabaseIsLoadedOnlyIntoTheAccountThatOwnsIt(t *testing.T) {
	dir := t.TempDir()
	dump := filepath.Join(dir, "c1_shop.sql")
	if err := os.WriteFile(dump, []byte("-- dump\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &Fake{Databases: map[string][]string{"c1": {"c1_shop"}}}
	if err := fake.LoadDatabase(t.Context(), "c2", "c1_shop", dump); err == nil {
		t.Fatal("another account's database was loaded")
	}
	if err := fake.LoadDatabase(t.Context(), "c1", "c1_shop", dump); err != nil {
		t.Fatalf("the account's own database was refused: %v", err)
	}
}

// Everything that reaches a MySQL statement is checked rather than escaped:
// the mysql client has no parameter binding, and these values come out of a
// file in a backup, which is exactly the kind nobody has looked at.
func TestWhatGoesIntoAMySQLStatementIsHeldToWhatItCanBe(t *testing.T) {
	for _, bad := range []string{"", "local host", "a`b", "a'b", "host;DROP", strings.Repeat("h", 256)} {
		if usableDatabaseHost(bad) {
			t.Errorf("%q was accepted as a host", bad)
		}
	}
	for _, good := range []string{"localhost", "182.54.236.144", "%", "10.0.%", "::1", "db-1.example"} {
		if !usableDatabaseHost(good) {
			t.Errorf("%q was refused as a host", good)
		}
	}
	for _, bad := range []string{"", "Caching_SHA2", "plugin'; --", "a b"} {
		if usableAuthPlugin(bad) {
			t.Errorf("%q was accepted as an authentication plugin", bad)
		}
	}
	for _, good := range []string{"mysql_native_password", "caching_sha2_password"} {
		if !usableAuthPlugin(good) {
			t.Errorf("%q was refused as an authentication plugin", good)
		}
	}
	// The hash is written into the statement as 0x<hash>, so anything but
	// an even number of hex digits would end the literal early.
	for _, bad := range []string{"2A46F", "2A46G0", "0x2A46", "2a46'"} {
		if usableAuthHash(bad) {
			t.Errorf("%q was accepted as a stored password", bad)
		}
	}
	for _, good := range []string{"", "2A46", "2a46bcDE"} {
		if !usableAuthHash(good) {
			t.Errorf("%q was refused as a stored password", good)
		}
	}
	for _, bad := range []string{"", "select", "ALL PRIVILEGES ", "SELECT,INSERT", "SELECT;"} {
		if usableDatabasePrivilege(bad) {
			t.Errorf("%q was accepted as a privilege", bad)
		}
	}
	for _, good := range []string{"SELECT", "ALL PRIVILEGES", "LOCK TABLES"} {
		if !usableDatabasePrivilege(good) {
			t.Errorf("%q was refused as a privilege", good)
		}
	}
}

// A user name in a backup is the name it had when the backup was taken. If
// another account holds it now, recreating it from this backup would hand
// that account's login -- and its password -- to whoever asked.
func TestADatabaseUserIsNotRecreatedForAnAccountThatLostTheName(t *testing.T) {
	fake := &Fake{
		Databases:    map[string][]string{"c1": {"c1_shop"}},
		DBUserOwners: map[string]string{"c1_shop": "c2"},
	}
	err := fake.PutDatabaseUsers(context.Background(), "c1", []panel.DatabaseUser{{
		Name: "c1_shop", Host: "localhost", Plugin: "mysql_native_password",
		Hash:   "2A46",
		Grants: []panel.DatabaseGrant{{Database: "c1_shop", Privileges: []string{"ALL PRIVILEGES"}}},
	}})
	if err == nil {
		t.Fatal("a user another account holds was recreated")
	}
	if len(fake.RestoredDBUsers) != 0 {
		t.Errorf("it reached the account anyway: %+v", fake.RestoredDBUsers)
	}
}

// A grant is only ever given on a database the account has now. The backup
// names what it had then, and that is not the same question.
func TestAGrantIsOnlyGivenOnTheAccountsOwnDatabase(t *testing.T) {
	fake := &Fake{Databases: map[string][]string{"c1": {"c1_shop"}, "c2": {"c2_secret"}}}
	err := fake.PutDatabaseUsers(context.Background(), "c1", []panel.DatabaseUser{{
		Name: "c1_shop", Host: "localhost", Plugin: "mysql_native_password",
		Hash:   "2A46",
		Grants: []panel.DatabaseGrant{{Database: "c2_secret", Privileges: []string{"ALL PRIVILEGES"}}},
	}})
	if err == nil {
		t.Fatal("a grant on another account's database was accepted")
	}
	if len(fake.RestoredDBUsers) != 0 {
		t.Errorf("it reached the account anyway: %+v", fake.RestoredDBUsers)
	}
}

// The database of a GRANT is a pattern. Written without the escape,
// "GRANT ... ON `c1_shop`.*" also covers c1Xshop -- another account's
// database if one exists by that name.
func TestAGrantNamesOneDatabaseAndNotEveryNameLikeIt(t *testing.T) {
	if got := grantPattern("c1_shop"); got != `c1\_shop` {
		t.Errorf("grantPattern(c1_shop) = %q", got)
	}
	if got := grantPattern("shop"); got != "shop" {
		t.Errorf("grantPattern(shop) = %q", got)
	}
}
