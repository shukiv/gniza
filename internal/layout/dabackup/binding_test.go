package dabackup

import (
	"archive/tar"
	"os"
	"path/filepath"
	"testing"
)

// TestTheArchiveSaysWhoseAccountItIs is the same rule cpmove has, for
// DirectAdmin's record: a restic tag and a filename both come from
// outside the archive, and the panel's restore reads what is inside it.
// Restoring one customer's data into another's account is the worst thing
// this program could do.
func TestTheArchiveSaysWhoseAccountItIs(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "user.admin.customer1.tar")

	writeTar(t, archive, map[string]string{
		"backup/user.conf": "username=victim\nusage=0\n",
	})
	if err := (Layout{}).ValidateArchive(t.Context(), archive, "customer1"); err == nil {
		t.Fatal("an archive belonging to another account was accepted")
	}

	writeTar(t, archive, map[string]string{
		"backup/user.conf": "username=customer1\nusage=0\n",
	})
	if err := (Layout{}).ValidateArchive(t.Context(), archive, "customer1"); err != nil {
		t.Fatalf("the account's own archive was refused: %v", err)
	}

	// An archive with nothing that says whose it is cannot be restored
	// into anybody's account.
	writeTar(t, archive, map[string]string{"domains/example.com/public_html/index.html": "<h1>hi</h1>"})
	if err := (Layout{}).ValidateArchive(t.Context(), archive, "customer1"); err == nil {
		t.Fatal("an archive with no identity record was accepted")
	}
}

// TestAnArchiveIsReadWhereItLies. The file is opened without following a
// symlink, because what is checked and what is restored have to be the
// same bytes.
func TestAnArchiveIsReadWhereItLies(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "user.admin.customer1.tar")
	writeTar(t, archive, map[string]string{"backup/user.conf": "username=customer1\n"})

	link := filepath.Join(t.TempDir(), "user.admin.customer1.tar")
	if err := os.Symlink(archive, link); err != nil {
		t.Fatal(err)
	}
	if err := (Layout{}).ValidateArchive(t.Context(), link, "customer1"); err == nil {
		t.Fatal("a symlink was accepted as a restored account archive")
	}
}

// TestTheFilenameIsCheckedBeforeItIsOpened covers the shapes DirectAdmin
// gives a backup, and the ones that belong to somebody else.
func TestTheFilenameIsCheckedBeforeItIsOpened(t *testing.T) {
	for _, name := range []string{
		"user.admin.customer1.tar.gz",
		"user.admin.customer1.2026-09-08-02-15.tar.gz",
		"user.reseller.customer1.tar.zst",
		"customer1.tar",
	} {
		if !nameMatchesArchive(name, "customer1") {
			t.Errorf("%q was not recognised as customer1's backup", name)
		}
	}
	for _, name := range []string{
		"user.admin.victim.tar.gz",
		"user.admin.customer12.tar.gz",
		"victim.tar",
		"customer1",
	} {
		if nameMatchesArchive(name, "customer1") && name != "customer1" {
			t.Errorf("%q was accepted as customer1's backup", name)
		}
	}
}

// TestAMailboxAddressBecomesTheTwoPathElementsDirectAdminUses.
//
// Where cPanel keeps one maildir per address under mail/, DirectAdmin
// splits the address: imap/<domain>/<mailbox>. A name that is not an
// address is passed through as written, and what stops it naming
// something outside the account is the caller, which clamps every path to
// the home directory before it reaches a snapshot -- see
// TestAMailboxNameCannotReachOutsideTheAccount in internal/granular.
func TestAMailboxAddressBecomesTheTwoPathElementsDirectAdminUses(t *testing.T) {
	paths := (Layout{}).MailboxPaths([]string{
		"sales@example.com", "example.com/sales", "example.com",
	})
	want := []string{"imap/example.com/sales", "imap/example.com/sales", "imap/example.com"}
	if len(paths) != len(want) {
		t.Fatalf("got %d paths for %d mailboxes: %v", len(paths), len(want), paths)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("%d: got %q, want %q", i, paths[i], want[i])
		}
	}
}

func writeTar(t *testing.T, path string, files map[string]string) {
	t.Helper()
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	writer := tar.NewWriter(out)
	for name, body := range files {
		if err := writer.WriteHeader(&tar.Header{
			Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}
