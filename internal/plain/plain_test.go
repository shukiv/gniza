package plain_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/panel"
	"github.com/shukiv/gniza/internal/pkgacct"
	"github.com/shukiv/gniza/internal/plain"
)

// fakeMySQL writes a mysql and a mysqldump that answer from a script:
// mysql prints the database list, or the sizes when asked from
// information_schema; mysqldump prints a dump, and fails for a database
// named in $GNIZA_BAD_DUMP.
func fakeMySQL(t *testing.T, databases ...string) (mysql, mysqldump string) {
	t.Helper()
	dir := t.TempDir()
	mysql = filepath.Join(dir, "mysql")
	mysqldump = filepath.Join(dir, "mysqldump")
	list := strings.Join(databases, "\n")
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *information_schema*) for d in " + strings.Join(databases, " ") + "; do printf '%s\\t4096\\n' \"$d\"; done ;;\n" +
		"  *'SHOW DATABASES'*) printf '%s\\n' '" + list + "' ;;\n" +
		"  *) cat > /dev/null; printf 'loaded %s\\n' \"$*\" >> \"$(dirname \"$0\")/mysql.log\" ;;\n" +
		"esac\n"
	if err := os.WriteFile(mysql, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	dump := "#!/bin/sh\n" +
		"for a; do name=$a; done\n" +
		"[ \"$name\" = \"${GNIZA_BAD_DUMP:-}\" ] && { echo 'mysqldump: Got error: 1146: Table gone' >&2; exit 2; }\n" +
		"printf -- '-- dump of %s\\n' \"$name\"\n"
	if err := os.WriteFile(mysqldump, []byte(dump), 0o755); err != nil {
		t.Fatal(err)
	}
	return mysql, mysqldump
}

func site(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(dir, "public"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "public", "index.php"), []byte("<?php echo 'hi';"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestEverySubdirectoryOfARootIsAnAccount: a LAMP server keeps one site
// per directory under /var/www, a container host one stack per directory
// under /opt. Those directories are the accounts; a file or a dot
// directory beside them is not.
func TestEverySubdirectoryOfARootIsAnAccount(t *testing.T) {
	www, opt := t.TempDir(), t.TempDir()
	site(t, www, "shop")
	site(t, www, "blog")
	site(t, opt, "stack")
	os.MkdirAll(filepath.Join(www, ".cache"), 0o755)
	os.WriteFile(filepath.Join(www, "index.html"), []byte("x"), 0o644)

	provider := &plain.Provider{Roots: []string{www, opt}}
	accounts, err := provider.Accounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range accounts {
		got = append(got, a.User+"="+a.HomeDir)
	}
	want := []string{
		"blog=" + filepath.Join(www, "blog"),
		"shop=" + filepath.Join(www, "shop"),
		"stack=" + filepath.Join(opt, "stack"),
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("accounts = %v, want %v", got, want)
	}
}

// TestAnAccountOwnsTheDatabasesNamedAfterIt follows the convention every
// panel uses: the database is called after the account, alone or with an
// underscore and a suffix. "shop10" is somebody else's.
func TestAnAccountOwnsTheDatabasesNamedAfterIt(t *testing.T) {
	www := t.TempDir()
	site(t, www, "shop")
	mysql, mysqldump := fakeMySQL(t, "information_schema", "mysql", "shop", "shop_wp", "shop10", "blog")
	provider := &plain.Provider{Roots: []string{www}, MySQLPath: mysql, MysqldumpPath: mysqldump}

	account, err := provider.Account(context.Background(), "shop")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(account.Databases, ",") != "shop,shop_wp" {
		t.Errorf("databases = %v, want [shop shop_wp]", account.Databases)
	}
	if account.HomeDir != filepath.Join(www, "shop") {
		t.Errorf("home = %s", account.HomeDir)
	}
	if account.SizeBytes == 0 {
		t.Error("the account was not measured")
	}
	if account.LeanBytes != 2*4096 {
		t.Errorf("lean bytes = %d, want the two databases' 8192", account.LeanBytes)
	}
	if _, err := provider.Account(context.Background(), "nobody"); err == nil {
		t.Error("an account that is not under any root was found")
	}
}

// TestAStagedAccountIsReadWhereItLiesWithItsDumpsBeside: the files are
// backed up from where they are, and what the staging directory holds
// is the dumps and a record of what this account is.
func TestAStagedAccountIsReadWhereItLiesWithItsDumpsBeside(t *testing.T) {
	www := t.TempDir()
	home := site(t, www, "shop")
	mysql, mysqldump := fakeMySQL(t, "shop", "shop_wp")
	provider := &plain.Provider{Roots: []string{www}, MySQLPath: mysql, MysqldumpPath: mysqldump}
	staging := t.TempDir()

	account, err := provider.Account(context.Background(), "shop")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := provider.Stage(context.Background(), panel.StageRequest{
		Account: account, StagingDir: staging, Mode: pkgacct.ModeSplit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !provider.ReadsHomeInPlace() {
		t.Error("the provider does not say it reads the files where they lie")
	}
	var metadata, homedir string
	for _, part := range payload.Parts {
		switch part.Kind {
		case pkgacct.PartMetadata:
			metadata = part.Path
		case pkgacct.PartHomedir:
			homedir = part.Path
		}
	}
	if homedir != home {
		t.Errorf("homedir part = %q, want the account's own directory %q", homedir, home)
	}
	if metadata == "" || !strings.HasPrefix(metadata, staging) {
		t.Errorf("metadata part = %q, want a directory under %s", metadata, staging)
	}
	for _, db := range []string{"shop", "shop_wp"} {
		body, err := os.ReadFile(payload.DumpPaths[db])
		if err != nil {
			t.Errorf("dump of %s: %v", db, err)
			continue
		}
		if !strings.Contains(string(body), "dump of "+db) {
			t.Errorf("dump of %s holds %q", db, body)
		}
		if !strings.HasPrefix(payload.DumpPaths[db], metadata) {
			t.Errorf("dump of %s is at %s, outside the metadata part", db, payload.DumpPaths[db])
		}
	}
	record, err := os.ReadFile(filepath.Join(metadata, "gniza", "account.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Account   string   `json:"account"`
		Path      string   `json:"path"`
		Databases []string `json:"databases"`
	}
	if err := json.Unmarshal(record, &got); err != nil {
		t.Fatal(err)
	}
	if got.Account != "shop" || got.Path != home || strings.Join(got.Databases, ",") != "shop,shop_wp" {
		t.Errorf("account.json = %s", record)
	}
	if len(payload.Missing) != 0 {
		t.Errorf("missing = %v", payload.Missing)
	}
}

// TestADumpThatFailsIsAnOmissionNotARefusal keeps ADR 0017: a backup
// with a hole in it is worth having, and worth being told about.
func TestADumpThatFailsIsAnOmissionNotARefusal(t *testing.T) {
	www := t.TempDir()
	site(t, www, "shop")
	mysql, mysqldump := fakeMySQL(t, "shop", "shop_wp")
	t.Setenv("GNIZA_BAD_DUMP", "shop_wp")
	provider := &plain.Provider{Roots: []string{www}, MySQLPath: mysql, MysqldumpPath: mysqldump}

	account, err := provider.Account(context.Background(), "shop")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := provider.Stage(context.Background(), panel.StageRequest{
		Account: account, StagingDir: t.TempDir(), Mode: pkgacct.ModeSplit,
	})
	if err != nil {
		t.Fatalf("one bad dump refused the whole backup: %v", err)
	}
	if len(payload.Missing) != 1 || !strings.Contains(payload.Missing[0].String(), "shop_wp") ||
		!strings.Contains(payload.Missing[0].String(), "Table gone") {
		t.Errorf("missing = %v, want shop_wp with mysqldump's own words", payload.Missing)
	}
	if _, ok := payload.DumpPaths["shop_wp"]; ok {
		t.Error("a dump that failed is still listed as taken")
	}
	if _, err := os.Stat(payload.DumpPaths["shop"]); err != nil {
		t.Errorf("the good dump was not kept: %v", err)
	}
}

// TestASkipOfTheDatabasesLeavesNoDumps: a schedule that says files only
// gets files only.
func TestASkipOfTheDatabasesLeavesNoDumps(t *testing.T) {
	www := t.TempDir()
	site(t, www, "shop")
	mysql, mysqldump := fakeMySQL(t, "shop")
	provider := &plain.Provider{Roots: []string{www}, MySQLPath: mysql, MysqldumpPath: mysqldump}
	account, _ := provider.Account(context.Background(), "shop")
	payload, err := provider.Stage(context.Background(), panel.StageRequest{
		Account: account, StagingDir: t.TempDir(), Mode: pkgacct.ModeSplit, SkipDatabases: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(payload.DumpPaths) != 0 {
		t.Errorf("dumps = %v", payload.DumpPaths)
	}
}

// TestOnlyTheSplitShapeIsStaged: there is no native archive to make.
func TestOnlyTheSplitShapeIsStaged(t *testing.T) {
	www := t.TempDir()
	site(t, www, "shop")
	provider := &plain.Provider{Roots: []string{www}}
	account, _ := provider.Account(context.Background(), "shop")
	_, err := provider.Stage(context.Background(), panel.StageRequest{
		Account: account, StagingDir: t.TempDir(), Mode: pkgacct.ModeMonolithic,
	})
	if err == nil {
		t.Fatal("a monolithic backup of a plain server was accepted")
	}
}

// TestTheNativeRestoreIsRefusedAsUnverified: there is no panel to hand
// an archive to. Files go back with PutHomeDir, dumps with LoadDatabase.
func TestTheNativeRestoreIsRefusedAsUnverified(t *testing.T) {
	provider := &plain.Provider{Roots: []string{t.TempDir()}}
	_, err := provider.Apply(context.Background(), "/nowhere.tar", panel.ApplyOptions{Overwrite: true, Unrestricted: true})
	if !errors.Is(err, plain.ErrUnverified) {
		t.Errorf("Apply = %v, want ErrUnverified", err)
	}
	if err := provider.PutCrontab(context.Background(), "shop", t.TempDir()); !errors.Is(err, plain.ErrUnverified) {
		t.Errorf("PutCrontab = %v, want ErrUnverified", err)
	}
	if err := provider.PutDatabaseUsers(context.Background(), "shop", nil); !errors.Is(err, plain.ErrUnverified) {
		t.Errorf("PutDatabaseUsers = %v, want ErrUnverified", err)
	}
}

// TestTheFilesGoBackWhereTheyWere: PutHomeDir writes the restored tree
// over the account's own directory, and a file that was added since
// stays.
func TestTheFilesGoBackWhereTheyWere(t *testing.T) {
	www := t.TempDir()
	home := site(t, www, "shop")
	os.WriteFile(filepath.Join(home, "public", "new.txt"), []byte("since"), 0o644)
	from := t.TempDir()
	os.MkdirAll(filepath.Join(from, "public"), 0o755)
	os.WriteFile(filepath.Join(from, "public", "index.php"), []byte("<?php echo 'restored';"), 0o644)
	os.WriteFile(filepath.Join(from, "config.php"), []byte("<?php"), 0o600)

	provider := &plain.Provider{Roots: []string{www}}
	if err := provider.PutHomeDir(context.Background(), "shop", from); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(home, "public", "index.php"))
	if string(body) != "<?php echo 'restored';" {
		t.Errorf("index.php = %q", body)
	}
	if _, err := os.Stat(filepath.Join(home, "public", "new.txt")); err != nil {
		t.Error("a file added since the backup was removed by the restore")
	}
	info, err := os.Stat(filepath.Join(home, "config.php"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("config.php mode = %o, want 600", info.Mode().Perm())
	}
	if err := provider.PutHomeDir(context.Background(), "nobody", from); err == nil {
		t.Error("files were written for an account that is not under any root")
	}
}

// TestADumpIsLoadedIntoTheAccountsOwnDatabase: LoadDatabase feeds the
// dump to mysql, and only for a database the account owns by name.
func TestADumpIsLoadedIntoTheAccountsOwnDatabase(t *testing.T) {
	www := t.TempDir()
	site(t, www, "shop")
	mysql, mysqldump := fakeMySQL(t, "shop", "blog")
	provider := &plain.Provider{Roots: []string{www}, MySQLPath: mysql, MysqldumpPath: mysqldump}
	dump := filepath.Join(t.TempDir(), "shop.sql")
	os.WriteFile(dump, []byte("CREATE TABLE t (id int);"), 0o600)

	if err := provider.LoadDatabase(context.Background(), "shop", "shop", dump); err != nil {
		t.Fatal(err)
	}
	log, _ := os.ReadFile(filepath.Join(filepath.Dir(mysql), "mysql.log"))
	if !strings.Contains(string(log), "loaded") || !strings.Contains(string(log), "shop") {
		t.Errorf("mysql was not asked to load the dump: %q", log)
	}
	if err := provider.LoadDatabase(context.Background(), "shop", "blog", dump); err == nil {
		t.Error("a dump was loaded into a database the account does not own")
	}
	if err := provider.CreateDatabase(context.Background(), "shop", "blog"); err == nil {
		t.Error("a database was created under a name the account does not own")
	}
	if err := provider.CreateDatabase(context.Background(), "shop", "shop_new"); err != nil {
		t.Errorf("CreateDatabase = %v", err)
	}
}

// TestTheLayoutNamesWhatAPlainServerHas: files and databases, settings
// as the record beside them, and no domains, zones or mailboxes.
func TestTheLayoutNamesWhatAPlainServerHas(t *testing.T) {
	provider := &plain.Provider{}
	layout := provider.Layout()
	if layout.Panel() != provider.Name() {
		t.Errorf("layout panel %q, provider name %q", layout.Panel(), provider.Name())
	}
	if layout.HomedirDir() == "" || layout.DatabaseDir() == "" {
		t.Error("the layout does not say where files and dumps go")
	}
	tree := t.TempDir()
	os.MkdirAll(filepath.Join(tree, "shop", layout.HomedirDir()), 0o755)
	root, err := layout.AccountRoot(tree, "shop")
	if err != nil || root != filepath.Join(tree, "shop") {
		t.Errorf("AccountRoot = %q, %v", root, err)
	}
	if _, err := layout.AccountRoot(tree, "blog"); err == nil {
		t.Error("a tree that does not hold the account was accepted")
	}
	if len(layout.SettingsMembers()) == 0 {
		t.Error("the account record is not a settings member")
	}
	if _, err := layout.DNSMembers(nil); err == nil {
		t.Error("a plain server claimed to have DNS zones")
	}
	if len(layout.MailboxPaths([]string{"a@b"})) != 0 || len(layout.WebsitePaths()) == 0 {
		t.Errorf("mailboxes = %v, website = %v", layout.MailboxPaths([]string{"a@b"}), layout.WebsitePaths())
	}
}

// TestAnAccountsIdentityIsItsDirectory: a directory removed and made
// again under the same name is another account, and a name with no
// directory is no account.
func TestAnAccountsIdentityIsItsDirectory(t *testing.T) {
	www := t.TempDir()
	home := site(t, www, "shop")
	provider := &plain.Provider{Roots: []string{www}}
	first, err := provider.AccountIdentity("shop")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(home); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.AccountIdentity("shop"); !errors.Is(err, panel.ErrNoSuchAccount) {
		t.Errorf("a name whose directory is gone = %v, want ErrNoSuchAccount", err)
	}
	// The kernel stamps a directory's birth with its coarse clock, a few
	// milliseconds wide, and ext4 hands a freed inode number straight
	// back: two directories made within the same tick are the same to
	// stat. Nobody removes and recreates a site inside four milliseconds.
	time.Sleep(20 * time.Millisecond)
	site(t, www, "shop")
	second, err := provider.AccountIdentity("shop")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Error("the same identity was given to a directory made again under the old name")
	}
}
