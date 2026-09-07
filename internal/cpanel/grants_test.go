package cpanel

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/panel"
)

// fakeMysql is a stand-in for the mysql client that answers the two
// queries the grants writer asks, with what a real MySQL 8 answered on
// cPanel 136.
func fakeMysql(t *testing.T) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "mysql")
	body := `#!/bin/sh
for arg in "$@"; do last="$arg"; done
case "$last" in
  *"FROM mysql.user WHERE user = 'cprtest1_app'"*)
    # HEX(authentication_string), which is what the query asks for now: a
    # caching_sha2_password string is binary and is lost read as text.
    printf 'cprtest1_app\tlocalhost\t2A45323337383134443141414141414141414141414141414141414141414141414141414141414141\tmysql_native_password\n'
    ;;
  *"SHOW GRANTS FOR "*)
    printf 'GRANT USAGE ON *.* TO ` + "`cprtest1_app`@`localhost`" + `\n'
    printf 'GRANT ALL PRIVILEGES ON ` + "`cprtest1\\\\_proof`.* TO `cprtest1_app`@`localhost`" + `\n'
    printf 'GRANT SELECT ON ` + "`cprtest1\\\\_proof`.* TO `cprtest1_app`@`localhost` WITH GRANT OPTION" + `\n'
    ;;
esac
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// TestTheGrantsFileIsTheOneRestorepkgReads covers what a live restore
// turned up: a file of modern CREATE USER statements is valid SQL, sits
// exactly where a cpmove archive keeps its grants, and restorepkg ignores
// every line of it. The account came back with its tables and without the
// user that reads them.
//
// What is asserted here is the shape /scripts/pkgacct produced on cPanel
// 136.0.37: the password on the USAGE line, single quotes around the
// account, and the database name backtick-quoted with a single backslash
// before the underscore -- read through "mysql --batch" without --raw the
// client doubles that backslash, and the restored grant then names a
// database that does not exist.
func TestTheGrantsFileIsTheOneRestorepkgReads(t *testing.T) {
	dir := t.TempDir()
	databasesDir := filepath.Join(dir, "databases")
	if err := os.MkdirAll(databasesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	record := `{"MYSQL":{"owner":"cprtest1","dbs":{"cprtest1_proof":"127.0.0.1"},` +
		`"dbusers":{"cprtest1_app":{}},"noprefix":{}},"version":1}`
	if err := os.WriteFile(filepath.Join(databasesDir, "cprtest1.json"),
		[]byte(record), 0o600); err != nil {
		t.Fatal(err)
	}

	host := &Real{MysqlPath: fakeMysql(t), DatabasesDir: databasesDir}
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := host.dumpDatabaseUsers(context.Background(),
		panel.StageRequest{Account: panel.AccountInfo{User: "cprtest1"}}, out); err != nil {
		t.Fatalf("dumpDatabaseUsers: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(out, "_users.sql"))
	if err != nil {
		t.Fatal(err)
	}
	written := string(raw)

	const usage = "GRANT USAGE ON *.* TO 'cprtest1_app'@'localhost' IDENTIFIED BY PASSWORD " +
		"'*E237814D1AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA';"
	if !strings.Contains(written, usage) {
		t.Errorf("the password is not on the USAGE line, which is where cPanel keeps it:\n%s", written)
	}
	if strings.Contains(written, "CREATE USER") {
		t.Error("the file still uses statements restorepkg does not read")
	}
	if strings.Contains(written, `\\_proof`) {
		t.Errorf("the database name carries an escaped backslash, so the grant names a "+
			"database that does not exist:\n%s", written)
	}
	if !strings.Contains(written, "`cprtest1\\_proof`") {
		t.Errorf("the database name lost the escape MySQL needs:\n%s", written)
	}
	// A grant an operator made by hand can carry a trailing clause, and
	// the quoting has to leave it alone: one malformed line in the middle
	// of the file restorepkg reads is a half-restored account.
	const withOption = "TO 'cprtest1_app'@'localhost' WITH GRANT OPTION"
	if !strings.Contains(written, withOption) {
		t.Errorf("a grant with a trailing clause was mangled:\n%s", written)
	}

	// And the same users written so a person can run them. cPanel's
	// format uses GRANT ... IDENTIFIED BY PASSWORD, which MySQL 8
	// removed: handing somebody who asked for their database users a
	// file their database refuses is not giving them their database
	// users.
	runnableRaw, err := os.ReadFile(filepath.Join(out, "_users-runnable.sql"))
	if err != nil {
		t.Fatalf("no runnable copy of the users beside the grants: %v", err)
	}
	runnable := string(runnableRaw)
	const create = "CREATE USER IF NOT EXISTS `cprtest1_app`@`localhost` " +
		"IDENTIFIED WITH 'mysql_native_password' AS " +
		"'*E237814D1AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA';"
	if !strings.Contains(runnable, create) {
		t.Errorf("the runnable copy cannot create the user:\n%s", runnable)
	}
	if strings.Contains(runnable, "IDENTIFIED BY PASSWORD") {
		t.Errorf("the runnable copy uses syntax MySQL 8 removed:\n%s", runnable)
	}
	if !strings.Contains(runnable, "`cprtest1\\_proof`") {
		t.Errorf("the runnable copy lost the grants:\n%s", runnable)
	}

	// The authentication file travels with it: "IDENTIFIED BY PASSWORD"
	// is not valid on MySQL 8, and this is where cPanel's restore reads
	// the real hash and plugin from.
	authRaw, err := os.ReadFile(filepath.Join(out, "_users.sql-auth.json"))
	if err != nil {
		t.Fatalf("no authentication file beside the grants: %v", err)
	}
	var auth map[string]map[string]struct {
		PassHash string `json:"pass_hash"`
		Plugin   string `json:"auth_plugin"`
	}
	if err := json.Unmarshal(authRaw, &auth); err != nil {
		t.Fatal(err)
	}
	entry, found := auth["cprtest1_app"]["localhost"]
	if !found {
		t.Fatalf("the user is not in the authentication file: %s", authRaw)
	}
	if entry.Plugin != "mysql_native_password" {
		t.Errorf("plugin = %q, want the one MySQL reported", entry.Plugin)
	}
	// cPanel stores the hash as hex of its printable form.
	if !strings.HasPrefix(entry.PassHash, "2a") && !strings.HasPrefix(entry.PassHash, "2A") {
		t.Errorf("pass_hash = %q, want the hash hex-encoded as cPanel writes it", entry.PassHash)
	}
}
