package whm_test

import (
	"encoding/json"
	"fmt"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// cPanel's install_plugin inherits the installer's restrictive umask when it
// writes the Jupiter DynamicUI record. The record is later read while cPanel
// builds an account's application list, so leaving it at 0600 makes the tile
// disappear even when Feature Manager enables it.
func TestInstallerMakesDynamicUIRegistrationAccountReadable(t *testing.T) {
	scriptBytes, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)

	installAt := strings.Index(script, `/usr/local/cpanel/scripts/install_plugin "$PLUGIN_META" --theme=jupiter`)
	chmodAt := strings.Index(script, `chmod 0644 "$DYNAMICUI"`)
	if installAt < 0 {
		t.Fatal("installer no longer registers the Jupiter plugin")
	}
	if chmodAt < 0 {
		t.Fatal("installer does not make the generated DynamicUI record account-readable")
	}
	if chmodAt < installAt {
		t.Fatal("DynamicUI permissions are set before cPanel generates the record")
	}
	if !strings.Contains(script, `( umask 022; /usr/local/cpanel/scripts/install_plugin "$PLUGIN_META" --theme=jupiter )`) {
		t.Fatal("cPanel's plugin installer does not run with a public-metadata umask")
	}
	if touchAt := strings.Index(script, `touch "$DYNAMICUI"`); touchAt < chmodAt {
		t.Fatal("installer does not invalidate cPanel's per-account application cache after fixing permissions")
	}
}

func TestCPanelTileUsesTheRasterIcon(t *testing.T) {
	metadataBytes, err := os.ReadFile("../cpanel/install.json")
	if err != nil {
		t.Fatal(err)
	}
	var links []struct {
		Icon string `json:"icon"`
	}
	if err := json.Unmarshal(metadataBytes, &links); err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 {
		t.Fatalf("Jupiter metadata contains %d links, want 1", len(links))
	}
	if links[0].Icon != "gniza.png" {
		t.Fatalf("Jupiter metadata icon = %q, want gniza.png", links[0].Icon)
	}

	icon, err := os.Open("../branding/png/badge-48.png")
	if err != nil {
		t.Fatal(err)
	}
	defer icon.Close()
	config, err := png.DecodeConfig(icon)
	if err != nil {
		t.Fatalf("decode cPanel icon: %v", err)
	}
	if config.Width != 48 || config.Height != 48 {
		t.Fatalf("cPanel icon is %dx%d, want 48x48", config.Width, config.Height)
	}

	scriptBytes, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)
	for _, required := range []string{
		`install -m 0644 "$SOURCE_DIR/branding/badge-48.png" "$PLUGIN_META/gniza.png"`,
		`rm -f "$FRONTEND/assets/application_icons/gniza.svg"`,
		// The tile comes off a sprite sheet; the PNG alone is not what
		// an account sees.
		`--application=cpanel --theme=jupiter`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("installer is missing %q", required)
		}
	}
}

func TestInstallerDeploysAndRemovesTheSessionBridge(t *testing.T) {
	installerBytes, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	uninstallerBytes, err := os.ReadFile("uninstall.sh")
	if err != nil {
		t.Fatal(err)
	}
	installer := string(installerBytes)
	uninstaller := string(uninstallerBytes)

	for _, required := range []string{
		`$CPANEL_UAPI_DIR/Gniza.pm`,
		`$CPANEL_ADMIN_DIR/Session.pm`,
		`-m 0700 "$SOURCE_DIR/cpanel/admin/Gniza/Session.pm"`,
	} {
		if !strings.Contains(installer, required) {
			t.Errorf("installer is missing %q", required)
		}
	}
	// The uninstaller takes the admin module off the server by retiring
	// the whole directory it lives in, which is the only thing ever put
	// there, so Session.pm goes with it and no empty directory is left
	// behind for a later run to tidy up.
	for _, required := range []string{
		`retire /usr/local/cpanel/Cpanel/API/Gniza.pm `,
		`retire /var/cpanel/perl/Cpanel/Admin/Modules/Gniza `,
	} {
		if !strings.Contains(uninstaller, required) {
			t.Errorf("uninstaller is missing %q", required)
		}
	}
}

// Upgrading a server that was installed as cprest takes the old
// installation apart before building this one. The order is what makes it
// survivable: everything that can refuse has to refuse while the old
// service is still running, because a server that stops half way through
// has no backups at all until somebody notices.
func TestTheUpgradeFromCprestRefusesBeforeItRemovesAnything(t *testing.T) {
	scriptBytes, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)

	refuseAt := strings.Index(script, `die "both $old and $new exist.`)
	uninstallAt := strings.Index(script, `sh "$LEGACY_SHARE_DIR/uninstall.sh"`)
	moveAt := strings.Index(script, `mv -- "$old" "$new"`)
	if refuseAt < 0 || uninstallAt < 0 || moveAt < 0 {
		t.Fatal("the installer no longer takes an earlier cprest installation apart")
	}
	if refuseAt > uninstallAt || refuseAt > moveAt {
		t.Error("the installer removes the old installation before checking that it can finish")
	}
	if uninstallAt > moveAt {
		t.Error("the directories are moved before the old service that is using them is stopped")
	}

	// manage_hooks asks the registered executable to describe what it
	// registered, so the hooks have to go before the binary does.
	hooksAt := strings.Index(script, `manage_hooks delete script "$LEGACY_HOOK_BIN"`)
	removeAt := strings.Index(script, `rm -f "$LEGACY_HOOK_BIN" "$LEGACY_AGENT"`)
	if hooksAt < 0 || removeAt < 0 || hooksAt > removeAt {
		t.Error("the old hook binary is removed before its hook registrations are")
	}

	// The two directories that are moved rather than deleted: one holds
	// the key that decrypts the stored destination credentials, the other
	// every destination, schedule and backup this server has.
	for _, kept := range []string{"/etc/cprest", "/var/lib/cprest"} {
		if !strings.Contains(script, `LEGACY_CONFIG_DIR=/etc/cprest`) ||
			!strings.Contains(script, `LEGACY_STATE_DIR=/var/lib/cprest`) {
			t.Fatalf("the installer does not name %s as something to keep", kept)
		}
	}
	for _, destroyed := range []string{
		`rm -rf -- "$LEGACY_CONFIG_DIR"`,
		`rm -rf -- "$LEGACY_STATE_DIR"`,
		`rm -rf -- /etc/cprest`,
		`rm -rf -- /var/lib/cprest`,
	} {
		if strings.Contains(script, destroyed) {
			t.Errorf("the installer deletes rather than moves the old installation: %s", destroyed)
		}
	}
}

// The installer runs from inside the package, and on an upgrade from cprest
// the package is inside the directory it is about to move: releases are
// unpacked under /var/lib/cprest/upgrade. SOURCE_DIR is an absolute path
// worked out before the move, so every use of it afterwards names somewhere
// that no longer exists -- which is how v0.2.0 took the old installation
// apart on a live server and then could not install the new one.
func TestTheMoveCarriesThePackageWithIt(t *testing.T) {
	script, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	loop := loopAround(t, string(script), `mv -- "$old" "$new"`)

	root := t.TempDir()
	legacyState := filepath.Join(root, "var/lib/cprest")
	stateDir := filepath.Join(root, "var/lib/gniza")
	source := filepath.Join(legacyState, "upgrade/v9.9.9/cprest-plugin")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "gniza-agent"), []byte("binary"), 0o600); err != nil {
		t.Fatal(err)
	}

	harness := fmt.Sprintf(`set -eu
say() { :; }
die() { printf 'die: %%s\n' "$*" >&2; exit 1; }
LEGACY_CONFIG_DIR=%q
CONFIG_DIR=%q
LEGACY_STATE_DIR=%q
STATE_DIR=%q
SOURCE_DIR=%q
%s
printf '%%s\n' "$SOURCE_DIR"
`,
		filepath.Join(root, "etc/cprest"), filepath.Join(root, "etc/gniza"),
		legacyState, stateDir, source, loop)

	out, err := exec.Command("sh", "-c", harness).CombinedOutput()
	if err != nil {
		t.Fatalf("the move failed: %v\n%s", err, out)
	}
	moved := strings.TrimSpace(string(out))
	want := filepath.Join(stateDir, "upgrade/v9.9.9/cprest-plugin")
	if moved != want {
		t.Errorf("the installer looks for the rest of the package at %s, want %s", moved, want)
	}
	if _, err := os.Stat(filepath.Join(moved, "gniza-agent")); err != nil {
		t.Errorf("the package is not where the installer will look for it: %v", err)
	}
}

// loopAround returns the whole `for` loop that contains marker, so a test
// exercises the script's own text rather than a copy of it that can drift.
// Anchoring on something inside the loop rather than on its first line
// matters here: the migration has two loops over the same list, and the one
// that only refuses is not the one that moves anything.
func loopAround(t *testing.T, script, marker string) string {
	t.Helper()
	at := strings.Index(script, marker)
	if at < 0 {
		t.Fatalf("install.sh no longer contains %q", marker)
	}
	start := strings.LastIndex(script[:at], "for ")
	if start < 0 {
		t.Fatalf("%q is not inside a loop", marker)
	}
	end := strings.Index(script[at:], "\n    done\n")
	if end < 0 {
		t.Fatalf("the loop holding %q does not end", marker)
	}
	return script[start : at+end+len("\n    done\n")]
}
