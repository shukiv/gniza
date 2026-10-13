package directadmin

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// A copy lands under the account's home directory under one file name,
// and nothing that is not one file name is written anywhere.
func TestACopyTakesOnlyAFileName(t *testing.T) {
	for _, bad := range []string{"", ".hidden", "-flag", "a/b", "../x", "a..b", "a b", "x\n", strings.Repeat("a", 201)} {
		if err := usableDownloadName(bad); err == nil {
			t.Errorf("%q was taken as a file name", bad)
		}
	}
	for _, good := range []string{"studio-abcdef.tar.gz", "Files_2026-10-12.tar", "a.b-c_d"} {
		if err := usableDownloadName(good); err != nil {
			t.Errorf("%q was refused: %v", good, err)
		}
	}
}

// Who the account is comes from the system, and a name that is not an
// account's, or an account that is root or has no home of its own, is
// refused before anything is opened.
func TestTheCopyGoesToARealAccountsOwnHome(t *testing.T) {
	home := t.TempDir()
	lookups := map[string]*user.User{
		"studio": {Uid: "1001", Gid: "1001", HomeDir: home},
		"root":   {Uid: "0", Gid: "0", HomeDir: "/root"},
		"slash":  {Uid: "1002", Gid: "1002", HomeDir: "/"},
		"gone":   {Uid: "1003", Gid: "1003", HomeDir: filepath.Join(home, "missing")},
	}
	real := &Real{lookupUser: func(name string) (*user.User, error) {
		if found, ok := lookups[name]; ok {
			return found, nil
		}
		return nil, user.UnknownUserError(name)
	}}
	for _, refused := range []string{"nobody-here", "root", "slash", "gone", "../etc", ""} {
		if _, _, _, err := real.accountIdentity(refused); err == nil {
			t.Errorf("%q was taken as an account with a home of its own", refused)
		}
	}
	uid, gid, got, err := real.accountIdentity("studio")
	if err != nil || uid != 1001 || gid != 1001 || got != home {
		t.Fatalf("studio = %d/%d %q, %v", uid, gid, got, err)
	}
	// Only a path inside the account's downloads directory is ever
	// removed, and never by root: the check is before the command.
	for _, outside := range []string{filepath.Join(home, "public_html", "x"), "/etc/passwd",
		filepath.Join(home, DownloadsDir, "..", "x"), filepath.Join(home, DownloadsDir)} {
		if err := real.RemoveFromHome(context.Background(), "studio", outside); err == nil ||
			!strings.Contains(err.Error(), "is not a copy delivered") {
			t.Errorf("%s: removal was not refused as outside the downloads directory: %v", outside, err)
		}
	}
	// A copy that is not a file is not copied.
	if _, err := real.DeliverToHome(context.Background(), "studio", home, "x.tar.gz"); err == nil ||
		!strings.Contains(err.Error(), "not a file") {
		t.Errorf("a directory was taken as the copy: %v", err)
	}
	if os.Geteuid() != 0 {
		t.Skip("writing as another uid needs root; the refusals above are what runs here")
	}
}

// DirectAdmin puts nothing back yet, and says so, so a page does not
// offer what the provider would refuse.
func TestDirectAdminSaysItPutsNothingBackYet(t *testing.T) {
	if (&Real{}).PutsBack() {
		t.Error("the provider claims to put things back while every put-back answers ErrUnverified")
	}
}

// The kinds the account page may offer as a copy are the ones a split
// DirectAdmin backup can give up on their own: paths in the home tree
// and the whole account. The others need a databases part or an account
// archive the layout does not have, and each was measured failing on a
// production server on 2026-10-13.
func TestCopiesOnlyWhatTheSplitBackupCanGiveUp(t *testing.T) {
	real := &Real{}
	for _, kind := range []string{"account", "files", "mailbox"} {
		if !real.CopiesItem(kind) {
			t.Errorf("a copy of %s is refused", kind)
		}
	}
	for _, kind := range []string{"database", "dbusers", "cron", "dns", "domains", "ftp", "ssl", "settings", "website", ""} {
		if real.CopiesItem(kind) {
			t.Errorf("a copy of %q is offered, and the backup has nothing to take it out of", kind)
		}
	}
	if note := real.CopyNote(); !strings.Contains(note, "whole-account copy") {
		t.Errorf("the note does not say where the rest is: %q", note)
	}
}
