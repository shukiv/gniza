package directadmin

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/layout/dabackup"
	"github.com/shukiv/gniza/internal/panel"
	"github.com/shukiv/gniza/internal/pkgacct"
)

func TestOnlyAMemberOwnedByAnotherAccountIsCounted(t *testing.T) {
	var found foreignOwners
	found.see("public_html/index.php", 1005, 1005)
	found.see("public_html/wp-config.php", 0, 1005)
	found.see("public_html/uploads/a.jpg", 0, 1005)
	found.see("public_html/vendor/x.php", 1006, 1005)

	if found.Count != 3 {
		t.Errorf("counted %d files the restore cannot reproduce", found.Count)
	}
	if found.First != "public_html/wp-config.php" {
		t.Errorf("named %q as the first of them", found.First)
	}
	warning := found.warning()
	if !strings.Contains(warning, "public_html/wp-config.php") || !strings.Contains(warning, "3") {
		t.Errorf("the warning does not say what to go and look at: %q", warning)
	}
	if (foreignOwners{}).warning() != "" {
		t.Error("an account whose home is all its own was warned about")
	}
}

// The home read in place is never in an archive, so the only thing that
// can answer who owns what is the home itself.
func TestAHomeReadInPlaceIsCheckedOnDisk(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "public_html"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"public_html/index.php", "public_html/wp-config.php"} {
		if err := os.WriteFile(filepath.Join(home, name), []byte("<?php\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mine := os.Geteuid()

	found, err := homeOwners(home, mine)
	if err != nil {
		t.Fatal(err)
	}
	if found.Count != 0 {
		t.Errorf("a home the account owns outright was reported as %d foreign files", found.Count)
	}

	found, err = homeOwners(home, mine+1)
	if err != nil {
		t.Fatal(err)
	}
	// The directory counts with them: tar cannot chown a directory to
	// somebody else either, and it is reached before what is under it.
	if found.Count != 3 {
		t.Errorf("counted %d things owned by somebody else, wanted the directory and its two files", found.Count)
	}
	if found.First != "public_html" {
		t.Errorf("named %q, which is not the first thing in the home the restore reaches", found.First)
	}
}

// A home is written into the whole time it is read, and a check that
// costs a backup is worse than the condition it looks for.
func TestAHomeThatCannotBeWalkedDoesNotFailTheBackup(t *testing.T) {
	if _, err := homeOwners(filepath.Join(t.TempDir(), "no-such-home"), 1005); err != nil {
		t.Errorf("a home that is not there stopped the check: %v", err)
	}
}

// accountIs makes one account somebody other than whoever runs the test,
// which is what an account is on a real server. Everyone else -- admin,
// whose identity the native workspace is built from -- stays as they are.
func accountIs(t *testing.T, account string, uid int) func(string) (*user.User, error) {
	t.Helper()
	return func(name string) (*user.User, error) {
		me, err := user.Current()
		if err != nil || name != account {
			return me, err
		}
		other := *me
		other.Uid = strconv.Itoa(uid)
		other.Username = account
		return &other, nil
	}
}

// A home read in place is handed to restic as the path it is on, so
// nothing else in the backup ever looks at who owns what. This is where
// the six accounts on the validation host are found.
func TestAnInPlaceBackupSaysWhatWillStopItsRestore(t *testing.T) {
	r := nativeHost(t)
	r.ReadHomeInPlace = true
	// The account is somebody other than whoever runs the test, so the
	// files in the fixture home are not the account's own.
	r.lookupUser = accountIs(t, "studio", os.Geteuid()+1)
	staging := privateStaging(t)

	payload, err := r.Stage(t.Context(), panel.StageRequest{
		Account: panel.AccountInfo{User: "studio"}, StagingDir: staging, Mode: pkgacct.ModeSplit,
	})
	if err != nil {
		t.Fatalf("staging in parts: %v", err)
	}
	if len(payload.Warnings) != 1 {
		t.Fatalf("a home full of files the account does not own produced %d warnings", len(payload.Warnings))
	}
	if !strings.Contains(payload.Warnings[0], "owned by another account") {
		t.Errorf("the warning does not say what is wrong: %q", payload.Warnings[0])
	}
	if len(payload.Missing) != 0 {
		t.Error("a backup that holds the whole account reported something missing from it")
	}
}

// And the same account whose home the account does own is not warned
// about, which is the other half of a warning being worth reading.
func TestAnInPlaceBackupOfAnOrdinaryHomeSaysNothing(t *testing.T) {
	r := nativeHost(t)
	r.ReadHomeInPlace = true
	staging := privateStaging(t)

	payload, err := r.Stage(t.Context(), panel.StageRequest{
		Account: panel.AccountInfo{User: "studio"}, StagingDir: staging, Mode: pkgacct.ModeSplit,
	})
	if err != nil {
		t.Fatalf("staging in parts: %v", err)
	}
	if len(payload.Warnings) != 0 {
		t.Errorf("an ordinary account was warned about: %q", payload.Warnings)
	}
}

// The other shape: this DirectAdmin will not read the home in place, so
// the account arrives as an archive and the only record of who owns what
// is the manifest the unpack wrote.
func TestASplitBackupSaysWhatWillStopItsRestore(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(t.TempDir(), "user.admin.studio.tar")
	writeArchiveWithNestedHome(t, archive, "studio")
	if err := (dabackup.Layout{}).UnpackArchive(t.Context(), archive, "studio", dir); err != nil {
		t.Fatal(err)
	}

	found, err := archiveOwners(dir, 1005)
	if err != nil {
		t.Fatal(err)
	}
	if found.Count != 1 || found.First != "public_html/wp-config.php" {
		t.Errorf("the manifest reported %d files owned by somebody else, the first %q",
			found.Count, found.First)
	}
	if warning := found.warning(); !strings.Contains(warning, "public_html/wp-config.php") {
		t.Errorf("the warning does not name the file: %q", warning)
	}

	// A directory that is not an unpacked account says so rather than
	// reporting an account whose home is all its own: a check that
	// cannot run must not read as a clean bill of health.
	if _, err := archiveOwners(t.TempDir(), 1005); err == nil {
		t.Error("a directory holding no manifest passed the check")
	}
}

// writeArchiveWithNestedHome writes what a DirectAdmin that ignores the
// backup selection writes: the account's records, and the account's own
// files in a nested archive under backup/.
func writeArchiveWithNestedHome(t *testing.T, filename, account string) {
	t.Helper()
	var nested bytes.Buffer
	inner := tar.NewWriter(&nested)
	for _, member := range []struct {
		name string
		uid  int
	}{{"public_html/index.php", 1005}, {"public_html/wp-config.php", 0}} {
		body := "<?php\n"
		if err := inner.WriteHeader(&tar.Header{
			Name: member.name, Mode: 0o644, Size: int64(len(body)),
			Typeflag: tar.TypeReg, Uid: member.uid, Gid: 1005,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := inner.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := inner.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	outer := tar.NewWriter(f)
	conf := "username=" + account + "\nusertype=user\n"
	if err := outer.WriteHeader(&tar.Header{
		Name: "backup/user.conf", Mode: 0o600, Size: int64(len(conf)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := outer.Write([]byte(conf)); err != nil {
		t.Fatal(err)
	}
	if err := outer.WriteHeader(&tar.Header{
		Name: "backup/home.tar", Mode: 0o600, Size: int64(nested.Len()), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := outer.Write(nested.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(outer.Close(), f.Close()); err != nil {
		t.Fatal(err)
	}
}

// And end to end through the staging that takes an archive apart: this
// is what a DirectAdmin that will not read the home in place produces,
// and the warning has to come out of the payload either way.
func TestTakingAnArchiveApartAlsoSaysWhatWillStopTheRestore(t *testing.T) {
	r := nativeHost(t)
	r.lookupUser = accountIs(t, "studio", 1005)
	t.Setenv("GNIZA_NATIVE_SCENARIO", "root-owned-home")
	staging := privateStaging(t)

	payload, err := r.Stage(t.Context(), panel.StageRequest{
		Account: panel.AccountInfo{User: "studio"}, StagingDir: staging, Mode: pkgacct.ModeSplit,
	})
	if err != nil {
		t.Fatalf("staging in parts: %v", err)
	}
	if len(payload.Warnings) != 1 {
		t.Fatalf("an archive holding a root-owned home file produced %d warnings", len(payload.Warnings))
	}
	if !strings.Contains(payload.Warnings[0], "public_html/wp-config.php") {
		t.Errorf("the warning does not name the file: %q", payload.Warnings[0])
	}
}
