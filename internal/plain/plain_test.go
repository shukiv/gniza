package plain_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/panel"
	"github.com/shukiv/gniza/internal/pkgacct"
	"github.com/shukiv/gniza/internal/plain"
)

// catalog keeps choices in memory, as the node's store does on disk.
type catalog struct{ sources map[string]panel.Source }

func newCatalog() *catalog { return &catalog{sources: map[string]panel.Source{}} }

func (c *catalog) Sources() ([]panel.Source, error) {
	var out []panel.Source
	for _, s := range c.sources {
		out = append(out, s)
	}
	return out, nil
}
func (c *catalog) SaveSource(s panel.Source) error { c.sources[s.Name] = s; return nil }
func (c *catalog) DeleteSource(name string) error  { delete(c.sources, name); return nil }

// fakeMySQL writes a mysql and a mysqldump that answer from a script:
// mysql prints the sizes when asked from information_schema; mysqldump
// prints a dump, and fails for a database named in $GNIZA_BAD_DUMP.
func fakeMySQL(t *testing.T, databases ...string) (mysql, mysqldump string) {
	t.Helper()
	dir := t.TempDir()
	mysql = filepath.Join(dir, "mysql")
	mysqldump = filepath.Join(dir, "mysqldump")
	// Every database has one account, <name>_app@localhost, with rights
	// on it; SHOW GRANTS answers for any account with one line.
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *schema_privileges*) for d in " + strings.Join(databases, " ") + "; do printf \"%s\\t'%s_app'@'localhost'\\n\" \"$d\" \"$d\"; done ;;\n" +
		"  *information_schema*) for d in " + strings.Join(databases, " ") + "; do printf '%s\\t4096\\n' \"$d\"; done ;;\n" +
		"  *SHOW\\ GRANTS*) printf 'GRANT ALL PRIVILEGES ON `x`.* TO %s\\n' \"${*##*FOR }\" ;;\n" +
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

// fakePostgres writes a psql and a pg_dump: psql lists the databases
// with their sizes, and logs a CREATE DATABASE or a load; pg_dump prints
// a dump.
func fakePostgres(t *testing.T, databases ...string) (psql, pgdump string) {
	t.Helper()
	dir := t.TempDir()
	psql = filepath.Join(dir, "psql")
	pgdump = filepath.Join(dir, "pg_dump")
	// Each database is owned by <name>_owner. pg_dumpall lives beside
	// pg_dump and answers --roles-only with one role.
	roles := "#!/bin/sh\nprintf 'CREATE ROLE erp_owner;\\n'\n"
	if err := os.WriteFile(filepath.Join(dir, "pg_dumpall"), []byte(roles), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *pg_database_size*) for d in " + strings.Join(databases, " ") + "; do printf '%s\\t8192\\t%s_owner\\n' \"$d\" \"$d\"; done ;;\n" +
		"  *) cat > /dev/null; printf 'psql %s\\n' \"$*\" >> \"$(dirname \"$0\")/psql.log\" ;;\n" +
		"esac\n"
	if err := os.WriteFile(psql, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	dump := "#!/bin/sh\nfor a; do name=$a; done\nprintf -- '-- pg dump of %s\\n' \"$name\"\n"
	if err := os.WriteFile(pgdump, []byte(dump), 0o755); err != nil {
		t.Fatal(err)
	}
	return psql, pgdump
}

// fakeDocker writes a docker that lists the containers given and
// answers inspect from $GNIZA_INSPECT_DIR/<name>.json.
func fakeDocker(t *testing.T, inspectDir string, containers ...string) string {
	t.Helper()
	docker := filepath.Join(t.TempDir(), "docker")
	var lines []string
	for _, c := range containers {
		lines = append(lines, c+"\\timage:1\\tUp 2 hours")
	}
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  ps) printf '" + strings.Join(lines, "\\n") + "\\n' ;;\n" +
		// inspect answers one array for every name asked, the way the
		// engines do; each file holds a one-element array.
		"  inspect) shift; out=''; for name; do case $name in --type|container) continue;; esac; body=$(cat \"" + inspectDir + "/$name.json\" 2>/dev/null) || { echo \"Error: no such container: $name\" >&2; exit 1; }; body=${body#[}; body=${body%]}; out=\"${out:+$out,}$body\"; done; printf '[%s]\\n' \"$out\" ;;\n" +
		"  *) exit 1 ;;\n" +
		"esac\n"
	if err := os.WriteFile(docker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return docker
}

func folderPaths(c panel.Candidates) []string {
	var paths []string
	for _, f := range c.Folders {
		paths = append(paths, f.Path)
	}
	return paths
}

func databaseNames(databases []panel.DatabaseCandidate) []string {
	var names []string
	for _, d := range databases {
		names = append(names, d.Name)
	}
	return names
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

func chosen(t *testing.T, provider *plain.Provider, source panel.Source) {
	t.Helper()
	if err := provider.AddSource(context.Background(), source); err != nil {
		t.Fatalf("AddSource(%s): %v", source.Name, err)
	}
}

// TestNothingIsBackedUpUntilChosen: the directories under the roots are
// offered, not assumed. A folder becomes a source when the operator
// says so, and is no longer offered once it is.
func TestNothingIsBackedUpUntilChosen(t *testing.T) {
	www, opt := t.TempDir(), t.TempDir()
	shop := site(t, www, "shop")
	site(t, www, "blog")
	site(t, opt, "stack")
	os.MkdirAll(filepath.Join(www, ".cache"), 0o755)
	os.WriteFile(filepath.Join(www, "index.html"), []byte("x"), 0o644)
	provider := &plain.Provider{Roots: []string{www, opt, "/nowhere"}, Catalog: newCatalog(), PostgresUser: "-"}

	accounts, err := provider.Accounts(context.Background())
	if err != nil || len(accounts) != 0 {
		t.Fatalf("accounts before any choice = %v, %v", accounts, err)
	}
	candidates, err := provider.Candidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(www, "blog"), shop, filepath.Join(opt, "stack")}
	if strings.Join(folderPaths(candidates), " ") != strings.Join(want, " ") {
		t.Errorf("folders offered = %v, want %v", candidates.Folders, want)
	}

	chosen(t, provider, panel.Source{Path: shop})
	accounts, _ = provider.Accounts(context.Background())
	if len(accounts) != 1 || accounts[0].User != "shop" || accounts[0].HomeDir != shop || accounts[0].Missing {
		t.Errorf("accounts after choosing = %+v", accounts)
	}
	candidates, _ = provider.Candidates(context.Background())
	for _, folder := range candidates.Folders {
		if folder.Path == shop && folder.ChosenAs != "shop" {
			t.Error("a folder already chosen is offered as if it were not")
		}
	}
	if err := provider.RemoveSource(context.Background(), "shop"); err != nil {
		t.Fatal(err)
	}
	if accounts, _ = provider.Accounts(context.Background()); len(accounts) != 0 {
		t.Errorf("accounts after removing = %v", accounts)
	}
	if err := provider.RemoveSource(context.Background(), "shop"); !errors.Is(err, panel.ErrNoSuchAccount) {
		t.Errorf("removing what is not chosen = %v", err)
	}
}

// TestAChoiceIsChecked: what cannot be backed up is refused where the
// operator can read why, not on the night.
func TestAChoiceIsChecked(t *testing.T) {
	www := t.TempDir()
	shop := site(t, www, "shop")
	provider := &plain.Provider{Roots: []string{www}, Catalog: newCatalog(), PostgresUser: "-"}
	chosen(t, provider, panel.Source{Name: "shop", Path: shop})

	for _, refused := range []panel.Source{
		{},
		{Name: "rel", Path: "var/www"},
		{Name: "file", Path: filepath.Join(shop, "public", "index.php")},
		{Name: "gone", Path: filepath.Join(www, "gone")},
		{Name: "shop", Path: www},
		{Name: "again", Path: shop},
		{Name: "bad name!", Path: www},
		{Name: "baddb", MySQL: []string{"drop table"}},
		{Name: "root", Path: "/"},
	} {
		if err := provider.AddSource(context.Background(), refused); err == nil {
			t.Errorf("%+v was accepted", refused)
		}
	}
	if err := provider.AddSource(context.Background(), panel.Source{MySQL: []string{"erp"}}); err != nil {
		t.Errorf("a source of databases only was refused: %v", err)
	}
	sources, _ := provider.Sources(context.Background())
	if len(sources) != 2 || sources[0].Name != "mysql-erp" || sources[0].Path != "" {
		t.Errorf("sources = %+v; a database-only source should be named after its engine and database", sources)
	}
}

// TestASourceCarriesTheDatabasesTicked: the databases are the operator's
// choice, not a naming rule, and a PostgreSQL one is marked so a dump's
// name says which client it came from.
func TestASourceCarriesTheDatabasesTicked(t *testing.T) {
	www := t.TempDir()
	shop := site(t, www, "shop")
	mysql, mysqldump := fakeMySQL(t, "shop", "shop_wp", "blog")
	psql, pgdump := fakePostgres(t, "erp", "crm")
	provider := &plain.Provider{Roots: []string{www}, Catalog: newCatalog(),
		MySQLPath: mysql, MysqldumpPath: mysqldump, PsqlPath: psql, PgDumpPath: pgdump, PostgresUser: "-"}

	candidates, _ := provider.Candidates(context.Background())
	if strings.Join(databaseNames(candidates.MySQL), ",") != "blog,shop,shop_wp" || strings.Join(databaseNames(candidates.PostgreSQL), ",") != "crm,erp" {
		t.Errorf("databases offered: mysql %v, postgresql %v", candidates.MySQL, candidates.PostgreSQL)
	}
	// Whose data each is: MySQL's grantees, PostgreSQL's owner.
	if candidates.MySQL[1].Users == nil || candidates.MySQL[1].Users[0] != "'shop_app'@'localhost'" ||
		candidates.PostgreSQL[1].Users == nil || candidates.PostgreSQL[1].Users[0] != "erp_owner" {
		t.Errorf("users listed: mysql %+v, postgresql %+v", candidates.MySQL[1], candidates.PostgreSQL[1])
	}
	chosen(t, provider, panel.Source{Name: "shop", Path: shop, MySQL: []string{"shop_wp", "shop"}, PostgreSQL: []string{"erp"}})

	account, err := provider.Account(context.Background(), "shop")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(account.Databases, ",") != "shop,shop_wp,erp.pg" {
		t.Errorf("databases = %v, want [shop shop_wp erp.pg]", account.Databases)
	}
	if account.SizeBytes == 0 {
		t.Error("the source was not measured")
	}
	if account.LeanBytes != 2*4096+8192 {
		t.Errorf("lean bytes = %d, want the three databases' 16384", account.LeanBytes)
	}
	if _, err := provider.Account(context.Background(), "nobody"); !errors.Is(err, panel.ErrNoSuchAccount) {
		t.Errorf("a name never chosen = %v", err)
	}
}

// TestAStagedSourceIsReadWhereItLiesWithItsDumpsBeside: the files are
// backed up from where they are, and what the staging directory holds
// is the dumps and a record of what this source is.
func TestAStagedSourceIsReadWhereItLiesWithItsDumpsBeside(t *testing.T) {
	www := t.TempDir()
	home := site(t, www, "shop")
	mysql, mysqldump := fakeMySQL(t, "shop", "shop_wp")
	psql, pgdump := fakePostgres(t, "erp")
	provider := &plain.Provider{Roots: []string{www}, Catalog: newCatalog(),
		MySQLPath: mysql, MysqldumpPath: mysqldump, PsqlPath: psql, PgDumpPath: pgdump, PostgresUser: "-"}
	chosen(t, provider, panel.Source{Name: "shop", Path: home, MySQL: []string{"shop", "shop_wp"}, PostgreSQL: []string{"erp"}})
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
		t.Errorf("homedir part = %q, want the source's own directory %q", homedir, home)
	}
	if metadata == "" || !strings.HasPrefix(metadata, staging) {
		t.Errorf("metadata part = %q, want a directory under %s", metadata, staging)
	}
	for db, want := range map[string]string{"shop": "dump of shop", "shop_wp": "dump of shop_wp", "erp.pg": "pg dump of erp"} {
		body, err := os.ReadFile(payload.DumpPaths[db])
		if err != nil {
			t.Errorf("dump of %s: %v", db, err)
			continue
		}
		if !strings.Contains(string(body), want) {
			t.Errorf("dump of %s holds %q", db, body)
		}
		if !strings.HasPrefix(payload.DumpPaths[db], filepath.Join(metadata, "databases")) ||
			!strings.HasSuffix(payload.DumpPaths[db], db+".sql") {
			t.Errorf("dump of %s is at %s", db, payload.DumpPaths[db])
		}
	}
	record, err := os.ReadFile(filepath.Join(metadata, "gniza", "account.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got plain.Record
	if err := json.Unmarshal(record, &got); err != nil {
		t.Fatal(err)
	}
	if got.Account != "shop" || got.Path != home || strings.Join(got.Databases, ",") != "shop,shop_wp,erp.pg" ||
		strings.Join(got.MySQL, ",") != "shop,shop_wp" || strings.Join(got.PostgreSQL, ",") != "erp" {
		t.Errorf("account.json = %s", record)
	}
	if len(payload.Missing) != 0 {
		t.Errorf("missing = %v", payload.Missing)
	}
}

// TestASourceOfDatabasesOnlyStillHasAFilesPart: a snapshot is read back
// through one home directory part and one metadata part, so a source
// with no folder is given one holding a note, and its files cannot be restored
// because there are none.
func TestASourceOfDatabasesOnlyStillHasAFilesPart(t *testing.T) {
	mysql, mysqldump := fakeMySQL(t, "erp")
	provider := &plain.Provider{Catalog: newCatalog(), MySQLPath: mysql, MysqldumpPath: mysqldump, PostgresUser: "-"}
	chosen(t, provider, panel.Source{Name: "erp", MySQL: []string{"erp"}})
	staging := t.TempDir()
	account, _ := provider.Account(context.Background(), "erp")
	payload, err := provider.Stage(context.Background(), panel.StageRequest{
		Account: account, StagingDir: staging, Mode: pkgacct.ModeSplit,
	})
	if err != nil {
		t.Fatal(err)
	}
	var homedir string
	for _, part := range payload.Parts {
		if part.Kind == pkgacct.PartHomedir {
			homedir = part.Path
		}
	}
	if !strings.HasPrefix(homedir, staging) {
		t.Errorf("files part = %q, want an empty directory under %s", homedir, staging)
	}
	if entries, err := os.ReadDir(homedir); err != nil || len(entries) != 1 || entries[0].Name() != plain.NoFolderNote {
		t.Errorf("files part holds %v, %v; want only the note", entries, err)
	}
	if _, ok := payload.DumpPaths["erp"]; !ok {
		t.Error("the database was not dumped")
	}
	// Who used it is kept beside the record, not under the dumps where
	// every .sql is expected to create something.
	grants, err := os.ReadFile(filepath.Join(staging, "metadata", plain.RecordDir, plain.MySQLGrantsFile("erp")))
	if err != nil || !strings.Contains(string(grants), "'erp_app'@'localhost'") || !strings.Contains(string(grants), "GRANT ALL") {
		t.Errorf("the grants beside the record = %q, %v", grants, err)
	}
	if entries, _ := os.ReadDir(filepath.Join(staging, "metadata", plain.DumpDir)); len(entries) != 1 {
		t.Errorf("the dump directory holds %d files, want the dump alone", len(entries))
	}
	// A database source nobody named is named after its engine and
	// database, so it does not fight a folder of the same name.
	chosen(t, provider, panel.Source{MySQL: []string{"erp"}})
	if _, err := provider.Account(context.Background(), "mysql-erp"); err != nil {
		t.Errorf("the unnamed database source is not mysql-erp: %v", err)
	}
	if err := provider.PutHomeDir(context.Background(), "erp", t.TempDir()); err == nil {
		t.Error("files were restored for a source that has none")
	}
	if id, err := provider.AccountIdentity("erp"); err != nil || id == 0 {
		t.Errorf("identity of a database-only source = %d, %v", id, err)
	}
}

// TestADumpThatFailsIsAnOmissionNotARefusal keeps ADR 0017: a backup
// with a hole in it is worth having, and worth being told about.
func TestADumpThatFailsIsAnOmissionNotARefusal(t *testing.T) {
	www := t.TempDir()
	shop := site(t, www, "shop")
	mysql, mysqldump := fakeMySQL(t, "shop", "shop_wp")
	t.Setenv("GNIZA_BAD_DUMP", "shop_wp")
	provider := &plain.Provider{Roots: []string{www}, Catalog: newCatalog(), MySQLPath: mysql, MysqldumpPath: mysqldump, PostgresUser: "-"}
	chosen(t, provider, panel.Source{Name: "shop", Path: shop, MySQL: []string{"shop", "shop_wp"}})

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
	shop := site(t, www, "shop")
	mysql, mysqldump := fakeMySQL(t, "shop")
	provider := &plain.Provider{Roots: []string{www}, Catalog: newCatalog(), MySQLPath: mysql, MysqldumpPath: mysqldump, PostgresUser: "-"}
	chosen(t, provider, panel.Source{Name: "shop", Path: shop, MySQL: []string{"shop"}})
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
	shop := site(t, www, "shop")
	provider := &plain.Provider{Roots: []string{www}, Catalog: newCatalog(), PostgresUser: "-"}
	chosen(t, provider, panel.Source{Name: "shop", Path: shop})
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
	provider := &plain.Provider{Roots: []string{t.TempDir()}, Catalog: newCatalog()}
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
	if err := (&plain.Provider{}).AddSource(context.Background(), panel.Source{Path: t.TempDir()}); !errors.Is(err, plain.ErrNoCatalog) {
		t.Errorf("a provider with no catalog took a choice: %v", err)
	}
}

// TestTheFilesGoBackWhereTheyWere: PutHomeDir writes the restored tree
// over the source's own directory, and a file that was added since
// stays.
func TestTheFilesGoBackWhereTheyWere(t *testing.T) {
	www := t.TempDir()
	home := site(t, www, "shop")
	os.WriteFile(filepath.Join(home, "public", "new.txt"), []byte("since"), 0o644)
	from := t.TempDir()
	os.MkdirAll(filepath.Join(from, "public"), 0o755)
	os.WriteFile(filepath.Join(from, "public", "index.php"), []byte("<?php echo 'restored';"), 0o644)
	os.WriteFile(filepath.Join(from, "config.php"), []byte("<?php"), 0o600)

	provider := &plain.Provider{Roots: []string{www}, Catalog: newCatalog(), PostgresUser: "-"}
	chosen(t, provider, panel.Source{Name: "shop", Path: home})
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
		t.Error("files were written for a source nobody chose")
	}
}

// TestADumpIsLoadedOnlyIntoADatabaseTheSourceWasChosenWith: LoadDatabase
// feeds a MySQL dump to mysql and a PostgreSQL one to psql, and refuses
// a database the operator did not tick for this source.
func TestADumpIsLoadedOnlyIntoADatabaseTheSourceWasChosenWith(t *testing.T) {
	www := t.TempDir()
	shop := site(t, www, "shop")
	mysql, mysqldump := fakeMySQL(t, "shop", "blog")
	psql, pgdump := fakePostgres(t, "erp")
	provider := &plain.Provider{Roots: []string{www}, Catalog: newCatalog(),
		MySQLPath: mysql, MysqldumpPath: mysqldump, PsqlPath: psql, PgDumpPath: pgdump, PostgresUser: "-"}
	chosen(t, provider, panel.Source{Name: "shop", Path: shop, MySQL: []string{"shop"}, PostgreSQL: []string{"erp", "crm"}})
	dump := filepath.Join(t.TempDir(), "shop.sql")
	os.WriteFile(dump, []byte("CREATE TABLE t (id int);"), 0o600)

	if err := provider.LoadDatabase(context.Background(), "shop", "shop", dump); err != nil {
		t.Fatal(err)
	}
	log, _ := os.ReadFile(filepath.Join(filepath.Dir(mysql), "mysql.log"))
	if !strings.Contains(string(log), "loaded shop") {
		t.Errorf("mysql was not asked to load the dump: %q", log)
	}
	if err := provider.LoadDatabase(context.Background(), "shop", "blog", dump); err == nil {
		t.Error("a dump was loaded into a MySQL database the source was not chosen with")
	}
	if err := provider.CreateDatabase(context.Background(), "shop", "blog"); err == nil {
		t.Error("a MySQL database was created under a name the source was not chosen with")
	}
	if err := provider.CreateDatabase(context.Background(), "shop", "shop"); err != nil {
		t.Errorf("CreateDatabase(shop) = %v", err)
	}

	if err := provider.LoadDatabase(context.Background(), "shop", "erp.pg", dump); err != nil {
		t.Fatal(err)
	}
	pglog, _ := os.ReadFile(filepath.Join(filepath.Dir(psql), "psql.log"))
	if !strings.Contains(string(pglog), "-d erp") {
		t.Errorf("psql was not asked to load the dump into erp: %q", pglog)
	}
	if err := provider.LoadDatabase(context.Background(), "shop", "erp", dump); err == nil {
		t.Error("a PostgreSQL dump was handed to mysql because its name lost the mark")
	}
	// erp is there, so nothing is created; crm is gone, so it is.
	if err := provider.CreateDatabase(context.Background(), "shop", "erp.pg"); err != nil {
		t.Errorf("CreateDatabase(erp.pg) = %v", err)
	}
	if err := provider.CreateDatabase(context.Background(), "shop", "crm.pg"); err != nil {
		t.Errorf("CreateDatabase(crm.pg) = %v", err)
	}
	pglog, _ = os.ReadFile(filepath.Join(filepath.Dir(psql), "psql.log"))
	if strings.Contains(string(pglog), `CREATE DATABASE "erp"`) || !strings.Contains(string(pglog), `CREATE DATABASE "crm"`) {
		t.Errorf("psql log = %q; want crm created and erp left alone", pglog)
	}
}

// TestAContainerBecomesASourcePerMount: ticking a container chooses each
// of its volumes and bind mounts, read where they lie on the host, and
// its backup carries the container's own description and compose file
// beside the record. A volume read while the container runs is said to
// be so.
func TestAContainerBecomesASourcePerMount(t *testing.T) {
	host := t.TempDir()
	data := filepath.Join(host, "volumes", "web_data", "_data")
	html := filepath.Join(host, "srv", "web", "html")
	for _, dir := range []string{data, html} {
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644)
	}
	compose := filepath.Join(host, "srv", "web", "docker-compose.yml")
	os.WriteFile(compose, []byte("services: {}\n"), 0o644)
	inspectDir := t.TempDir()
	web := map[string]any{
		"Name":  "/web",
		"State": map[string]any{"Running": true},
		"Config": map[string]any{"Image": "nginx:1", "Labels": map[string]string{
			"com.docker.compose.project":              "web",
			"com.docker.compose.project.working_dir":  filepath.Join(host, "srv", "web"),
			"com.docker.compose.project.config_files": "docker-compose.yml",
		}},
		"Mounts": []map[string]string{
			{"Type": "volume", "Name": "web_data", "Source": data, "Destination": "/var/lib/data"},
			{"Type": "bind", "Source": html, "Destination": "/usr/share/nginx/html"},
			{"Type": "bind", "Source": filepath.Join(host, "gone"), "Destination": "/gone"},
		},
	}
	body, _ := json.Marshal([]any{web})
	os.WriteFile(filepath.Join(inspectDir, "web.json"), body, 0o644)
	body, _ = json.Marshal([]any{map[string]any{"Name": "/redis", "State": map[string]any{"Running": false}}})
	os.WriteFile(filepath.Join(inspectDir, "redis.json"), body, 0o644)
	docker := fakeDocker(t, inspectDir, "web", "redis")
	provider := &plain.Provider{Catalog: newCatalog(), DockerPath: docker, PodmanPath: filepath.Join(host, "no-podman"), PostgresUser: "-"}

	candidates, err := provider.Candidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates.Containers) != 2 || candidates.Containers[0].Name != "redis" || candidates.Containers[1].Image != "image:1" {
		t.Errorf("containers offered = %+v", candidates.Containers)
	}
	// The stack the compose file makes, with its directory as the
	// configuration, and how much each container would back up.
	if candidates.Containers[1].Stack != "web" || candidates.Containers[1].Mounts != 3 || candidates.Containers[0].Stack != "" {
		t.Errorf("stack and mounts = %+v", candidates.Containers)
	}
	if len(candidates.Stacks) != 1 || candidates.Stacks[0].Name != "web" || candidates.Stacks[0].Dir != filepath.Join(host, "srv", "web") ||
		strings.Join(candidates.Stacks[0].Containers, ",") != "web" {
		t.Errorf("stacks = %+v", candidates.Stacks)
	}
	var engines []string
	for _, e := range candidates.Engines {
		engines = append(engines, fmt.Sprintf("%s:%v", e.Name, e.Present))
	}
	if strings.Join(engines, " ") != "docker:true podman:false" {
		t.Errorf("engines = %v", engines)
	}

	made, err := provider.AddContainer(context.Background(), "docker", "web")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, source := range made {
		names = append(names, source.Name+"="+source.Path)
	}
	want := []string{"web-data=" + data, "web-html=" + html}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Errorf("sources made = %v, want %v", names, want)
	}
	if made[0].Container == nil || made[0].Container.Mount != "/var/lib/data" || made[0].Container.Engine != "docker" {
		t.Errorf("container ref = %+v", made[0].Container)
	}
	if _, err := provider.AddContainer(context.Background(), "docker", "web"); err == nil {
		t.Error("a container was chosen twice")
	}
	if _, err := provider.AddContainer(context.Background(), "docker", "nothere"); err == nil {
		t.Error("a container docker does not know was chosen")
	}
	candidates, _ = provider.Candidates(context.Background())
	for _, c := range candidates.Containers {
		if (c.Name == "web") != c.Chosen {
			t.Errorf("%s chosen = %v", c.Name, c.Chosen)
		}
	}

	account, err := provider.Account(context.Background(), "web-data")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := provider.Stage(context.Background(), panel.StageRequest{
		Account: account, StagingDir: t.TempDir(), Mode: pkgacct.ModeSplit,
	})
	if err != nil {
		t.Fatal(err)
	}
	var metadata string
	for _, part := range payload.Parts {
		if part.Kind == pkgacct.PartMetadata {
			metadata = part.Path
		}
	}
	if _, err := os.Stat(filepath.Join(metadata, "gniza", "container.json")); err != nil {
		t.Errorf("the container's description was not kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(metadata, "gniza", "compose", "docker-compose.yml")); err != nil {
		t.Errorf("the compose file was not kept: %v", err)
	}
	if len(payload.Warnings) != 1 || !strings.Contains(payload.Warnings[0], "was running") {
		t.Errorf("warnings = %v, want one about the running container", payload.Warnings)
	}
	record, _ := os.ReadFile(filepath.Join(metadata, "gniza", "account.json"))
	var got plain.Record
	json.Unmarshal(record, &got)
	if got.Container == nil || got.Container.Name != "web" || len(got.ComposeFiles) != 1 {
		t.Errorf("account.json = %s", record)
	}

	// A container with no mounts is chosen as its description alone.
	made, err = provider.AddContainer(context.Background(), "docker", "redis")
	if err != nil {
		t.Fatal(err)
	}
	if len(made) != 1 || made[0].Name != "redis" || made[0].Path != "" || made[0].Container.Mount != "" {
		t.Errorf("a container with no mounts made %+v", made)
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

// TestASourcesIdentityIsItsDirectory: a directory removed and made
// again under the same name is another account, and a source whose
// directory is gone has no identity to give.
func TestASourcesIdentityIsItsDirectory(t *testing.T) {
	www := t.TempDir()
	home := site(t, www, "shop")
	provider := &plain.Provider{Roots: []string{www}, Catalog: newCatalog(), PostgresUser: "-"}
	chosen(t, provider, panel.Source{Name: "shop", Path: home})
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
	if accounts, _ := provider.Accounts(context.Background()); len(accounts) != 1 || !accounts[0].Missing {
		t.Errorf("a source whose directory is gone is not listed as missing: %+v", accounts)
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
	if _, err := provider.AccountIdentity("nobody"); !errors.Is(err, panel.ErrNoSuchAccount) {
		t.Errorf("a name never chosen = %v", err)
	}
}

// TestBrowseListsTheDirectoriesUnderOne: the folder browser shows the
// directories under a path, marks the ones chosen, leaves files and
// links out, and refuses what is not a directory.
func TestBrowseListsTheDirectoriesUnderOne(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"www/shop", "www/blog", "www/.hidden"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "www", "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "www", "shop"), filepath.Join(root, "www", "link")); err != nil {
		t.Fatal(err)
	}
	provider := &plain.Provider{Catalog: newCatalog()}
	if err := provider.AddSource(context.Background(), panel.Source{Path: filepath.Join(root, "www", "shop")}); err != nil {
		t.Fatal(err)
	}
	listing, err := provider.Browse(context.Background(), filepath.Join(root, "www"))
	if err != nil {
		t.Fatal(err)
	}
	// Folders first, then files, each in order of name; the link to a
	// folder is a folder that says it is a link.
	var got []string
	for _, entry := range listing.Entries {
		item := entry.Name + ":" + entry.Kind
		if entry.Link {
			item += ":link"
		}
		if entry.ChosenAs != "" {
			item += ":" + entry.ChosenAs
		}
		got = append(got, item)
	}
	if want := ".hidden:folder blog:folder link:folder:link shop:folder:shop notes.txt:file"; strings.Join(got, " ") != want {
		t.Errorf("browse = %q, want %q", strings.Join(got, " "), want)
	}
	if listing.Parent != root || listing.Dir != filepath.Join(root, "www") || listing.Within != "" {
		t.Errorf("listing says dir %s under %s, within %q", listing.Dir, listing.Parent, listing.Within)
	}
	if listing.Entries[4].Size != 1 {
		t.Errorf("notes.txt is %d bytes", listing.Entries[4].Size)
	}
	// Inside the folder chosen, and under it, everything is that source's.
	if inside, err := provider.Browse(context.Background(), filepath.Join(root, "www", "shop")); err != nil || inside.Within != "shop" {
		t.Errorf("browsing the chosen folder = %+v, %v", inside, err)
	}
	if err := os.MkdirAll(filepath.Join(root, "www", "shop", "public", "img"), 0o755); err != nil {
		t.Fatal(err)
	}
	if deep, err := provider.Browse(context.Background(), filepath.Join(root, "www", "shop", "public")); err != nil || deep.Within != "shop" {
		t.Errorf("browsing under the chosen folder = %+v, %v", deep, err)
	}
	// A folder of many files shows the first ones and counts the rest.
	many := filepath.Join(root, "many")
	if err := os.MkdirAll(many, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 205; i++ {
		if err := os.WriteFile(filepath.Join(many, fmt.Sprintf("f%03d", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if capped, err := provider.Browse(context.Background(), many); err != nil || len(capped.Entries) != 200 || capped.More != 5 {
		t.Errorf("browsing 205 files = %d entries, %d more, %v", len(capped.Entries), capped.More, err)
	}
	if _, err := provider.Browse(context.Background(), filepath.Join(root, "www", "notes.txt")); err == nil || !strings.Contains(err.Error(), "is a file") {
		t.Errorf("browsing a file: %v", err)
	}
	if _, err := provider.Browse(context.Background(), "www"); err == nil || !strings.Contains(err.Error(), "not an absolute path") {
		t.Errorf("browsing a relative path: %v", err)
	}
	top, err := provider.Browse(context.Background(), "/")
	if err != nil {
		t.Fatal(err)
	}
	if top.Parent != "" {
		t.Errorf("/ has a parent: %s", top.Parent)
	}
	for _, entry := range top.Entries {
		switch entry.Path {
		case "/proc", "/sys", "/dev", "/run":
			t.Errorf("/ lists %s", entry.Path)
		}
	}
}
