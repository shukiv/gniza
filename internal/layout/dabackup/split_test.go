package dabackup

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// A DirectAdmin archive is one compressed tar with a second compressed
// tar inside it, and the entries in both carry owners and groups as
// names -- gzv0908a/apache on .php/, gzv0908a/mail on Maildir/, observed
// on 1.709. Restic cannot deduplicate either archive, so split mode has
// to take them apart; what comes back has to be the same archive, header
// for header, or the account restored from it has a mail directory
// Dovecot cannot write.
func TestAnArchiveComesBackTheSameAfterItIsTakenApart(t *testing.T) {
	account := "gzv0908a"
	original := buildSplitFixture(t, account)
	dir := t.TempDir()

	if err := (Layout{}).UnpackArchive(context.Background(), original, account, dir); err != nil {
		t.Fatalf("taking the archive apart: %v", err)
	}
	rebuilt := filepath.Join(t.TempDir(), filepath.Base(original))
	if err := (Layout{}).PackArchive(context.Background(), dir, account, rebuilt); err != nil {
		t.Fatalf("putting the archive back together: %v", err)
	}
	sameArchive(t, original, rebuilt)
}

// The home directory arrives in three places -- domains/, imap/ and the
// nested archive -- and they are three views of one directory. Split
// mode is only worth running if what restic sees is that directory, at
// paths that are the same tonight as they were last night.
func TestTheThreePlacesTheHomeDirectoryArrivesInBecomeOne(t *testing.T) {
	account := "gzv0908a"
	dir := t.TempDir()
	if err := (Layout{}).UnpackArchive(context.Background(),
		buildSplitFixture(t, account), account, dir); err != nil {
		t.Fatalf("taking the archive apart: %v", err)
	}
	for _, want := range []string{
		"home/domains/gzv0908a.gniza-test.invalid/public_html/index.html",
		"home/imap/gzv0908a.gniza-test.invalid/sales/Maildir/cur/1.eml",
		"home/.bashrc",
		"home/.php/php-mail.log",
		"backup/user.conf",
	} {
		if _, err := os.Stat(filepath.Join(dir, TreeDirName, want)); err != nil {
			t.Errorf("%s is not in the unpacked tree: %v", want, err)
		}
	}
	// And the archive inside the archive is not left lying in it as a
	// file, because a compressed blob is the thing split mode exists to
	// get rid of.
	if _, err := os.Stat(filepath.Join(dir, TreeDirName, BackupDir, NestedHomeArchive)); err == nil {
		t.Error("the nested archive was stored rather than opened")
	}
}

// The archive is what another machine's DirectAdmin produced, and it is
// taken apart as root. A member that climbs out of the tree is refused
// wherever it is -- including inside the nested archive, which is the
// one an account's own files reach directly.
func TestAMemberThatClimbsOutIsRefused(t *testing.T) {
	for _, test := range []struct {
		name   string
		member string
		nested bool
	}{
		{"a parent directory", "../etc/cron.d/x", false},
		{"an absolute path", "/etc/cron.d/x", false},
		{"a parent directory inside the nested archive", "../../etc/cron.d/x", true},
		{"an absolute path inside the nested archive", "/root/.ssh/authorized_keys", true},
		{"a parent hidden by cleaning", "backup/../../etc/cron.d/x", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			account := "gzv0908a"
			archive := buildSplitFixture(t, account, fixtureExtra{
				name: test.member, body: "root ALL=(ALL) NOPASSWD: ALL\n", nested: test.nested,
			})
			err := (Layout{}).UnpackArchive(context.Background(), archive, account, t.TempDir())
			if err == nil {
				t.Fatalf("%q was unpacked", test.member)
			}
			if !strings.Contains(err.Error(), "unsafe") {
				t.Errorf("refused, but not as unsafe: %v", err)
			}
		})
	}
}

// The nested archive holds what is not under domains/ or imap/, which is
// how one real archive was measured. That is an observation about one
// version, not a promise: if the two ever name the same file, both
// bodies have to survive, because the repack writes both entries back.
func TestTwoMembersThatWantTheSameFileBothSurvive(t *testing.T) {
	account := "gzv0908a"
	// The outer archive puts a file under domains/, and the nested one
	// puts a different file at the same place in the home directory.
	original := buildSplitFixture(t, account, fixtureExtra{
		name: "domains/collision.txt", body: "from the outer archive\n",
	}, fixtureExtra{
		name: "domains/collision.txt", body: "from the nested archive\n", nested: true,
	})
	dir := t.TempDir()
	if err := (Layout{}).UnpackArchive(context.Background(), original, account, dir); err != nil {
		t.Fatalf("taking the archive apart: %v", err)
	}
	rebuilt := filepath.Join(t.TempDir(), filepath.Base(original))
	if err := (Layout{}).PackArchive(context.Background(), dir, account, rebuilt); err != nil {
		t.Fatalf("putting the archive back together: %v", err)
	}
	sameArchive(t, original, rebuilt)
}

// A host configured for gzip writes home.tar.gz, and an account with
// nothing outside its domains has no nested archive at all. Neither is a
// different format.
func TestAnArchiveWithNoNestedHomeStillComesBack(t *testing.T) {
	account := "gzv0908a"
	original := buildSplitFixture(t, account, fixtureExtra{noNestedHome: true})
	dir := t.TempDir()
	if err := (Layout{}).UnpackArchive(context.Background(), original, account, dir); err != nil {
		t.Fatalf("taking the archive apart: %v", err)
	}
	rebuilt := filepath.Join(t.TempDir(), filepath.Base(original))
	if err := (Layout{}).PackArchive(context.Background(), dir, account, rebuilt); err != nil {
		t.Fatalf("putting the archive back together: %v", err)
	}
	sameArchive(t, original, rebuilt)
}

// The manifest is the whole of what the headers said. A tree without one
// cannot be repacked into anything DirectAdmin would restore, and
// guessing the headers back from the files on disk is exactly the
// flattening that loses gzv0908a/mail on Maildir.
func TestATreeWithNoManifestIsNotRepacked(t *testing.T) {
	account := "gzv0908a"
	dir := t.TempDir()
	if err := (Layout{}).UnpackArchive(context.Background(),
		buildSplitFixture(t, account), account, dir); err != nil {
		t.Fatalf("taking the archive apart: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, ManifestFile)); err != nil {
		t.Fatal(err)
	}
	err := (Layout{}).PackArchive(context.Background(), dir,
		account, filepath.Join(t.TempDir(), "user.admin."+account+".tar.zst"))
	if err == nil {
		t.Fatal("a tree with no manifest was repacked")
	}
}

// A file that is not the size its header said is a tree that was damaged
// between the backup and the restore. Writing it anyway produces a tar
// whose headers and bodies disagree, which tar reads as a corrupt
// archive several members later.
func TestABodyThatIsNotItsRecordedSizeIsRefused(t *testing.T) {
	account := "gzv0908a"
	dir := t.TempDir()
	if err := (Layout{}).UnpackArchive(context.Background(),
		buildSplitFixture(t, account), account, dir); err != nil {
		t.Fatalf("taking the archive apart: %v", err)
	}
	victim := filepath.Join(dir, TreeDirName, "home", ".bashrc")
	if err := os.WriteFile(victim, []byte("truncated"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := (Layout{}).PackArchive(context.Background(), dir,
		account, filepath.Join(t.TempDir(), "user.admin."+account+".tar.zst"))
	if err == nil {
		t.Fatal("a body that changed size was packed")
	}
	if !strings.Contains(err.Error(), ".bashrc") {
		t.Errorf("the refusal does not name the file: %v", err)
	}
}

// What the manifest is for, said as a test rather than as a comment: the
// owner and group names DirectAdmin wrote are in it, per member.
func TestTheManifestKeepsTheOwnerAndGroupOfEveryMember(t *testing.T) {
	account := "gzv0908a"
	dir := t.TempDir()
	if err := (Layout{}).UnpackArchive(context.Background(),
		buildSplitFixture(t, account), account, dir); err != nil {
		t.Fatalf("taking the archive apart: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("the manifest is not readable: %v", err)
	}
	if manifest.Account != account {
		t.Errorf("the manifest is for %q", manifest.Account)
	}
	groups := map[string]string{}
	for _, member := range append(manifest.Outer.Members, manifest.Home.Members...) {
		groups[path.Clean(member.Name)] = member.Gname
	}
	for name, want := range map[string]string{
		".php":    "apache",
		"Maildir": "mail",
	} {
		if got := groups[name]; got != want {
			t.Errorf("%s is owned by group %q, not %q", name, got, want)
		}
	}
}

// fixtureExtra is one more member put into a fixture archive, or the one
// switch that leaves the nested archive out of it.
type fixtureExtra struct {
	name         string
	body         string
	nested       bool
	noNestedHome bool
}

// buildSplitFixture writes an archive shaped like the one DirectAdmin
// 1.709 wrote for gzv0908a: a zstd tar whose three roots are backup/,
// domains/ and imap/, with the rest of the home directory in a second
// zstd tar at backup/home.tar.zst. The variety is the point -- owners
// that are not the account's own group, a directory mode that is not
// 0755, a symlink, a hard link, a dotfile, and an mtime with nanoseconds
// on it -- because each of those is a header field a flattening rebuild
// would quietly drop.
func buildSplitFixture(t *testing.T, account string, extras ...fixtureExtra) string {
	t.Helper()
	when := time.Date(2026, 9, 8, 11, 22, 33, 0, time.UTC)
	precise := time.Date(2026, 9, 8, 11, 22, 33, 456789123, time.UTC)
	domain := account + ".gniza-test.invalid"

	noNested := false
	for _, extra := range extras {
		if extra.noNestedHome {
			noNested = true
		}
	}

	nested := packTar(t, func(tw *tar.Writer) {
		writeMember(t, tw, &tar.Header{
			Name: "./", Typeflag: tar.TypeDir, Mode: 0o711,
			Uid: 1005, Gid: 1005, Uname: account, Gname: account, ModTime: when,
		}, "")
		writeMember(t, tw, &tar.Header{
			Name: ".bashrc", Typeflag: tar.TypeReg, Mode: 0o644,
			Uid: 1005, Gid: 1005, Uname: account, Gname: account, ModTime: precise,
		}, "# .bashrc\nexport PATH\n")
		writeMember(t, tw, &tar.Header{
			Name: ".php/", Typeflag: tar.TypeDir, Mode: 0o770,
			Uid: 1005, Gid: 48, Uname: account, Gname: "apache", ModTime: when,
		}, "")
		writeMember(t, tw, &tar.Header{
			Name: ".php/php-mail.log", Typeflag: tar.TypeReg, Mode: 0o660,
			Uid: 1005, Gid: 48, Uname: account, Gname: "apache", ModTime: when,
		}, "mail log\n")
		writeMember(t, tw, &tar.Header{
			Name: "Maildir/", Typeflag: tar.TypeDir, Mode: 0o770,
			Uid: 1005, Gid: 12, Uname: account, Gname: "mail", ModTime: when,
		}, "")
		writeMember(t, tw, &tar.Header{
			Name: ".bash_profile", Typeflag: tar.TypeSymlink, Mode: 0o777,
			Uid: 1005, Gid: 1005, Uname: account, Gname: account,
			ModTime: when, Linkname: ".bashrc",
		}, "")
		writeMember(t, tw, &tar.Header{
			Name: ".bash_logout", Typeflag: tar.TypeLink, Mode: 0o644,
			Uid: 1005, Gid: 1005, Uname: account, Gname: account,
			ModTime: when, Linkname: ".bashrc",
		}, "")
		for _, extra := range extras {
			if extra.nested {
				writeMember(t, tw, &tar.Header{
					Name: extra.name, Typeflag: tar.TypeReg, Mode: 0o644,
					Uid: 1005, Gid: 1005, Uname: account, Gname: account, ModTime: when,
				}, extra.body)
			}
		}
	})
	nested = compressZstd(t, nested)

	file := filepath.Join(t.TempDir(), "user.admin."+account+".tar.zst")
	plain := packTar(t, func(tw *tar.Writer) {
		writeMember(t, tw, &tar.Header{
			Name: BackupDir + "/", Typeflag: tar.TypeDir, Mode: 0o700,
			Uid: 0, Gid: 0, Uname: "root", Gname: "root", ModTime: when,
		}, "")
		writeMember(t, tw, &tar.Header{
			Name: path.Join(BackupDir, UserConf), Typeflag: tar.TypeReg, Mode: 0o600,
			Uid: 0, Gid: 0, Uname: "root", Gname: "root", ModTime: when,
		}, "username="+account+"\n")
		writeMember(t, tw, &tar.Header{
			Name: path.Join(BackupDir, "gzv0908a_shop.sql"), Typeflag: tar.TypeReg, Mode: 0o600,
			Uid: 0, Gid: 0, Uname: "root", Gname: "root", ModTime: when,
		}, "CREATE TABLE orders (id int);\n")
		if !noNested {
			writeMember(t, tw, &tar.Header{
				Name: path.Join(BackupDir, NestedHomeArchive), Typeflag: tar.TypeReg,
				Mode: 0o640, Uid: 1005, Gid: 1005, Uname: account, Gname: account,
				ModTime: when,
			}, string(nested))
		}
		writeMember(t, tw, &tar.Header{
			Name: DomainsDir + "/", Typeflag: tar.TypeDir, Mode: 0o755,
			Uid: 1005, Gid: 1005, Uname: account, Gname: account, ModTime: when,
		}, "")
		writeMember(t, tw, &tar.Header{
			Name:     path.Join(DomainsDir, domain, "public_html", "index.html"),
			Typeflag: tar.TypeReg, Mode: 0o644,
			Uid: 1005, Gid: 48, Uname: account, Gname: "apache", ModTime: when,
		}, "<html>hello</html>\n")
		writeMember(t, tw, &tar.Header{
			Name:     path.Join(MailDir, domain, "sales", "Maildir", "cur", "1.eml"),
			Typeflag: tar.TypeReg, Mode: 0o600,
			Uid: 1005, Gid: 12, Uname: account, Gname: "mail", ModTime: when,
		}, "Subject: hello\n\nhello\n")
		for _, extra := range extras {
			if !extra.nested && extra.name != "" {
				writeMember(t, tw, &tar.Header{
					Name: extra.name, Typeflag: tar.TypeReg, Mode: 0o644,
					Uid: 1005, Gid: 1005, Uname: account, Gname: account, ModTime: when,
				}, extra.body)
			}
		}
	})
	if err := os.WriteFile(file, compressZstd(t, plain), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func writeMember(t *testing.T, tw *tar.Writer, header *tar.Header, body string) {
	t.Helper()
	header.Size = int64(len(body))
	if header.Typeflag != tar.TypeReg {
		header.Size = 0
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

func packTar(t *testing.T, members func(*tar.Writer)) []byte {
	t.Helper()
	var buffer strings.Builder
	tw := tar.NewWriter(&buffer)
	members(tw)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return []byte(buffer.String())
}

func compressZstd(t *testing.T, body []byte) []byte {
	t.Helper()
	var out strings.Builder
	writer, err := zstd.NewWriter(&out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return []byte(out.String())
}

// snapshot is one archive member as everything that has to survive a
// round trip, and nothing that does not. The tar encoding itself is not
// in it: DirectAdmin's restore runs tar, which reads any of them, and a
// recompressed nested archive is a different length by definition.
type snapshot struct {
	archive  string
	name     string
	typeflag byte
	mode     int64
	uid, gid int
	uname    string
	gname    string
	modTime  time.Time
	size     int64
	linkname string
	body     string
}

// sameArchive holds a rebuilt archive to the original, member by member,
// through both tars.
func sameArchive(t *testing.T, want, got string) {
	t.Helper()
	wantMembers := readSnapshots(t, want)
	gotMembers := readSnapshots(t, got)
	if len(wantMembers) != len(gotMembers) {
		t.Fatalf("the archive has %d members, the original had %d:\n%s\n%s",
			len(gotMembers), len(wantMembers), listNames(gotMembers), listNames(wantMembers))
	}
	for i := range wantMembers {
		if wantMembers[i] != gotMembers[i] {
			t.Errorf("member %d came back different:\n want %+v\n  got %+v",
				i, wantMembers[i], gotMembers[i])
		}
	}
}

func listNames(members []snapshot) string {
	var names []string
	for _, member := range members {
		names = append(names, member.archive+":"+member.name)
	}
	return strings.Join(names, "\n")
}

func readSnapshots(t *testing.T, filename string) []snapshot {
	t.Helper()
	body, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	return readSnapshotsFrom(t, "outer", decompress(t, filename, body))
}

func readSnapshotsFrom(t *testing.T, archive string, body []byte) []snapshot {
	t.Helper()
	var members []snapshot
	tr := tar.NewReader(strings.NewReader(string(body)))
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return members
		}
		if err != nil {
			t.Fatalf("reading the %s archive: %v", archive, err)
		}
		payload, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		nestedHome := archive == "outer" &&
			strings.HasPrefix(path.Base(path.Clean(header.Name)), "home.tar")
		member := snapshot{
			archive: archive, name: header.Name, typeflag: header.Typeflag,
			mode: header.Mode, uid: header.Uid, gid: header.Gid,
			uname: header.Uname, gname: header.Gname,
			modTime: header.ModTime.UTC(), size: header.Size,
			linkname: header.Linkname, body: digest(payload),
		}
		if nestedHome {
			// Compressing the same bytes twice does not produce the same
			// file, so the nested archive is compared through rather than
			// as a body.
			member.size, member.body = -1, ""
		}
		members = append(members, member)
		if nestedHome {
			members = append(members,
				readSnapshotsFrom(t, "home", decompress(t, header.Name, payload))...)
		}
	}
}

func decompress(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	if !strings.HasSuffix(name, ".zst") {
		return body
	}
	reader, err := zstd.NewReader(strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	plain, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return plain
}

func digest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
