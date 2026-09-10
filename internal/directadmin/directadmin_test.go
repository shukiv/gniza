package directadmin

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/panel"
	"github.com/shukiv/gniza/internal/pkgacct"
)

// fakeHost lays out the parts of a DirectAdmin installation this provider
// reads: one directory per account, and a home directory for each.
func fakeHost(t *testing.T, accounts ...string) *Real {
	t.Helper()
	root := t.TempDir()
	data := filepath.Join(root, "data", "users")
	homes := filepath.Join(root, "home")
	for _, account := range accounts {
		if err := os.MkdirAll(filepath.Join(data, account), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(data, account, "user.conf"),
			[]byte("username="+account+"\ndomain="+account+".example\nsuspended=no\n"),
			0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(homes, account, "domains"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return &Real{DataDir: data, HomeRoot: homes}
}

// TestEveryAccountIsListedEvenWhenItsHomeIsGone. An account left off the
// page is an account nobody notices is not being backed up.
func TestEveryAccountIsListedEvenWhenItsHomeIsGone(t *testing.T) {
	provider := fakeHost(t, "studio", "rtflow")
	if err := os.RemoveAll(filepath.Join(provider.HomeRoot, "rtflow")); err != nil {
		t.Fatal(err)
	}

	accounts, err := provider.Accounts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 2 {
		t.Fatalf("got %d accounts, want 2: %+v", len(accounts), accounts)
	}
	byName := map[string]panel.AccountInfo{}
	for _, account := range accounts {
		byName[account.User] = account
	}
	if got := byName["studio"].PrimaryDomain; got != "studio.example" {
		t.Errorf("primary domain = %q, want studio.example", got)
	}
	if byName["studio"].Missing {
		t.Error("an account whose home directory is there is reported as missing")
	}
	if !byName["rtflow"].Missing {
		t.Error("an account whose home directory is gone is not reported as missing")
	}
}

// TestAnAccountNameCannotBeAPath. The name becomes a directory under the
// data root and a prefix in a database query.
func TestAnAccountNameCannotBeAPath(t *testing.T) {
	provider := fakeHost(t, "studio")
	for _, name := range []string{"../../etc", "studio/../admin", "studio'", "", "a b"} {
		if _, err := provider.Account(t.Context(), name); err == nil {
			t.Errorf("%q was accepted as an account name", name)
		}
	}
}

// TestWhatIsNotEstablishedIsRefusedRatherThanGuessed.
//
// Each of these needs an answer from a running DirectAdmin. A provider
// that guessed would produce backups that look successful and restore
// into nothing, so every one of them fails with the same sentinel and
// names the decision record.
func TestWhatIsNotEstablishedIsRefusedRatherThanGuessed(t *testing.T) {
	provider := fakeHost(t, "studio")
	ctx := t.Context()

	_, partialErr := provider.Stage(ctx, panel.StageRequest{
		Account:     panel.AccountInfo{User: "studio", HomeDir: "/home/studio"},
		StagingDir:  t.TempDir(),
		Mode:        pkgacct.ModeSplit,
		SkipHomedir: true,
	})
	_, systemErr := provider.StageSystem(ctx, t.TempDir())
	_, applyErr := provider.Apply(ctx, "/staging/user.admin.studio.tar", panel.ApplyOptions{})

	for what, err := range map[string]error{
		"staging part of one":   partialErr,
		"staging the server":    systemErr,
		"applying an archive":   applyErr,
		"putting files back":    provider.PutHomeDir(ctx, "studio", "/staging/tree"),
		"creating a database":   provider.CreateDatabase(ctx, "studio", "studio_wp"),
		"loading a dump":        provider.LoadDatabase(ctx, "studio", "studio_wp", "/staging/wp.sql"),
		"putting back the cron": provider.PutCrontab(ctx, "studio", "/staging/cron"),
		"putting back db users": provider.PutDatabaseUsers(ctx, "studio", nil),
	} {
		if !errors.Is(err, ErrUnverified) {
			t.Errorf("%s: err = %v, want it to say this is not established yet", what, err)
		}
		if !strings.Contains(err.Error(), "ADR 0019") {
			t.Errorf("%s: err = %v, want it to name the decision record", what, err)
		}
	}
}

// TestTheLayoutSaysWhichPanelItIs, so that what is written against a
// snapshot says where it came from.
func TestTheLayoutSaysWhichPanelItIs(t *testing.T) {
	if got := (&Real{}).Layout().Panel(); got != "directadmin" {
		t.Errorf("panel = %q, want directadmin", got)
	}
}

// TestTheAccountsOwnBackupDirectoriesAreLeftOut. A backup that copies the
// account's own backups doubles every night.
func TestTheAccountsOwnBackupDirectoriesAreLeftOut(t *testing.T) {
	excludes := (&Real{}).NativeExcludes("/home/studio")
	want := map[string]bool{
		"/home/studio/backups": false, "/home/studio/user_backups": false,
		"/home/studio/tmp": false,
	}
	for _, exclude := range excludes {
		if _, listed := want[exclude]; listed {
			want[exclude] = true
		}
	}
	for path, found := range want {
		if !found {
			t.Errorf("%s is not excluded", path)
		}
	}
}

// JetBackup keeps a directory of its own inside every account's home, and
// it belongs to root rather than to the account. DirectAdmin's own restore
// extracts the home archive as the account, so a root-owned member in it
// fails on the first attempt to set its times and takes the whole restore
// down with it:
//
//	Error extracting /home/gzv0908a/backups/backup/home.tar.zst :
//	/bin/tar: .jb-roundcube: Cannot utime: Operation not permitted
//
// It was found by the first restore drill on a live account. Nothing of
// the customer's is in it -- it was empty on all 142 accounts of the
// validation host -- and JetBackup makes it again.
func TestTheDirectoriesTheAccountDoesNotOwnAreLeftOut(t *testing.T) {
	excludes := (&Real{}).NativeExcludes("/home/studio")
	found := false
	for _, exclude := range excludes {
		found = found || exclude == "/home/studio/.jb-roundcube"
	}
	if !found {
		t.Errorf("JetBackup's own directory is not excluded, and a restore that "+
			"carries it fails: %v", excludes)
	}
}

// Installatron and Softaculous keep an archive of the site and its
// database under the account's home, one per automatic update, and each
// one is a fresh compression of data that has barely moved. Restic
// deduplicates identical chunks and a new archive of a changed site
// shares almost none, so this is gigabytes of near-full ingest every
// night to store a copy of what the snapshot already holds directly.
//
// On server-182-54-236-143.da.direct it was 12 GiB of a 20 GiB account.
func TestTheAccountsApplicationBackupsAreLeftOut(t *testing.T) {
	excludes := (&Real{}).NativeExcludes("/home/studio")
	found := false
	for _, exclude := range excludes {
		found = found || exclude == "/home/studio/application_backups"
	}
	if !found {
		t.Errorf("the account's own application backups are not excluded, so a "+
			"backup of a backup is stored every night: %v", excludes)
	}
}

// The file manager's trash is deleted files, and on the validation host
// it was 1.3 GiB of a 20 GiB account -- three of whose four foreign-owned
// files were in it. DirectAdmin's restore unpacks the home as the
// account, so a root-owned member in the trash stops the restore of
// everything after it: files the customer threw away, standing between
// them and the files they kept.
func TestTheFileManagersTrashIsLeftOut(t *testing.T) {
	excludes := (&Real{}).NativeExcludes("/home/studio")
	found := false
	for _, exclude := range excludes {
		found = found || exclude == "/home/studio/.trash"
	}
	if !found {
		t.Errorf("the file manager's trash is backed up, so deleted files are "+
			"stored and can stop a restore: %v", excludes)
	}
}
