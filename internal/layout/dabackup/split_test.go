package dabackup

import (
	"archive/tar"
	"compress/gzip"
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
		"metadata/backup/user.conf",
	} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("%s is not in the unpacked tree: %v", want, err)
		}
	}
	// And the archive inside the archive is not left lying in it as a
	// file, because a compressed blob is the thing split mode exists to
	// get rid of.
	if _, err := os.Stat(filepath.Join(dir, MetadataTreeDir, BackupDir, NestedHomeArchive)); err == nil {
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

// An account with nothing outside its domains has no nested archive at
// all. That is the same format with one member missing, not a different
// one.
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

// A host DirectAdmin is configured for gzip on writes user.admin.x.tar.gz
// with home.tar.gz inside it. Nothing about the shape changes, and the
// archive has to go back the way it came: a host that reads gz and writes
// zst hands its own restore an archive its tar was not asked for.
func TestAGzipHostsArchiveComesBackTheSame(t *testing.T) {
	account := "gzv0908a"
	original := buildSplitFixture(t, account, fixtureExtra{compression: "gz"})
	if !strings.HasSuffix(original, ".tar.gz") {
		t.Fatalf("the fixture is %s", original)
	}
	dir := t.TempDir()
	if err := (Layout{}).UnpackArchive(context.Background(), original, account, dir); err != nil {
		t.Fatalf("taking the archive apart: %v", err)
	}
	// The nested archive is found by name, and on this host its name is
	// not the one the constant spells.
	if _, err := os.Stat(filepath.Join(HomedirPart(dir), ".bashrc")); err != nil {
		t.Errorf("the gzip nested archive was not opened: %v", err)
	}
	rebuilt := filepath.Join(t.TempDir(), filepath.Base(original))
	if err := (Layout{}).PackArchive(context.Background(), dir, account, rebuilt); err != nil {
		t.Fatalf("putting the archive back together: %v", err)
	}
	sameArchive(t, original, rebuilt)
}

// A host that compresses nothing writes user.admin.x.tar with home.tar
// inside it, and that is the same archive without the compression around
// it.
func TestAnUncompressedArchiveComesBackTheSame(t *testing.T) {
	account := "gzv0908a"
	original := buildSplitFixture(t, account, fixtureExtra{compression: "none"})
	if !strings.HasSuffix(original, ".tar") {
		t.Fatalf("the fixture is %s", original)
	}
	dir := t.TempDir()
	if err := (Layout{}).UnpackArchive(context.Background(), original, account, dir); err != nil {
		t.Fatalf("taking the archive apart: %v", err)
	}
	if _, err := os.Stat(filepath.Join(HomedirPart(dir), ".bashrc")); err != nil {
		t.Errorf("the uncompressed nested archive was not opened: %v", err)
	}
	rebuilt := filepath.Join(t.TempDir(), filepath.Base(original))
	if err := (Layout{}).PackArchive(context.Background(), dir, account, rebuilt); err != nil {
		t.Fatalf("putting the archive back together: %v", err)
	}
	sameArchive(t, original, rebuilt)
}

// An extended attribute is where an ACL lives, and losing one restores a
// file the account cannot read. tar keeps them as pax records, and they
// are the one kind of pax record the manifest carries: everything else
// Go's tar writer works out again from the fields beside it.
func TestTheExtendedAttributesOfAMemberSurvive(t *testing.T) {
	account := "gzv0908a"
	const key, value = "SCHILY.xattr.user.gniza", "kept"
	original := buildSplitFixture(t, account, fixtureExtra{
		name: "domains/marked.txt", body: "marked\n",
		xattrs: map[string]string{key: value},
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

	body, err := os.ReadFile(rebuilt)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(strings.NewReader(string(decompress(t, rebuilt, body))))
	found := false
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == "domains/marked.txt" {
			found = true
			if header.PAXRecords[key] != value {
				t.Errorf("the extended attribute came back as %q", header.PAXRecords[key])
			}
		}
	}
	if !found {
		t.Error("the member carrying the extended attribute is not in the rebuilt archive")
	}
}

// The manifest and the tree share a directory with the account's own
// files, so an archive carrying a member named like one of Gniza's is
// refused rather than allowed to overwrite it.
func TestAMemberNamedLikeGnizasOwnFilesIsRefused(t *testing.T) {
	account := "gzv0908a"
	original := buildSplitFixture(t, account, fixtureExtra{
		name: ManifestFile, body: `{"version":1,"account":"somebody-else"}`,
	})
	err := (Layout{}).UnpackArchive(context.Background(), original, account, t.TempDir())
	if err == nil {
		t.Fatal("a member named like Gniza's manifest was unpacked")
	}
	if !strings.Contains(err.Error(), ManifestFile) {
		t.Errorf("the refusal does not name the member: %v", err)
	}
}

// One archive has one home archive in it. A second is an archive this
// does not understand, and guessing which of the two is the home
// directory is guessing what an account gets restored from.
func TestASecondNestedHomeArchiveIsRefused(t *testing.T) {
	account := "gzv0908a"
	original := buildSplitFixture(t, account, fixtureExtra{
		name: path.Join(BackupDir, NestedHomeArchive), body: "another one\n",
	})
	err := (Layout{}).UnpackArchive(context.Background(), original, account, t.TempDir())
	if err == nil {
		t.Fatal("an archive with two nested home archives was unpacked")
	}
	if !strings.Contains(err.Error(), "two nested home archives") {
		t.Errorf("refused, but not for that: %v", err)
	}
}

// The tree says whose account it came from, and it is checked, for the
// same reason the archive's own identity record is: restoring one
// customer's data into another's account is the worst thing this program
// could do.
func TestATreeFromAnotherAccountIsNotRepacked(t *testing.T) {
	account := "gzv0908a"
	dir := t.TempDir()
	if err := (Layout{}).UnpackArchive(context.Background(),
		buildSplitFixture(t, account), account, dir); err != nil {
		t.Fatalf("taking the archive apart: %v", err)
	}
	err := (Layout{}).PackArchive(context.Background(), dir, "someone-else",
		filepath.Join(t.TempDir(), "user.admin.someone-else.tar.zst"))
	if err == nil {
		t.Fatal("a tree was repacked into another account's archive")
	}
	if !strings.Contains(err.Error(), account) {
		t.Errorf("the refusal does not say whose tree it is: %v", err)
	}
}

// A tree written by a later Gniza is refused rather than read as though
// its manifest meant what this version means by it.
func TestATreeFromAnotherVersionIsRefused(t *testing.T) {
	account := "gzv0908a"
	dir := t.TempDir()
	if err := (Layout{}).UnpackArchive(context.Background(),
		buildSplitFixture(t, account), account, dir); err != nil {
		t.Fatalf("taking the archive apart: %v", err)
	}
	manifest := filepath.Join(MetadataPart(dir), ManifestFile)
	body, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(body), `"version": 1`, `"version": 2`, 1)
	if changed == string(body) {
		t.Fatal("the manifest does not say which version wrote it")
	}
	if err := os.WriteFile(manifest, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (Layout{}).PackArchive(context.Background(), dir, account,
		filepath.Join(t.TempDir(), "user.admin."+account+".tar.zst")); err == nil {
		t.Fatal("a tree from another version of Gniza was repacked")
	}
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
	if err := os.Remove(filepath.Join(MetadataPart(dir), ManifestFile)); err != nil {
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
	victim := filepath.Join(HomedirPart(dir), ".bashrc")
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
	body, err := os.ReadFile(filepath.Join(MetadataPart(dir), ManifestFile))
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

// fixtureExtra is one more member put into a fixture archive, or one of
// the switches that changes the archive itself.
type fixtureExtra struct {
	name         string
	body         string
	nested       bool
	noNestedHome bool
	// compression is what the host is configured for: zst unless this
	// says otherwise, and "none" for a host that compresses nothing.
	compression string
	// xattrs are put on the member as pax records, which is where tar
	// keeps an extended attribute.
	xattrs map[string]string
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
	compression := "zst"
	for _, extra := range extras {
		if extra.noNestedHome {
			noNested = true
		}
		if extra.compression != "" {
			compression = extra.compression
		}
	}
	suffix := "." + compression
	if compression == "none" {
		compression, suffix = "", ""
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
			if extra.nested && extra.name != "" {
				writeMember(t, tw, &tar.Header{
					Name: extra.name, Typeflag: tar.TypeReg, Mode: 0o644,
					Uid: 1005, Gid: 1005, Uname: account, Gname: account, ModTime: when,
					PAXRecords: extra.xattrs,
				}, extra.body)
			}
		}
	})
	nested = compress(t, nested, compression)

	file := filepath.Join(t.TempDir(), "user.admin."+account+".tar"+suffix)
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
				Name: path.Join(BackupDir, "home.tar"+suffix), Typeflag: tar.TypeReg,
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
					PAXRecords: extra.xattrs,
				}, extra.body)
			}
		}
	})
	if err := os.WriteFile(file, compress(t, plain, compression), 0o600); err != nil {
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

func compress(t *testing.T, body []byte, compression string) []byte {
	t.Helper()
	var out strings.Builder
	var writer io.WriteCloser
	switch compression {
	case "zst":
		z, err := zstd.NewWriter(&out)
		if err != nil {
			t.Fatal(err)
		}
		writer = z
	case "gz":
		writer = gzip.NewWriter(&out)
	case "":
		return body
	default:
		t.Fatalf("the fixture cannot write %q archives", compression)
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
	var reader io.Reader
	switch {
	case strings.HasSuffix(name, ".zst"):
		z, err := zstd.NewReader(strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		defer z.Close()
		reader = z
	case strings.HasSuffix(name, ".gz"):
		z, err := gzip.NewReader(strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		defer z.Close()
		reader = z
	default:
		return body
	}
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
