package reassemble

import (
	"archive/tar"
	"context"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/shukiv/gniza/internal/layout/cpmove"
	"github.com/shukiv/gniza/internal/layout/dabackup"
	"github.com/shukiv/gniza/internal/panel"
	"github.com/shukiv/gniza/internal/pkgacct"
	"github.com/shukiv/gniza/internal/resticrun"
)

// A DirectAdmin backup is the account's own archive taken apart, because
// restic cannot deduplicate a compressed one. Putting it back is not the
// cpmove rebuild with different directory names: that walks the tree and
// tars what it finds, which writes whatever the restoring server's own
// group file says and restores an account whose mail directory Dovecot
// cannot write. It has to be the panel's own repack.
func TestADirectAdminSnapshotIsPutBackIntoTheArchiveItsRestoreReads(t *testing.T) {
	const account = "gzv0908a"
	restorer, root, name := buildPackedSnapshot(t, account)

	result, err := Run(context.Background(), restorer, Request{
		Layout: dabackup.Layout{}, Account: account,
		SnapshotID: "40dc15203b1cf9aa", WorkDir: filepath.Join(root, "work"),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Mode != pkgacct.ModeSplit {
		t.Errorf("the rebuild reports mode %q", result.Mode)
	}
	// The archive has to come back under the name DirectAdmin gave it:
	// its own restore reads the account out of the filename before it
	// reads anything inside.
	if got := filepath.Base(result.ArchivePath); got != name {
		t.Errorf("the rebuilt archive is called %s, DirectAdmin called it %s", got, name)
	}
	// And the members are the ones that went in, owners and all. The
	// group on .php/ is not the account's own, which is the whole reason
	// the headers are carried rather than rebuilt.
	members := packedMembers(t, result.ArchivePath)
	for name, want := range map[string]string{
		"backup/user.conf": "root",
		".php/":            "apache",
	} {
		if members[name] != want {
			t.Errorf("%s came back owned by group %q, not %q", name, members[name], want)
		}
	}

	// A rehearsal of this reads the archive, because that is what the
	// panel's restore reads. Walking the staged tree would check Gniza's
	// own copy instead.
	passed, err := Verify(context.Background(), result)
	if err != nil {
		t.Fatalf("the rebuilt archive did not verify: %v", err)
	}
	if !strings.Contains(strings.Join(passed, "; "), "database dump parses") {
		t.Errorf("the rehearsal did not read inside the archive: %v", passed)
	}
}

// The tar is skipped in a rehearsal where the panel's restore takes a
// directory as readily as an archive, because it is a second full copy of
// the account on the same disk. DirectAdmin's restore takes an archive
// only, so here it is not an extra -- it is the thing being rehearsed.
func TestARehearsalOfADirectAdminSnapshotStillBuildsTheArchive(t *testing.T) {
	const account = "gzv0908a"
	restorer, root, _ := buildPackedSnapshot(t, account)

	result, err := Run(context.Background(), restorer, Request{
		Layout: dabackup.Layout{}, Account: account,
		SnapshotID: "40dc15203b1cf9aa", WorkDir: filepath.Join(root, "work"),
		TreeOnly: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ArchivePath == "" {
		t.Fatal("a rehearsal produced no archive, and this panel's restore reads nothing else")
	}
	if _, err := os.Stat(result.ArchivePath); err != nil {
		t.Errorf("the archive it reported is not there: %v", err)
	}
}

// A snapshot with a part this shape of backup does not have is one this
// version did not write. Rebuilding it while quietly leaving that part
// out is a restore that is missing whatever was in it.
func TestADirectAdminSnapshotWithAPartItDoesNotHaveIsRefused(t *testing.T) {
	const account = "gzv0908a"
	restorer, root, _ := buildPackedSnapshot(t, account)
	restorer.snapshot.Paths = append(restorer.snapshot.Paths,
		"/var/lib/gniza/staging/"+account+"/databases")
	restorer.source["/var/lib/gniza/staging/"+account+"/databases"] = t.TempDir()

	_, err := Run(context.Background(), restorer, Request{
		Layout: dabackup.Layout{}, Account: account,
		SnapshotID: "40dc15203b1cf9aa", WorkDir: filepath.Join(root, "work"),
	})
	if err == nil {
		t.Fatal("a snapshot with a separate database part was rebuilt anyway")
	}
	if !strings.Contains(err.Error(), "database") {
		t.Errorf("refused, but not for that: %v", err)
	}
}

// cPanel is asked for the parts and produces them, so its layout must not
// answer to this: a cpmove rebuild that went down the repack path would
// hand restorepkg an archive built by a panel that never wrote one.
func TestOnlyThePanelThatCannotProduceThePartsRepacksThem(t *testing.T) {
	if _, packs := any(cpmove.Layout{}).(panel.ArchivePacker); packs {
		t.Error("cpmove offers to repack an archive cPanel produces itself")
	}
	if _, packs := any(dabackup.Layout{}).(panel.ArchivePacker); !packs {
		t.Error("dabackup cannot put back the archive it takes apart")
	}
}

// buildPackedSnapshot writes a DirectAdmin account archive, takes it
// apart the way a backup does, and serves the two parts as a snapshot. It
// reports where the work can go and what DirectAdmin called the archive.
func buildPackedSnapshot(t *testing.T, account string) (*fakeRestorer, string, string) {
	t.Helper()
	root := t.TempDir()
	name := "user.admin." + account + ".tar.zst"
	archive := writeDirectAdminArchive(t, filepath.Join(root, "native"), account, name)

	staged := filepath.Join(root, "staged")
	if err := (dabackup.Layout{}).UnpackArchive(context.Background(), archive, account, staged); err != nil {
		t.Fatalf("staging the account in parts: %v", err)
	}
	metadata := "/var/lib/gniza/staging/" + account + "/metadata"
	homedir := "/var/lib/gniza/staging/" + account + "/home"
	return &fakeRestorer{
		snapshot: resticrun.Snapshot{
			ID: "40dc15203b1cf9aa", ShortID: "40dc1520",
			Tags:  []string{"account:" + account, "mode:split"},
			Paths: []string{metadata, homedir},
		},
		source: map[string]string{
			metadata: dabackup.MetadataPart(staged),
			homedir:  dabackup.HomedirPart(staged),
		},
	}, root, name
}

// writeDirectAdminArchive builds the shape a real 1.709 archive has: an
// outer zstd tar with backup/ and domains/ in it, and the rest of the
// home directory in a second zstd tar inside it.
func writeDirectAdminArchive(t *testing.T, dir, account, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 9, 8, 5, 41, 0, 0, time.UTC)
	member := func(tw *tar.Writer, name string, flag byte, gname, body string) {
		t.Helper()
		header := &tar.Header{
			Name: name, Typeflag: flag, Mode: 0o640, ModTime: when,
			Uid: 1005, Gid: 1005, Uname: account, Gname: gname,
		}
		if flag == tar.TypeReg {
			header.Size = int64(len(body))
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			if _, err := io.WriteString(tw, body); err != nil {
				t.Fatal(err)
			}
		}
	}
	pack := func(members func(*tar.Writer)) []byte {
		t.Helper()
		var out strings.Builder
		writer, err := zstd.NewWriter(&out)
		if err != nil {
			t.Fatal(err)
		}
		tw := tar.NewWriter(writer)
		members(tw)
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		return []byte(out.String())
	}

	nested := pack(func(tw *tar.Writer) {
		member(tw, ".bashrc", tar.TypeReg, account, "# .bashrc\n")
		member(tw, ".php/", tar.TypeDir, "apache", "")
	})
	body := pack(func(tw *tar.Writer) {
		member(tw, path.Join(dabackup.BackupDir, dabackup.UserConf), tar.TypeReg, "root",
			"username="+account+"\n")
		member(tw, path.Join(dabackup.BackupDir, account+"_shop.sql"), tar.TypeReg, "root",
			"CREATE TABLE orders (id int);\n")
		member(tw, path.Join(dabackup.BackupDir, dabackup.NestedHomeArchive), tar.TypeReg,
			account, string(nested))
		member(tw, path.Join(dabackup.DomainsDir, account+".gniza-test.invalid",
			"public_html", "index.html"), tar.TypeReg, "apache", "<html>hello</html>\n")
	})
	file := filepath.Join(dir, name)
	if err := os.WriteFile(file, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

// packedMembers reads an archive back as member name to owning group,
// through the nested archive as well as the outer one.
func packedMembers(t *testing.T, filename string) map[string]string {
	t.Helper()
	body, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	members := map[string]string{}
	var read func(compressed []byte)
	read = func(compressed []byte) {
		reader, err := zstd.NewReader(strings.NewReader(string(compressed)))
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		tr := tar.NewReader(reader)
		for {
			header, err := tr.Next()
			if err == io.EOF {
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			payload, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			members[header.Name] = header.Gname
			if strings.HasPrefix(path.Base(header.Name), "home.tar") {
				read(payload)
			}
		}
	}
	read(body)
	return members
}
