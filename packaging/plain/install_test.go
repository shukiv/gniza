package plain_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func read(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// TestTheScriptsParse: a script that does not parse installs nothing and
// says so in sh's words rather than this package's.
func TestTheScriptsParse(t *testing.T) {
	for _, name := range []string{"install.sh", "uninstall.sh"} {
		out, err := exec.Command("sh", "-n", name).CombinedOutput()
		if err != nil {
			t.Errorf("%s does not parse: %v: %s", name, err, out)
		}
	}
}

// TestTheServiceRunsThePlainPanelFromItsRoots: the unit the installer
// writes runs the agent as a plain server, with the roots read from the
// file an operator edits. A unit that named the roots itself would put
// the default back on every reinstall.
func TestTheServiceRunsThePlainPanelFromItsRoots(t *testing.T) {
	script := read(t, "install.sh")
	for _, want := range []string{
		"-standalone -panel=plain -plain-roots=${GNIZA_PLAIN_ROOTS}",
		"EnvironmentFile=/etc/gniza/plain.env",
		"GNIZA_PLAIN_ROOTS=${GNIZA_PLAIN_ROOTS:-/var/www,/srv,/opt}",
		`if [ ! -f "$ENV_FILE" ]; then`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the installer does not contain %q", want)
		}
	}
}

// TestTheInstallerAsksWhereTheBrowserInterfaceListens: the operator is
// offered this machine only, every address, or none; the answer is kept
// in the environment file the unit already reads, so the unit stays as
// it is; an answer given before the script, or on an earlier install,
// is not asked for again; and the password goes through the agent.
func TestTheInstallerAsksWhereTheBrowserInterfaceListens(t *testing.T) {
	script := read(t, "install.sh")
	for _, want := range []string{
		"Where should the browser interface listen?",
		"127.0.0.1:8443",
		"0.0.0.0:8443",
		`if [ -n "${GNIZA_WEB_LISTEN+set}" ]; then`,
		`elif grep -q '^GNIZA_WEB_LISTEN=' "$ENV_FILE"; then`,
		"GNIZA_WEB_LISTEN=$WEB_LISTEN",
		`-web-set-password -web-password-file "$WEB_DIR/password"`,
		"stty -echo < /dev/tty",
		"stty echo < /dev/tty",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the installer does not contain %q", want)
		}
	}
	if strings.Contains(script, "-web-listen") {
		t.Error("the unit names the address itself; it should come from the environment file")
	}
}

// TestTheUninstallerIsWhereTheRemoveButtonLooks: the node runs
// /usr/local/share/gniza/uninstall.sh for a panel that is not
// DirectAdmin, and the installer has to have put it there.
func TestTheUninstallerIsWhereTheRemoveButtonLooks(t *testing.T) {
	script := read(t, "install.sh")
	if !strings.Contains(script, `install_binary 0755 "$SOURCE_DIR/uninstall.sh" "$SHARE_DIR/uninstall.sh"`) ||
		!strings.Contains(script, "SHARE_DIR=/usr/local/share/gniza") {
		t.Error("the installer does not place uninstall.sh at /usr/local/share/gniza/uninstall.sh")
	}
	uninstall := read(t, "uninstall.sh")
	if !strings.Contains(uninstall, `retire "$SHARE_DIR" share`) ||
		!strings.Contains(uninstall, "retire /usr/local/bin/gniza-agent gniza-agent") {
		t.Error("the uninstaller does not take off what the installer put on")
	}
}

// TestAPanelServerIsRefused: the package for a server with no panel must
// not be installed beside a panel's plugin. get.sh chooses correctly; a
// tarball run by hand on the wrong machine is the case here.
func TestAPanelServerIsRefused(t *testing.T) {
	script := read(t, "install.sh")
	if !strings.Contains(script, "if [ -d /usr/local/cpanel ] || [ -d /usr/local/directadmin ]; then") {
		t.Error("the installer does not refuse a server that has a panel")
	}
}

// TestEveryExecutableArrivesByRename keeps the rule the other installers
// keep: a binary is written beside its target and renamed into place.
func TestEveryExecutableArrivesByRename(t *testing.T) {
	script := read(t, "install.sh")
	for number, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "install_binary()") {
			continue
		}
		if strings.HasPrefix(trimmed, "install -m 0755") || strings.HasPrefix(trimmed, "cp ") {
			t.Errorf("line %d writes an executable in place: %s", number+1, trimmed)
		}
	}
}
