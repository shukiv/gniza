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

	out := t.TempDir()
	rebuilt, err := (Layout{}).PackArchive(context.Background(), dir, "gzv0908a", out)
	if err != nil {
		t.Fatal(err)
	}
	outer, home := members(t, rebuilt)
	for _, want := range []string{
		path.Join(BackupDir, UserConf),
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
