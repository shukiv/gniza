package directadmin_test

import (
	"os"
	"path"
	"path/filepath"
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

// DirectAdmin reads the request body itself and hands the plugin the
// whole url-encoded form in POST, leaving CONTENT_LENGTH empty and stdin
// at end of file. A page that reads stdin therefore forwards an empty
// body to every save, the browser reloads, and nothing was written --
// which looks exactly like success. Measured on 1.709.
func TestTheFormIsForwardedFromWhereDirectAdminPutsIt(t *testing.T) {
	page := read(t, "admin/index.html")
	if !strings.Contains(page, `--data-raw "${POST:-}"`) {
		t.Error("the page does not forward the form DirectAdmin handed it in POST")
	}
	for _, reading := range []string{"head -c", "CONTENT_LENGTH", "--data-binary @-"} {
		if strings.Contains(stripComments(page), reading) {
			t.Errorf("the page still reads the request body with %q, which DirectAdmin never fills", reading)
		}
	}
	// And the method is left to curl. Forcing it holds POST across the
	// redirect the form answers with, so the browser posted again to a
	// page that only answers GET: pressing Run now on a schedule did the
	// work and then printed "Method Not Allowed" inside DirectAdmin's
	// chrome. Without the flag curl turns the 303 into the GET it means.
	if regexp.MustCompile(`(--request|-X)\s+POST`).MatchString(stripComments(page)) {
		t.Error("the page forces the method, which re-posts to the page the redirect lands on")
	}
}

// stripComments leaves the code, so a comment explaining what was
// measured does not read as the code doing it.
func stripComments(script string) string {
	var kept []string
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// Gniza drives restic and does not carry a copy of it, so an installer
// that does not put one there leaves a service that cannot start: on a
// real 1.709 host the unit came up and died on
// `run restic version: exec: "restic": executable file not found in
// $PATH`, once every five seconds. WHM's installer has fetched and
// verified one since the beginning; this one has to do the same, and it
// has to check the download against restic's own published checksum
// before anything becomes executable.
func TestTheInstallerPutsResticThereBecauseNothingWorksWithoutIt(t *testing.T) {
	script := read(t, "install.sh")
	for _, want := range []string{
		"restic",
		"SHA256SUMS",
		"sha256sum -c",
		"install -m 0755",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the installer does not mention %q", want)
		}
	}
	// The same version the rest of Gniza is built against. A different
	// one would be a repository format nothing else on the server reads.
	version := regexp.MustCompile(`RESTIC_VERSION=([0-9.]+)`).FindStringSubmatch(script)
	if version == nil {
		t.Fatal("the installer does not pin a restic version")
	}
	whm := read(t, "../whm/install.sh")
	if want := "RESTIC_VERSION=" + version[1]; !strings.Contains(whm, want) {
		t.Errorf("this installs restic %s and WHM's installs another", version[1])
	}
	// And it does not fetch one over a plaintext connection, or from
	// anywhere but restic's own releases.
	for _, url := range regexp.MustCompile(`https?://\S+`).FindAllString(script, -1) {
		if !strings.HasPrefix(url, "https://github.com/restic/restic/releases/") {
			t.Errorf("the installer downloads from %s", url)
		}
	}
}

// The plugin directory has to be reachable by DirectAdmin and by the
// account DirectAdmin runs the page as. This script sets umask 077 --
// correctly, since it also writes the service's state directories -- so
// every directory it creates without a mode of its own comes out 0700
// root:root, and on a real 1.709 host that produced a plugin that
// installed cleanly and answered 404: the page was there and nothing
// could traverse to it.
//
// WHM's installer learned this already; its comment says the modes are
// explicit because the script runs under umask 077 and these are not
// secret. Nothing under the plugin directory is secret either.
func TestThePluginDirectoriesAreReachableUnderUmask077(t *testing.T) {
	script := read(t, "install.sh")
	if !strings.Contains(script, "umask 077") {
		t.Fatal("this test is about a umask the installer no longer sets")
	}
	// Every directory the plugin needs, named as the installer names it.
	// A shell continuation is still one statement, so they are joined
	// before the directories are looked for in them.
	joined := strings.ReplaceAll(script, "\\\n", " ")
	made := strings.Join(
		regexp.MustCompile(`install -d -m 07[05]5[^\n]*`).FindAllString(joined, -1), "\n")
	for _, dir := range []string{
		`"$PLUGIN_DIR"`,
		`"$PLUGIN_DIR/hooks"`,
		`"$PLUGIN_DIR/admin"`,
		`"$PLUGIN_DIR/user"`,
		`"$PLUGIN_DIR/images"`,
	} {
		if !strings.Contains(made, dir) {
			t.Errorf("%s is created without a mode, so umask 077 makes it 0700", dir)
		}
	}
	// And a bare mkdir under the plugin directory is the bug coming back.
	if regexp.MustCompile(`mkdir -p[^\n]*\$PLUGIN_DIR`).MatchString(script) {
		t.Error("the plugin directory is still created with mkdir, which umask 077 makes unreachable")
	}
}

// Every form in Gniza answers with a redirect: post, then see the page
// again with a message on it. WHM's CGI passes headers through, so the
// browser follows it. DirectAdmin does not give a plugin any way to emit
// a header -- the script's output is the page body and nothing else -- so
// a redirect arrives at the browser as its own empty body, and every
// button on the DirectAdmin page produced a blank white screen: adding a
// destination worked, noting its recovery key worked, testing it worked,
// and all three looked like nothing had happened.
//
// So the script follows the redirect itself and prints what it lands on.
func TestARedirectIsFollowedHereBecauseDirectAdminCannotPassOneOn(t *testing.T) {
	page := read(t, "admin/index.html")
	if !regexp.MustCompile(`--location\b`).MatchString(page) {
		t.Error("the page does not follow the redirect every form answers with")
	}
	// And it does not follow one forever. A loop between two routes would
	// otherwise hold a DirectAdmin request open until it timed out.
	if !regexp.MustCompile(`--max-redirs [0-9]+`).MatchString(page) {
		t.Error("the page follows redirects without a limit")
	}
	// The redirects are Gniza's own, over the socket. Following one off
	// this server would replay the administrator's DirectAdmin session to
	// wherever it pointed, so the request must stay on the unix socket.
	if strings.Contains(page, "--proto") == false && !strings.Contains(page, "--unix-socket") {
		t.Error("the page does not pin the request to Gniza's socket")
	}
}

// DirectAdmin lists a plugin under Extra Features and links to it inside
// the skin only when the level's *_txt.html is a link. Ours was the plain
// words "Gniza Backups", so Evolution had nothing to point at: the page
// could be reached by typing its address, which serves it outside the
// skin with none of DirectAdmin's own chrome around it, and that is what
// it looked like on a real 1.709 host. JetBackup ships an anchor in
// admin_txt.html and an anchor with an icon in admin_img.html, and is
// listed where an administrator looks for it.
func TestTheMenuEntriesAreLinksDirectAdminCanFollow(t *testing.T) {
	const target = "/CMD_PLUGINS_ADMIN/gniza/index.html"
	for _, name := range []string{"hooks/admin_txt.html", "hooks/admin_img.html"} {
		entry := read(t, name)
		if !strings.Contains(entry, `href="`+target+`"`) {
			t.Errorf("%s does not link to %s", name, target)
		}
		if !strings.Contains(entry, "Gniza") {
			t.Errorf("%s does not name the plugin", name)
		}
	}
	// The tile carries the icon, and the icon has to be a file the
	// installer puts where that address resolves.
	img := read(t, "hooks/admin_img.html")
	icon := regexp.MustCompile(`src="(/CMD_PLUGINS_ADMIN/gniza/images/[^"]+)"`).FindStringSubmatch(img)
	if icon == nil {
		t.Fatal("admin_img.html has no icon under the plugin's own images directory")
	}
	if _, err := os.Stat(strings.TrimPrefix(icon[1], "/CMD_PLUGINS_ADMIN/gniza/")); err != nil {
		t.Errorf("the icon %s is not in the package: %v", icon[1], err)
	}
	install := read(t, "install.sh")
	if !strings.Contains(install, "images") {
		t.Error("the installer does not install the images directory")
	}
	// And it installs both menu files. A glob that matched only
	// *_txt.html left the tile behind, which is the half of the pair
	// Evolution actually draws.
	for _, name := range []string{"admin_txt.html", "admin_img.html"} {
		matched := false
		for _, glob := range regexp.MustCompile(`hooks/(\*[^"\s]*\.html)`).FindAllStringSubmatch(install, -1) {
			if ok, _ := path.Match(glob[1], name); ok {
				matched = true
			}
		}
		if !matched {
			t.Errorf("the installer's hooks glob does not cover %s", name)
		}
	}
}

// Evolution opens a plugin through its own wrapper, /evo/plugin?src=...,
// and that wrapper arrives as a POST with no form in it -- a page view,
// not a submission. Forwarding it as a post asked Gniza to post to the
// overview, which only answers GET, and the administrator got
//
//	Method Not Allowed
//
// inside DirectAdmin's chrome. What tells the two apart is whether
// DirectAdmin put a form in POST: a real submission always carries at
// least the token, and a page view carries nothing.
func TestAPageViewIsNotForwardedAsASubmission(t *testing.T) {
	page := read(t, "admin/index.html")
	// The method is decided by the form as well as by REQUEST_METHOD.
	if !regexp.MustCompile(`\[ "\$METHOD" = POST \] && \[ -n "\$\{POST:-\}" \]`).MatchString(page) {
		t.Error("the page forwards a post without asking whether there is a form in it")
	}
}

// A page that asks the plugin script for a font gets DirectAdmin's skin
// wrapped around a woff2, which the browser drops -- the console on 1.709
// said so for all five faces. DirectAdmin serves a plugin's images
// directory as files, so the fonts have to be installed there.
func TestTheTypefacesAreInstalledWhereDirectAdminServesFiles(t *testing.T) {
	installer := read(t, "install.sh")
	if !strings.Contains(installer, "images/fonts") {
		t.Error("the installer does not put the typefaces where DirectAdmin serves them")
	}
	if !regexp.MustCompile(`install -m 0644 "\$font"`).MatchString(installer) {
		t.Error("the typefaces are not installed readable")
	}
	// And the package has to carry them, or the installer has nothing to
	// install: they live once in the repository, embedded in the agent
	// for WHM and copied into the package for DirectAdmin.
	makefile, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(makefile), "internal/webui/fonts/*.woff2") {
		t.Error("the DirectAdmin package does not carry the typefaces")
	}
}

// DirectAdmin's own plugin manager installs a plugin from a tar.gz: it
// unpacks it into the plugins directory and runs scripts/install.sh as
// root. Every plugin on a DirectAdmin server arrives that way, and one
// that has no such script is a directory the manager unpacks and then
// does nothing with.
func TestThePluginManagerFindsSomethingToRun(t *testing.T) {
	for _, name := range []string{"install", "uninstall", "update"} {
		path := filepath.Join("scripts", name+".sh")
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("%s is missing, so the plugin manager cannot %s: %v", path, name, err)
			continue
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s is not executable", path)
		}
		if !strings.HasPrefix(read(t, path), "#!/bin/sh") {
			t.Errorf("%s has no interpreter line", path)
		}
	}
}

// One installer, called from both ways in. A second copy of the work in
// the plugin manager's script is a second copy to keep in step, and the
// one that drifts is the one nobody runs by hand.
func TestThePluginManagerScriptsRunTheOneInstaller(t *testing.T) {
	for _, name := range []string{"install", "update"} {
		body := read(t, filepath.Join("scripts", name+".sh"))
		if !strings.Contains(body, "install.sh") {
			t.Errorf("scripts/%s.sh does not run the installer", name)
		}
		for _, own := range []string{"useradd", "systemctl", "restic"} {
			if strings.Contains(body, own) {
				t.Errorf("scripts/%s.sh does %s itself; that belongs in the one installer", name, own)
			}
		}
	}
	if !strings.Contains(read(t, "scripts/uninstall.sh"), "uninstall.sh") {
		t.Error("scripts/uninstall.sh does not run the uninstaller")
	}
}

// Everything a plugin script prints is the page DirectAdmin shows after
// the install, wrapped in its skin. Plain text arrives as one run-on
// line, so the transcript is printed as preformatted HTML.
func TestThePluginManagerIsToldTheOutcomeInHTML(t *testing.T) {
	for _, name := range []string{"install", "uninstall", "update"} {
		body := read(t, filepath.Join("scripts", name+".sh"))
		if !strings.Contains(body, "<pre>") || !strings.Contains(body, "</pre>") {
			t.Errorf("scripts/%s.sh prints a transcript DirectAdmin will run together", name)
		}
	}
}

// The plugin manager has already unpacked the plugin into the directory
// the installer installs to, and runs the installer from inside it.
// Copying those files over themselves fails -- "are the same file" -- so
// the installer has to recognise where it is standing.
func TestTheInstallerRecognisesFilesAlreadyInPlace(t *testing.T) {
	body := read(t, "install.sh")
	if !regexp.MustCompile(`SOURCE_DIR/plugin\.conf`).MatchString(body) {
		t.Error("the installer cannot tell it was run from inside the plugin directory")
	}
	if !regexp.MustCompile(`(?m)^PLUGIN_DIR=`).MatchString(body) {
		t.Error("the installer no longer names the plugin directory")
	}
}

// The card in the plugin manager shows the version out of plugin.conf.
// A version that never changes reads as a plugin that is never updated,
// so the packaged copy carries the version that was built.
func TestThePluginManagerIsToldWhichVersionThisIs(t *testing.T) {
	makefile := read(t, filepath.Join("..", "..", "Makefile"))
	rewrites := regexp.MustCompile(`sed 's\|\^version=\.\*\|version=\$\(VERSION\)\|'`).
		FindAllString(makefile, -1)
	// Both ways in: the release tarball an administrator unpacks, and the
	// tar.gz the plugin manager takes.
	if len(rewrites) < 2 {
		t.Errorf("%d packaging steps write the built version into plugin.conf, wanted both", len(rewrites))
	}
	if !strings.Contains(makefile, "directadmin-plugin:") {
		t.Error("nothing builds the tar.gz DirectAdmin's plugin manager installs")
	}
}

// A server set up from the release tarball must still be updatable and
// removable from the page an administrator goes to for that. So the
// installer puts the plugin manager's scripts -- and what they run -- in
// place too, and both ways in leave the same directory behind.
func TestAHandInstallLeavesThePluginManagerSomethingToRun(t *testing.T) {
	body := read(t, "install.sh")
	for _, want := range []string{
		`install -d -m 0755 "$PLUGIN_DIR/scripts"`,
		`install -m 0755 "$SOURCE_DIR/install.sh" "$PLUGIN_DIR/install.sh"`,
		`install -m 0755 "$SOURCE_DIR/gniza-agent" "$PLUGIN_DIR/gniza-agent"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the installer does not leave %s behind", want)
		}
	}
}

// DirectAdmin's plugin manager unpacks the archive into the directory it
// makes for the plugin, so the archive holds the plugin's own files at
// its root and no directory wrapping them. Its own example plugin is
// packed that way -- hello_world.tar.gz begins "admin/ ... plugin.conf"
// -- and so is the skeleton Installatron publishes for the update
// button. An archive with a directory around it installs a plugin whose
// plugin.conf is one level too deep, which the manager reads as no
// plugin at all.
func TestThePluginManagerGetsThePluginAtTheArchiveRoot(t *testing.T) {
	makefile := read(t, filepath.Join("..", "..", "Makefile"))
	recipe := regexp.MustCompile(`(?s)\ndirectadmin-plugin:.*?\n\n`).FindString(makefile)
	if recipe == "" {
		t.Fatal("no directadmin-plugin recipe to read")
	}
	tarLine := regexp.MustCompile(`(?s)\ttar -C .*?\n\t*-czf [^\n]*\n(\t[^\n]*\n)*`).
		FindString(recipe)
	if tarLine == "" {
		t.Fatalf("the recipe does not pack an archive:\n%s", recipe)
	}
	if !regexp.MustCompile(`(^|[ \t\\])plugin\.conf([ \t\\]|$)`).MatchString(tarLine) {
		t.Errorf("the archive does not carry plugin.conf at its root:\n%s", tarLine)
	}
}
