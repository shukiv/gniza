package cpanel

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/pkgacct"
)

// fakeMysqldump writes a stand-in for mysqldump that fails the way the real
// one does when the server dies under it: something on stderr, and an exit
// status that on its own says nothing.
func fakeMysqldump(t *testing.T, stderr string, code int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mysqldump")
	script := "#!/bin/sh\nprintf '%s\\n' " + shellQuote(stderr) + " >&2\nexit " + itoa(code) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for ; n > 0; n /= 10 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
	}
	return string(digits)
}

// TestAFailedDumpSaysWhatMysqldumpSaid is the whole reason this test
// exists. A backup that failed reported "mysqldump db: exit status 2" and
// nothing else, so finding out why meant running mysqldump by hand on the
// server. mysqldump had already said why -- on stderr, which was thrown
// away.
func TestAFailedDumpSaysWhatMysqldumpSaid(t *testing.T) {
	const said = "mysqldump: Couldn't execute 'SELECT * FROM `cache`': " +
		"Lost connection to MySQL server during query (2013)"
	host := newFakeHost(t, "customer1")
	host.MysqldumpPath = fakeMysqldump(t, said, 2)
	host.MysqlPath = fakeMysqldump(t, "", 0)

	dir := t.TempDir()
	missing, err := host.dumpDatabases(context.Background(),
		StageRequest{StagingDir: dir, Account: AccountInfo{User: "customer1"}},
		pkgacct.Payload{DumpPaths: map[string]string{
			"customer1_wp": filepath.Join(dir, "databases", "customer1_wp.sql"),
		}})
	if err != nil {
		t.Fatalf("one database that would not dump stopped the rest: %v", err)
	}
	if len(missing) != 1 {
		t.Fatalf("a dump that failed was reported as a success: %v", missing)
	}
	if !strings.Contains(missing[0].Why, "Lost connection to MySQL server") {
		t.Fatalf("the failure does not say what mysqldump said: %v", missing[0])
	}
	if !strings.Contains(missing[0].What, "customer1_wp") {
		t.Fatalf("the failure does not say which database: %v", missing[0])
	}
}

// TestWhatWasRunIsInTheLogAtDebug is what the log level is for: at debug,
// the commands this provider runs are written down, so a failure can be
// reproduced by hand exactly as it happened.
func TestWhatWasRunIsInTheLogAtDebug(t *testing.T) {
	var written bytes.Buffer
	host := newFakeHost(t, "customer1")
	host.MysqldumpPath = fakeMysqldump(t, "", 0)
	host.MysqlPath = fakeMysqldump(t, "", 0)
	host.Log = slog.New(slog.NewTextHandler(&written, &slog.HandlerOptions{Level: slog.LevelDebug}))

	dir := t.TempDir()
	if _, err := host.dumpDatabases(context.Background(),
		StageRequest{StagingDir: dir, Account: AccountInfo{User: "customer1"}},
		pkgacct.Payload{DumpPaths: map[string]string{
			"customer1_wp": filepath.Join(dir, "databases", "customer1_wp.sql"),
		}}); err != nil {
		t.Fatalf("dump: %v", err)
	}
	log := written.String()
	// account= is on the line because the service log is filtered by it.
	for _, want := range []string{"level=DEBUG", "customer1_wp", "--single-transaction", "account=customer1"} {
		if !strings.Contains(log, want) {
			t.Errorf("the debug log does not carry %q:\n%s", want, log)
		}
	}
}

// TestNothingIsWrittenWhenTheLevelIsNotDebug keeps the loud lines behind
// the level. A server left at info must not pay for them.
func TestNothingIsWrittenWhenTheLevelIsNotDebug(t *testing.T) {
	var written bytes.Buffer
	host := newFakeHost(t, "customer1")
	host.MysqldumpPath = fakeMysqldump(t, "", 0)
	host.MysqlPath = fakeMysqldump(t, "", 0)
	host.Log = slog.New(slog.NewTextHandler(&written, &slog.HandlerOptions{Level: slog.LevelInfo}))

	dir := t.TempDir()
	if _, err := host.dumpDatabases(context.Background(),
		StageRequest{StagingDir: dir, Account: AccountInfo{User: "customer1"}},
		pkgacct.Payload{DumpPaths: map[string]string{
			"customer1_wp": filepath.Join(dir, "databases", "customer1_wp.sql"),
		}}); err != nil {
		t.Fatalf("dump: %v", err)
	}
	if strings.Contains(written.String(), "--single-transaction") {
		t.Error("a server at info wrote the debug lines anyway")
	}
}
