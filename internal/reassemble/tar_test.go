package reassemble

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractTarRefusesPathTraversal(t *testing.T) {
	// The archive comes from the server being restored, which in the
	// threat model this program is built around may be the compromised
	// one. An entry that writes outside the extraction directory must
	// never be honoured.
	for _, name := range []string{
		"../escape.txt",
		"cpmove-c1/../../escape.txt",
		"/etc/cron.d/escape",
	} {
		dir := t.TempDir()
		archive := filepath.Join(dir, "evil.tar")
		writeRawTar(t, archive, []tar.Header{
			{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: 4},
		}, []string{"evil"})

		err := extractTar(archive, filepath.Join(dir, "out"))
		if name == "/etc/cron.d/escape" {
			// An absolute path is cleaned into the extraction directory
			// rather than escaping, which is also acceptable.
			if err == nil {
				if _, statErr := os.Stat("/etc/cron.d/escape"); statErr == nil {
					t.Fatal("an absolute entry was written outside the extraction directory")
				}
			}
			continue
		}
		if err == nil {
			t.Errorf("entry %q was extracted instead of refused", name)
		}
	}
}

func TestExtractTarRefusesEscapingSymlink(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "evil.tar")
	writeRawTar(t, archive, []tar.Header{
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../../../etc/passwd", Mode: 0o777},
	}, []string{""})

	if err := extractTar(archive, filepath.Join(dir, "out")); err == nil {
		t.Error("a symlink pointing outside the tree was created")
	}
}

func TestExtractTarDropsSetuidBits(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "a.tar")
	// 0o4755 is setuid root; only the permission bits should survive.
	writeRawTar(t, archive, []tar.Header{
		{Name: "bin/tool", Typeflag: tar.TypeReg, Mode: 0o4755, Size: 2},
	}, []string{"hi"})

	out := filepath.Join(dir, "out")
	if err := extractTar(archive, out); err != nil {
		t.Fatalf("extractTar: %v", err)
	}
	info, err := os.Stat(filepath.Join(out, "bin", "tool"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSetuid != 0 {
		t.Errorf("mode = %v, setuid survived extraction", info.Mode())
	}
}

func TestTarRoundTrip(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "tree")
	if err := os.MkdirAll(filepath.Join(source, "cpmove-c1", "homedir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "cpmove-c1", "homedir", "index.html"),
		[]byte("<h1>hi</h1>"), 0o600); err != nil {
		t.Fatal(err)
	}

	archive := filepath.Join(dir, "out.tar")
	if err := createTar(source, archive); err != nil {
		t.Fatalf("createTar: %v", err)
	}
	back := filepath.Join(dir, "back")
	if err := extractTar(archive, back); err != nil {
		t.Fatalf("extractTar: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(back, "cpmove-c1", "homedir", "index.html"))
	if err != nil {
		t.Fatalf("read restored file: %v", err)
	}
	if string(body) != "<h1>hi</h1>" {
		t.Errorf("round trip changed the content: %q", body)
	}
}

func TestSafeName(t *testing.T) {
	for _, name := range []string{"a/b", "./a", "a/../b"} {
		got, err := safeName(name)
		if err != nil {
			t.Errorf("safeName(%q) returned %v", name, err)
			continue
		}
		if got == "" || strings.HasPrefix(got, "/") || strings.HasPrefix(got, "..") {
			t.Errorf("safeName(%q) = %q, which is not inside the directory", name, got)
		}
	}
	for _, name := range []string{"../x", "a/../../x", "/etc/shadow"} {
		if got, err := safeName(name); err == nil {
			t.Errorf("safeName(%q) = %q, want an error", name, got)
		}
	}
}

func writeRawTar(t *testing.T, path string, headers []tar.Header, bodies []string) {
	t.Helper()
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()

	writer := tar.NewWriter(out)
	for i, header := range headers {
		if err := writer.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg && bodies[i] != "" {
			if _, err := writer.Write([]byte(bodies[i])); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}

// A granular restore takes named parts out of an account archive: the DNS
// zones without the home directory, the certificates without the mail.
func TestExtractMembersTakesOnlyWhatWasAskedFor(t *testing.T) {
	root := t.TempDir()
	archive := filepath.Join(root, "cpmove-studio.tar")
	writeTestTar(t, archive, map[string]string{
		"cpmove-studio/dnszones/studio.co.il.db":  "zone",
		"cpmove-studio/dnszones/other.co.il.db":   "zone",
		"cpmove-studio/cp/studio":                 "settings",
		"cpmove-studio/quota":                     "1024",
		"cpmove-studio/homedir/public_html/i.php": "<?php",
		"cpmove-studio/mysql/studio_db.sql":       "insert",
	})

	out := filepath.Join(root, "out")
	written, err := ExtractMembers(archive, out, []string{"dnszones/", "quota"})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if written != 3 {
		t.Errorf("extracted %d files, want 3", written)
	}
	for _, want := range []string{
		"cpmove-studio/dnszones/studio.co.il.db",
		"cpmove-studio/dnszones/other.co.il.db",
		"cpmove-studio/quota",
	} {
		if _, err := os.Stat(filepath.Join(out, want)); err != nil {
			t.Errorf("%s was not extracted", want)
		}
	}
	for _, unwanted := range []string{
		"cpmove-studio/homedir/public_html/i.php",
		"cpmove-studio/cp/studio",
		"cpmove-studio/mysql/studio_db.sql",
	} {
		if _, err := os.Stat(filepath.Join(out, unwanted)); err == nil {
			t.Errorf("%s was extracted but nobody asked for it", unwanted)
		}
	}
}

// Nothing matching is a failed restore, not an empty one.
func TestExtractMembersReportsWhenNothingMatched(t *testing.T) {
	root := t.TempDir()
	archive := filepath.Join(root, "cpmove-studio.tar")
	writeTestTar(t, archive, map[string]string{"cpmove-studio/quota": "1024"})

	written, err := ExtractMembers(archive, filepath.Join(root, "out"), []string{"dnszones/"})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if written != 0 {
		t.Errorf("extracted %d files from an archive with none of them", written)
	}
	if _, err := ExtractMembers(archive, filepath.Join(root, "out2"), nil); err == nil {
		t.Error("extracting no members at all should be refused")
	}
}

// A Maildir is a directory of hard links: Dovecot moves a message
// between folders by linking it under the new name and unlinking the
// old, and restic puts those links back as links when it restores the
// home directory. Packing each name as a copy of its own makes an
// archive as large as the copies, and an account restored from it is
// larger than the account backed up -- against a reservation that
// counted the content once, so the repack can also run the disk out
// halfway through.
func TestALinkedFileIsPackedAsALink(t *testing.T) {
	dir := t.TempDir()
	tree := filepath.Join(dir, "tree")
	if err := os.MkdirAll(filepath.Join(tree, "cur"), 0o700); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(tree, "cur", "1.eml")
	if err := os.WriteFile(first, []byte(strings.Repeat("m", 4096)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(first, filepath.Join(tree, "cur", "2.eml")); err != nil {
		t.Fatal(err)
	}

	archive := filepath.Join(dir, "rebuilt.tar")
	if err := createTar(tree, archive); err != nil {
		t.Fatal(err)
	}

	file, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader := tar.NewReader(file)
	found := false
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name != "cur/2.eml" {
			continue
		}
		found = true
		if header.Typeflag != tar.TypeLink {
			t.Errorf("the second name is typeflag %q, not a link", header.Typeflag)
		}
		if header.Linkname != "cur/1.eml" {
			t.Errorf("the link points at %q", header.Linkname)
		}
		if header.Size != 0 {
			t.Errorf("the link carries %d bytes of its own", header.Size)
		}
	}
	if !found {
		t.Fatal("the second name is not in the rebuilt archive")
	}
}
