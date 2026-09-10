package dabackup

import (
	"archive/tar"
	"context"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// writeLeanArchive builds what DirectAdmin writes when the backup task
// asks for every option except "domain": the account's records, and the
// messages, and nothing of the account's own files. See
// testdata/lean-account.tar.list.
func writeLeanArchive(t *testing.T, account string, extra map[string]string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "user.admin."+account+".tar")
	f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	write := func(name, body string) {
		t.Helper()
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	write(path.Join(BackupDir, UserConf), "username="+account+"\n")
	write(path.Join(BackupDir, "backup_options.list"), "email\nsubdomain\n")
	write(path.Join(MailDir, fixtureDomain, "sales", "Maildir", "new", "1"), "a message")
	for name, body := range extra {
		write(path.Clean(name), body)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return file
}

// The messages are under the home path already: DirectAdmin sends them
// because asking for the mailboxes' own passwords means asking for
// "email", and "email" brings imap/ with it. Keeping them would store
// every message twice, which is the whole of what this mode exists to
// avoid.
func TestTheMessagesAreLeftToTheHomeDirectory(t *testing.T) {
	dir := t.TempDir()
	archive := writeLeanArchive(t, "gzv0908a", nil)
	if err := (Layout{}).UnpackLeanArchive(context.Background(), archive, "gzv0908a", dir); err != nil {
		t.Fatal(err)
	}
	manifest, err := readManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !manifest.Lean {
		t.Error("the manifest does not say the account's files are read where they lie")
	}
	for _, member := range manifest.Outer.Members {
		if strings.HasPrefix(member.Name, MailDir+"/") {
			t.Errorf("the manifest still carries %s", member.Name)
		}
	}
	if _, err := os.Lstat(HomedirPart(dir)); err == nil {
		t.Error("a home tree was written for a mode that reads the home directory in place")
	}
	if _, err := os.Lstat(filepath.Join(MetadataPart(dir), BackupDir, UserConf)); err != nil {
		t.Errorf("the account's own records are not in the metadata part: %v", err)
	}
}

// If DirectAdmin ignored the selection, this is a whole-account archive
// arriving where a metadata one was expected, and the run that produced
// it read and wrote the account in full. Saying so is the difference
// between a server that is known not to support this and a server that
// silently backs up half an account.
func TestAnArchiveThatIgnoredTheSelectionIsRefused(t *testing.T) {
	for _, member := range []string{
		path.Join(DomainsDir, fixtureDomain, "public_html", "index.html"),
		path.Join(BackupDir, "home.tar.zst"),
	} {
		archive := writeLeanArchive(t, "gzv0908a", map[string]string{member: "x"})
		err := (Layout{}).UnpackLeanArchive(context.Background(), archive, "gzv0908a", t.TempDir())
		if err == nil {
			t.Fatalf("an archive carrying %s was accepted as a metadata archive", member)
		}
		if !strings.Contains(err.Error(), "selection") {
			t.Errorf("the error for %s does not say the selection was ignored: %v", member, err)
		}
	}
}

// homeTree builds what restic restores: a copy of the account's home
// directory, with the ownership and modes the account's own files have.
func homeTree(t *testing.T, dir string) {
	t.Helper()
	for _, each := range []string{
		path.Join(DomainsDir, fixtureDomain, "public_html"),
		path.Join(MailDir, fixtureDomain, "sales", "Maildir", "new"),
		".php",
	} {
		if err := os.MkdirAll(filepath.Join(HomedirPart(dir), each), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{
		path.Join(DomainsDir, fixtureDomain, "public_html", "index.html"): "<html>",
		path.Join(MailDir, fixtureDomain, "sales", "Maildir", "new", "1"): "a message",
		".bashrc":           "umask 022\n",
		".php/php-mail.log": "sent\n",
	} {
		if err := os.WriteFile(filepath.Join(HomedirPart(dir), name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("public_html", filepath.Join(HomedirPart(dir), DomainsDir, fixtureDomain, "www")); err != nil {
		t.Fatal(err)
	}
}

// members lists what a rebuilt archive carries, and what is inside its
// nested home archive.
func members(t *testing.T, archivePath string) (outer, home []string) {
	t.Helper()
	f, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return outer, home
		}
		if err != nil {
			t.Fatal(err)
		}
		outer = append(outer, strings.TrimSuffix(header.Name, "/"))
		if !isNestedHomeArchive(path.Clean(header.Name), header.Typeflag) {
			continue
		}
		reader, closer, err := decompressed(tr, header.Name)
		if err != nil {
			t.Fatal(err)
		}
		inner := tar.NewReader(reader)
		for {
			nested, err := inner.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			home = append(home, strings.TrimSuffix(nested.Name, "/"))
		}
		closer()
	}
}

// The account's files were never in the archive, so putting one back
// means building those members out of the tree restic restored. What
// DirectAdmin's own restore reads has to be the archive it would have
// written itself.
func TestTheRebuiltArchiveCarriesWhatWasReadInPlace(t *testing.T) {
	dir := t.TempDir()
	archive := writeLeanArchive(t, "gzv0908a", nil)
	if err := (Layout{}).UnpackLeanArchive(context.Background(), archive, "gzv0908a", dir); err != nil {
		t.Fatal(err)
	}
	homeTree(t, dir)
	// The webmail data is exported into the metadata part after the
	// archive is unpacked, at the member DirectAdmin's restore reads.
	webmail := filepath.Join(MetadataPart(dir), BackupDir, fixtureDomain, "email", "data")
	if err := os.MkdirAll(webmail, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(webmail, "roundcube.xml"), []byte("<ROUNDCUBE/>\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (Layout{}).AddMetadataMember(dir, path.Join(BackupDir, fixtureDomain, "email", "data", "roundcube.xml")); err != nil {
		t.Fatal(err)
	}

	out := t.TempDir()
	rebuilt, err := (Layout{}).PackArchive(context.Background(), dir, "gzv0908a", out)
	if err != nil {
		t.Fatal(err)
	}
	outer, home := members(t, rebuilt)
	for _, want := range []string{
		path.Join(BackupDir, UserConf),
		path.Join(BackupDir, fixtureDomain, "email", "data", "roundcube.xml"),
		path.Join(DomainsDir, fixtureDomain, "public_html", "index.html"),
		path.Join(DomainsDir, fixtureDomain, "www"),
		path.Join(MailDir, fixtureDomain, "sales", "Maildir", "new", "1"),
	} {
		if !carries(outer, want) {
			t.Errorf("the rebuilt archive does not carry %s: %v", want, outer)
		}
	}
	for _, want := range []string{".bashrc", ".php/php-mail.log"} {
		if !carries(home, want) {
			t.Errorf("the nested home archive does not carry %s: %v", want, home)
		}
	}
	// The home archive is the complement of the two directories that are
	// in the outer one. A copy in both restores an account twice its size.
	for _, unwanted := range []string{DomainsDir, MailDir} {
		if carries(home, unwanted) {
			t.Errorf("%s is in the nested home archive as well as the outer one: %v", unwanted, home)
		}
	}
	// And it is where DirectAdmin put it: the last member of backup/,
	// before the account's own directories. Its own restore reads the
	// archive in order.
	seen := map[string]int{}
	for i, name := range outer {
		if isNestedHomeArchive(path.Clean(name), tar.TypeReg) {
			seen["home"] = i
			continue
		}
		first, _, _ := strings.Cut(name, "/")
		if _, already := seen[first]; !already {
			seen[first] = i
		}
		if first == BackupDir {
			seen["last backup"] = i
		}
	}
	if seen["home"] < seen["last backup"] {
		t.Errorf("the home archive is at %d, before the last of %s at %d: %v",
			seen["home"], BackupDir, seen["last backup"], outer)
	}
	if seen["home"] > seen[DomainsDir] {
		t.Errorf("the home archive is at %d, after %s at %d: %v",
			seen["home"], DomainsDir, seen[DomainsDir], outer)
	}
}

// A whole-account archive carries its own webmail data, and its members
// are in DirectAdmin's order with the account's own directories already
// after backup/. Adding to that would put a backup/ member where the
// restore does not look for one, so it is refused.
func TestOnlyALeanTreeTakesAnAddedMember(t *testing.T) {
	dir := t.TempDir()
	if err := (Layout{}).UnpackArchive(context.Background(), writeArchive(t, "gzv0908a", nil), "gzv0908a", dir); err != nil {
		t.Fatal(err)
	}
	name := path.Join(BackupDir, fixtureDomain, "email", "data", "roundcube.xml")
	target := filepath.Join(MetadataPart(dir), filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("<ROUNDCUBE/>\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (Layout{}).AddMetadataMember(dir, name); err == nil {
		t.Fatal("a whole-account tree took an added member")
	}
}

// DirectAdmin's restore reads backup_options.list to decide what it is
// being handed. A rebuilt archive is a whole account, so a list that
// still says what the backup asked for would have the restore skip the
// files it is holding.
func TestTheRebuiltArchiveSaysItHoldsEverything(t *testing.T) {
	dir := t.TempDir()
	archive := writeLeanArchive(t, "gzv0908a", nil)
	if err := (Layout{}).UnpackLeanArchive(context.Background(), archive, "gzv0908a", dir); err != nil {
		t.Fatal(err)
	}
	homeTree(t, dir)
	rebuilt, err := (Layout{}).PackArchive(context.Background(), dir, "gzv0908a", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(rebuilt)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			t.Fatal("the rebuilt archive has no backup_options.list")
		}
		if err != nil {
			t.Fatal(err)
		}
		if path.Clean(header.Name) != path.Join(BackupDir, "backup_options.list") {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"domain", "email_data"} {
			found := false
			for _, line := range strings.Split(string(body), "\n") {
				found = found || line == want
			}
			if !found {
				t.Errorf("the rebuilt archive's options do not include %q: %q", want, body)
			}
		}
		return
	}
}

// The owners are the account's own, and not always the same one:
// DirectAdmin's archive carried gzv0908a/mail on Maildir and
// gzv0908a/apache on .php. A rebuilt archive takes them from the tree,
// which is a copy of the home directory those owners are on.
func TestTheRebuiltMembersKeepWhatTheTreeSays(t *testing.T) {
	dir := t.TempDir()
	archive := writeLeanArchive(t, "gzv0908a", nil)
	if err := (Layout{}).UnpackLeanArchive(context.Background(), archive, "gzv0908a", dir); err != nil {
		t.Fatal(err)
	}
	homeTree(t, dir)
	log := filepath.Join(HomedirPart(dir), ".php", "php-mail.log")
	if err := os.Chmod(log, 0o640); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(log)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)

	rebuilt, err := (Layout{}).PackArchive(context.Background(), dir, "gzv0908a", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(rebuilt)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			t.Fatal("the rebuilt archive has no nested home archive")
		}
		if err != nil {
			t.Fatal(err)
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
			if err == io.EOF {
				t.Fatalf("the nested home archive has no .php/php-mail.log")
			}
			if err != nil {
				t.Fatal(err)
			}
			if path.Clean(nested.Name) != ".php/php-mail.log" {
				continue
			}
			if nested.Mode != 0o640 {
				t.Errorf("the member's mode is %o, the tree says %o", nested.Mode, 0o640)
			}
			if nested.Uid != int(stat.Uid) || nested.Gid != int(stat.Gid) {
				t.Errorf("the member is owned by %d:%d, the tree says %d:%d",
					nested.Uid, nested.Gid, stat.Uid, stat.Gid)
			}
			if !nested.ModTime.Equal(info.ModTime().Truncate(time.Second)) {
				t.Errorf("the member's time is %s, the tree says %s",
					nested.ModTime, info.ModTime())
			}
			return
		}
	}
}

// headersOf reads a rebuilt archive as a name-to-header map, for the
// outer archive and for the one nested inside it.
func headersOf(t *testing.T, archivePath string) (outer, home map[string]*tar.Header) {
	t.Helper()
	outer, home = map[string]*tar.Header{}, map[string]*tar.Header{}
	f, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return outer, home
		}
		if err != nil {
			t.Fatal(err)
		}
		outer[strings.TrimSuffix(path.Clean(header.Name), "/")] = header
		if !isNestedHomeArchive(path.Clean(header.Name), header.Typeflag) {
			continue
		}
		reader, closer, err := decompressed(tr, header.Name)
		if err != nil {
			t.Fatal(err)
		}
		inner := tar.NewReader(reader)
		for {
			nested, err := inner.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			home[strings.TrimSuffix(path.Clean(nested.Name), "/")] = nested
		}
		closer()
	}
}

// DirectAdmin's own backup carries a hard link as a link, and restic
// restores one as a link. Writing both names as whole files would restore
// an account larger than the one that was backed up -- a Maildir where
// every message is linked from two folders comes back twice.
//
// A link is only a link inside the archive that carries its target: the
// account's own directories go into the outer archive and the rest into
// the nested one, and a name in one cannot point at a name in the other.
func TestALinkedFileIsCarriedAsALink(t *testing.T) {
	dir := t.TempDir()
	archive := writeLeanArchive(t, "gzv0908a", nil)
	if err := (Layout{}).UnpackLeanArchive(context.Background(), archive, "gzv0908a", dir); err != nil {
		t.Fatal(err)
	}
	homeTree(t, dir)
	home := HomedirPart(dir)
	// Two names for one file, both outside the account's own directories.
	if err := os.Link(filepath.Join(home, ".bashrc"), filepath.Join(home, ".bash_profile")); err != nil {
		t.Fatal(err)
	}
	// And two names for one file either side of the boundary between the
	// two archives.
	if err := os.Link(
		filepath.Join(home, DomainsDir, fixtureDomain, "public_html", "index.html"),
		filepath.Join(home, ".php", "index.html")); err != nil {
		t.Fatal(err)
	}

	rebuilt, err := (Layout{}).PackArchive(context.Background(), dir, "gzv0908a", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outer, nested := headersOf(t, rebuilt)

	first, second := nested[".bash_profile"], nested[".bashrc"]
	if first == nil || second == nil {
		t.Fatalf("the nested archive lost one of the two names: %v", nested)
	}
	if first.Typeflag != tar.TypeReg || first.Size == 0 {
		t.Errorf(".bash_profile is not the file the link points at: %c %d", first.Typeflag, first.Size)
	}
	if second.Typeflag != tar.TypeLink {
		t.Errorf(".bashrc is carried as %c rather than a link", second.Typeflag)
	}
	if second.Linkname != ".bash_profile" {
		t.Errorf(".bashrc points at %q", second.Linkname)
	}
	if second.Size != 0 {
		t.Errorf(".bashrc carries %d bytes of a file that is already in the archive", second.Size)
	}

	// The two archives are written and read separately, so neither name
	// may be a link: DirectAdmin's restore would have nothing to point at.
	across := path.Join(DomainsDir, fixtureDomain, "public_html", "index.html")
	if header := outer[across]; header == nil || header.Typeflag != tar.TypeReg {
		t.Errorf("%s is not a whole file in the outer archive: %v", across, header)
	}
	if header := nested[".php/index.html"]; header == nil || header.Typeflag != tar.TypeReg {
		t.Errorf(".php/index.html is not a whole file in the nested archive: %v", header)
	}
}

// DirectAdmin's restore reads the messages out of imap/ only when
// backup/<domain>/email/data/imap/.direct_imap_backup says they are
// there; its own backup writes that marker with email_data, which a lean
// backup leaves out. A rebuilt archive without it restored an account on
// 2026-09-11 with every message left behind, while DirectAdmin's own
// archive of the same account put them back.
func TestTheRebuiltArchiveSaysItsMailIsDirect(t *testing.T) {
	dir := t.TempDir()
	archive := writeLeanArchive(t, "gzv0908a", map[string]string{
		path.Join(BackupDir, fixtureDomain, "email", "passwd"): "sales:x\nsupport:y\n",
	})
	if err := (Layout{}).UnpackLeanArchive(context.Background(), archive, "gzv0908a", dir); err != nil {
		t.Fatal(err)
	}
	homeTree(t, dir)
	rebuilt, err := (Layout{}).PackArchive(context.Background(), dir, "gzv0908a", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	marker := path.Join(BackupDir, fixtureDomain, "email", "data", "imap", ".direct_imap_backup")
	body, header := memberBody(t, rebuilt, marker)
	if header == nil {
		t.Fatalf("the rebuilt archive does not carry %s, so DirectAdmin's restore will leave the messages behind", marker)
	}
	if string(body) != "num_emails=2\n" {
		t.Errorf("%s says %q, want the mailbox count as DirectAdmin writes it, %q", marker, body, "num_emails=2\n")
	}
	if header.Uname != "gzv0908a" || header.Mode != 0o644 {
		t.Errorf("%s is %s mode %o, want the account's own like the rest of backup/, mode 644", marker, header.Uname, header.Mode)
	}
	// And it is with that domain's other records, before the account's
	// own directories, where DirectAdmin's restore reads it in order.
	outer, _ := members(t, rebuilt)
	at, home := -1, -1
	for i, name := range outer {
		switch {
		case path.Clean(name) == marker:
			at = i
		case isNestedHomeArchive(path.Clean(name), tar.TypeReg):
			home = i
		}
	}
	if at > home {
		t.Errorf("%s is at %d, after the home archive at %d: %v", marker, at, home, outer)
	}
}

// memberBody reads one regular member of the outer archive.
func memberBody(t *testing.T, archive, name string) ([]byte, *tar.Header) {
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
		if path.Clean(header.Name) == name {
			body, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			return body, header
		}
	}
}
