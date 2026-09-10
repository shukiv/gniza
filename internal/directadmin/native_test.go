package directadmin

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/shukiv/gniza/internal/panel"
	"github.com/shukiv/gniza/internal/pkgacct"
)

func nativeHost(t *testing.T) *Real {
	t.Helper()
	r := fakeHost(t, "studio")
	conf := filepath.Join(r.DataDir, "studio", "user.conf")
	if err := os.WriteFile(conf, []byte("username=studio\nusertype=user\ndomain=studio.example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.NativeRoot = filepath.Join(t.TempDir(), "native")
	r.lookupUser = func(string) (*user.User, error) { return user.Current() }
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r.BinaryPath = filepath.Join(t.TempDir(), "directadmin")
	script := "#!/bin/sh\nGNIZA_NATIVE_HELPER=1 exec '" + strings.ReplaceAll(binary, "'", "'\\''") + "' -test.run=^TestNativeCommandProcess$ -- \"$@\"\n"
	if err := os.WriteFile(r.BinaryPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GNIZA_NATIVE_CAPTURE", filepath.Join(t.TempDir(), "task"))
	return r
}

func privateStaging(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A subprocess fixture exercises actual exec, argument boundaries, output
// parsing, filesystem handoff and cancellation, without a hosting panel.
func TestNativeCommandProcess(t *testing.T) {
	if os.Getenv("GNIZA_NATIVE_HELPER") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	args = args[1:]
	scenario := os.Getenv("GNIZA_NATIVE_SCENARIO")
	var task url.Values
	if args[0] == "admin-backup" {
		task = url.Values{"action": {"backup"}, "select0": {strings.TrimPrefix(args[2], "--user=")},
			"local_path": {strings.TrimPrefix(args[1], "--destination=")}, "owner": {"admin"}, "type": {"admin"},
			"value": {"multiple"}, "where": {"local"}, "when": {"now"}}
	} else {
		if args[0] != "taskq" || len(args) != 2 || !strings.HasPrefix(args[1], "--run=") {
			os.Exit(2)
		}
		var err error
		task, err = url.ParseQuery(strings.TrimPrefix(args[1], "--run="))
		if err != nil {
			os.Exit(2)
		}
	}
	if err := os.WriteFile(os.Getenv("GNIZA_NATIVE_CAPTURE"), []byte(task.Encode()), 0o600); err != nil {
		os.Exit(2)
	}
	if scenario == "wrong-task" {
		task.Set("select1", "victim")
	}
	fmt.Printf("2026/09/08 05:41:22  info executing task            task=%s\n", task.Encode())
	if scenario == "cancel" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	if task.Get("action") == "backup" && scenario != "missing-archive" {
		account := task.Get("select0")
		identity := account
		if scenario == "wrong-identity" {
			identity = "victim"
		}
		archive := filepath.Join(task.Get("local_path"), "user.admin."+account+".tar.zst")
		// A DirectAdmin that honours the selection writes the account's
		// records and its messages and nothing of the account's own
		// files. One that does not -- the "ignores-selection" scenario --
		// writes the whole account whatever it was asked for.
		switch {
		case task.Get("what") == "select" && scenario != "ignores-selection":
			// The real one includes the messages unless the client said
			// it knows what email_data is -- measured on 1.709, and the
			// difference between a 15 MB archive and a 296 MB one.
			writeLeanNativeArchive(t, archive, identity, task.Get("email_data_aware") != "yes")
		case scenario == "root-owned-home":
			writeNativeArchiveWith(t, archive, identity, map[string]string{
				"backup/home.tar": nestedHomeTar(t),
			})
		default:
			writeNativeArchive(t, archive, identity)
		}
		if scenario == "symlink" {
			if err := os.Rename(archive, archive+".held"); err != nil {
				os.Exit(2)
			}
			if err := os.Symlink(archive+".held", archive); err != nil {
				os.Exit(2)
			}
		}
		if scenario == "hardlink" {
			if err := os.Link(archive, archive+".held"); err != nil {
				os.Exit(2)
			}
		}
	} else if task.Get("action") == "restore" {
		if _, err := os.Stat(filepath.Join(task.Get("local_path"), task.Get("select0"))); err != nil {
			os.Exit(2)
		}
	}
	if scenario == "failure-zero" {
		fmt.Println("2026/09/08 05:41:23 error running backup task       error=error code 1: disk full")
	}
	if scenario == "overflow" {
		fmt.Print(strings.Repeat("x", (1<<20)+1))
	}
	if scenario != "no-completion" {
		fmt.Printf("2026/09/08 05:41:23  info finished task             duration=739ms task=%s\n", task.Encode())
	}
	if scenario == "exit-failure" {
		os.Exit(1)
	}
	os.Exit(0)
}

// writeLeanNativeArchive is what DirectAdmin writes for a backup that
// asked for everything except "domain": no domains/, no nested home
// archive. withMail is what it does for a client that did not say
// email_data_aware=yes: the messages come along anyway.
func writeLeanNativeArchive(t *testing.T, filename, account string, withMail bool) {
	t.Helper()
	f, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	z, err := zstd.NewWriter(f, zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	tarball := tar.NewWriter(z)
	members := map[string]string{
		"backup/user.conf":           "username=" + account + "\nusertype=user\n",
		"backup/backup_options.list": "email\nsubdomain\n",
	}
	if withMail {
		members["imap/studio.example/hi/Maildir/new/1"] = "a message"
	}
	for name, body := range members {
		if err := tarball.WriteHeader(&tar.Header{
			Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarball.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarball.Close(); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeNativeArchive(t *testing.T, filename, account string) {
	t.Helper()
	writeNativeArchiveWith(t, filename, account, nil)
}

func writeNativeArchiveWith(t *testing.T, filename, account string, extra map[string]string) {
	t.Helper()
	f, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	z, err := zstd.NewWriter(f, zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	tarball := tar.NewWriter(z)
	members := map[string]string{"backup/user.conf": "username=" + account + "\nusertype=user\n", "domains/studio.example/public_html/index.html": "fixture"}
	for name, body := range extra {
		members[name] = body
	}
	for name, body := range members {
		if err := tarball.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarball.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := errors.Join(tarball.Close(), z.Close(), f.Close()); err != nil {
		t.Fatal(err)
	}
}

// nestedHomeTar is the account's own files as DirectAdmin packs them,
// with one of them owned by root: the shape six accounts on the
// validation host are in.
func nestedHomeTar(t *testing.T) string {
	t.Helper()
	var out bytes.Buffer
	tarball := tar.NewWriter(&out)
	for _, member := range []struct {
		name string
		uid  int
	}{{"public_html/index.php", 1005}, {"public_html/wp-config.php", 0}} {
		body := "<?php\n"
		if err := tarball.WriteHeader(&tar.Header{
			Name: member.name, Mode: 0o644, Size: int64(len(body)),
			Typeflag: tar.TypeReg, Uid: member.uid, Gid: 1005,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarball.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarball.Close(); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestNativeStageProducesVerifiedPrivateZstdArchive(t *testing.T) {
	r := nativeHost(t)
	staging := privateStaging(t)
	payload, err := r.Stage(t.Context(), panel.StageRequest{Account: panel.AccountInfo{User: "studio"}, StagingDir: staging, Mode: pkgacct.ModeMonolithic})
	if err != nil {
		t.Fatal(err)
	}
	if payload.Mode != pkgacct.ModeMonolithic || !payload.Degraded || len(payload.Parts) != 1 {
		t.Fatalf("unexpected payload: %+v", payload)
	}
	archive := payload.Parts[0].Path
	if filepath.Dir(archive) != staging {
		t.Fatalf("payload outside private staging: %s", archive)
	}
	if err := r.Layout().ValidateArchive(t.Context(), archive, "studio"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(archive)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("archive is not private: %v %v", info, err)
	}
	contents, err := os.ReadFile(os.Getenv("GNIZA_NATIVE_CAPTURE"))
	if err != nil {
		t.Fatal(err)
	}
	task, _ := url.ParseQuery(string(contents))
	if task.Get("local_path") == staging {
		t.Fatal("root-private staging was passed to DirectAdmin")
	}
	if _, err := os.Stat(task.Get("local_path")); !os.IsNotExist(err) {
		t.Fatalf("native scratch remained: %v", err)
	}
	if info, _ := os.Stat(staging); info.Mode().Perm() != 0o700 {
		t.Fatal("staging permissions were broadened")
	}
}

func TestNativeStageNeverAcceptsFailedStaleOrUntrustedOutput(t *testing.T) {
	for _, scenario := range []string{"failure-zero", "exit-failure", "wrong-task", "wrong-identity", "symlink", "hardlink", "overflow", "no-completion", "missing-archive"} {
		t.Run(scenario, func(t *testing.T) {
			r := nativeHost(t)
			t.Setenv("GNIZA_NATIVE_SCENARIO", scenario)
			staging := privateStaging(t)
			// A stale archive must never turn a failed native run into success.
			old := filepath.Join(staging, "user.admin.studio.tar.zst")
			writeNativeArchive(t, old, "studio")
			before, _ := os.ReadFile(old)
			if _, err := r.Stage(t.Context(), panel.StageRequest{Account: panel.AccountInfo{User: "studio"}, StagingDir: staging, Mode: pkgacct.ModeMonolithic}); err == nil {
				t.Fatal("invalid native backup reported success")
			}
			after, _ := os.ReadFile(old)
			if string(before) != string(after) {
				t.Fatal("stale archive overwritten")
			}
		})
	}
}

func TestNativeStageRejectsWrongIdentityWithoutStaleOutput(t *testing.T) {
	r := nativeHost(t)
	t.Setenv("GNIZA_NATIVE_SCENARIO", "wrong-identity")
	staging := privateStaging(t)
	if _, err := r.Stage(t.Context(), panel.StageRequest{Account: panel.AccountInfo{User: "studio"}, StagingDir: staging, Mode: pkgacct.ModeMonolithic}); err == nil {
		t.Fatal("foreign archive accepted")
	}
	if files, _ := os.ReadDir(staging); len(files) != 0 {
		t.Fatal("invalid handoff remained")
	}
}

func TestNativeOperationsSerializePerAccount(t *testing.T) {
	r := nativeHost(t)
	w, err := r.nativeWorkspace("studio")
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	if _, err := r.nativeWorkspace("studio"); err == nil {
		t.Fatal("concurrent native operation accepted")
	}
}

func TestNativeOperationCancellation(t *testing.T) {
	r := nativeHost(t)
	t.Setenv("GNIZA_NATIVE_SCENARIO", "cancel")
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_, err := r.Stage(ctx, panel.StageRequest{Account: panel.AccountInfo{User: "studio"}, StagingDir: privateStaging(t), Mode: pkgacct.ModeMonolithic})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wanted cancellation, got %v", err)
	}
	w, err := r.nativeWorkspace("studio")
	if err != nil {
		t.Fatal("lock not released after cancelled command: ", err)
	}
	w.close()
}

func TestNativeRestoreUsesOneInlineTaskAndExplicitOverwrite(t *testing.T) {
	for _, scenario := range []string{"", "failure-zero", "wrong-task", "no-completion"} {
		t.Run(scenario, func(t *testing.T) {
			r := nativeHost(t)
			t.Setenv("GNIZA_NATIVE_SCENARIO", scenario)
			archive := filepath.Join(t.TempDir(), "user.admin.studio.tar.zst")
			writeNativeArchive(t, archive, "studio")
			transcript, err := r.Apply(t.Context(), archive, panel.ApplyOptions{Overwrite: true, Unrestricted: true})
			if (err != nil) != (scenario != "") {
				t.Fatalf("unexpected result: %v\n%s", err, transcript)
			}
			data, readErr := os.ReadFile(os.Getenv("GNIZA_NATIVE_CAPTURE"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			task, _ := url.ParseQuery(string(data))
			if task.Get("select0") != filepath.Base(archive) || task.Has("select1") || task.Get("action") != "restore" {
				t.Fatalf("wrong task: %v", task)
			}
			if _, err := os.Stat(archive); err != nil {
				t.Fatal("input archive changed: ", err)
			}
		})
	}
}

func TestNativeRestoreRefusesUnsupportedSafetyOptionsBeforeRunning(t *testing.T) {
	r := nativeHost(t)
	for _, opts := range []panel.ApplyOptions{{}, {Overwrite: true}, {Unrestricted: true}, {Overwrite: true, Unrestricted: true, SkipDNS: true}, {Overwrite: true, Unrestricted: true, NewUser: "victim"}} {
		if _, err := r.Apply(t.Context(), "/absent/user.admin.studio.tar", opts); !errors.Is(err, ErrUnverified) {
			t.Fatalf("unsupported options %+v: %v", opts, err)
		}
	}
	if _, err := os.Stat(os.Getenv("GNIZA_NATIVE_CAPTURE")); !os.IsNotExist(err) {
		t.Fatal("native restore was executed")
	}
}

func TestAccountSizeIncludesHomeAndDatabaseData(t *testing.T) {
	r := fakeHost(t, "studio")
	r.MysqlPath = filepath.Join(t.TempDir(), "mysql")
	script := "#!/bin/sh\ncase \"$*\" in\n*information_schema.tables*) printf '1024\\n';;\n*) printf 'studio_wp\\nvictim_wp\\n';;\nesac\n"
	if err := os.WriteFile(r.MysqlPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.HomeRoot, "studio", "example.txt"), []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := r.Account(t.Context(), "studio")
	if err != nil {
		t.Fatal(err)
	}
	if info.SizeBytes != 1029 || len(info.Databases) != 1 || info.Databases[0] != "studio_wp" {
		t.Fatalf("wrong account inventory: %+v", info)
	}
}

func TestNativeSpaceAndPrivateStagingAreCheckedBeforeCommand(t *testing.T) {
	for _, public := range []bool{false, true} {
		r := nativeHost(t)
		staging := privateStaging(t)
		if public {
			if err := os.Chmod(staging, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		_, err := r.Stage(t.Context(), panel.StageRequest{Account: panel.AccountInfo{User: "studio", SizeBytes: ^uint64(0)}, StagingDir: staging, Mode: pkgacct.ModeMonolithic})
		if err == nil {
			t.Fatal("unsafe staging accepted")
		}
		if _, err := os.Stat(os.Getenv("GNIZA_NATIVE_CAPTURE")); !os.IsNotExist(err) {
			t.Fatal("command ran before preflight")
		}
	}
}

// fakeMySQL answers the two questions the provider asks a database
// server: which databases an account has, and how much they weigh.
func fakeMySQL(t *testing.T, r *Real, present ...string) {
	t.Helper()
	r.MysqlPath = filepath.Join(t.TempDir(), "mysql")
	script := "#!/bin/sh\ncase \"$*\" in\n*information_schema.tables*) printf '0\\n';;\n*) printf '" +
		strings.Join(present, "\\n") + "\\n';;\nesac\n"
	if err := os.WriteFile(r.MysqlPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

// TestARestoreThatDidNotPutTheDatabasesBackIsNotASuccess is the same rule
// the cPanel side already has, for the panel that reaches it differently.
//
// DirectAdmin's restore is one task carried out by several modules. The
// native transcript is checked for error records, but a module that puts
// the files back and not the databases is the failure this program was
// written after: the account is there afterwards, the website answers,
// and the shop's orders are gone. Nobody looks again, because the restore
// said it worked.
//
// What the archive says the account's databases are is what has to be on
// the account when the restore is over. The names come out of the archive
// itself, not out of a filename or a tag.
func TestARestoreThatDidNotPutTheDatabasesBackIsNotASuccess(t *testing.T) {
	archiveWith := func(t *testing.T) string {
		t.Helper()
		archive := filepath.Join(t.TempDir(), "user.admin.studio.tar.zst")
		writeNativeArchiveWith(t, archive, "studio", map[string]string{
			"backup/studio_shop.sql": "-- dump\n",
			"backup/studio_wp.sql":   "-- dump\n",
		})
		return archive
	}
	whole := panel.ApplyOptions{Overwrite: true, Unrestricted: true}

	// DirectAdmin said yes and put neither database back.
	r := nativeHost(t)
	fakeMySQL(t, r)
	_, err := r.Apply(t.Context(), archiveWith(t), whole)
	if err == nil {
		t.Fatal("a restore that put none of the databases back was reported as a success")
	}
	for _, name := range []string{"studio_shop", "studio_wp"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the error does not name %s: %v", name, err)
		}
	}

	// One of the two, which is the shape a single failed module leaves.
	r = nativeHost(t)
	fakeMySQL(t, r, "studio_wp")
	_, err = r.Apply(t.Context(), archiveWith(t), whole)
	if err == nil {
		t.Fatal("a restore missing one of two databases was reported as a success")
	}
	if strings.Contains(err.Error(), "studio_wp") || !strings.Contains(err.Error(), "studio_shop") {
		t.Errorf("the error should name only the database that is missing: %v", err)
	}

	// And the restore that worked passes. A database the account has
	// gained since the backup is not a reason to fail a restore.
	r = nativeHost(t)
	fakeMySQL(t, r, "studio_shop", "studio_wp", "studio_later")
	if _, err := r.Apply(t.Context(), archiveWith(t), whole); err != nil {
		t.Errorf("a restore that put the databases back was reported as failed: %v", err)
	}
}

// TestACustomerSQLFileIsNotOneOfTheAccountsDatabases covers what a
// customer keeps in their own web root.
//
// A .sql file in public_html is one of the most ordinary things on a
// hosting account: phpMyAdmin writes one on every export, and the popular
// WordPress migration plugins leave one behind. It is named after the
// database it came from, so it carries the account prefix, and it stays
// there long after that database has been dropped.
//
// Counting it as one of the account's databases would fail a restore that
// worked, for an account that is fine, every time -- and a restore
// reported as failed is a restore somebody runs again. Only the dumps
// DirectAdmin's own backup writes are the account's databases.
func TestACustomerSQLFileIsNotOneOfTheAccountsDatabases(t *testing.T) {
	r := nativeHost(t)
	fakeMySQL(t, r, "studio_shop")
	archive := filepath.Join(t.TempDir(), "user.admin.studio.tar.zst")
	writeNativeArchiveWith(t, archive, "studio", map[string]string{
		"backup/studio_shop.sql":                            "-- dump\n",
		"domains/studio.example/public_html/studio_old.sql": "-- the customer's own export\n",
	})
	if _, err := r.Apply(t.Context(), archive, panel.ApplyOptions{Overwrite: true, Unrestricted: true}); err != nil {
		t.Errorf("a restore was failed over a file in the customer's web root: %v", err)
	}
}

// An archive that names no databases fails nothing. DirectAdmin may carry
// its dumps somewhere this cannot read them -- nested inside another
// compressed member, for one -- and a check that cannot see them must
// stay silent rather than fail every restore of an account that has them.
func TestARestoreIsNotFailedForDatabasesTheArchiveDoesNotName(t *testing.T) {
	// No fake database client: an archive that names no databases must
	// not ask the database server anything, so a restore is never failed
	// because that server could not be reached.
	r := nativeHost(t)
	archive := filepath.Join(t.TempDir(), "user.admin.studio.tar.zst")
	writeNativeArchive(t, archive, "studio")
	if _, err := r.Apply(t.Context(), archive, panel.ApplyOptions{Overwrite: true, Unrestricted: true}); err != nil {
		t.Errorf("a restore was failed over databases the archive does not name: %v", err)
	}
}

// TestASweepClearsAWorkspaceAKilledRunLeft covers gigabytes found on a
// live server: the service was restarted while an account was being
// staged, so the deferred close never ran and 1.9 GiB of that account's
// archive stayed under the native root. The disk it filled is the one the
// next night's backups are refused for want of.
func TestASweepClearsAWorkspaceAKilledRunLeft(t *testing.T) {
	r := nativeHost(t)
	w, err := r.nativeWorkspace("studio")
	if err != nil {
		t.Fatal(err)
	}
	left := w.path
	// What a killed process leaves: the directory, and no lock held by
	// anyone, because the process that held it is gone.
	if err := w.lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(left, "output", "big"), []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}

	swept, err := r.SweepNativeWorkspaces()
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if swept != 1 {
		t.Errorf("swept = %d, want 1", swept)
	}
	if _, err := os.Stat(left); !os.IsNotExist(err) {
		t.Errorf("the workspace is still there: %v", err)
	}
}

// TestASweepLeavesAWorkspaceThatIsInUse is why the sweep takes each
// account's own lock first. An operator running a restore by hand while
// the service restarts must not have the archive removed underneath them.
func TestASweepLeavesAWorkspaceThatIsInUse(t *testing.T) {
	r := nativeHost(t)
	w, err := r.nativeWorkspace("studio")
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()

	swept, err := r.SweepNativeWorkspaces()
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if swept != 0 {
		t.Errorf("swept = %d, want 0: the lock is held", swept)
	}
	if _, err := os.Stat(w.path); err != nil {
		t.Errorf("a workspace in use was removed: %v", err)
	}
}

// The staging preflight for a backup that reads the home in place needs
// what that backup writes, which is the mail and the database dumps and
// nothing else under the home. Measured in the same walk that measures
// the account, because a second walk of a 19.7 GiB home costs what this
// exists to save.
func TestAccountAlsoMeasuresWhatALeanBackupWrites(t *testing.T) {
	r := fakeHost(t, "studio")
	r.MysqlPath = filepath.Join(t.TempDir(), "mysql")
	script := "#!/bin/sh\ncase \"$*\" in\n*information_schema.tables*) printf '1024\\n';;\n*) printf 'studio_wp\\n';;\nesac\n"
	if err := os.WriteFile(r.MysqlPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(r.HomeRoot, "studio")
	if err := os.MkdirAll(filepath.Join(home, "imap", "studio.example", "sales", "Maildir", "cur"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "imap", "studio.example", "sales", "Maildir", "cur", "1"), []byte("message"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Under the home and not in a lean archive: restic reads it in place.
	if err := os.WriteFile(filepath.Join(home, "domains", "big"), make([]byte, 5000), 0o600); err != nil {
		t.Fatal(err)
	}
	// Nor is a sibling whose name begins with the mail directory's.
	if err := os.WriteFile(filepath.Join(home, "imap-old"), make([]byte, 900), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := r.Account(t.Context(), "studio")
	if err != nil {
		t.Fatal(err)
	}
	if info.SizeBytes != 6931 {
		t.Errorf("the account measures %d bytes, want 6931", info.SizeBytes)
	}
	// The databases and nothing else: the messages are read where they
	// lie now that the task line says the client knows what email_data
	// is, and the account's files never were in this archive. Measured
	// on server-182-54-236-143.da.direct, 2026-09-10: 15,124,712 bytes
	// for an account whose mail is 733,507,893 on disk.
	if info.LeanBytes != 1024 {
		t.Errorf("a lean backup is reserved %d bytes, want 1024", info.LeanBytes)
	}
}

// An account is measured for the room its backup needs, so what the
// backup will not take must not be counted. DirectAdmin's own backup
// leaves the account's backup directories out, which means they are in no
// archive Gniza ever stages -- and on a live server they are almost all
// of the account: admin's home on server-182-54-236-143.da.direct was
// 6,737,318,299 bytes, of which 6,732,830,003 was admin_backups. Counting
// them asked for 57.2 GiB of staging on a server with 16.5 GiB free, for
// a backup that would have written megabytes.
func TestAnAccountIsNotMeasuredByWhatItsBackupLeavesOut(t *testing.T) {
	r := fakeHost(t, "studio")
	r.MysqlPath = filepath.Join(t.TempDir(), "mysql")
	script := "#!/bin/sh\ncase \"$*\" in\n*information_schema.tables*) printf '0\\n';;\n*) printf 'studio_wp\\n';;\nesac\n"
	if err := os.WriteFile(r.MysqlPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(r.HomeRoot, "studio")
	for _, dir := range []string{"admin_backups", "user_backups", "backups", "tmp"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, dir, "archive"), make([]byte, 100000), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// What DirectAdmin does put in its archive, and Gniza leaves out of
	// the tree restic reads. Staging still has to hold it on the whole-
	// archive path, so it is counted.
	if err := os.MkdirAll(filepath.Join(home, "application_backups"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "application_backups", "wp.tar.gz"), make([]byte, 3000), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "domains", "site"), make([]byte, 5000), 0o600); err != nil {
		t.Fatal(err)
	}

	info, err := r.Account(t.Context(), "studio")
	if err != nil {
		t.Fatal(err)
	}
	if info.SizeBytes != 8000 {
		t.Errorf("the account measures %d bytes, want 8000: the four directories "+
			"DirectAdmin's own backup skips are in no archive Gniza stages", info.SizeBytes)
	}
}

// DirectAdmin's backup page sends database_data_aware=yes and
// email_data_aware=yes with every request, and its meaning is exact: a
// client that says so and leaves email_data out gets no messages, and a
// client that does not say so is taken for one written before the option
// existed and gets them anyway. Gniza never said so, and every "lean"
// archive it asked for carried the account's whole mailbox -- 732,924,695
// bytes of 1,281,932,189 on the validation host. Two flags took the
// archive from 310,915,651 bytes to 15,124,712.
func TestTheLeanTaskLineSaysItKnowsWhatEmailDataIs(t *testing.T) {
	task := backupTask("studio", "/tmp/x", true)
	for _, flag := range []string{"email_data_aware", "database_data_aware"} {
		if task.Get(flag) != "yes" {
			t.Errorf("%s is %q, want yes: without it DirectAdmin includes the data anyway", flag, task.Get(flag))
		}
	}
	for _, values := range task {
		for _, v := range values {
			if v == "email_data" {
				t.Error("email_data is selected, which is the whole mailbox compressed into the archive")
			}
		}
	}
	// A whole-account request is not a selection and says nothing about
	// options it did not use.
	if whole := backupTask("studio", "/tmp/x", false); whole.Has("what") || whole.Has("email_data_aware") {
		t.Errorf("a whole-account task carries selection flags: %v", whole)
	}
}
