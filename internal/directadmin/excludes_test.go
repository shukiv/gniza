package directadmin

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/destination"
	"github.com/shukiv/gniza/internal/resticrun"
)

// What the exclude list is meant to leave out, and what it must not.
//
// The paths are the shapes a DirectAdmin home actually has: a site lives
// at domains/<domain>/public_html, a subdomain one level under that, and
// a private_html beside it -- so a cache directory sits at no fixed
// depth and the patterns have to say so.
var (
	excludedByPolicy = []string{
		"application_backups/wp-2026-09-10.tar.gz",
		"softaculous_backups/soft.tar.gz",
		".trash/files/gone/info.php",
		".cagefs/etc/passwd",
		"domains/a.example/public_html/wp-content/cache/page.html",
		"domains/a.example/public_html/sub/wp-content/cache/page.html",
		"domains/a.example/public_html/wp-content/widget-cache/w.dat",
		"domains/a.example/public_html/wp-content/uploads/wpcf7_captcha/c.png",
		"domains/a.example/public_html/wptsc-cachedir/x.dat",
		"domains/b.example/public_html/cache/smarty/tpl.php",
		"domains/b.example/private_html/var/cache/mage.dat",
		"domains/b.example/private_html/var/session/sess_1",
		"domains/b.example/private_html/var/tmp/t",
		"domains/b.example/private_html/var/report/r",
		"domains/b.example/private_html/var/backups/db.sql",
		"domains/b.example/public_html/com_akeeba/backup/site.jpa",
		"domains/c.example/public_html/backupbuddy_backups/b.zip",
		"domains/c.example/public_html/error_log",
	}
	// The customer's own files, which look like the ones above and are
	// not them. Leaving one of these out is losing data, which is worse
	// than storing a cache.
	keptByPolicy = []string{
		"domains/a.example/public_html/index.php",
		"domains/a.example/public_html/wp-content/uploads/photo.jpg",
		"domains/a.example/public_html/wp-content/plugins/cache-helper/main.php",
		"domains/b.example/public_html/var/www/app.php",
		"domains/c.example/public_html/my-backup.tar.gz",
		"domains/c.example/public_html/error_log.php",
		"application_backups.txt",
		".trashcan/note.txt",
	}
)

// The patterns are handed to restic, so restic is what decides whether
// they mean what they are written to mean. A list checked only against
// itself proves the strings were typed twice.
func TestTheExcludeListMeansWhatItSaysToRestic(t *testing.T) {
	binary, err := exec.LookPath("restic")
	if err != nil {
		t.Skip("restic is not installed")
	}
	root := t.TempDir()
	home := filepath.Join(root, "home", "studio")
	for _, name := range append(append([]string{}, excludedByPolicy...), keptByPolicy...) {
		full := filepath.Join(home, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// A split backup hands restic the staged metadata as well as the home,
	// and that tree is DirectAdmin's own records: Gniza's file, not the
	// account's, and the archive is rebuilt from it header for header.
	// Nothing in the list may reach it, which is what anchoring the
	// patterns under the home is for.
	metadata := filepath.Join(root, "staging", "metadata")
	records := []string{
		"backup/a.example/error_log",
		"backup/a.example/domains/x/public_html/wp-content/cache/page.html",
		"backup/user.conf",
	}
	for _, name := range records {
		full := filepath.Join(metadata, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ctx := t.Context()
	runner := resticrun.New(resticrun.Config{Binary: binary, RuntimeDir: privateStaging(t), CacheDir: privateStaging(t)}, nil)
	repo := resticrun.Repository{Dest: &destination.Local{Root: privateStaging(t)}, Path: "excludes", Password: "local-test-only-password"}
	if err := runner.Init(ctx, repo, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Backup(ctx, repo, resticrun.BackupSpec{
		Paths:   []string{metadata, home},
		Tags:    []string{"account:studio"},
		Exclude: (&Real{}).NativeExcludes(home),
	}); err != nil {
		t.Fatal(err)
	}

	stored := map[string]bool{}
	list := exec.CommandContext(ctx, binary, "-r", filepath.Join(repo.Dest.(*destination.Local).Root, "excludes"),
		"ls", "latest")
	list.Env = append(os.Environ(), "RESTIC_PASSWORD="+repo.Password)
	out, err := list.Output()
	if err != nil {
		t.Fatalf("list the snapshot: %v: %s", err, err.(*exec.ExitError).Stderr)
	}
	kept := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		trimmed := strings.TrimSpace(line)
		if rel, ok := strings.CutPrefix(trimmed, home+"/"); ok {
			stored[rel] = true
		}
		if rel, ok := strings.CutPrefix(trimmed, metadata+"/"); ok {
			kept[rel] = true
		}
	}
	for _, name := range records {
		if !kept[name] {
			t.Errorf("%s is DirectAdmin's own record and the exclude list reached "+
				"into it, so the rebuilt archive would be missing a member", name)
		}
	}

	for _, name := range excludedByPolicy {
		if stored[name] {
			t.Errorf("%s was stored, and the list says it should not be", name)
		}
	}
	for _, name := range keptByPolicy {
		if !stored[name] {
			t.Errorf("%s is the customer's own file and it was left out", name)
		}
	}
}
