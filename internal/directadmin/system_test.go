package directadmin

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/destination"
	"github.com/shukiv/gniza/internal/pkgacct"
	"github.com/shukiv/gniza/internal/resticrun"
)

// systemHost is a DirectAdmin server's configuration as a tree of its
// own: enough of it to tell copied from absent, a pattern that matches
// two PHP versions, a symlink and a file that belongs to no list.
func systemHost(t *testing.T) *Real {
	t.Helper()
	root := t.TempDir()
	for name, body := range map[string]string{
		"usr/local/directadmin/conf/directadmin.conf":       "servername=uscp.example\ndnssec=1\n",
		"usr/local/directadmin/data/admin/ip.list":          "182.54.236.10\n",
		"usr/local/directadmin/custombuild/options.conf":    "webserver=apache\nphp1_release=8.3\n",
		"usr/local/directadmin/custombuild/custom/ap2/x":    "custom\n",
		"usr/local/php83/lib/php.ini":                       "memory_limit = 256M\n",
		"usr/local/php74/lib/php.ini":                       "memory_limit = 128M\n",
		"usr/local/php83/lib/php.conf.d/10-directadmin.ini": "date.timezone = UTC\n",
		"etc/virtual/domains":                               "example.co.il\n",
		"etc/exim.conf":                                     "# exim\n",
		"etc/exim.variables.conf":                           "# variables\n",
		"var/named/example.co.il.db":                        "$TTL 3600\n",
		"var/named/Kexample.co.il.+013+12345.private":       "Private-key-format: v1.3\n",
		"etc/passwd":                        "root:x:0:0::/root:/bin/bash\n",
		"usr/local/directadmin/directadmin": "not configuration\n",
		"home/alice/public_html/index.php":  "an account's own\n",
	} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(root, "usr/local/directadmin/conf/directadmin.conf"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/exim.conf", filepath.Join(root, "etc/virtual/linked")); err != nil {
		t.Fatal(err)
	}
	return &Real{SystemRoot: root, BinaryPath: filepath.Join(root, "no-such-binary")}
}

// The finding from the fleet check of 2026-09-22: the system backup of a
// DirectAdmin server was a refusal, so it failed every night on three
// servers and none of them had its own configuration stored anywhere.
func TestTheServersOwnConfigurationIsStaged(t *testing.T) {
	provider := systemHost(t)
	staging := t.TempDir()

	payload, err := provider.StageSystem(t.Context(), staging)
	if err != nil {
		t.Fatalf("StageSystem: %v", err)
	}
	if payload.Mode != pkgacct.ModeSystem || payload.Account != "@system" || len(payload.Parts) != 1 {
		t.Fatalf("payload = %+v", payload)
	}
	files := filepath.Join(payload.Parts[0].Path, "files")
	for _, want := range []string{
		"usr/local/directadmin/conf/directadmin.conf",
		"usr/local/directadmin/data/admin/ip.list",
		"usr/local/directadmin/custombuild/options.conf",
		"usr/local/directadmin/custombuild/custom/ap2/x",
		"usr/local/php83/lib/php.ini", "usr/local/php74/lib/php.ini",
		"usr/local/php83/lib/php.conf.d/10-directadmin.ini",
		"etc/virtual/domains", "etc/exim.conf", "etc/exim.variables.conf",
		"var/named/Kexample.co.il.+013+12345.private", "etc/passwd",
	} {
		if _, err := os.Stat(filepath.Join(files, want)); err != nil {
			t.Errorf("%s was not carried", want)
		}
	}
	for _, not := range []string{"usr/local/directadmin/directadmin", "home"} {
		if _, err := os.Lstat(filepath.Join(files, not)); err == nil {
			t.Errorf("%s was carried, and is not the server's configuration", not)
		}
	}

	info, err := os.Stat(filepath.Join(files, "usr/local/directadmin/conf/directadmin.conf"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("a file that was 0600 came through as %v", info.Mode().Perm())
	}
	if link, err := os.Readlink(filepath.Join(files, "etc/virtual/linked")); err != nil || link != "/etc/exim.conf" {
		t.Errorf("a symlink came through as %q, %v", link, err)
	}

	manifest, err := os.ReadFile(filepath.Join(payload.Parts[0].Path, "manifest.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"panel\tDirectAdmin", "copied\t/usr/local/php74/lib/php.ini",
		"copied\t/etc/virtual", "absent\t/etc/nginx", "absent\t/etc/csf",
	} {
		if !strings.Contains(string(manifest), want) {
			t.Errorf("the manifest does not say %q:\n%s", want, manifest)
		}
	}
}

// A backup of nothing that succeeded would be found out on the day the
// server was lost.
func TestAServerWithNoneOfItIsAFailureNotAnEmptyBackup(t *testing.T) {
	provider := &Real{SystemRoot: t.TempDir()}
	if _, err := provider.StageSystem(t.Context(), t.TempDir()); err == nil {
		t.Fatal("a server where nothing was found was backed up as nothing")
	}
}

// What is left out is said, so nobody takes this for a whole machine.
func TestWhatTheSystemBackupLeavesOutIsSaid(t *testing.T) {
	if len(SystemNotCarried) == 0 {
		t.Fatal("nothing is said about what the system backup leaves out")
	}
	said := strings.Join(SystemNotCarried, " ")
	for _, want := range []string{"account", "plugins", "key that decrypts"} {
		if !strings.Contains(said, want) {
			t.Errorf("what is left out does not mention %q", want)
		}
	}
}

// What happened on 182.54.236.143 the evening this was installed: the
// backup of the server's configuration succeeded and held no file. The
// server's settings are not an account and have no home, and the list of
// what an account's backup leaves out was built under that empty home:
// "etc", "var", "usr". restic matches a bare name wherever it finds one,
// and those are the names the configuration is staged under.
func TestTheAccountsExcludeListIsNotAppliedToTheServersOwnFiles(t *testing.T) {
	for _, home := range []string{"", ".", "home/alice"} {
		if excludes := (&Real{}).NativeExcludes(home); len(excludes) != 0 {
			t.Errorf("a home of %q leaves out %v", home, excludes)
		}
	}
	if excludes := (&Real{}).NativeExcludes("/home/alice"); len(excludes) == 0 {
		t.Error("an account's own list is empty")
	}

	binary, err := exec.LookPath("restic")
	if err != nil {
		t.Skip("restic is not installed")
	}
	provider := systemHost(t)
	payload, err := provider.StageSystem(t.Context(), privateStaging(t))
	if err != nil {
		t.Fatal(err)
	}
	runner := resticrun.New(resticrun.Config{Binary: binary, RuntimeDir: privateStaging(t),
		CacheDir: privateStaging(t)}, nil)
	repo := resticrun.Repository{Dest: &destination.Local{Root: privateStaging(t)},
		Path: "system", Password: "local-test-only-password"}
	if err := runner.Init(t.Context(), repo, nil); err != nil {
		t.Fatal(err)
	}
	// As the node hands it over: the provider's list for the home of the
	// account being backed up, which for the server's settings is none.
	stored, err := runner.Backup(t.Context(), repo, resticrun.BackupSpec{
		Paths:   payload.Paths(),
		Tags:    []string{"account:@system", "mode:system"},
		Exclude: provider.NativeExcludes(""),
	})
	if err != nil {
		t.Fatal(err)
	}
	if stored.Summary.TotalFilesProcessed < 10 {
		t.Errorf("the backup of the server's configuration holds %d files",
			stored.Summary.TotalFilesProcessed)
	}
}
