package cpanel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/panel"
)

func databaseUserRestoreHost(t *testing.T) (*Real, string) {
	t.Helper()
	dir := t.TempDir()
	record := `{"MYSQL":{"owner":"customer1","dbs":{"customer1_wp":{}},"dbusers":{"customer1_app":{}},"noprefix":{}}}`
	if err := os.WriteFile(filepath.Join(dir, "customer1.json"), []byte(record), 0600); err != nil {
		t.Fatal(err)
	}
	mutations := filepath.Join(dir, "mutations")
	client := filepath.Join(dir, "mysql")
	body := "#!/bin/sh\nfor arg in \"$@\"; do case \"$arg\" in --execute=*) printf '726F6F74\\n73657276696365\\n637573746F6D6572315F617070\\n'; exit 0;; esac; done\ncat >> " + shellQuoteForTest(mutations) + "\n"
	if err := os.WriteFile(client, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	return &Real{DatabasesDir: dir, MysqlPath: client, DBMapTool: "/bin/true", UapiPath: "/bin/true"}, mutations
}

func TestRestoringDatabaseUsersCannotTakeOverUnmappedServerLogins(t *testing.T) {
	for _, name := range []string{"root", "service"} {
		t.Run(name, func(t *testing.T) {
			r, mutations := databaseUserRestoreHost(t)
			err := r.PutDatabaseUsers(t.Context(), "customer1", []panel.DatabaseUser{{Name: name, Host: "localhost", Plugin: "mysql_native_password", Hash: "2A46"}})
			if err == nil {
				t.Fatal("unmapped server login was accepted")
			}
			if body, _ := os.ReadFile(mutations); len(body) != 0 {
				t.Fatalf("SQL ran before ownership was proved: %s", body)
			}
		})
	}
}

func TestDatabaseUserRestoreFailsClosedOnUnreadableOwnership(t *testing.T) {
	r, mutations := databaseUserRestoreHost(t)
	if err := os.WriteFile(filepath.Join(r.DatabasesDir, "customer2.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	err := r.PutDatabaseUsers(t.Context(), "customer1", []panel.DatabaseUser{{Name: "newuser", Host: "localhost", Plugin: "mysql_native_password", Hash: "2A46"}})
	if err == nil {
		t.Fatal("a partial ownership index authorized a restore")
	}
	if body, _ := os.ReadFile(mutations); len(body) != 0 {
		t.Fatalf("SQL ran with an unreadable ownership index: %s", body)
	}
}

func TestDeletedDatabaseUserMustBeCreatedWithoutAlteringACollidingLogin(t *testing.T) {
	r, mutations := databaseUserRestoreHost(t)
	err := r.PutDatabaseUsers(t.Context(), "customer1", []panel.DatabaseUser{{Name: "newuser", Host: "localhost", Plugin: "mysql_native_password", Hash: "2A46"}})
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(mutations)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "IF NOT EXISTS") || strings.Contains(string(body), "ALTER USER") {
		t.Fatalf("a user created after the ownership check could be taken over: %s", body)
	}
}
