package dabackup

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// held is what a directory holds on disk, counting a file with two names
// once.
func held(t *testing.T, dir string) int64 {
	t.Helper()
	seen := map[[2]uint64]bool{}
	var total int64
	err := filepath.WalkDir(dir, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			key := [2]uint64{uint64(stat.Dev), uint64(stat.Ino)}
			if seen[key] {
				return nil
			}
			seen[key] = true
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return total
}

// noise writes a file that does not compress, which is what the large
// accounts are made of.
func noise(t *testing.T, name string, size int) []byte {
	t.Helper()
	body := make([]byte, size)
	if _, err := rand.Read(body); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return body
}

// nestedBody reads one regular member of the home archive inside a
// rebuilt archive.
func nestedBody(t *testing.T, archive, name string) ([]byte, *tar.Header) {
	t.Helper()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		header, err := tr.Next()
		if err != nil {
			return nil, nil
		}
		if !isNestedHomeArchive(path.Clean(header.Name), header.Typeflag) {
			continue
		}
		reader, closer, err := decompressed(tr, header.Name)
		if err != nil {
			t.Fatal(err)
		}
		defer closer()
		inner := tar.NewReader(reader)
		for {
			nested, err := inner.Next()
			if err != nil {
				return nil, nil
			}
			if path.Clean(nested.Name) != name {
				continue
			}
			body, err := io.ReadAll(inner)
			if err != nil {
				t.Fatal(err)
			}
			return body, nested
		}
	}
}

// A restore on DirectAdmin was given room for three copies of the
// account, because three were on the disk at once: the tree restic
// restored, the home archive built from it, and the account archive that
// home archive is copied into. On 182.54.236.10 that refused the restore
// of an account of 17 GiB with 42.7 free.
//
// The files that go into the home archive are taken out of the tree as
// they go, and the home archive is removed once it is in the outer one.
// What is measured here is the most the work directory holds at any
// point of the rebuild, for an account that does not compress and keeps
// a file under two names.
func TestARebuildHoldsTwoCopiesOfTheAccountAtTheMost(t *testing.T) {
	const account = "gzv0908a"
	work := t.TempDir()
	dir := filepath.Join(work, "tree")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	archive := writeLeanArchive(t, account, nil)
	if err := (Layout{}).UnpackLeanArchive(context.Background(), archive, account, dir); err != nil {
		t.Fatal(err)
	}
	homeTree(t, dir)
	home := HomedirPart(dir)
	video := path.Join(DomainsDir, fixtureDomain, "public_html", "video.bin")
	site := noise(t, filepath.Join(home, video), 2<<20)
	noise(t, filepath.Join(home, ".cache", "one.bin"), 3<<20)
	kept := noise(t, filepath.Join(home, ".cache", "two.bin"), 3<<20)
	if err := os.Link(filepath.Join(home, ".cache", "one.bin"),
		filepath.Join(home, ".cache", "one.again")); err != nil {
		t.Fatal(err)
	}

	whole := held(t, work)
	var most int64
	afterMember = func() { most = max(most, held(t, work)) }
	t.Cleanup(func() { afterMember = func() {} })

	rebuilt, err := (Layout{}).PackArchive(context.Background(), dir, account, work)
	if err != nil {
		t.Fatal(err)
	}
	if most == 0 {
		t.Fatal("nothing was measured")
	}
	// Tar's own headers and padding, which are a block or two a member.
	const headers = 256 << 10
	if most > 2*whole+headers {
		t.Errorf("the rebuild held %d bytes of an account of %d: %.2f copies",
			most, whole, float64(most)/float64(whole))
	}

	// What the archive carries is what the tree held.
	if body, _ := memberBody(t, rebuilt, video); !bytes.Equal(body, site) {
		t.Errorf("%s did not come back as it was", video)
	}
	if body, _ := nestedBody(t, rebuilt, ".cache/two.bin"); !bytes.Equal(body, kept) {
		t.Error(".cache/two.bin did not come back as it was")
	}
	_, nested := headersOf(t, rebuilt)
	first, second := nested[".cache/one.again"], nested[".cache/one.bin"]
	if first == nil || second == nil || first.Typeflag != tar.TypeReg ||
		second.Typeflag != tar.TypeLink || first.Size != 3<<20 {
		t.Errorf("the file with two names is not carried once and linked: %v %v", first, second)
	}

	// And what is left behind is what something still reads: the records
	// the databases are named from, and not the account's files again.
	for _, gone := range []string{".cache/one.bin", ".cache/one.again", ".cache/two.bin", ".bashrc"} {
		if _, err := os.Lstat(filepath.Join(home, gone)); !os.IsNotExist(err) {
			t.Errorf("%s is still in the tree after it went into the archive: %v", gone, err)
		}
	}
	if _, err := os.Stat(filepath.Join(MetadataPart(dir), BackupDir, UserConf)); err != nil {
		t.Errorf("the account's records were removed with its files: %v", err)
	}
	entries, err := os.ReadDir(work)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".gniza-home-") {
			t.Errorf("the home archive was left beside the one it went into: %s", entry.Name())
		}
	}
}
