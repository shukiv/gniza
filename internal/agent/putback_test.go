package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/cpanel"
	"github.com/shukiv/gniza/internal/granular"
	"github.com/shukiv/gniza/internal/panel"
	"github.com/shukiv/gniza/internal/protocol"
	"github.com/shukiv/gniza/internal/reassemble"
)

// restoredTree is what restoreItems leaves behind before anything is done
// with it: the parts of the account, under names that say where they belong.
func restoredTree(t *testing.T) string {
	t.Helper()
	out := t.TempDir()
	if err := os.MkdirAll(filepath.Join(out, "homedir", "public_html"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "homedir", "public_html", "index.php"),
		[]byte("<?php\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(out, "databases"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"c1_shop.sql", "c1_wp.sql", granular.DatabaseUsersFile} {
		if err := os.WriteFile(filepath.Join(out, "databases", name),
			[]byte("-- dump\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func quietAgent(provider panel.Provider) *Agent {
	return &Agent{
		provider: provider,
		log:      slog.New(slog.DiscardHandler),
	}
}

func TestApplyingADatabaseLoadsOnlyWhatWasAskedFor(t *testing.T) {
	out := restoredTree(t)
	fake := &cpanel.Fake{Databases: map[string][]string{"c1": {"c1_shop", "c1_wp"}}}
	agent := quietAgent(fake)

	written, _, err := agent.applyItems(context.Background(), agent.log,
		protocol.RestoreAssignment{
			CPanelUser: "c1",
			ItemKind:   string(granular.KindDatabase),
			ItemNames:  []string{"c1_shop"},
		}, out)
	if err != nil {
		t.Fatalf("applyItems: %v", err)
	}
	if len(fake.LoadedDatabases) != 1 {
		t.Fatalf("loaded %d databases, want 1: %+v", len(fake.LoadedDatabases), fake.LoadedDatabases)
	}
	loaded := fake.LoadedDatabases[0]
	if loaded.User != "c1" || loaded.Database != "c1_shop" {
		t.Errorf("loaded %+v", loaded)
	}
	if filepath.Base(loaded.DumpPath) != "c1_shop.sql" {
		t.Errorf("loaded the dump %s", loaded.DumpPath)
	}
	if len(fake.PutBackHome) != 0 {
		t.Errorf("a database restore also wrote files: %+v", fake.PutBackHome)
	}
	if written == "" {
		t.Error("nothing was reported as written")
	}
}

// A database the account does not own is refused by the provider. The name
// comes out of a backup, and a name can have changed hands since it was
// taken.
func TestApplyingADatabaseTheAccountDoesNotOwnIsRefused(t *testing.T) {
	out := restoredTree(t)
	fake := &cpanel.Fake{Databases: map[string][]string{"c1": {"c1_shop"}}}
	agent := quietAgent(fake)

	_, _, err := agent.applyItems(context.Background(), agent.log,
		protocol.RestoreAssignment{
			CPanelUser: "c1",
			ItemKind:   string(granular.KindDatabase),
			ItemNames:  []string{"c2_shop"},
		}, out)
	if err == nil {
		t.Fatal("a database belonging to another account was loaded")
	}
	if len(fake.LoadedDatabases) != 0 {
		t.Errorf("loaded %+v", fake.LoadedDatabases)
	}
}

// The case this feature exists for: somebody dropped their database this
// morning and wants it back. cPanel no longer lists it, so it is made
// again -- as the account, so the panel applies the account's own quota and
// prefix and records it -- and then the dump goes into it.
func TestARestoreIntoADroppedDatabaseMakesItAgain(t *testing.T) {
	out := restoredTree(t)
	fake := &cpanel.Fake{Databases: map[string][]string{"c1": {"c1_wp"}}}
	agent := quietAgent(fake)

	wrote, hint, err := agent.applyItems(context.Background(), agent.log,
		protocol.RestoreAssignment{
			CPanelUser: "c1",
			ItemKind:   string(granular.KindDatabase),
			ItemNames:  []string{"c1_shop"},
		}, out)
	if err != nil {
		t.Fatalf("applyItems: %v (%s)", err, hint)
	}
	if len(fake.CreatedDatabases) != 1 || fake.CreatedDatabases[0].Database != "c1_shop" {
		t.Fatalf("created %+v", fake.CreatedDatabases)
	}
	if len(fake.LoadedDatabases) != 1 || fake.LoadedDatabases[0].Database != "c1_shop" {
		t.Fatalf("loaded %+v", fake.LoadedDatabases)
	}
	// A database that was already there is not made again.
	if !strings.Contains(wrote, "created c1_shop") ||
		!strings.Contains(wrote, "loaded into c1_shop") {
		t.Errorf("wrote = %q", wrote)
	}
}

// A database the account still has is loaded into, not made again: making
// it would fail, and a restore that reported a database it did not make
// would be saying something untrue about the account.
func TestARestoreIntoADatabaseThatIsStillThereMakesNothing(t *testing.T) {
	out := restoredTree(t)
	fake := &cpanel.Fake{Databases: map[string][]string{"c1": {"c1_shop"}}}
	agent := quietAgent(fake)

	if _, hint, err := agent.applyItems(context.Background(), agent.log,
		protocol.RestoreAssignment{
			CPanelUser: "c1",
			ItemKind:   string(granular.KindDatabase),
			ItemNames:  []string{"c1_shop"},
		}, out); err != nil {
		t.Fatalf("applyItems: %v (%s)", err, hint)
	}
	if len(fake.CreatedDatabases) != 0 {
		t.Errorf("created %+v", fake.CreatedDatabases)
	}
	if len(fake.LoadedDatabases) != 1 {
		t.Errorf("loaded %+v", fake.LoadedDatabases)
	}
}

// An account with as many databases as its plan allows cannot be given
// another one. cPanel refuses, and nothing is loaded: the restore stops
// where it is rather than filling some of what was asked for.
func TestADatabaseThatCannotBeMadeStopsTheRestore(t *testing.T) {
	out := restoredTree(t)
	fake := &cpanel.Fake{
		Databases:    map[string][]string{"c1": {}},
		RefuseCreate: "the account has reached its database limit",
	}
	agent := quietAgent(fake)

	_, hint, err := agent.applyItems(context.Background(), agent.log,
		protocol.RestoreAssignment{
			CPanelUser: "c1",
			ItemKind:   string(granular.KindDatabase),
			ItemNames:  []string{"c1_shop"},
		}, out)
	if err == nil {
		t.Fatal("a database cPanel refused to make was loaded into")
	}
	if len(fake.LoadedDatabases) != 0 {
		t.Errorf("loaded %+v", fake.LoadedDatabases)
	}
	// cPanel's own words, so the customer knows what to do about it.
	if !strings.Contains(hint, "c1_shop") ||
		!strings.Contains(hint, "reached its database limit") {
		t.Errorf("hint = %q", hint)
	}
	// What a customer is shown must not name a path or a repository.
	if strings.Contains(hint, "/") {
		t.Errorf("the hint carries a path: %q", hint)
	}
}

// A basket holding a database and the users that open it: the database is
// made, filled, and only then are the grants given. A grant on a database
// that is not there is refused by MySQL, so the order is the point.
func TestADroppedDatabaseAndItsUsersComeBackTogether(t *testing.T) {
	out := restoredTree(t)
	stagedUsers(t, out,
		`{"c1_wp":{"localhost":{"pass_hash":"2A4636","auth_plugin":"mysql_native_password"}}}`,
		"GRANT ALL PRIVILEGES ON `c1\\_shop`.* TO 'c1_wp'@'localhost';\n")
	fake := &cpanel.Fake{Databases: map[string][]string{"c1": {}}}
	agent := quietAgent(fake)

	wrote, hint, err := agent.applyItems(context.Background(), agent.log,
		protocol.RestoreAssignment{
			CPanelUser: "c1",
			Items: []protocol.RestoreSelection{
				{Kind: string(granular.KindDatabase), Names: []string{"c1_shop"}},
				{Kind: string(granular.KindDBUsers), Names: []string{"c1_wp"}},
			},
		}, out)
	if err != nil {
		t.Fatalf("applyItems: %v (%s)", err, hint)
	}
	if len(fake.CreatedDatabases) != 1 || len(fake.LoadedDatabases) != 1 ||
		len(fake.RestoredDBUsers) != 1 {
		t.Fatalf("created %+v loaded %+v users %+v",
			fake.CreatedDatabases, fake.LoadedDatabases, fake.RestoredDBUsers)
	}
	if !strings.Contains(wrote, "created c1_shop") ||
		!strings.Contains(wrote, "recreated c1_wp") {
		t.Errorf("wrote = %q", wrote)
	}
}

// A grant on a database that is neither on the account nor in this restore
// is still refused. What a basket is about to make is one thing; making an
// empty database nobody asked for, to hold a grant, is another.
func TestAGrantOnADatabaseNobodyIsRestoringIsStillRefused(t *testing.T) {
	out := restoredTree(t)
	stagedUsers(t, out,
		`{"c1_wp":{"localhost":{"pass_hash":"2A4636","auth_plugin":"mysql_native_password"}}}`,
		"GRANT ALL PRIVILEGES ON `c1\\_gone`.* TO 'c1_wp'@'localhost';\n")
	fake := &cpanel.Fake{Databases: map[string][]string{"c1": {}}}
	agent := quietAgent(fake)

	_, hint, err := agent.applyItems(context.Background(), agent.log,
		protocol.RestoreAssignment{
			CPanelUser: "c1",
			Items: []protocol.RestoreSelection{
				{Kind: string(granular.KindDatabase), Names: []string{"c1_shop"}},
				{Kind: string(granular.KindDBUsers), Names: []string{"c1_wp"}},
			},
		}, out)
	if err == nil {
		t.Fatal("a grant on a database nobody has was given")
	}
	if len(fake.CreatedDatabases) != 0 || len(fake.LoadedDatabases) != 0 {
		t.Errorf("created %+v loaded %+v", fake.CreatedDatabases, fake.LoadedDatabases)
	}
	if !strings.Contains(hint, "c1_gone") || !strings.Contains(hint, "Add that database") {
		t.Errorf("hint = %q", hint)
	}
}

// A backup with no stored passwords cannot restore users, and a basket
// holding a database beside them must not make the database first and fail
// afterwards: the account would come out of the restore with an empty
// database it did not have when it went in.
func TestABasketThatCannotRestoreItsUsersMakesNothing(t *testing.T) {
	out := restoredTree(t)
	fake := &cpanel.Fake{Databases: map[string][]string{"c1": {}}}
	agent := quietAgent(fake)

	if _, _, err := agent.applyItems(context.Background(), agent.log,
		protocol.RestoreAssignment{
			CPanelUser: "c1",
			Items: []protocol.RestoreSelection{
				{Kind: string(granular.KindDatabase), Names: []string{"c1_shop"}},
				{Kind: string(granular.KindDBUsers), Names: []string{"c1_wp"}},
			},
		}, out); err == nil {
		t.Fatal("users this backup does not hold were restored")
	}
	if len(fake.CreatedDatabases) != 0 || len(fake.LoadedDatabases) != 0 {
		t.Errorf("created %+v loaded %+v", fake.CreatedDatabases, fake.LoadedDatabases)
	}
}

// An account's cron jobs are lines in one file, so the file is what comes
// back -- through the provider, which puts it where cron reads it.
func TestApplyingCronPutsTheWholeCrontabBack(t *testing.T) {
	out := restoredTree(t)
	crontab := "SHELL=\"/usr/local/cpanel/bin/jailshell\"\nMAILTO=\"\"\n" +
		"*/15 * * * * /usr/local/bin/ea-php83 /home/c1/public_html/wp-cron.php\n"
	stagedCron(t, out, "c1", crontab)
	fake := &cpanel.Fake{}
	agent := quietAgent(fake)

	wrote, hint, err := agent.applyItems(context.Background(), agent.log,
		protocol.RestoreAssignment{
			CPanelUser: "c1",
			ItemKind:   string(granular.KindCron),
		}, out)
	if err != nil {
		t.Fatalf("applyItems: %v (%s)", err, hint)
	}
	if len(fake.PutBackCrontabs) != 1 || fake.PutBackCrontabs[0].Body != crontab {
		t.Fatalf("put back %+v", fake.PutBackCrontabs)
	}
	if !strings.Contains(wrote, "cron jobs of c1") {
		t.Errorf("wrote = %q", wrote)
	}
}

// An account that had no cron jobs has no file in its archive. Writing an
// empty crontab would read as a restore and be a deletion of whatever is
// running now.
func TestApplyingCronFromABackupWithNoneChangesNothing(t *testing.T) {
	out := restoredTree(t)
	fake := &cpanel.Fake{}
	agent := quietAgent(fake)

	_, hint, err := agent.applyItems(context.Background(), agent.log,
		protocol.RestoreAssignment{
			CPanelUser: "c1",
			ItemKind:   string(granular.KindCron),
		}, out)
	if err == nil {
		t.Fatal("an empty crontab was written over the account's jobs")
	}
	if len(fake.PutBackCrontabs) != 0 {
		t.Errorf("put back %+v", fake.PutBackCrontabs)
	}
	if !strings.Contains(hint, "no cron jobs") {
		t.Errorf("hint = %q", hint)
	}
}

// stagedCron writes an account's cron jobs where a restore leaves them:
// inside the archive's own top-level directory, under the metadata tree.
func stagedCron(t *testing.T, out, user, body string) {
	t.Helper()
	dir := filepath.Join(out, "metadata", "cpmove-"+user, "cron")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, user), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// cPanel's restore exits zero for things that are not a restored account.
// Seen on a live server: an account terminated a moment before the restore
// started came back "Success." from restorepkg and was not there
// afterwards. A restore that reports success and leaves nothing is worse
// than one that fails, because nobody looks again.
func TestARestoreThatLeavesNoAccountIsNotASuccess(t *testing.T) {
	agent := quietAgent(&cpanel.Fake{Gone: map[string]bool{"c1": true}})

	err := agent.confirmRestored(context.Background(), agent.log, "c1", reassemble.Result{})
	if err == nil {
		t.Fatal("a restore that left no account was reported as successful")
	}
	if !strings.Contains(err.Error(), "not on this server afterwards") {
		t.Errorf("error = %q", err)
	}

	// The account that is there passes, which is every other restore.
	here := quietAgent(&cpanel.Fake{Root: t.TempDir()})
	if err := here.confirmRestored(context.Background(), here.log, "c1", reassemble.Result{}); err != nil {
		t.Errorf("a restored account was reported as missing: %v", err)
	}
}

func TestApplyingFilesWritesTheHomeDirectory(t *testing.T) {
	out := restoredTree(t)
	fake := &cpanel.Fake{}
	agent := quietAgent(fake)

	for _, kind := range []granular.Kind{
		granular.KindFiles, granular.KindWebsite, granular.KindMailbox,
	} {
		fake.PutBackHome = nil
		if _, _, err := agent.applyItems(context.Background(), agent.log,
			protocol.RestoreAssignment{
				CPanelUser: "c1", ItemKind: string(kind),
				ItemNames: []string{"public_html"},
			}, out); err != nil {
			t.Fatalf("applyItems(%s): %v", kind, err)
		}
		if len(fake.PutBackHome) != 1 || fake.PutBackHome[0].User != "c1" {
			t.Fatalf("%s wrote %+v", kind, fake.PutBackHome)
		}
		if filepath.Base(fake.PutBackHome[0].From) != "homedir" {
			t.Errorf("%s wrote from %s", kind, fake.PutBackHome[0].From)
		}
	}
}

// The node refuses these before a job exists. The agent refuses them again,
// because this is the code that runs as root against a live account.
func TestApplyingWhatCannotBeAppliedIsRefusedByTheAgentToo(t *testing.T) {
	out := restoredTree(t)
	fake := &cpanel.Fake{Databases: map[string][]string{"c1": {"c1_shop"}}}
	agent := quietAgent(fake)

	for _, kind := range []granular.Kind{
		granular.KindDNS, granular.KindSSL, granular.KindDBUsers,
		granular.KindFTP, granular.KindCron, granular.KindDomains,
		granular.KindSettings, granular.KindSystem,
	} {
		if _, _, err := agent.applyItems(context.Background(), agent.log,
			protocol.RestoreAssignment{
				CPanelUser: "c1", ItemKind: string(kind),
			}, out); err == nil {
			t.Errorf("a %s restore was written into the live account", kind)
		}
	}
	if len(fake.LoadedDatabases) != 0 || len(fake.PutBackHome) != 0 {
		t.Errorf("something was written: %+v %+v", fake.LoadedDatabases, fake.PutBackHome)
	}
}

// stagedUsers writes the two files a dbusers restore reads: the hashes and
// plugins, as cPanel records them, and the grants beside them.
func stagedUsers(t *testing.T, out, auth, grants string) {
	t.Helper()
	dir := filepath.Join(out, "databases")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, granular.DatabaseUsersAuthFile),
		[]byte(auth), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, granular.RunnableDatabaseUsersFile),
		[]byte(grants), 0o600); err != nil {
		t.Fatal(err)
	}
}

const shopUserAuth = `{"c1_shop":{"localhost":{"pass_hash":"2A4636","auth_plugin":"mysql_native_password"}}}`

// The same user on the two hosts cPanel gives every account. It is one
// login as far as anyone reading the page is concerned.
const shopUserOnTwoHosts = `{"c1_shop":{"localhost":{"pass_hash":"2A4636","auth_plugin":"mysql_native_password"},` +
	`"10.0.0.1":{"pass_hash":"2A4636","auth_plugin":"mysql_native_password"}}}`

// cPanel gives every database user a login on several hosts. Reporting
// "recreated c1_shop, c1_shop, c1_shop" would make one restored user look
// like three.
func TestAUserOnSeveralHostsIsReportedOnce(t *testing.T) {
	out := restoredTree(t)
	stagedUsers(t, out, shopUserOnTwoHosts,
		"GRANT ALL PRIVILEGES ON `c1\\_shop`.* TO 'c1_shop'@'localhost';\n"+
			"GRANT ALL PRIVILEGES ON `c1\\_shop`.* TO 'c1_shop'@'10.0.0.1';\n")
	fake := &cpanel.Fake{Databases: map[string][]string{"c1": {"c1_shop"}}}
	agent := quietAgent(fake)

	written, _, err := agent.applyItems(context.Background(), agent.log,
		protocol.RestoreAssignment{
			CPanelUser: "c1",
			ItemKind:   string(granular.KindDBUsers),
		}, out)
	if err != nil {
		t.Fatalf("applyItems: %v", err)
	}
	if written != "recreated c1_shop" {
		t.Errorf("reported %q", written)
	}
	// Both hosts still have to reach the provider: a grant put back
	// against the wrong host is an application that cannot connect.
	if len(fake.RestoredDBUsers[0].Users) != 2 {
		t.Errorf("restored %+v", fake.RestoredDBUsers[0].Users)
	}
}

// A restored database with no user to read it is a site that still cannot
// start, so the users come back with the password they had and the access
// they had -- not merely as names.
func TestDatabaseUsersComeBackWithTheirPasswordsAndTheirGrants(t *testing.T) {
	out := restoredTree(t)
	stagedUsers(t, out, shopUserAuth,
		"-- Database users and grants for c1\n"+
			"CREATE USER IF NOT EXISTS `c1_shop`@`localhost` IDENTIFIED WITH 'mysql_native_password' AS '*46';\n"+
			"GRANT ALL PRIVILEGES ON `c1\\_shop`.* TO 'c1_shop'@'localhost';\n")
	fake := &cpanel.Fake{Databases: map[string][]string{"c1": {"c1_shop", "c1_wp"}}}
	agent := quietAgent(fake)

	written, _, err := agent.applyItems(context.Background(), agent.log,
		protocol.RestoreAssignment{
			CPanelUser: "c1",
			ItemKind:   string(granular.KindDBUsers),
		}, out)
	if err != nil {
		t.Fatalf("applyItems: %v", err)
	}
	if len(fake.RestoredDBUsers) != 1 {
		t.Fatalf("restored %d sets of users, want 1", len(fake.RestoredDBUsers))
	}
	restored := fake.RestoredDBUsers[0].Users
	if len(restored) != 1 {
		t.Fatalf("restored %+v", restored)
	}
	user := restored[0]
	if user.Name != "c1_shop" || user.Host != "localhost" {
		t.Errorf("restored %s@%s", user.Name, user.Host)
	}
	// Without these the login exists and nothing can connect as it.
	if user.Hash != "2A4636" || user.Plugin != "mysql_native_password" {
		t.Errorf("restored the user without its password: %+v", user)
	}
	if len(user.Grants) != 1 || user.Grants[0].Database != "c1_shop" ||
		strings.Join(user.Grants[0].Privileges, ",") != "ALL PRIVILEGES" {
		t.Errorf("restored the grants as %+v", user.Grants)
	}
	if !strings.Contains(written, "c1_shop") {
		t.Errorf("reported %q", written)
	}
}

// SHOW GRANTS prints the database as the LIKE pattern MySQL stores, so the
// underscore in every cPanel database name arrives escaped. Restoring the
// name with the backslash still in it would ask cPanel for a database that
// does not exist, which is every database on the server.
func TestAnEscapedUnderscoreInAGrantIsStillAnUnderscore(t *testing.T) {
	out := restoredTree(t)
	stagedUsers(t, out, shopUserAuth,
		"GRANT ALL PRIVILEGES ON `c1\\_shop`.* TO 'c1_shop'@'localhost';\n")
	fake := &cpanel.Fake{Databases: map[string][]string{"c1": {"c1_shop"}}}
	agent := quietAgent(fake)

	if _, _, err := agent.applyItems(context.Background(), agent.log,
		protocol.RestoreAssignment{
			CPanelUser: "c1",
			ItemKind:   string(granular.KindDBUsers),
		}, out); err != nil {
		t.Fatalf("applyItems: %v", err)
	}
	if len(fake.RestoredDBUsers) != 1 {
		t.Fatalf("restored %d sets of users, want 1", len(fake.RestoredDBUsers))
	}
	grants := fake.RestoredDBUsers[0].Users[0].Grants
	if len(grants) != 1 || grants[0].Database != "c1_shop" {
		t.Errorf("the grant was restored on %+v", grants)
	}
}

// A grant on anything but one of the account's own databases is refused
// rather than guessed at. Dropping it would leave an application unable to
// connect while the restore reported success; widening it would hand the
// account more than it had.
func TestAGrantThatIsNotOnASingleDatabaseIsRefused(t *testing.T) {
	for _, grant := range []string{
		"GRANT SELECT ON *.* TO 'c1_shop'@'localhost';",
		"GRANT ALL PRIVILEGES ON `c1_%`.* TO 'c1_shop'@'localhost';",
		"GRANT ALL PRIVILEGES ON `c1\\_shop`.* TO 'c1_shop'@'localhost' WITH GRANT OPTION;",
		"DROP DATABASE c1_shop;",
	} {
		out := restoredTree(t)
		stagedUsers(t, out, shopUserAuth, grant+"\n")
		fake := &cpanel.Fake{Databases: map[string][]string{"c1": {"c1_shop"}}}
		agent := quietAgent(fake)

		if _, _, err := agent.applyItems(context.Background(), agent.log,
			protocol.RestoreAssignment{
				CPanelUser: "c1",
				ItemKind:   string(granular.KindDBUsers),
			}, out); err == nil {
			t.Errorf("%q was accepted", grant)
		}
		if len(fake.RestoredDBUsers) != 0 {
			t.Errorf("%q reached the live account", grant)
		}
	}
}

// Restoring the users before the database they read is the order a customer
// will try, so being told "ask your host" would make the likeliest mistake
// the one thing the page cannot explain.
func TestRestoringUsersForAMissingDatabaseSaysWhatToDoAboutIt(t *testing.T) {
	out := restoredTree(t)
	stagedUsers(t, out, shopUserAuth,
		"GRANT ALL PRIVILEGES ON `c1\\_shop`.* TO 'c1_shop'@'localhost';\n")
	fake := &cpanel.Fake{Databases: map[string][]string{"c1": {"c1_wp"}}}
	agent := quietAgent(fake)

	_, hint, err := agent.applyItems(context.Background(), agent.log,
		protocol.RestoreAssignment{
			CPanelUser: "c1",
			ItemKind:   string(granular.KindDBUsers),
		}, out)
	if err == nil {
		t.Fatal("granting on a database the account does not have should fail")
	}
	if !strings.Contains(hint, "c1_shop") || !strings.Contains(hint, "first") {
		t.Errorf("the customer was told %q", hint)
	}
	if len(fake.RestoredDBUsers) != 0 {
		t.Errorf("users were restored anyway: %+v", fake.RestoredDBUsers)
	}
}

// A backup that is too old to hold the users says so, rather than
// reporting a restore of nothing as a success.
func TestABackupWithoutTheUsersSaysSoRatherThanSucceeding(t *testing.T) {
	out := restoredTree(t)
	fake := &cpanel.Fake{Databases: map[string][]string{"c1": {"c1_shop"}}}
	agent := quietAgent(fake)

	_, hint, err := agent.applyItems(context.Background(), agent.log,
		protocol.RestoreAssignment{
			CPanelUser: "c1",
			ItemKind:   string(granular.KindDBUsers),
		}, out)
	if err == nil {
		t.Fatal("a backup with no database users should fail the restore")
	}
	if hint == "" {
		t.Error("the customer was given no explanation")
	}
}

// A basket restores its parts in the order the account needs them and says
// what each one did. The database has to be loaded before its user is given
// access to it, or the grant lands on a database that is not there yet.
func TestABasketRestoresTheDatabaseAndItsUsersTogether(t *testing.T) {
	out := restoredTree(t)
	stagedUsers(t, out, shopUserAuth,
		"GRANT ALL PRIVILEGES ON `c1\\_shop`.* TO 'c1_shop'@'localhost';\n")
	fake := &cpanel.Fake{Databases: map[string][]string{"c1": {"c1_shop"}}}
	agent := quietAgent(fake)

	written, _, err := agent.applyItems(context.Background(), agent.log,
		protocol.RestoreAssignment{
			CPanelUser: "c1",
			Items: []protocol.RestoreSelection{
				{Kind: string(granular.KindDBUsers)},
				{Kind: string(granular.KindDatabase), Names: []string{"c1_shop"}},
			},
		}, out)
	if err != nil {
		t.Fatalf("applyItems: %v", err)
	}
	if len(fake.LoadedDatabases) != 1 || fake.LoadedDatabases[0].Database != "c1_shop" {
		t.Fatalf("loaded %+v", fake.LoadedDatabases)
	}
	if len(fake.RestoredDBUsers) != 1 {
		t.Fatalf("restored %+v", fake.RestoredDBUsers)
	}
	// Named in the order the account was changed, so an operator reading
	// it back knows what happened first.
	if written != "loaded into c1_shop; recreated c1_shop" {
		t.Errorf("reported %q", written)
	}
}

// Everything a basket asks for is checked before any of it is written. A
// basket whose users cannot be restored must not leave the database
// replaced and its users missing: the account would come out of the restore
// worse off than it went in.
func TestABasketWritesNothingWhenOnePartWouldBeRefused(t *testing.T) {
	out := restoredTree(t)
	// The users had access to a database the account no longer has, which
	// is the one thing a restore cannot make for them.
	stagedUsers(t, out, shopUserAuth,
		"GRANT ALL PRIVILEGES ON `c1\\_gone`.* TO 'c1_shop'@'localhost';\n")
	fake := &cpanel.Fake{Databases: map[string][]string{"c1": {"c1_shop"}}}
	agent := quietAgent(fake)

	_, hint, err := agent.applyItems(context.Background(), agent.log,
		protocol.RestoreAssignment{
			CPanelUser: "c1",
			Items: []protocol.RestoreSelection{
				{Kind: string(granular.KindDatabase), Names: []string{"c1_shop"}},
				{Kind: string(granular.KindDBUsers)},
			},
		}, out)
	if err == nil {
		t.Fatal("a basket with a grant on a missing database was applied")
	}
	if len(fake.LoadedDatabases) != 0 {
		t.Errorf("the database was replaced anyway: %+v", fake.LoadedDatabases)
	}
	if !strings.Contains(hint, "c1_gone") {
		t.Errorf("hint %q does not say which database is missing", hint)
	}
}

// A basket may not be applied at all if any one part of it cannot be
// written back, rather than applying the parts that can and quietly
// leaving out the rest.
func TestABasketCarryingSomethingUnappliableIsRefused(t *testing.T) {
	out := restoredTree(t)
	fake := &cpanel.Fake{Databases: map[string][]string{"c1": {"c1_shop"}}}
	agent := quietAgent(fake)

	if _, _, err := agent.applyItems(context.Background(), agent.log,
		protocol.RestoreAssignment{
			CPanelUser: "c1",
			Items: []protocol.RestoreSelection{
				{Kind: string(granular.KindDatabase), Names: []string{"c1_shop"}},
				{Kind: string(granular.KindDNS)},
			},
		}, out); err == nil {
		t.Fatal("a basket carrying DNS was applied")
	}
	if len(fake.LoadedDatabases) != 0 {
		t.Errorf("the database was loaded anyway: %+v", fake.LoadedDatabases)
	}
}

// Records written before baskets existed name one part of an account in
// their own two fields, and are read the same way as one written after.
func TestARestoreRecordedBeforeBasketsStillNamesItsPart(t *testing.T) {
	assignment := protocol.RestoreAssignment{
		CPanelUser: "c1",
		ItemKind:   string(granular.KindDatabase),
		ItemNames:  []string{"c1_shop"},
	}
	selections := assignment.Selections()
	if len(selections) != 1 || selections[0].Kind != "database" ||
		len(selections[0].Names) != 1 || selections[0].Names[0] != "c1_shop" {
		t.Fatalf("selections = %+v", selections)
	}
	if (protocol.RestoreAssignment{}).Selections() != nil {
		t.Error("a whole-account restore names a part of one")
	}
}
