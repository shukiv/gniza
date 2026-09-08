package directadmin_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func read(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// setting reads one key=value line out of plugin.conf, ignoring the
// comments around it.
func setting(t *testing.T, key string) (string, bool) {
	t.Helper()
	for _, line := range strings.Split(read(t, "plugin.conf"), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		if name, value, found := strings.Cut(line, "="); found && name == key {
			return value, true
		}
	}
	return "", false
}

// A plugin directory on disk is not a plugin. DirectAdmin serves one only
// when its configuration says it is installed and active and gives it an
// id, and it links to one only when the level has a *_txt.html to name it
// in the menu. Every one of these was learned from a page that answered
// 404 on a real 1.709 host.
func TestDirectAdminHasEverythingItNeedsToServeThePlugin(t *testing.T) {
	for key, want := range map[string]string{
		"id": "gniza", "active": "yes", "installed": "yes",
	} {
		if got, found := setting(t, key); !found || got != want {
			t.Errorf("plugin.conf says %s=%q, wanted %q", key, got, want)
		}
	}
	for _, path := range []string{"hooks/admin_txt.html", "admin/index.html"} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s is missing, so DirectAdmin serves nothing: %v", path, err)
		}
	}
}

// The account the page runs as, the account the installer creates, and
// the account the service gives the socket to must be one account. If any
// two of them drift, the page connects to a socket it cannot open, and
// the failure is a permission error that names none of this.
func TestOneAccountRunsThePageOwnsTheSocketAndIsCreated(t *testing.T) {
	owner, found := setting(t, "admin_run_as")
	if !found || owner == "" {
		t.Fatal("plugin.conf does not say which account DirectAdmin runs the page as")
	}
	if owner == "root" {
		t.Fatal("plugin.conf runs the page as root, which DirectAdmin refuses and ADR 0020 refuses")
	}

	installer := read(t, "install.sh")
	if !strings.Contains(installer, "PLUGIN_USER="+owner) {
		t.Errorf("the installer does not create %q", owner)
	}
	if !strings.Contains(installer, `useradd`) {
		t.Error("the installer never creates the account the page runs as")
	}

	// The service's own default has to name the same account: it is what
	// the socket ends up owned by.
	agent := read(t, "../../cmd/agent/main.go")
	flagValue := regexp.MustCompile(`"directadmin-plugin-user",\s*"([^"]*)"`).FindStringSubmatch(agent)
	if flagValue == nil {
		t.Fatal("gniza-agent no longer has a -directadmin-plugin-user default to compare")
	}
	if flagValue[1] != owner {
		t.Errorf("the service gives the socket to %q and DirectAdmin runs the page as %q",
			flagValue[1], owner)
	}
}

// The account's own page must run as the account. That is what lets Gniza
// read the customer's identity from the socket rather than believing what
// the page says about it, and DirectAdmin does that only when nothing
// overrides it.
func TestTheAccountsPageStillRunsAsTheAccount(t *testing.T) {
	if owner, found := setting(t, "user_run_as"); found {
		t.Errorf("plugin.conf runs a customer's page as %q, so the socket can no longer say who they are", owner)
	}
}

// The session is a bearer credential for as long as it lives. It goes to
// Gniza in a header and to DirectAdmin from there, and nowhere else --
// not into the page, not into a URL, not into an error.
func TestTheAdminPageNeverPrintsTheSession(t *testing.T) {
	page := read(t, "admin/index.html")
	for _, line := range strings.Split(page, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !strings.Contains(trimmed, "SESSION_ID") && !strings.Contains(trimmed, "SESSION_KEY") {
			continue
		}
		if !strings.Contains(trimmed, "--header") {
			t.Errorf("the session is used somewhere other than the header it travels in: %q", trimmed)
		}
	}
	if strings.Contains(page, "$SESSION_ID") && !strings.Contains(page, "${SESSION_ID:-}") {
		t.Error("the page reads the session without a default, so a request without one aborts the script")
	}
}
