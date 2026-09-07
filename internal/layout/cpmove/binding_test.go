package cpmove

import (
	"archive/tar"
	"os"
	"path/filepath"
	"testing"
)

func TestAccountArchiveRejectsSymlinkAndConflictingUSER(t *testing.T) {
	root := t.TempDir()
	archive := filepath.Join(root, "cpmove-customer1.tar")
	writeTestTar(t, archive, map[string]string{"cpmove-customer1/cp/customer1": "USER=victim\n"})
	if err := (Layout{}).ValidateArchive(t.Context(), archive, "customer1"); err == nil {
		t.Fatal("conflicting USER field was accepted")
	}
	writeTestTar(t, archive, map[string]string{"cpmove-customer1/cp/customer1": "USER=customer1\n"})
	if err := (Layout{}).ValidateArchive(t.Context(), archive, "customer1"); err != nil {
		t.Fatalf("valid identity was refused: %v", err)
	}
	link := filepath.Join(t.TempDir(), "cpmove-customer1.tar")
	if err := os.Symlink(archive, link); err != nil {
		t.Fatal(err)
	}
	if err := (Layout{}).ValidateArchive(t.Context(), link, "customer1"); err == nil {
		t.Fatal("a symlink was accepted as a restored account archive")
	}
}

// writeTestTar builds a tar holding exactly the members named, which is
// how an archive with somebody else's identity record inside it is made.
func writeTestTar(t *testing.T, path string, files map[string]string) {
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
