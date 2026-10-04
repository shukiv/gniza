package reassemble_test

import (
	"archive/tar"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/layout/cpmove"
	"github.com/shukiv/gniza/internal/layout/dabackup"
	"github.com/shukiv/gniza/internal/pkgacct"
	"github.com/shukiv/gniza/internal/reassemble"
)

// buildDirectAdminArchive writes a whole-account DirectAdmin archive with
// the identity record its restore reads and whatever dumps the caller
// asks for.
func buildDirectAdminArchive(t *testing.T, account string, dumps map[string]string) reassemble.Result {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "user.admin."+account+".tar")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	write := func(name, body string) {
		t.Helper()
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	write(dabackup.BackupDir+"/"+dabackup.UserConf, "username="+account+"\n")
	// A real DirectAdmin archive carries the account's own files beside
	// DirectAdmin's records of it, and one that does not is refused as
	// an account with nothing in it.
	write(dabackup.DomainsDir+"/example.invalid/public_html/index.html", "<html>")
	for name, body := range dumps {
		write(dabackup.BackupDir+"/"+name, body)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return reassemble.Result{
		Account: account, ArchivePath: path,
		Layout: dabackup.Layout{}, Mode: pkgacct.ModeMonolithic,
	}
}

// A dump that came back empty restores an empty database. The split path
// has always refused one; a whole-account snapshot went the other way,
// because a rehearsal of one stopped at "the archive is there" and never
// looked inside it. DirectAdmin has no other shape, so on DirectAdmin
// that was every rehearsal.
func TestARehearsalRefusesAnEmptyDumpInsideAWholeAccountArchive(t *testing.T) {
	rebuilt := buildDirectAdminArchive(t, "webshop", map[string]string{
		"webshop_shop.sql": "",
	})
	passed, err := reassemble.Verify(context.Background(), rebuilt)
	if err == nil {
		t.Fatalf("an empty dump rehearsed clean, checks: %v", passed)
	}
	if !strings.Contains(err.Error(), "webshop_shop") {
		t.Errorf("the failure does not name the dump: %v", err)
	}
}

// And a truncated one, which is the same outcome arriving less obviously.
func TestARehearsalRefusesADumpWithNothingToRestore(t *testing.T) {
	rebuilt := buildDirectAdminArchive(t, "webshop", map[string]string{
		"webshop_shop.sql": "-- MySQL dump 10.13\n-- Host: localhost\n",
	})
	if _, err := reassemble.Verify(context.Background(), rebuilt); err == nil {
		t.Fatal("a dump with no CREATE statement rehearsed clean")
	}
}

// A rehearsal that did read the dumps says so, because a report of what
// was checked is what an operator reads instead of running the restore.
func TestARehearsalSaysItReadTheDumpsInsideTheArchive(t *testing.T) {
	rebuilt := buildDirectAdminArchive(t, "webshop", map[string]string{
		"webshop_shop.sql":  "CREATE TABLE orders (id int);\n",
		"webshop_blog.sql":  "CREATE TABLE posts (id int);\n",
		"webshop_shop.conf": "name=webshop_shop\n",
	})
	passed, err := reassemble.Verify(context.Background(), rebuilt)
	if err != nil {
		t.Fatalf("a sound archive failed: %v", err)
	}
	if !strings.Contains(strings.Join(passed, "; "), "2 database dumps parse") {
		t.Errorf("the rehearsal does not report reading the dumps: %v", passed)
	}
}

// An account with no databases is not a failure, and is not reported as
// dumps that were read either.
func TestAWholeAccountArchiveWithNoDatabasesRehearsesClean(t *testing.T) {
	rebuilt := buildDirectAdminArchive(t, "webshop", nil)
	passed, err := reassemble.Verify(context.Background(), rebuilt)
	if err != nil {
		t.Fatalf("an account with no databases failed: %v", err)
	}
	if strings.Contains(strings.Join(passed, "; "), "dumps parse") {
		t.Errorf("the rehearsal claims dumps it never had: %v", passed)
	}
}

// cPanel's own archive is a format this cannot read into, so a rehearsal
// of one reports what it did check and claims nothing more.
func TestACpanelArchiveIsNotClaimedToHaveBeenReadInside(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cpmove-webshop.tar.gz")
	if err := os.WriteFile(path, []byte("not really an archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	passed, err := reassemble.Verify(context.Background(), reassemble.Result{
		Account: "webshop", ArchivePath: path,
		Layout: cpmove.Layout{}, Mode: pkgacct.ModeMonolithic,
	})
	if err != nil {
		t.Fatalf("a cPanel archive failed a rehearsal: %v", err)
	}
	if strings.Contains(strings.Join(passed, "; "), "dumps parse") {
		t.Errorf("the rehearsal claims to have read inside cPanel's format: %v", passed)
	}
}

// A database with no table in it is dumped as a header, a footer and
// nothing between them. That is a faithful backup of what the account
// has. On 2026-10-02 it failed the rehearsal of six accounts on one
// server and one on another, each of which had such a database, and
// told the operator their backups did not rebuild.
func TestARehearsalPassesAWholeDumpOfADatabaseWithNothingInIt(t *testing.T) {
	const emptyDump = "-- MariaDB dump 10.19  Distrib 10.6.20-MariaDB, for Linux (x86_64)\n" +
		"-- Host: localhost    Database: webshop_new\n" +
		"/*!40111 SET @OLD_SQL_NOTES=@@SQL_NOTES, SQL_NOTES=0 */;\n" +
		"/*!40111 SET SQL_NOTES=@OLD_SQL_NOTES */;\n\n" +
		"-- Dump completed on 2026-10-04  3:33:03\n"
	rebuilt := buildDirectAdminArchive(t, "webshop", map[string]string{
		"webshop_shop.sql": "CREATE TABLE orders (id int);\n",
		"webshop_new.sql":  emptyDump,
	})
	passed, err := reassemble.Verify(context.Background(), rebuilt)
	if err != nil {
		t.Fatalf("an account with an empty database failed its rehearsal: %v", err)
	}
	said := strings.Join(passed, "; ")
	if !strings.Contains(said, "2 database dumps parse") ||
		!strings.Contains(said, "with nothing in it, as on the server: webshop_new") {
		t.Errorf("the rehearsal does not say which database is empty: %v", passed)
	}

	// The same dump stopped before its end is still refused.
	cut := buildDirectAdminArchive(t, "webshop", map[string]string{
		"webshop_new.sql": emptyDump[:strings.Index(emptyDump, "/*!40111 SET SQL_NOTES=@OLD")],
	})
	if _, err := reassemble.Verify(context.Background(), cut); err == nil ||
		!strings.Contains(err.Error(), "does not say it was finished") {
		t.Errorf("a dump that was cut short rehearsed clean: %v", err)
	}
}
