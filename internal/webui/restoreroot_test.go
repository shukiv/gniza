package webui_test

import (
	"html"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// closedDir is a directory nobody but its owner can write to, all the
// way up, which a test's own temporary directory is not on every machine.
func closedDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gniza-restore-root-")
	if err != nil {
		t.Skipf("no closed directory to test in: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// Settings says where a restore is rebuilt and how much room is there,
// takes another directory, and says why when it will not have one.
func TestSettingsMovesWhereRestoresAreRebuilt(t *testing.T) {
	client, _, engine := newUI(t)
	staging := engine.Settings().StagingRoot

	_, page := get(t, client, "/settings?tab=storage")
	for _, want := range []string{"Where restores are rebuilt", "the staging directory",
		`action="?p=settings/restore-root"`, `name="restore_root"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the page does not say %q", want)
		}
	}

	post := func(path string) string {
		t.Helper()
		resp, err := client.PostForm("http://ui/settings/restore-root", url.Values{
			"csrf": {csrfToken(t, page)}, "restore_root": {path},
		})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		said, err := url.QueryUnescape(resp.Header.Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		return said
	}

	root := closedDir(t)
	open := filepath.Join(root, "open")
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	said := post(filepath.Join(open, "restores"))
	if !strings.Contains(said, "kind=error") || !strings.Contains(said, "can be written by others") {
		t.Errorf("a directory anybody can write to was not refused with a reason: %q", said)
	}
	if engine.RestoreRoot() != staging {
		t.Fatalf("a refused directory moved the restores to %s", engine.RestoreRoot())
	}

	larger := filepath.Join(root, "gniza-restores")
	said = post("  " + larger + "  ")
	if !strings.Contains(said, "kind=ok") || !strings.Contains(said, larger) {
		t.Errorf("the answer does not name the directory: %q", said)
	}
	if engine.RestoreRoot() != larger {
		t.Fatalf("restores are rebuilt in %s", engine.RestoreRoot())
	}
	_, page = get(t, client, "/settings?tab=storage")
	if !strings.Contains(page, `value="`+html.EscapeString(larger)+`"`) {
		t.Error("the page does not show the directory it was given")
	}
	if !strings.Contains(page, staging) {
		t.Error("the page no longer says where staging is")
	}

	said = post("")
	if !strings.Contains(said, "kind=ok") || engine.RestoreRoot() != staging {
		t.Errorf("an empty directory did not go back to staging: %q, %s", said, engine.RestoreRoot())
	}
}

// Moving where root writes files is not something a page that was not
// asked by this session may do.
func TestTheRestoreDirectoryIsNotMovedWithoutTheSessionsToken(t *testing.T) {
	client, _, engine := newUI(t)
	resp, err := client.PostForm("http://ui/settings/restore-root", url.Values{
		"restore_root": {filepath.Join(closedDir(t), "restores")},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if engine.RestoreRoot() != engine.Settings().StagingRoot {
		t.Errorf("a request without a token moved the restores to %s", engine.RestoreRoot())
	}
}

// How often accounts are rehearsed is chosen on the page, and a page that
// is saved without having chosen leaves it where it was.
func TestSettingsSaysHowOftenAccountsAreRehearsed(t *testing.T) {
	client, _, engine := newUI(t)
	_, page := get(t, client, "/settings")
	for _, want := range []string{`name="rehearse_days"`, `<option value="30" selected>`,
		"Only when I ask"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page does not say %q", want)
		}
	}
	save := func(chosen string) int {
		t.Helper()
		values := url.Values{"csrf": {csrfToken(t, page)}, "max_concurrent": {"1"}}
		if chosen != "" {
			values.Set("rehearse_days", chosen)
		}
		resp, err := client.PostForm("http://ui/settings/save", values)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		settings, err := engine.Store().Settings()
		if err != nil {
			t.Fatal(err)
		}
		return settings.RehearseDays
	}
	if got := save("-1"); got != -1 {
		t.Errorf("never was saved as %d", got)
	}
	_, page = get(t, client, "/settings")
	if !strings.Contains(page, `<option value="-1" selected>`) {
		t.Error("the page does not show that rehearsals are left to the operator")
	}
	for _, refused := range []string{"", "0", "-7", "9999", "soon"} {
		if got := save(refused); got != -1 {
			t.Errorf("%q changed the setting to %d", refused, got)
		}
	}
	if got := save("45"); got != 45 {
		t.Errorf("45 days was saved as %d", got)
	}
	_, page = get(t, client, "/settings")
	if !strings.Contains(page, `<option value="45" selected>Every 45 days</option>`) {
		t.Error("a period the form does not offer is not shown as the one in force")
	}
}
