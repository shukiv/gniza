package cpanel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/panel"
)

// fakeRestorepkg is a stand-in for cPanel's script that records the
// arguments it was given.
func fakeRestorepkg(t *testing.T) (script, record string) {
	t.Helper()
	dir := t.TempDir()
	record = filepath.Join(dir, "args")
	script = filepath.Join(dir, "restorepkg")
	body := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + record + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, record
}

// TestApplyAsksForARestrictedRestore covers the default this program
// deliberately differs from cPanel on. The archive being unpacked holds a
// customer's own home directory, and restorepkg runs as root: an account
// compromised at any point since its last backup could have left
// something in there for exactly this moment. cPanel's Restricted Restore
// is for that, and cPanel's own default is off.
func TestApplyAsksForARestrictedRestore(t *testing.T) {
	script, record := fakeRestorepkg(t)
	archive := filepath.Join(t.TempDir(), "cpmove-customer1.tar")
	if err := os.WriteFile(archive, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	host := &Real{RestorepkgPath: script}

	if _, err := host.Apply(context.Background(), archive, panel.ApplyOptions{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	args := readArgs(t, record)
	if !args["--restricted"] {
		t.Error("a restore with no options asked cPanel for an unrestricted restore")
	}
	if args["--unrestricted"] {
		t.Error("both modes were asked for at once")
	}
	if args["--force"] {
		t.Error("a restore that was not asked to overwrite anything asked to overwrite")
	}
	if !args[archive] {
		t.Errorf("the archive was not passed: %v", args)
	}

	// An operator restoring onto an account that is still here has asked
	// for it to be replaced. cPanel refuses --force with --restricted --
	// "You may not force Restricted Restore" -- and --skipaccount is what
	// --force means once the account exists, by cPanel's own help. A live
	// restore proved this: --restricted --force failed outright, so every
	// apply would have.
	if _, err := host.Apply(context.Background(), archive,
		panel.ApplyOptions{Overwrite: true}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	args = readArgs(t, record)
	if args["--force"] {
		t.Error("--force was passed with --restricted, which cPanel refuses outright")
	}
	if !args["--skipaccount"] {
		t.Errorf("a restore onto a live account did not ask to restore into it: %v", args)
	}
}

// TestAnOperatorCanStillAskForAnUnrestrictedRestore covers the other side:
// restricted mode refuses some legitimate archive content, so the choice
// has to exist or an operator with a real archive cannot restore it.
func TestAnOperatorCanStillAskForAnUnrestrictedRestore(t *testing.T) {
	script, record := fakeRestorepkg(t)
	archive := filepath.Join(t.TempDir(), "cpmove-customer1.tar")
	if err := os.WriteFile(archive, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	host := &Real{RestorepkgPath: script}

	if _, err := host.Apply(context.Background(), archive,
		panel.ApplyOptions{Unrestricted: true}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	args := readArgs(t, record)
	if !args["--unrestricted"] || args["--restricted"] {
		t.Errorf("the operator's choice was not passed on: %v", args)
	}

	// --force is only available in unrestricted mode, so that is where
	// an overwrite has to use it.
	if _, err := host.Apply(context.Background(), archive,
		panel.ApplyOptions{Unrestricted: true, Overwrite: true}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if args := readArgs(t, record); !args["--force"] {
		t.Errorf("an unrestricted overwrite did not force: %v", args)
	}
}

func TestLiveCertificationUsesDisposableAccountAndCleansItUp(t *testing.T) {
	root := t.TempDir()
	users := filepath.Join(root, "users")
	homes := filepath.Join(root, "home")
	if err := os.MkdirAll(users, 0o700); err != nil {
		t.Fatal(err)
	}
	restoreRecord := filepath.Join(root, "restore-args")
	removeRecord := filepath.Join(root, "remove-args")
	restoreScript := filepath.Join(root, "restorepkg")
	removeScript := filepath.Join(root, "removeacct")
	restoreBody := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + restoreRecord + "\n" +
		"user=\nfor arg in \"$@\"; do case \"$arg\" in --newuser=*) user=${arg#*=};; esac; done\n" +
		"mkdir -p " + homes + "/$user\n: > " + users + "/$user\n"
	removeBody := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + removeRecord + "\n" +
		"rm -f " + users + "/$1\nrm -rf " + homes + "/$1\n"
	if err := os.WriteFile(restoreScript, []byte(restoreBody), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(removeScript, []byte(removeBody), 0o755); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(root, "cpmove-customer1.tar")
	if err := os.WriteFile(archive, []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	host := &Real{
		RestorepkgPath: restoreScript, RemoveacctPath: removeScript,
		UsersDir: users, HomeRoot: homes,
	}

	if err := host.Certify(context.Background(), archive, "cprv1234"); err != nil {
		t.Fatalf("Certify: %v", err)
	}
	restoreArgs := readArgs(t, restoreRecord)
	if !restoreArgs["--restricted"] || !restoreArgs["--newuser=cprv1234"] ||
		!restoreArgs["--update_dns_zone=0"] {
		t.Fatalf("unsafe certification restore arguments: %v", restoreArgs)
	}
	removeArgs := readArgs(t, removeRecord)
	if !removeArgs["cprv1234"] || !removeArgs["--force"] {
		t.Fatalf("disposable account was not removed: %v", removeArgs)
	}
	if _, err := os.Stat(filepath.Join(users, "cprv1234")); !os.IsNotExist(err) {
		t.Fatal("certification account remains registered")
	}
}

func readArgs(t *testing.T, record string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("restorepkg was not run: %v", err)
	}
	args := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		args[line] = true
	}
	return args
}
