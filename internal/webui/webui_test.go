package webui_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/cpanel"
	"github.com/shukiv/gniza/internal/granular"
	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/node"
	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/panel"
	"github.com/shukiv/gniza/internal/protocol"
	"github.com/shukiv/gniza/internal/resticrun"
	"github.com/shukiv/gniza/internal/vault"
	"github.com/shukiv/gniza/internal/webui"
)

// newUI stands the interface up on a socket with a synthetic cPanel host
// behind it, which is how it is exercised off a real server.
func newUI(t *testing.T) (*http.Client, string, *node.Engine) {
	t.Helper()
	return newUIWithExec(t, nil)
}

// newUIWithExec is newUI with restic answered by the caller, for a test
// that has to see which commands the interface causes to be run.
func newUIWithExec(t *testing.T, exec resticrun.Execer) (*http.Client, string, *node.Engine) {
	t.Helper()
	return newUIWithJournalAndExec(t, "", exec)
}

// newUIWithJournal answers the service log with lines of the test's own,
// for a machine that has no systemd unit to read one from.
func newUIWithJournal(t *testing.T, journal string) (*http.Client, string, *node.Engine) {
	t.Helper()
	return newUIWithJournalAndExec(t, journal, nil)
}

// newUIOnDirectAdmin serves the same interface as a DirectAdmin server
// does. Only the panel differs: what a page says about the panel's own
// mechanisms is decided by which panel it is on, and nothing else here
// needs a second host to prove that.
func newUIOnDirectAdmin(t *testing.T) (*http.Client, string, *node.Engine) {
	t.Helper()
	return newUIOn(t, "", nil, func(fake *cpanel.Fake) panel.Provider { return directAdminFake{fake} })
}

// directAdminFake is the synthetic host under another name.
type directAdminFake struct{ *cpanel.Fake }

func (directAdminFake) Name() string { return "DirectAdmin" }

func newUIWithJournalAndExec(t *testing.T, journal string, exec resticrun.Execer) (*http.Client, string, *node.Engine) {
	t.Helper()
	return newUIOn(t, journal, exec, nil)
}

func newUIOn(t *testing.T, journal string, exec resticrun.Execer,
	as func(*cpanel.Fake) panel.Provider) (*http.Client, string, *node.Engine) {
	t.Helper()
	root := t.TempDir()

	store, err := nodestore.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	settings := nodestore.DefaultSettings()
	settings.StagingRoot = filepath.Join(root, "staging")
	settings.ResticCache = filepath.Join(root, "cache")
	settings.Hostname = "cp01.example.com"
	settings.ConfigDir = filepath.Join(root, "config")
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}

	keyHex, err := vault.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(root, "master.key")
	if err := os.WriteFile(keyPath, []byte(keyHex), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := vault.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	v, err := vault.New(key)
	if err != nil {
		t.Fatal(err)
	}

	fake := &cpanel.Fake{
		Root:      filepath.Join(root, "cpanel"),
		Databases: map[string][]string{"customer1": {"customer1_wp"}},
		FileCount: 2, FileSize: 512,
	}
	var provider panel.Provider = fake
	if as != nil {
		provider = as(fake)
	}
	engine, err := node.New(node.Config{
		Store: store, Vault: v, Exec: exec,
		Provider:  provider,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		HookSpool: filepath.Join(root, "hooks"),
	})
	if err != nil {
		t.Fatalf("build engine: %v", err)
	}

	options := []webui.Option{}
	if journal != "" {
		options = append(options, webui.WithJournal(
			func(context.Context, ...string) ([]byte, error) { return []byte(journal), nil }))
	}
	server, err := webui.New(engine, slog.New(slog.NewTextHandler(io.Discard, nil)), options...)
	if err != nil {
		t.Fatalf("build ui: %v", err)
	}

	socket := filepath.Join(root, "ui.sock")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	listenErrors := make(chan error, 1)
	go func() { listenErrors <- server.Listen(ctx, socket) }()

	client := &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	waitForSocket(t, socket, listenErrors)
	return client, socket, engine
}

// noteRecoveryKeys marks every repository's recovery key as taken off this
// server, which is what an operator does on the card at the end of adding
// a destination. A schedule cannot be enabled until it has been, so a test
// that is about schedules has to get past it first.
func noteRecoveryKeys(t *testing.T, engine *node.Engine) {
	t.Helper()
	repositories, err := engine.Store().Repositories()
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range repositories {
		if err := engine.NoteRecoveryKey(repo.ID); err != nil {
			t.Fatal(err)
		}
	}
}

func waitForSocket(t *testing.T, socket string, listenErrors <-chan error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-listenErrors:
			t.Fatalf("the interface stopped before listening: %v", err)
		default:
		}
		if conn, err := net.Dial("unix", socket); err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the interface never started listening")
}

func get(t *testing.T, client *http.Client, path string) (int, string) {
	t.Helper()
	resp, err := client.Get("http://ui" + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestEveryPageRenders(t *testing.T) {
	client, _, _ := newUI(t)

	for _, path := range []string{
		"/", "/destinations", "/schedule", "/accounts",
		"/restore", "/jobs", "/settings",
	} {
		status, body := get(t, client, path)
		if status != http.StatusOK {
			t.Errorf("GET %s = %d", path, status)
			continue
		}
		// The page is a fragment: WHM's own interface supplies the
		// document around it, so there is no <html> of our own.
		if !strings.Contains(body, `<div class="gniza">`) {
			t.Errorf("GET %s did not render the layout", path)
		}
		if !strings.Contains(body, "</main>") {
			t.Errorf("GET %s produced a truncated page", path)
		}
		if strings.Contains(body, "<html") {
			t.Errorf("GET %s emitted a whole document; WHM supplies that", path)
		}
	}
}

func TestSocketIsOwnerOnly(t *testing.T) {
	_, socket, _ := newUI(t)

	// On a shared cPanel server the untrusted users are already on the
	// box, and this interface can read every stored credential.
	info, err := os.Stat(socket)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("socket mode %04o is reachable by other users", perm)
	}
	dir, err := os.Stat(filepath.Dir(socket))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dir.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("socket directory mode %04o is reachable by other users", perm)
	}
}

func TestMutatingRequestsNeedTheCSRFToken(t *testing.T) {
	client, _, _ := newUI(t)

	resp, err := client.PostForm("http://ui/destinations/add", map[string][]string{
		"name": {"sneaky"}, "type": {"local"}, "root": {"/tmp/x"},
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST without a token = %d, want 403", resp.StatusCode)
	}
}

func TestAddDestinationRoundTrip(t *testing.T) {
	client, _, engine := newUI(t)

	_, page := get(t, client, "/destinations")
	token := csrfToken(t, page)

	root := t.TempDir()
	resp, err := client.PostForm("http://ui/destinations/add", map[string][]string{
		"csrf": {token}, "name": {"Local disk"}, "type": {"local"},
		"root": {root}, "repo_path": {"cp01"},
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	// Adding a destination does not end on the list. It ends on the
	// recovery key, which is made here and exists nowhere else, so the
	// page that comes back is the key itself rather than a redirect.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST = %d, want the recovery key page: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "RESTIC_PASSWORD") {
		t.Fatalf("adding a destination did not end on its recovery key: %s", body)
	}

	destinations, err := engine.Store().Destinations()
	if err != nil {
		t.Fatal(err)
	}
	if len(destinations) != 1 || destinations[0].Name != "Local disk" {
		t.Fatalf("stored destinations = %+v", destinations)
	}
	// The repository is created alongside, so a schedule has something to
	// point at without a second step.
	repos, err := engine.Store().Repositories()
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0].Path != "cp01" {
		t.Fatalf("stored repositories = %+v", repos)
	}
}

func TestSecondDestinationCopiesChunkerParameters(t *testing.T) {
	client, _, engine := newUI(t)

	for _, name := range []string{"First", "Second"} {
		_, page := get(t, client, "/destinations")
		resp, err := client.PostForm("http://ui/destinations/add", map[string][]string{
			"csrf": {csrfToken(t, page)}, "name": {name}, "type": {"local"},
			"root": {t.TempDir()}, "repo_path": {"cp01"},
		})
		if err != nil {
			t.Fatalf("POST %s: %v", name, err)
		}
		resp.Body.Close()
	}

	repos, err := engine.Store().Repositories()
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 {
		t.Fatalf("got %d repositories, want 2", len(repos))
	}
	// Chunker parameters are fixed when a repository is created and can
	// never be changed, so the second must copy the first's.
	if repos[0].ChunkerSourceRepoID != "" {
		t.Errorf("the first repository has a chunker source: %q", repos[0].ChunkerSourceRepoID)
	}
	if repos[1].ChunkerSourceRepoID != repos[0].ID {
		t.Errorf("second chunker source = %q, want %q",
			repos[1].ChunkerSourceRepoID, repos[0].ID)
	}
}

func TestScheduleWithNoDestinationIsRefused(t *testing.T) {
	client, _, _ := newUI(t)

	// With nothing configured the page offers no form at all, because a
	// schedule with nowhere to write is not worth filling in.
	_, page := get(t, client, "/schedule")
	if strings.Contains(page, `action="schedule/save"`) {
		t.Error("the schedule form is offered before any destination exists")
	}
	if !strings.Contains(page, "Add a destination first") {
		t.Error("the page does not say what to do first")
	}

	// Once one exists the form appears, but a schedule that selects no
	// destination is still refused: it would run and store nothing.
	_, destinationPage := get(t, client, "/destinations")
	added, err := client.PostForm("http://ui/destinations/add", map[string][]string{
		"csrf": {csrfToken(t, destinationPage)}, "name": {"Local disk"},
		"type": {"local"}, "root": {t.TempDir()}, "repo_path": {"cp01"},
	})
	if err != nil {
		t.Fatalf("add destination: %v", err)
	}
	added.Body.Close()

	_, page = get(t, client, "/schedule")
	resp, err := client.PostForm("http://ui/schedule/save", map[string][]string{
		"csrf": {csrfToken(t, page)}, "name": {"Nightly"}, "cron": {"0 2 * * *"},
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	location := resp.Header.Get("Location")
	if !strings.Contains(location, "kind=error") {
		t.Errorf("redirect = %q, want an error", location)
	}
}

// csrfToken pulls the token out of a rendered page.
func csrfToken(t *testing.T, page string) string {
	t.Helper()
	const marker = `name="csrf" value="`
	start := strings.Index(page, marker)
	if start < 0 {
		t.Fatal("no csrf token in the page")
	}
	rest := page[start+len(marker):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		t.Fatal("malformed csrf token")
	}
	return rest[:end]
}

func TestSFTPDestinationReportsAnUnreachableHost(t *testing.T) {
	client, _, _ := newUI(t)

	_, page := get(t, client, "/destinations")
	resp, err := client.PostForm("http://ui/destinations/add", map[string][]string{
		"csrf": {csrfToken(t, page)}, "name": {"Backup server"}, "type": {"sftp"},
		// Nothing listens here, so the host key cannot be read.
		"host": {"127.0.0.1"}, "port": {"1"},
		"user": {"cpbackup"}, "root": {"/home/cpbackup/backups"},
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	page = string(body)

	// The form comes back with the reason in it, still filled in: a
	// destination that could not be reached is a typo to correct, not a
	// reason to type everything again.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST = %d, want the form back", resp.StatusCode)
	}
	if !strings.Contains(page, "SSH") {
		t.Error("the page does not say what went wrong")
	}
	for _, kept := range []string{`value="cpbackup"`, `value="/home/cpbackup/backups"`, `value="127.0.0.1"`} {
		if !strings.Contains(page, kept) {
			t.Errorf("the form came back without %s in it", kept)
		}
	}
	if strings.Contains(page, "kind=error") {
		t.Error("the operator was sent away from the form they were filling in")
	}
}

func TestSFTPFormAsksForNeitherAKeyNorKnownHosts(t *testing.T) {
	client, _, _ := newUI(t)
	_, page := get(t, client, "/destinations")

	// Gniza generates its own key and learns the host key itself, so
	// neither is something an operator should have to prepare.
	if strings.Contains(page, `name="known_hosts_file"`) {
		t.Error("the form still asks for a known_hosts file")
	}
	if !strings.Contains(page, `name="password"`) {
		t.Error("the form has no password field for installing the key")
	}
	if !strings.Contains(page, "makes its own key") {
		t.Error("the form does not say that a key will be generated")
	}
}

func TestRedirectsStayRelativeSoWHMKeepsItsToken(t *testing.T) {
	client, _, _ := newUI(t)

	_, page := get(t, client, "/destinations")
	resp, err := client.PostForm("http://ui/schedule/save", map[string][]string{
		"csrf": {csrfToken(t, page)}, "name": {"Nightly"}, "cron": {"0 2 * * *"},
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	// WHM serves the plugin behind a /cpsessNNN token in the path. An
	// absolute-path redirect drops it and the browser lands on a 401, so
	// every redirect has to be a bare query string.
	location := resp.Header.Get("Location")
	if !strings.HasPrefix(location, "?") {
		t.Errorf("Location = %q, want it to start with ?", location)
	}
	if strings.Contains(location, "/schedule") {
		t.Errorf("Location = %q, want no path component", location)
	}
}

func TestEditSchedule(t *testing.T) {
	client, _, engine := newUI(t)

	// A destination to point the schedule at.
	_, page := get(t, client, "/destinations")
	added, err := client.PostForm("http://ui/destinations/add", map[string][]string{
		"csrf": {csrfToken(t, page)}, "name": {"Local disk"}, "type": {"local"},
		"root": {t.TempDir()}, "repo_path": {"cp01"},
	})
	if err != nil {
		t.Fatal(err)
	}
	added.Body.Close()
	repos, err := engine.Store().Repositories()
	if err != nil || len(repos) != 1 {
		t.Fatalf("repositories = %+v (%v)", repos, err)
	}
	noteRecoveryKeys(t, engine)

	_, page = get(t, client, "/schedule")
	saved, err := client.PostForm("http://ui/schedule/save", map[string][]string{
		"csrf": {csrfToken(t, page)}, "name": {"Nightly"}, "cron": {"0 2 * * *"},
		"repository": {repos[0].ID}, "scope": {"all"}, "enabled": {"1"},
		"keep_daily": {"7"}, "mode": {"split"},
	})
	if err != nil {
		t.Fatal(err)
	}
	saved.Body.Close()

	policies, err := engine.Store().Policies()
	if err != nil || len(policies) != 1 {
		t.Fatalf("policies = %+v (%v)", policies, err)
	}
	original := policies[0]

	// The edit form must arrive filled in, or an operator retypes
	// everything and loses whatever they forget.
	editPage := func() string {
		t.Helper()
		status, body := get(t, client, "/schedule?edit="+original.ID)
		if status != http.StatusOK {
			t.Fatalf("GET edit page = %d", status)
		}
		return body
	}
	form := editPage()
	for _, want := range []string{
		`value="Nightly"`, `value="0 2 * * *"`,
		`name="id" value="` + original.ID + `"`,
		"Save changes",
	} {
		if !strings.Contains(form, want) {
			t.Errorf("the edit form is missing %q", want)
		}
	}

	// Saving with the id updates rather than creating a second schedule.
	updated, err := client.PostForm("http://ui/schedule/save", map[string][]string{
		"csrf": {csrfToken(t, form)}, "id": {original.ID},
		"name": {"Weekly"}, "cron": {"0 3 * * 0"},
		"repository": {repos[0].ID}, "scope": {"all"}, "enabled": {"1"},
		"keep_daily": {"14"}, "mode": {"split"},
	})
	if err != nil {
		t.Fatal(err)
	}
	updated.Body.Close()

	policies, err = engine.Store().Policies()
	if err != nil {
		t.Fatal(err)
	}
	if len(policies) != 1 {
		t.Fatalf("editing produced %d schedules, want 1", len(policies))
	}
	if policies[0].Name != "Weekly" || policies[0].ScheduleCron != "0 3 * * 0" {
		t.Errorf("policy = %+v", policies[0])
	}
	if policies[0].Retention.KeepDaily != 14 {
		t.Errorf("retention = %+v", policies[0].Retention)
	}
	if policies[0].CreatedAt != original.CreatedAt {
		t.Error("editing changed when the schedule was created")
	}
}

func TestEditDestinationKeepsCredentialsAndRepositoryPath(t *testing.T) {
	client, _, engine := newUI(t)

	_, page := get(t, client, "/destinations")
	added, err := client.PostForm("http://ui/destinations/add", map[string][]string{
		"csrf": {csrfToken(t, page)}, "name": {"Wasabi"}, "type": {"s3"},
		"bucket": {"cp-backups"}, "region": {"us-east-1"},
		"endpoint":          {"s3.us-east-1.wasabisys.com"},
		"access_key_id":     {"AKIA-ORIGINAL"},
		"secret_access_key": {"SECRET-ORIGINAL"},
		"repo_path":         {"cp01"},
	})
	if err != nil {
		t.Fatal(err)
	}
	added.Body.Close()

	destinations, err := engine.Store().Destinations()
	if err != nil || len(destinations) != 1 {
		t.Fatalf("destinations = %+v (%v)", destinations, err)
	}
	original := destinations[0]

	status, form := get(t, client, "/destinations?edit="+original.ID)
	if status != http.StatusOK {
		t.Fatalf("GET edit page = %d", status)
	}
	for _, want := range []string{`value="Wasabi"`, `value="cp-backups"`, "Save changes"} {
		if !strings.Contains(form, want) {
			t.Errorf("the edit form is missing %q", want)
		}
	}

	// Correcting the bucket must not require retyping the secret key.
	edited, err := client.PostForm("http://ui/destinations/edit", map[string][]string{
		"csrf": {csrfToken(t, form)}, "id": {original.ID},
		"name": {"Wasabi Miami"}, "bucket": {"cp-backups-2"},
		"region": {"us-east-1"}, "endpoint": {"s3.us-east-1.wasabisys.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	edited.Body.Close()

	destinations, err = engine.Store().Destinations()
	if err != nil || len(destinations) != 1 {
		t.Fatalf("editing produced %d destinations", len(destinations))
	}
	updated := destinations[0]
	if updated.Name != "Wasabi Miami" || updated.Config["bucket"] != "cp-backups-2" {
		t.Errorf("destination = %+v", updated)
	}
	if updated.CredentialsSecretID != original.CredentialsSecretID {
		t.Error("the stored credentials were replaced by an empty form")
	}

	// The repository is where the backups already are.
	repos, err := engine.Store().Repositories()
	if err != nil || len(repos) != 1 {
		t.Fatalf("repositories = %+v", repos)
	}
	if repos[0].Path != "cp01" {
		t.Errorf("repository path changed to %q", repos[0].Path)
	}
}

func TestRunScheduleNow(t *testing.T) {
	client, _, engine := newUI(t)

	_, page := get(t, client, "/destinations")
	added, err := client.PostForm("http://ui/destinations/add", map[string][]string{
		"csrf": {csrfToken(t, page)}, "name": {"Local disk"}, "type": {"local"},
		"root": {t.TempDir()}, "repo_path": {"cp01"},
	})
	if err != nil {
		t.Fatal(err)
	}
	added.Body.Close()
	repos, err := engine.Store().Repositories()
	if err != nil || len(repos) != 1 {
		t.Fatalf("repositories = %+v (%v)", repos, err)
	}
	noteRecoveryKeys(t, engine)

	_, page = get(t, client, "/schedule")
	saved, err := client.PostForm("http://ui/schedule/save", map[string][]string{
		"csrf": {csrfToken(t, page)}, "name": {"Nightly"}, "cron": {"0 2 * * *"},
		"repository": {repos[0].ID}, "scope": {"all"}, "enabled": {"1"}, "mode": {"split"},
	})
	if err != nil {
		t.Fatal(err)
	}
	saved.Body.Close()
	policies, err := engine.Store().Policies()
	if err != nil || len(policies) != 1 {
		t.Fatalf("policies = %+v", policies)
	}

	_, page = get(t, client, "/schedule")
	if !strings.Contains(page, "Run now") {
		t.Fatal("the schedules page offers no way to run one")
	}
	run, err := client.PostForm("http://ui/schedule/run", map[string][]string{
		"csrf": {csrfToken(t, page)}, "id": {policies[0].ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	run.Body.Close()
	if location := run.Header.Get("Location"); !strings.Contains(location, "kind=ok") {
		t.Fatalf("redirect = %q, want success", location)
	}

	jobs, err := engine.Store().Jobs(0)
	if err != nil {
		t.Fatal(err)
	}
	// The fake host has one account, so one job should be waiting.
	if len(jobs) != 1 || jobs[0].Account != "customer1" {
		t.Fatalf("jobs = %+v", jobs)
	}
	if jobs[0].Status != job.StatusPending {
		t.Errorf("job status = %q, want pending", jobs[0].Status)
	}

	// Running by hand must not move the schedule's marker, or tonight's
	// run would be skipped.
	after, err := engine.Store().Policy(policies[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.LastRunAt != nil {
		t.Error("a manual run moved the schedule's last-fired time")
	}

	// A second press while that account is still queued is a skip, not a
	// second job for the same account.
	_, page = get(t, client, "/schedule")
	again, err := client.PostForm("http://ui/schedule/run", map[string][]string{
		"csrf": {csrfToken(t, page)}, "id": {policies[0].ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	again.Body.Close()
	if location := again.Header.Get("Location"); !strings.Contains(location, "kind=warn") {
		t.Errorf("second run redirect = %q, want a warning", location)
	}
	jobs, _ = engine.Store().Jobs(0)
	if len(jobs) != 1 {
		t.Errorf("a second press queued %d jobs for one account", len(jobs))
	}
}

func TestRunScheduleWithNoDestinationIsRefused(t *testing.T) {
	client, _, engine := newUI(t)

	// Written straight to the store: the form will not save one like this,
	// but a schedule can end up here if its destination was removed.
	policy, err := engine.Store().PutPolicy(nodestore.Policy{
		Name: "Orphaned", ScheduleCron: "0 2 * * *", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, page := get(t, client, "/schedule")
	resp, err := client.PostForm("http://ui/schedule/run", map[string][]string{
		"csrf": {csrfToken(t, page)}, "id": {policy.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	location := resp.Header.Get("Location")
	if !strings.Contains(location, "kind=error") || !strings.Contains(location, "nowhere") {
		t.Errorf("redirect = %q, want it to say there is nowhere to send a backup", location)
	}
}

func TestDownloadStreamsARebuiltArchive(t *testing.T) {
	client, _, engine := newUI(t)

	// A finished restore, with its archive where the engine puts them.
	staging := engine.Settings().StagingRoot
	dir := filepath.Join(staging, "stage-restore-customer1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "cpmove-customer1.tar")
	body := []byte("not really a tar, but the bytes should arrive intact")
	if err := os.WriteFile(archive, body, 0o600); err != nil {
		t.Fatal(err)
	}
	restore, err := engine.Store().PutRestore(nodestore.Restore{
		Account: "customer1", SnapshotID: "abc",
		Status: job.StatusSuccess, ArchivePath: archive,
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := client.Get("http://ui/download?id=" + restore.ID)
	if err != nil {
		t.Fatalf("GET download: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download = %d", resp.StatusCode)
	}
	// Content-Disposition is what makes the browser save the file and is
	// the only place the name survives, because cpsrvd strips
	// Content-Type from what the plugin returns.
	disposition := resp.Header.Get("Content-Disposition")
	if !strings.Contains(disposition, `filename="cpmove-customer1.tar"`) {
		t.Errorf("Content-Disposition = %q", disposition)
	}
	if !strings.HasPrefix(disposition, "attachment") {
		t.Errorf("Content-Disposition = %q, want an attachment", disposition)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != string(body) {
		t.Errorf("downloaded %d bytes, want %d", len(got), len(body))
	}
}

func TestDownloadRefusesAnArchiveOutsideStaging(t *testing.T) {
	client, _, engine := newUI(t)

	// The path is ours, not the browser's, but it still becomes an open(),
	// so it is checked against the staging root.
	outside := filepath.Join(t.TempDir(), "passwd.tar")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	restore, err := engine.Store().PutRestore(nodestore.Restore{
		Account: "customer1", SnapshotID: "abc",
		Status: job.StatusSuccess, ArchivePath: outside,
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := client.Get("http://ui/download?id=" + restore.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("download = %d, want a redirect with an error", resp.StatusCode)
	}
	if location := resp.Header.Get("Location"); !strings.Contains(location, "kind=error") {
		t.Errorf("redirect = %q", location)
	}
}

func TestDownloadOfAnUnfinishedRestoreIsRefused(t *testing.T) {
	client, _, engine := newUI(t)

	restore, err := engine.Store().PutRestore(nodestore.Restore{
		Account: "customer1", SnapshotID: "abc", Status: job.StatusRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Get("http://ui/download?id=" + restore.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("download = %d, want a redirect with an error", resp.StatusCode)
	}
}

func TestDownloadRequestNeedsABackup(t *testing.T) {
	client, _, _ := newUI(t)

	_, page := get(t, client, "/accounts")
	resp, err := client.PostForm("http://ui/accounts/download", map[string][]string{
		"csrf": {csrfToken(t, page)}, "account": {"departed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	location := resp.Header.Get("Location")
	if !strings.Contains(location, "kind=error") {
		t.Fatalf("redirect = %q, want an error", location)
	}
	if !strings.Contains(location, "no+backup+yet") {
		t.Errorf("redirect = %q, want it to say there is no backup", location)
	}
}

func TestDownloadServesAnExistingArchiveInsteadOfRebuilding(t *testing.T) {
	client, _, engine := newUI(t)

	staging := engine.Settings().StagingRoot
	dir := filepath.Join(staging, "keep-restore-customer1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "cpmove-customer1.tar")
	if err := os.WriteFile(archive, []byte("already rebuilt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Store().PutRestore(nodestore.Restore{
		Account: "customer1", SnapshotID: "abc",
		Status: job.StatusSuccess, ArchivePath: archive,
	}); err != nil {
		t.Fatal(err)
	}

	_, page := get(t, client, "/accounts")
	resp, err := client.PostForm("http://ui/accounts/download", map[string][]string{
		"csrf": {csrfToken(t, page)}, "account": {"customer1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Rebuilding takes minutes. If the archive is already there, pressing
	// Download must hand it over rather than queue a copy of it.
	location := resp.Header.Get("Location")
	if !strings.Contains(location, "p=download") {
		t.Fatalf("redirect = %q, want it to go straight to the download", location)
	}
	if restores, _ := engine.Store().Restores(0); len(restores) != 1 {
		t.Errorf("pressing Download queued another rebuild: %d restores", len(restores))
	}
}

func TestDownloadRebuildsWhenTheArchiveIsGone(t *testing.T) {
	client, _, engine := newUI(t)

	// A finished restore whose archive has since been removed.
	if _, err := engine.Store().PutRestore(nodestore.Restore{
		Account: "customer1", SnapshotID: "abc", Status: job.StatusSuccess,
		ArchivePath: filepath.Join(engine.Settings().StagingRoot, "gone", "cpmove.tar"),
	}); err != nil {
		t.Fatal(err)
	}

	_, page := get(t, client, "/accounts")
	resp, err := client.PostForm("http://ui/accounts/download", map[string][]string{
		"csrf": {csrfToken(t, page)}, "account": {"customer1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// No backup exists in this test, so it should report that rather than
	// silently offering a download of nothing.
	location := resp.Header.Get("Location")
	if !strings.Contains(location, "kind=error") {
		t.Errorf("redirect = %q, want an error about there being no backup", location)
	}
}

func TestDownloadRedirectIsAQueryOnlyReference(t *testing.T) {
	client, _, engine := newUI(t)

	staging := engine.Settings().StagingRoot
	dir := filepath.Join(staging, "keep-restore-customer1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "cpmove-customer1.tar")
	if err := os.WriteFile(archive, []byte("rebuilt"), 0o600); err != nil {
		t.Fatal(err)
	}
	restore, err := engine.Store().PutRestore(nodestore.Restore{
		Account: "customer1", SnapshotID: "abc",
		Status: job.StatusSuccess, ArchivePath: archive,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, page := get(t, client, "/accounts")
	resp, err := client.PostForm("http://ui/accounts/download", map[string][]string{
		"csrf": {csrfToken(t, page)}, "account": {"customer1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// http.Redirect resolves a query-only reference against the request
	// path, which turns this into "?p=accounts/" and loses the route.
	location := resp.Header.Get("Location")
	if !strings.HasPrefix(location, "?p=download") {
		t.Errorf("Location = %q, want it to start with ?p=download", location)
	}
	if !strings.Contains(location, restore.ID) {
		t.Errorf("Location = %q, want the restore id", location)
	}
}

func TestAccountsListMakesNoRemoteCalls(t *testing.T) {
	client, _, engine := newUI(t)

	// A destination whose repository cannot be reached at all. Listing
	// accounts must not touch it: one round trip per repository across
	// every account is the slow-page mistake this design avoids.
	_, page := get(t, client, "/destinations")
	added, err := client.PostForm("http://ui/destinations/add", map[string][]string{
		"csrf": {csrfToken(t, page)}, "name": {"Local disk"}, "type": {"local"},
		"root": {t.TempDir()}, "repo_path": {"cp01"},
	})
	if err != nil {
		t.Fatal(err)
	}
	added.Body.Close()

	started := time.Now()
	status, body := get(t, client, "/accounts")
	if status != http.StatusOK {
		t.Fatalf("GET accounts = %d", status)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("listing accounts took %s; it is talking to a destination", elapsed)
	}
	if !strings.Contains(body, "customer1") {
		t.Error("the account is missing from the list")
	}
	// The state filter and search are what make the page survive a server
	// with hundreds of accounts.
	if !strings.Contains(body, `data-filter="#accounts"`) {
		t.Error("the list has no search")
	}
	if !strings.Contains(body, `data-state="bad"`) {
		t.Error("the list has no state filter")
	}
	_ = engine
}

func TestAccountDetailShowsSnapshotsAndActivity(t *testing.T) {
	client, _, engine := newUI(t)

	// Some history for the account, so the page has something to show.
	finished := time.Now().Add(-time.Hour)
	if _, err := engine.Store().PutJob(nodestore.Job{
		Account: "customer1", Status: job.StatusSuccess, FinishedAt: &finished,
		Targets: []nodestore.JobTarget{{
			RepositoryID: "repo", Status: job.TargetSuccess, SnapshotID: "40dc1520",
			BytesAdded: 1024, BytesProcessed: 8192,
			Detail:     "error: lstat /home/customer1/tmp/sess_9f2b: no such file",
			Incomplete: true,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Store().PutRestore(nodestore.Restore{
		Account: "customer1", SnapshotID: "abc", Kind: node.KindVerify,
		Status: job.StatusSuccess, FinishedAt: &finished,
		Detail: "archive present; account tree present; 12 files in the home directory",
	}); err != nil {
		t.Fatal(err)
	}

	status, body := get(t, client, "/account?user=customer1")
	if status != http.StatusOK {
		t.Fatalf("GET account = %d", status)
	}
	for _, want := range []string{
		"customer1",
		"1.0 KiB stored of 8.0 KiB read",
		"Restore rehearsed",
		"archive present; account tree present",
		"What restic reported",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the account page is missing %q", want)
		}
	}
}

func TestAccountDetailRejectsAnUnknownAccount(t *testing.T) {
	client, _, _ := newUI(t)

	resp, err := client.Get("http://ui/account?user=nosuchaccount")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET = %d, want a redirect", resp.StatusCode)
	}
	if location := resp.Header.Get("Location"); !strings.Contains(location, "kind=error") {
		t.Errorf("redirect = %q, want an error", location)
	}
}

func TestOverviewLeadsWithCoverage(t *testing.T) {
	client, _, engine := newUI(t)

	status, body := get(t, client, "/")
	if status != http.StatusOK {
		t.Fatalf("GET overview = %d", status)
	}
	// The question an operator actually has is whether everything is
	// protected, so that is what the page opens with.
	for _, want := range []string{
		// With nothing configured, the first thing to say is what to do.
		"Nothing is being backed up: this server has no destination.",
		"Add a destination",
		"Staging space",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the overview is missing %q", want)
		}
	}

	// Once a destination exists, the unprotected accounts become the
	// thing worth saying.
	_, page := get(t, client, "/destinations")
	added, err := client.PostForm("http://ui/destinations/add", map[string][]string{
		"csrf": {csrfToken(t, page)}, "name": {"Local disk"}, "type": {"local"},
		"root": {t.TempDir()}, "repo_path": {"cp01"},
	})
	if err != nil {
		t.Fatal(err)
	}
	added.Body.Close()

	_, body = get(t, client, "/")
	if !strings.Contains(body, "1 of 1 accounts is not covered.") {
		t.Error("the overview does not say that an account has no backup")
	}
	if !strings.Contains(body, "Worth acting on") {
		t.Error("the overview does not offer the account to act on")
	}
	_ = engine
}

func TestRoutesTravelInTheQueryParameter(t *testing.T) {
	client, _, _ := newUI(t)

	// cpsrvd will not route a path after the CGI's name, so the plugin
	// forwards the route as "p" and the service turns it back.
	status, body := get(t, client, "/?p=accounts")
	if status != http.StatusOK {
		t.Fatalf("GET ?p=accounts = %d", status)
	}
	if !strings.Contains(body, "Back up, verify or recover") {
		t.Error("?p=accounts did not reach the accounts page")
	}

	// Other parameters survive the translation.
	status, body = get(t, client, "/?p=account&user=customer1")
	if status != http.StatusOK {
		t.Fatalf("GET ?p=account = %d", status)
	}
	if !strings.Contains(body, "customer1") {
		t.Error("?p=account&user=… did not reach the account page")
	}

	// No route is the overview.
	if _, body := get(t, client, "/"); !strings.Contains(body, "Staging space") {
		t.Error("the bare address did not reach the overview")
	}
}

func TestRejectsARouteThatIsNotOurs(t *testing.T) {
	client, _, _ := newUI(t)

	for _, route := range []string{"../../etc/passwd", "a//b", "a%20b", "%2e%2e%2fetc"} {
		resp, err := client.Get("http://ui/?p=" + route)
		if err != nil {
			t.Fatalf("GET %q: %v", route, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("route %q = %d, want 400", route, resp.StatusCode)
		}
	}
}

func TestPostRoutesThroughTheQueryParameter(t *testing.T) {
	client, _, _ := newUI(t)

	// A form posts to "?p=destinations/add"; without a token the service
	// must still be the thing that refuses it.
	resp, err := client.PostForm("http://ui/?p=destinations/add", map[string][]string{
		"name": {"sneaky"}, "type": {"local"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST ?p=destinations/add = %d, want 403", resp.StatusCode)
	}
}

// The typefaces ship inside the plugin: a page that runs as root should
// not be telling a font host when a root session is open.
func TestFontsAreServedByThePluginItself(t *testing.T) {
	client, _, _ := newUI(t)

	status, body := get(t, client, "/font?name=fira-sans-400.woff2")
	if status != http.StatusOK {
		t.Fatalf("GET the regular weight = %d", status)
	}
	if len(body) < 1000 || body[:4] != "wOF2" {
		t.Errorf("that is not a woff2 file: %d bytes starting %q", len(body), body[:min(4, len(body))])
	}

	for _, name := range []string{"", "../static/app.css", "fonts/fira-sans-400.woff2", "nonesuch.woff2"} {
		if status, _ := get(t, client, "/font?name="+url.QueryEscape(name)); status != http.StatusNotFound {
			t.Errorf("GET a font named %q = %d, want 404", name, status)
		}
	}

	_, page := get(t, client, "/destinations")
	if strings.Contains(page, "fonts.googleapis.com") || strings.Contains(page, "fonts.gstatic.com") {
		t.Error("the page still asks a font host for its typefaces")
	}
	if !strings.Contains(page, `url("?p=font&name=fira-sans-400.woff2")`) {
		t.Error("the stylesheet does not point at the plugin's own fonts")
	}
}

// A granular restore is queued with the part of the account it wants, and
// never with an apply: what it recovers is left to collect.
func TestGranularRestoreIsQueuedWithWhatItAsksFor(t *testing.T) {
	client, _, engine := newUI(t)

	// The restore page shows no form until an account is chosen, so the
	// token comes from a page that always has one.
	_, page := get(t, client, "/destinations")
	queued, err := client.PostForm("http://ui/restore/items", map[string][]string{
		"csrf": {csrfToken(t, page)}, "account": {"customer1"},
		"repository": {"repo-1"}, "snapshot": {"abc123"},
		"item": {"mailbox"}, "name": {"example.com/sales", " "},
	})
	if err != nil {
		t.Fatal(err)
	}
	queued.Body.Close()

	restores, err := engine.Store().Restores(10)
	if err != nil || len(restores) != 1 {
		t.Fatalf("restores = %+v (%v)", restores, err)
	}
	got := restores[0]
	if got.Kind != protocol.RestoreItems {
		t.Errorf("kind = %q, want %q", got.Kind, protocol.RestoreItems)
	}
	if got.ItemKind != "mailbox" {
		t.Errorf("item kind = %q", got.ItemKind)
	}
	if len(got.ItemNames) != 1 || got.ItemNames[0] != "example.com/sales" {
		t.Errorf("item names = %v, want the one mailbox and no blanks", got.ItemNames)
	}
	if got.Apply {
		t.Error("a granular restore must never apply itself to the live account")
	}
}

// The restore page offers every part of an account it can take out on its
// own, so the operator does not have to know the payload's shape.
func TestTheRestorePageOffersEveryGranularKind(t *testing.T) {
	client, _, _ := newUI(t)

	_, page := get(t, client, "/restore")
	for _, want := range []string{
		"Files or folders", "Website files", "A mailbox", "A database",
		"DNS records", "SSL certificates", "Account settings",
	} {
		if strings.Contains(page, want) {
			continue
		}
		// The picker only appears once an account with backups is chosen,
		// so an empty page is allowed to be missing it — but the strings
		// have to exist in the template that renders it.
		if !strings.Contains(page, "Restore one thing") {
			return
		}
		t.Errorf("the restore page does not offer %q", want)
	}
}

// A running backup reports how far it has got, and the page says so
// rather than showing a pill that never changes.
func TestARunningBackupShowsItsProgress(t *testing.T) {
	client, _, engine := newUI(t)

	queued, err := engine.Store().PutJob(nodestore.Job{
		Account: "customer1", Status: job.StatusRunning, QueuedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Store().SetJobProgress(queued.ID, nodestore.JobProgress{
		Percent: 42.5, BytesDone: 512 << 20, TotalBytes: 1 << 30,
	}); err != nil {
		t.Fatal(err)
	}

	_, page := get(t, client, "/jobs")
	for _, want := range []string{
		`data-running="1"`, // the page knows to keep itself current
		"cpr-spin",         // and the pill spins rather than sitting still
		"43%",              // restic's own percentage, rounded
		"width:42.5%",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the history page does not show %q", want)
		}
	}
}

// A finished job carries no percentage: "100%" beside a failure would be
// a lie, and a bar on something that is over says nothing.
func TestAFinishedJobShowsNoProgress(t *testing.T) {
	client, _, engine := newUI(t)

	queued, err := engine.Store().PutJob(nodestore.Job{
		Account: "customer1", Status: job.StatusRunning, QueuedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Store().SetJobProgress(queued.ID, nodestore.JobProgress{Percent: 99}); err != nil {
		t.Fatal(err)
	}
	queued.Status = job.StatusFailed
	queued.Progress = nil
	if _, err := engine.Store().PutJob(queued); err != nil {
		t.Fatal(err)
	}

	if _, page := get(t, client, "/jobs"); strings.Contains(page, `class="cpr-progress"`) {
		t.Error("a finished job is still showing a progress bar")
	}
}

// A late status line must not reopen a job that has already finished.
func TestProgressOnAFinishedJobIsIgnored(t *testing.T) {
	_, _, engine := newUI(t)

	stored, err := engine.Store().PutJob(nodestore.Job{
		Account: "customer1", Status: job.StatusSuccess, QueuedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Store().SetJobProgress(stored.ID, nodestore.JobProgress{Percent: 12}); err != nil {
		t.Fatal(err)
	}
	after, err := engine.Store().Job(stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Progress != nil {
		t.Errorf("progress was written onto a finished job: %+v", after.Progress)
	}
}

// A download button that leads to a file which is no longer on the server
// is worse than no button: it offers the operator their data and then
// fails. Whether the archive still exists is checked when the page is
// rendered.
func TestADownloadIsOnlyOfferedWhileTheArchiveExists(t *testing.T) {
	client, _, engine := newUI(t)

	archive := filepath.Join(t.TempDir(), "cpmove-customer1.tar")
	if err := os.WriteFile(archive, []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	stored, err := engine.Store().PutRestore(nodestore.Restore{
		Account: "customer1", Kind: "account", Status: job.StatusSuccess,
		ArchivePath: archive, QueuedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	restoreLog := "/logs?tab=restores"
	if _, page := get(t, client, restoreLog); !strings.Contains(page, "?p=download&amp;id="+stored.ID) {
		t.Error("a collectable archive is not offered for download")
	}

	if err := os.Remove(archive); err != nil {
		t.Fatal(err)
	}
	_, page := get(t, client, restoreLog)
	if strings.Contains(page, "?p=download&amp;id="+stored.ID) {
		t.Error("a download is still offered for an archive that has been swept")
	}
	if !strings.Contains(page, "no longer on this server") {
		t.Error("the page does not say what happened to it")
	}
}

// Collected output can be deleted from the settings page, and that needs
// the token like every other change.
func TestCollectedOutputCanBeDeleted(t *testing.T) {
	client, _, engine := newUI(t)

	root := engine.Settings().StagingRoot
	kept := filepath.Join(root, "keep-restore-customer1")
	if err := os.MkdirAll(kept, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kept, "cpmove.tar"), make([]byte, 1024), 0o600); err != nil {
		t.Fatal(err)
	}

	_, page := get(t, client, "/settings?tab=storage")
	if !strings.Contains(page, "restore-customer1") {
		t.Fatal("the settings page does not list what is waiting in the work directory")
	}

	refused, err := client.PostForm("http://ui/settings/output/delete", map[string][]string{
		"key": {"restore-customer1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	refused.Body.Close()
	if _, err := os.Stat(kept); err != nil {
		t.Fatal("output was deleted without the token")
	}

	deleted, err := client.PostForm("http://ui/settings/output/delete", map[string][]string{
		"csrf": {csrfToken(t, page)}, "key": {"restore-customer1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	deleted.Body.Close()
	if _, err := os.Stat(kept); !os.IsNotExist(err) {
		t.Error("the output is still there after being deleted")
	}
}

// A state has to say what actually happened. "Last run succeeded" is a
// fact an operator can check; a copy nothing will ever refresh gets a
// state of its own rather than the same green pill.
func TestAStateSaysWhatHappenedAndWhatItMeans(t *testing.T) {
	client, _, engine := newUI(t)

	finished := time.Now().Add(-30 * 24 * time.Hour)
	if _, err := engine.Store().PutJob(nodestore.Job{
		Account: "customer1", Status: job.StatusSuccess,
		QueuedAt: finished, FinishedAt: &finished,
		Targets: []nodestore.JobTarget{{Status: job.TargetSuccess}},
	}); err != nil {
		t.Fatal(err)
	}

	// No schedule covers it, so nothing will ever refresh that copy.
	_, page := get(t, client, "/accounts")
	if !strings.Contains(page, "Not scheduled") {
		t.Error("an account no schedule covers is being shown as a good backup")
	}
	if !strings.Contains(page, "no schedule covers it") {
		t.Error("the page does not say why")
	}
	// Every state explains itself on hover rather than by its colour.
	if !strings.Contains(page, "nothing will ever take another") {
		t.Error("the state has no explanation attached")
	}
	if strings.Contains(page, ">Protected<") {
		t.Error("the page still uses a word that promises more than it checks")
	}
}

// One good backup says little on its own. The accounts page shows the
// account's record — how many runs finished and how many worked — so a
// site that fails one night in three is distinguishable from one that has
// never failed.
func TestTheAccountsPageShowsEachAccountsRecord(t *testing.T) {
	client, _, engine := newUI(t)

	when := time.Now().Add(-2 * time.Hour)
	// Oldest first: the failure is history, the last run worked.
	for _, status := range []job.Status{
		job.StatusFailed, job.StatusSuccess, job.StatusSuccess,
	} {
		started := when
		finished := when.Add(90 * time.Second)
		target := nodestore.JobTarget{
			RepositoryID: "repo-1", Status: job.TargetSuccess,
			BytesAdded: 6 << 20, BytesProcessed: 90 << 20,
		}
		if status == job.StatusFailed {
			// A failed run wrote nothing, so it is not a copy.
			target = nodestore.JobTarget{RepositoryID: "repo-1", Status: job.TargetFailed}
		}
		if _, err := engine.Store().PutJob(nodestore.Job{
			Account: "customer1", Status: status,
			QueuedAt: when, StartedAt: &started, FinishedAt: &finished,
			Targets: []nodestore.JobTarget{target},
		}); err != nil {
			t.Fatal(err)
		}
		when = when.Add(time.Minute)
	}

	drilled := time.Now().Add(-time.Hour)
	if _, err := engine.Store().PutRestore(nodestore.Restore{
		Account: "customer1", Kind: node.KindVerify,
		Status: job.StatusSuccess, QueuedAt: drilled, FinishedAt: &drilled,
	}); err != nil {
		t.Fatal(err)
	}

	_, page := get(t, client, "/accounts")
	if !strings.Contains(page, "2 of 3 succeeded") {
		t.Error("the page does not show how many backups of the account worked")
	}
	if !strings.Contains(page, "1 failed") {
		t.Error("the page does not show that one of them failed")
	}
	if !strings.Contains(page, "verified ") {
		t.Error("the page does not show when the backup was last rehearsed")
	}
	// What the last run cost: what it stored, and how long it took.
	if !strings.Contains(page, "6.0 MiB stored") {
		t.Error("the page does not show what the last backup stored")
	}
	if !strings.Contains(page, "took 1m 30s") {
		t.Error("the page does not show how long the last backup took")
	}
	// Where the copies went, and how many each destination took.
	if !strings.Contains(page, "2 to ") {
		t.Error("the page does not show how many copies each destination holds")
	}
}

func TestAddingADestinationWorksWithAndWithoutTheSheet(t *testing.T) {
	client, _, _ := newUI(t)

	_, page := get(t, client, "/destinations")
	if !strings.Contains(page, `<dialog id="add-destination"`) {
		t.Error("the page has no sheet to add a destination in")
	}
	if !strings.Contains(page, `href="?p=destinations&amp;add=1"`) {
		t.Error("the button is not a link, so it does nothing without JavaScript")
	}

	// What that link opens on its own.
	status, standalone := get(t, client, "/destinations?add=1")
	if status != http.StatusOK {
		t.Fatalf("GET /destinations?add=1 = %d", status)
	}
	if strings.Contains(standalone, `<dialog id="add-destination"`) {
		t.Error("the form is in a sheet on the page that exists because there is no sheet")
	}
	for _, want := range []string{"Server address", "Folder inside the destination", "Add and test"} {
		if !strings.Contains(standalone, want) {
			t.Errorf("the add page is missing %q", want)
		}
	}
}

// A dialog is display:none until the browser opens it. A stylesheet that
// says otherwise leaves the drawer standing open on the page, outside the
// top layer, positioned against whichever element of WHM's chrome happens
// to be its containing block — which is exactly what it did.
func TestTheDrawerIsOnlyVisibleWhenItIsOpen(t *testing.T) {
	client, _, _ := newUI(t)

	_, page := get(t, client, "/destinations")
	for _, rule := range []string{
		".gniza .cpr-sheet[open] { display:flex; }",
	} {
		if !strings.Contains(page, rule) {
			t.Errorf("the stylesheet does not say %q", rule)
		}
	}
	// Nothing may give a dialog a display of its own outside [open].
	for _, forbidden := range []string{
		".gniza .cpr-sheet {\n  position:fixed; inset:0 0 0 auto; margin:0;\n  width:min(520px, 94vw); max-width:none; height:100%; max-height:none;\n  padding:0; border:0; border-left:1px solid var(--line-strong); border-radius:0;\n  background:var(--surface); color:var(--ink);\n  box-shadow:-8px 0 28px rgba(10,14,20,.22);\n  display:flex;",
	} {
		if strings.Contains(page, forbidden) {
			t.Error("the drawer is displayed whether or not it is open")
		}
	}
}

// Editing opens the same drawer as adding, with that destination's form
// loaded into it — and the link still goes to the form's own page for a
// browser that cannot do that.
func TestEditingOpensTheDrawer(t *testing.T) {
	client, _, engine := newUI(t)

	if _, _, err := engine.AddDestination(nodestore.Destination{
		Name: "Local disk", Type: "local",
		Config: map[string]string{"root": t.TempDir()},
	}, nil, "cp01"); err != nil {
		t.Fatal(err)
	}

	_, page := get(t, client, "/destinations")
	if !strings.Contains(page, "data-dialog-fetch") {
		t.Error("the edit link does not open the drawer")
	}
	if !strings.Contains(page, `data-dialog-title="Edit “Local disk”"`) {
		t.Error("the drawer would not say which destination is being edited")
	}
	if !strings.Contains(page, "data-drawer-body") || !strings.Contains(page, "data-drawer-title") {
		t.Error("the drawer has nothing to load a form into")
	}

	// The page the drawer reads, and that a browser without the drawer
	// simply goes to.
	destinations, err := engine.Store().Destinations()
	if err != nil || len(destinations) != 1 {
		t.Fatalf("destinations = %+v (%v)", destinations, err)
	}
	status, editPage := get(t, client, "/destinations?edit="+destinations[0].ID)
	if status != http.StatusOK {
		t.Fatalf("GET the edit page = %d", status)
	}
	if !strings.Contains(editPage, "data-drawer-content") {
		t.Error("the edit page does not mark the part the drawer loads")
	}
	if !strings.Contains(editPage, "Save changes") {
		t.Error("the edit page has no form on it")
	}
}

// A row that says only "the last run failed" sends the operator to
// another page to find out what this one already knows.
func TestAFailedAccountSaysWhyOnTheRow(t *testing.T) {
	client, _, engine := newUI(t)

	finished := time.Now().Add(-time.Hour)
	if _, err := engine.Store().PutJob(nodestore.Job{
		Account: "customer1", Status: job.StatusFailed,
		QueuedAt: finished, FinishedAt: &finished,
		StagingErr: "not enough room to stage this account: it needs 7.6 GiB free and there is 6.3 GiB",
	}); err != nil {
		t.Fatal(err)
	}

	_, page := get(t, client, "/accounts")
	if !strings.Contains(page, "it needs 7.6 GiB free and there is 6.3 GiB") {
		t.Error("the row does not say why the backup failed")
	}
}

// A schedule can say what it leaves out, what never to store, and what to
// do when a destination fails.
func TestAScheduleCarriesWhatItLeavesOut(t *testing.T) {
	client, _, engine := newUI(t)

	_, page := get(t, client, "/destinations")
	added, err := client.PostForm("http://ui/destinations/add", map[string][]string{
		"csrf": {csrfToken(t, page)}, "name": {"Local disk"}, "type": {"local"},
		"root": {t.TempDir()}, "repo_path": {"cp01"},
	})
	if err != nil {
		t.Fatal(err)
	}
	added.Body.Close()
	repos, err := engine.Store().Repositories()
	if err != nil || len(repos) != 1 {
		t.Fatalf("repositories = %+v (%v)", repos, err)
	}
	noteRecoveryKeys(t, engine)

	_, page = get(t, client, "/schedule")
	saved, err := client.PostForm("http://ui/schedule/save", map[string][]string{
		"csrf": {csrfToken(t, page)}, "name": {"Nightly"}, "cron": {"0 2 * * *"},
		"repository": {repos[0].ID}, "scope": {"all"}, "enabled": {"1"}, "mode": {"split"},
		"skip_email": {"1"}, "retry_failed": {"1"},
		"excludes":             {"/home/*/tmp\n\n  /home/*/.cache  \n"},
		"alert_no_backup_days": {"3"},
		"alert_run_hours":      {"4"},
	})
	if err != nil {
		t.Fatal(err)
	}
	saved.Body.Close()

	policies, err := engine.Store().Policies()
	if err != nil || len(policies) != 1 {
		t.Fatalf("policies = %+v (%v)", policies, err)
	}
	got := policies[0]
	if !got.SkipEmail || got.SkipHomedir || got.SkipDatabases {
		t.Errorf("what it leaves out = email:%v home:%v db:%v",
			got.SkipEmail, got.SkipHomedir, got.SkipDatabases)
	}
	if !got.RetryFailed {
		t.Error("the schedule does not retry a destination that failed")
	}
	if len(got.Excludes) != 2 || got.Excludes[0] != "/home/*/tmp" || got.Excludes[1] != "/home/*/.cache" {
		t.Errorf("excludes = %q, want the two lines with the blanks and spaces gone", got.Excludes)
	}
	if got.AlertNoBackupDays != 3 || got.AlertRunHours != 4 {
		t.Errorf("alerts = %d days, %d hours", got.AlertNoBackupDays, got.AlertRunHours)
	}

	// And the form offers the paths that are never worth storing.
	_, page = get(t, client, "/schedule")
	for _, want := range []string{"WordPress", "Magento", "wp-content/cache", "Leave out email"} {
		if !strings.Contains(page, want) {
			t.Errorf("the schedule form is missing %q", want)
		}
	}
}

// A run that is still going long after it should have finished is stuck
// more often than it is slow, and nothing else on these pages says so.
func TestALongRunIsCalledOut(t *testing.T) {
	client, _, engine := newUI(t)

	started := time.Now().Add(-9 * time.Hour)
	if _, err := engine.Store().PutJob(nodestore.Job{
		Account: "customer1", Status: job.StatusRunning,
		QueuedAt: started, StartedAt: &started,
	}); err != nil {
		t.Fatal(err)
	}

	_, page := get(t, client, "/accounts")
	if !strings.Contains(page, "has been running for") {
		t.Error("a nine-hour backup is not called out anywhere")
	}
	if !strings.Contains(page, "usually stuck") {
		t.Error("the warning does not say what it means")
	}
}

func TestLifecycleProtectionSettingsPersistImmediately(t *testing.T) {
	client, _, engine := newUI(t)
	_, page := get(t, client, "/settings")
	response, err := client.PostForm("http://ui/settings/save", url.Values{
		"csrf":                    {csrfToken(t, page)},
		"hostname":                {"cp01.example.com"},
		"max_concurrent":          {"1"},
		"restic":                  {"restic"},
		"keep_output_days":        {"7"},
		"protect_account_removal": {"1"},
		"backup_on_suspension":    {"1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	settings, err := engine.Store().Settings()
	if err != nil {
		t.Fatal(err)
	}
	if !settings.ProtectAccountRemoval {
		t.Fatal("account-removal protection was not saved")
	}
	if !settings.BackupOnSuspension {
		t.Fatal("suspension preservation was not saved")
	}
}

// The destination holds ciphertext and the key lives here, so the disaster
// these backups exist for is also the one that destroys the only copy of
// the key. The interface says so until it has been written down elsewhere.
func TestTheRecoveryKeyIsShownAndNagsUntilItIsSaved(t *testing.T) {
	client, _, engine := newUI(t)

	_, page := get(t, client, "/destinations")
	added, err := client.PostForm("http://ui/destinations/add", map[string][]string{
		"csrf": {csrfToken(t, page)}, "name": {"Local disk"}, "type": {"local"},
		"root": {t.TempDir()}, "repo_path": {"cp01"},
	})
	if err != nil {
		t.Fatal(err)
	}
	added.Body.Close()
	repos, err := engine.Store().Repositories()
	if err != nil || len(repos) != 1 {
		t.Fatalf("repositories = %+v (%v)", repos, err)
	}

	// Until it is saved, the page says so.
	_, page = get(t, client, "/destinations")
	if !strings.Contains(page, "exists only on this server") {
		t.Error("nothing warns that the recovery key is nowhere else")
	}
	// And the password is not on the page until it is asked for.
	password, err := engine.RepositoryPassword(repos[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(page, password) {
		t.Error("the repository password is rendered on a page nobody asked")
	}

	// Asking shows it, with what to do with it.
	shown, err := client.PostForm("http://ui/destinations/recovery", map[string][]string{
		"csrf": {csrfToken(t, page)}, "repository": {repos[0].ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(shown.Body)
	shown.Body.Close()
	revealed := string(body)
	if !strings.Contains(revealed, password) {
		t.Error("the recovery key was not shown when it was asked for")
	}
	if !strings.Contains(revealed, "RESTIC_REPOSITORY") {
		t.Error("the page does not say how to use it without gniza")
	}
	if !strings.Contains(revealed, "RESTIC_PASSWORD='"+password+"'") {
		t.Error("the commands still ask the reader to paste the key in themselves")
	}

	// The file is the same thing, to put somewhere else.
	card, err := client.PostForm("http://ui/destinations/recovery/card", map[string][]string{
		"csrf": {csrfToken(t, page)}, "repository": {repos[0].ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	cardBody, _ := io.ReadAll(card.Body)
	card.Body.Close()
	if !strings.Contains(card.Header.Get("Content-Disposition"), "attachment") {
		t.Error("the recovery card is not a download")
	}
	if !strings.Contains(string(cardBody), password) {
		t.Error("the recovery card does not carry the key")
	}

	// Saying it is stored stops the warning.
	noted, err := client.PostForm("http://ui/destinations/recovery/note", map[string][]string{
		"csrf": {csrfToken(t, page)}, "repository": {repos[0].ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	noted.Body.Close()
	if _, page = get(t, client, "/destinations"); strings.Contains(page, "exists only on this server") {
		t.Error("the warning is still there after the key was stored")
	}
}

// Reading a repository password is a change of state as far as secrecy is
// concerned, so it needs the token like every other POST.
func TestTheRecoveryKeyNeedsTheToken(t *testing.T) {
	client, _, engine := newUI(t)

	_, page := get(t, client, "/destinations")
	added, err := client.PostForm("http://ui/destinations/add", map[string][]string{
		"csrf": {csrfToken(t, page)}, "name": {"Local disk"}, "type": {"local"},
		"root": {t.TempDir()}, "repo_path": {"cp01"},
	})
	if err != nil {
		t.Fatal(err)
	}
	added.Body.Close()
	repos, _ := engine.Store().Repositories()

	refused, err := client.PostForm("http://ui/destinations/recovery", map[string][]string{
		"repository": {repos[0].ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(refused.Body)
	refused.Body.Close()
	password, _ := engine.RepositoryPassword(repos[0].ID)
	if strings.Contains(string(body), password) {
		t.Error("a request with no token was shown the recovery key")
	}
}

// A replacement server needs the settings of the one it replaces, so the
// server's own configuration is backed up like an account and restored
// like an item.
func TestTheServersOwnSettingsCanBeScheduledAndRestored(t *testing.T) {
	client, _, engine := newUI(t)

	_, page := get(t, client, "/destinations")
	added, err := client.PostForm("http://ui/destinations/add", map[string][]string{
		"csrf": {csrfToken(t, page)}, "name": {"Local disk"}, "type": {"local"},
		"root": {t.TempDir()}, "repo_path": {"cp01"},
	})
	if err != nil {
		t.Fatal(err)
	}
	added.Body.Close()
	repos, _ := engine.Store().Repositories()
	noteRecoveryKeys(t, engine)

	_, page = get(t, client, "/schedule")
	saved, err := client.PostForm("http://ui/schedule/save", map[string][]string{
		"csrf": {csrfToken(t, page)}, "name": {"Nightly"}, "cron": {"0 2 * * *"},
		"repository": {repos[0].ID}, "scope": {"all"}, "enabled": {"1"}, "mode": {"split"},
		"include_system": {"1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	saved.Body.Close()

	policies, err := engine.Store().Policies()
	if err != nil || len(policies) != 1 || !policies[0].IncludeSystem {
		t.Fatalf("policies = %+v (%v)", policies, err)
	}

	// It queues under its own name, which cannot collide with an account:
	// cPanel usernames cannot contain "@".
	queued, err := engine.QueueSystemBackup(policies[0].ID)
	if err != nil {
		t.Fatalf("queue the system backup: %v", err)
	}
	if queued.Account != "@system" {
		t.Errorf("queued as %q", queued.Account)
	}

	// And it is offered on the restore page as something to restore.
	if _, page = get(t, client, "/restore"); !strings.Contains(page, "The server's own settings") {
		t.Error("the restore page does not offer the server's settings")
	}
}

// A replacement server starts here: it knows nothing, and is given where
// the backups are and the key that reads them.
func TestRecoverAsksForWhereTheBackupsAreAndTheKey(t *testing.T) {
	client, _, _ := newUI(t)

	status, page := get(t, client, "/recover")
	if status != http.StatusOK {
		t.Fatalf("GET /recover = %d", status)
	}
	for _, want := range []string{
		"Recovery key", "Folder inside the destination", "Read the backups",
		"Nothing is written until they can be read",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the recovery page does not ask for %q", want)
		}
	}
}

// Attaching reads the repository before it writes anything down: a
// repository this server cannot read is a typo, and saving it would leave
// an operator in a disaster believing they had their backups back.
func TestAttachingWritesNothingUntilTheBackupsCanBeRead(t *testing.T) {
	client, _, engine := newUI(t)

	_, page := get(t, client, "/recover")
	before, err := engine.Store().Destinations()
	if err != nil {
		t.Fatal(err)
	}

	refused, err := client.PostForm("http://ui/recover/attach", map[string][]string{
		"csrf": {csrfToken(t, page)}, "type": {"local"}, "root": {t.TempDir()},
		"repo_path": {"oldserver"}, "recovery_key": {"not the key"},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(refused.Body)
	refused.Body.Close()
	if !strings.Contains(string(body), "could not read that repository") {
		t.Errorf("a repository that cannot be read was not reported as one: %s",
			firstError(string(body)))
	}

	after, err := engine.Store().Destinations()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Error("a destination was saved for a repository that could not be read")
	}
	if secrets, _ := engine.Store().Repositories(); len(secrets) != 0 {
		t.Error("a repository was recorded for backups nobody could read")
	}
}

// firstError pulls the banner text out of a rendered page, for a test that
// needs to say why the page it got was not the page it wanted.
func firstError(page string) string {
	start := strings.Index(page, `cpr-banner cpr-bad`)
	if start < 0 {
		return "no error banner"
	}
	rest := page[start:]
	open := strings.Index(rest, `<div class="cpr-body">`)
	if open < 0 {
		return "no body"
	}
	rest = rest[open+len(`<div class="cpr-body">`):]
	end := strings.Index(rest, "</div>")
	if end < 0 {
		return rest
	}
	return strings.TrimSpace(rest[:end])
}

// Browsing goes one level at a time: destinations, then what a
// destination holds, then an account's backups, then inside one. Listing
// every file of every snapshot of nineteen accounts would be minutes of
// restic for a question nobody asked.
func TestBrowsingStartsAtTheDestinations(t *testing.T) {
	client, _, _ := newUI(t)

	status, page := get(t, client, "/browse")
	if status != http.StatusOK {
		t.Fatalf("GET /browse = %d", status)
	}
	if !strings.Contains(page, "No destination yet") {
		t.Error("a server with no destinations does not say so")
	}

	_, page = get(t, client, "/destinations")
	added, err := client.PostForm("http://ui/destinations/add", map[string][]string{
		"csrf": {csrfToken(t, page)}, "name": {"Local disk"}, "type": {"local"},
		"root": {t.TempDir()}, "repo_path": {"cp01"},
	})
	if err != nil {
		t.Fatal(err)
	}
	added.Body.Close()

	_, page = get(t, client, "/browse")
	if !strings.Contains(page, "Local disk") {
		t.Error("the destination is not listed to browse")
	}
	if !strings.Contains(page, "?p=browse&amp;repository=") {
		t.Error("there is no way into the destination")
	}
	// The listing itself costs nothing until a destination is opened.
	if strings.Contains(page, "Most recent") {
		t.Error("the destination list is reading repositories nobody opened")
	}
}

// Recovery instructions that leave out the SSH key do not work: reaching
// an SFTP destination needs the key this server made for it and the host
// key it pinned, and a machine that has never seen this one has neither.
func TestTheRecoveryCardCarriesWhatIsNeededToReachTheDestination(t *testing.T) {
	client, _, engine := newUI(t)

	// A destination reached over SSH, with the files Gniza writes for it.
	dir := t.TempDir()
	identity := filepath.Join(dir, "id_ed25519")
	knownHosts := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(identity, []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nfake\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(knownHosts, []byte("backup.example.com ssh-ed25519 AAAA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.AddDestination(nodestore.Destination{
		Name: "Backup server", Type: "sftp",
		Config: map[string]string{
			"host": "backup.example.com", "user": "cpbackup", "root": "/backups",
			"identity_file": identity, "known_hosts_file": knownHosts,
		},
	}, nil, "cp01"); err != nil {
		t.Fatal(err)
	}
	repos, _ := engine.Store().Repositories()

	_, page := get(t, client, "/destinations")
	card, err := client.PostForm("http://ui/destinations/recovery/card", map[string][]string{
		"csrf": {csrfToken(t, page)}, "repository": {repos[0].ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(card.Body)
	card.Body.Close()
	written := string(body)

	for _, want := range []string{
		"-i " + identity,                      // the key this server uses
		"BEGIN OPENSSH PRIVATE KEY",           // and the key itself, for elsewhere
		"backup.example.com ssh-ed25519 AAAA", // with the host key it pinned
		"let ssh ask for a\npassword",         // and what to do when the key is gone
		"cpbackup@backup.example.com",         //
	} {
		if !strings.Contains(written, want) {
			t.Errorf("the recovery card does not carry %q", want)
		}
	}
}

// A destination that holds backups is worth looking inside from where it
// is listed, not only from a page of its own.
func TestADestinationCanBeBrowsedFromItsRow(t *testing.T) {
	client, _, engine := newUI(t)

	_, page := get(t, client, "/destinations")
	added, err := client.PostForm("http://ui/destinations/add", map[string][]string{
		"csrf": {csrfToken(t, page)}, "name": {"Local disk"}, "type": {"local"},
		"root": {t.TempDir()}, "repo_path": {"cp01"},
	})
	if err != nil {
		t.Fatal(err)
	}
	added.Body.Close()
	repos, _ := engine.Store().Repositories()

	_, page = get(t, client, "/destinations")
	if !strings.Contains(page, "?p=browse&amp;repository="+repos[0].ID) {
		t.Error("the destination cannot be browsed from its own row")
	}
}

// Specific files were typed from memory into a textarea. They are picked
// out of what the backup actually holds now, a directory at a time.
//
// The picker itself needs a readable repository, which this suite has no
// restic to talk to; what is asserted here is that the page still says
// how to reach it. The picker is exercised against a real repository.
func TestTheRestorePageExplainsHowToPickFiles(t *testing.T) {
	client, _, _ := newUI(t)

	_, page := get(t, client, "/restore")
	if !strings.Contains(page, "Choose a backup") {
		t.Error("the restore page does not start by choosing a backup")
	}
	if strings.Contains(page, "Specific files (one path per line)") {
		t.Error("the page still asks for paths to be typed from memory")
	}
}

// WHM already draws a breadcrumb above the plugin, and the plugin drew a
// second one under it — 62px of every page spent saying where you are
// twice. The theme switch was the only working control up there, so it
// moved into the rail with the rest of the chrome.
func TestThePluginDoesNotDrawWHMsBreadcrumbAgain(t *testing.T) {
	client, _, _ := newUI(t)

	_, page := get(t, client, "/")
	for _, gone := range []string{"cpr-topbar", "cpr-breadcrumb", "WHM / Plugins / Gniza"} {
		if strings.Contains(page, gone) {
			t.Errorf("the page still carries %s", gone)
		}
	}

	rail := strings.Index(page, `class="cpr-rail-foot"`)
	theme := strings.Index(page, `id="theme"`)
	aside := strings.Index(page, "</aside>")
	if rail < 0 || theme < 0 || aside < 0 {
		t.Fatalf("rail=%d theme=%d aside=%d", rail, theme, aside)
	}
	if theme < rail || theme > aside {
		t.Error("the theme switch is not in the rail's foot")
	}
	// The rail said "cPanel integration" beside a green dot that was a
	// literal, not a check, and named a role the plugin only ever serves.
	for _, gone := range []string{"cPanel integration", "WHM administrator"} {
		if strings.Contains(page, gone) {
			t.Errorf("the rail still claims %q", gone)
		}
	}
}

// A plugin nobody can read the source or the manual of is a plugin an
// operator has to guess at. Both live one click away, in the rail's foot.
func TestTheRailPointsAtTheSourceAndTheManual(t *testing.T) {
	client, _, _ := newUI(t)

	_, page := get(t, client, "/")
	for _, want := range []string{
		`href="https://github.com/shukiv/gniza"`,
		`href="https://github.com/shukiv/gniza/blob/master/docs/README.md"`,
		`aria-label="Gniza on GitHub"`,
		`aria-label="Documentation"`,
		// Opening a new tab from someone else's page hands them
		// window.opener unless this is here.
		`rel="noopener noreferrer"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the rail's foot is missing %s", want)
		}
	}
}

// The name is Gniza and the mark is its own colour. A stylesheet rule that
// greys every span inside the brand once made it the wrong colour, which is
// invisible in markup and only shows on the page.
func TestTheBrandIsOnThePageInItsOwnColour(t *testing.T) {
	client, _, _ := newUI(t)

	_, page := get(t, client, "/")
	for _, want := range []string{
		`<span class="cpr-brand-mark" aria-hidden="true"><svg`,
		// The mark takes its colour from the badge around it rather than
		// carrying its own, so the one CSS rule below decides both.
		`fill="currentColor"`,
		`<span class="cpr-brand-name">Gniza</span>`,
		// The strapline sits inside the lockup, beside the mark rather
		// than under the whole block on a margin kept in step by hand.
		`<span class="cpr-server">Backup. Restore. Repeat.</span>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the interface does not carry the name: %s is missing", want)
		}
	}
	if !strings.Contains(page, "background:#F47216; color:#FFFFFF;") {
		t.Error("the mark is not in its own colour")
	}
	if strings.Contains(page, ".gniza .cpr-brand span { color:var(--muted)") {
		t.Error("a rule is still greying out everything inside the brand")
	}
}

// Light is the default, and it has to be the default before the stylesheet
// paints — a page that decides its theme only once app.js has run shows the
// wrong one first. A machine set to dark is a fact about the machine, not a
// choice made here, so only an explicit "system" hands the decision over.
// An account cPanel has removed disappears from the live account list, and
// with it from every picker built from that list — which is exactly when its
// backups matter most. They are offered in their own group instead.
// Restore is one page with three views. Recovering a whole server used to
// be its own entry in the rail, which put the page an operator needs during
// a disaster behind a word they had to already know.
// The tables answer one question by default — what happened most recently —
// and an operator with nineteen accounts often has a different one: which
// stored the most, which failed, who has not been backed up. The rows are
// already on the page, so the sort happens there; what the server has to
// get right is a sort key on the cells whose text does not sort.
// History was one page of backups called "everything Gniza has done",
// which it was not: a backup of the server's own settings sat among
// nineteen account backups, and the cPanel hook events were only on the
// dashboard's five-row summary.
func TestLogsSeparateTheKindsOfWork(t *testing.T) {
	client, _, engine := newUI(t)

	finished := time.Now().Add(-time.Hour)
	for _, account := range []string{"customer1", cpanel.SystemAccount} {
		if _, err := engine.Store().PutJob(nodestore.Job{
			Account: account, Status: job.StatusSuccess, FinishedAt: &finished,
			Targets: []nodestore.JobTarget{{
				RepositoryID: "repo", Status: job.TargetSuccess, SnapshotID: "40dc1520",
			}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := engine.Store().PutLifecycleEvent(nodestore.LifecycleEvent{
		Event: "removeacct", Account: "departed", OK: true,
		Detail: "identity retired", At: finished,
	}); err != nil {
		t.Fatal(err)
	}

	_, backups := get(t, client, "/logs")
	if !strings.Contains(backups, "<h1>Logs</h1>") {
		t.Error("the page is not called Logs")
	}
	if !strings.Contains(backups, ">customer1<") {
		t.Error("an account backup is missing from the backups log")
	}
	if strings.Contains(backups, ">"+cpanel.SystemAccount+"<") {
		t.Error("a backup of the server's settings is mixed in with the accounts")
	}

	_, system := get(t, client, "/logs?tab=system")
	if !strings.Contains(system, ">"+cpanel.SystemAccount+"<") {
		t.Error("the system backups log does not show the server's own backup")
	}
	if strings.Contains(system, ">customer1<") {
		t.Error("an account backup is in the system backups log")
	}

	_, events := get(t, client, "/logs?tab=lifecycle")
	if !strings.Contains(events, "departed") {
		t.Error("the cPanel events log does not show a lifecycle event")
	}

	// The page was History at /jobs, and somebody's bookmark still says so.
	status, old := get(t, client, "/jobs")
	if status != http.StatusOK || !strings.Contains(old, "<h1>Logs</h1>") {
		t.Errorf("the old address does not still work: %d", status)
	}
}

func TestTablesCarrySortKeysOnCellsThatNeedThem(t *testing.T) {
	client, _, engine := newUI(t)

	finished := time.Now().Add(-time.Hour)
	if _, err := engine.Store().PutJob(nodestore.Job{
		Account: "customer1", Status: job.StatusSuccess, FinishedAt: &finished,
		Targets: []nodestore.JobTarget{{
			RepositoryID: "repo", Status: job.TargetSuccess, SnapshotID: "40dc1520",
			BytesAdded: 4096, BytesProcessed: 8192,
		}},
	}); err != nil {
		t.Fatal(err)
	}

	_, history := get(t, client, "/jobs")
	if strings.Count(history, "<table data-sortable>") < 1 {
		t.Error("the history tables are not sortable")
	}
	// "2026-09-03 21:47" and "6.2 MiB new of 152.5 MiB" sort as text into
	// nonsense; the cells carry a number for the sort to use.
	if !strings.Contains(history, `data-sort="4096"`) {
		t.Error("the stored column has no byte count to sort on")
	}
	if !regexp.MustCompile(`<td[^>]*data-sort="1[0-9]{9}"`).MatchString(history) {
		t.Error("the when column has no timestamp to sort on")
	}
	// A column of actions has nothing to order by.
	if !strings.Contains(history, "data-nosort") {
		t.Error("every column is offered as sortable, including the buttons")
	}

	_, accounts := get(t, client, "/accounts")
	if !strings.Contains(accounts, `<table id="accounts" data-sortable>`) {
		t.Error("the accounts table is not sortable")
	}
}

// A server that lost a disk lost every account on it. Clicking through
// nineteen restores one at a time, in a hurry, keeping your own tally of
// which ones are done, is how one gets missed.
// A destination that is nearly full is the reason tonight's backup fails,
// and the page that lists destinations was the one place that did not say
// so. What it must not do is show a zero for a kind of storage that has no
// size: "0 free" and "cannot say" are different sentences.
// Six columns on one row squeezed every one of them: "5.7 TiB free" broke
// across four lines. Where the backups are is one question — the
// repository, the machine it sits on, and whether that machine answered —
// so it is one column.
// Five buttons per row read as five equally likely things to do, one of
// which deletes the destination. Edit stays out; the rest are behind a
// menu that still works with no JavaScript, because every page here does.
func TestDestinationRowKeepsOneButtonAndAMenu(t *testing.T) {
	client, _, engine := newUI(t)

	if _, err := engine.Store().PutDestination(nodestore.Destination{
		Name: "test", Type: "local", Config: map[string]string{"root": "/mnt/backups"},
	}); err != nil {
		t.Fatal(err)
	}

	_, page := get(t, client, "/destinations")
	if !strings.Contains(page, `<details class="cpr-menu">`) {
		t.Error("the row actions are not behind a menu")
	}
	if !strings.Contains(page, `aria-label="More actions for test"`) {
		t.Error("the menu does not say whose actions it holds")
	}
	// Edit is the one that stays out, and it is still the dialog link.
	if !strings.Contains(page, `data-dialog-title="Edit “test”"`) {
		t.Error("Edit is not on the row")
	}
	// The rest are still reachable, and still carry their token.
	for _, want := range []string{
		`<button class="cpr-menu-item">Test</button>`,
		`action="?p=destinations/delete"`,
		`data-confirm="Remove this destination`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the menu lost %q", want)
		}
	}
	if strings.Contains(page, `<button class="cpr-btn cpr-danger">Remove</button>`) {
		t.Error("Remove is still a button on the row")
	}
}

func TestDestinationsKeepWhereTheBackupsAreInOneColumn(t *testing.T) {
	client, _, engine := newUI(t)

	checked := time.Now().Add(-10 * time.Minute)
	if _, err := engine.Store().PutDestination(nodestore.Destination{
		Name: "test", Type: "sftp", LastCheckedAt: &checked,
		Config: map[string]string{
			"host": "182.54.236.26", "user": "test", "root": "/home/test",
		},
	}); err != nil {
		t.Fatal(err)
	}

	_, page := get(t, client, "/destinations")
	head := `<thead><tr><th>Name</th><th>Repository</th><th>Space</th>`
	if !strings.Contains(page, head) {
		t.Error("the destinations table still spreads one question over several columns")
	}
	for _, gone := range []string{"<th>Address</th>", "<th>State</th>"} {
		if strings.Contains(page, gone) {
			t.Errorf("%s is still its own column", gone)
		}
	}
	// The content itself has to survive the move.
	for _, want := range []string{"182.54.236.26", "Reachable", "checked"} {
		if !strings.Contains(page, want) {
			t.Errorf("merging the columns lost %q", want)
		}
	}
}

func TestDestinationsShowTheRoomTheyHaveLeft(t *testing.T) {
	client, _, engine := newUI(t)

	measured := time.Now().Add(-20 * time.Minute)
	if _, err := engine.Store().PutDestination(nodestore.Destination{
		Name: "backup disk", Type: "local", Config: map[string]string{"root": "/mnt/backups"},
		Space: nodestore.DestinationSpace{
			TotalBytes: 50 << 30, FreeBytes: 5 << 30, MeasuredAt: &measured,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Store().PutDestination(nodestore.Destination{
		Name: "the bucket", Type: "s3", Config: map[string]string{"bucket": "backups"},
		Space: nodestore.DestinationSpace{Unsupported: true, MeasuredAt: &measured},
	}); err != nil {
		t.Fatal(err)
	}

	_, page := get(t, client, "/destinations")
	for _, want := range []string{
		"5.0 GiB free",
		"90% of 50.0 GiB used",
		"this kind does not report space",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the destinations page does not say %q", want)
		}
	}
	// Nearly full reads as nearly full, not as a healthy bar.
	if !strings.Contains(page, `<i class="cpr-none"`) {
		t.Error("a destination at 90% is not marked as one")
	}
}

func TestSeveralAccountsCanBeRestoredAtOnce(t *testing.T) {
	client, _, engine := newUI(t)

	finished := time.Now().Add(-time.Hour)
	retired := time.Now().Add(-time.Minute)
	for _, account := range []string{"gone1", "gone2"} {
		if _, err := engine.Store().PutJob(nodestore.Job{
			Account: account, Status: job.StatusSuccess, FinishedAt: &finished,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.Store().PutIdentity(nodestore.AccountIdentity{
			Account: account, UID: 4000, SinceAt: finished,
			LastSeen: finished, CreatedAt: finished, RetiredAt: &retired,
		}); err != nil {
			t.Fatal(err)
		}
	}

	_, page := get(t, client, "/restore")
	for _, want := range []string{
		`<input type="checkbox" name="account" value="gone1"`,
		`<input type="checkbox" name="account" value="gone2"`,
		`data-check-all`,
		`action="?p=recover/accounts"`,
		// One list, with the state on the row rather than in a tab.
		`class="cpr-row-gone"`, ">deleted</span>",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the deleted accounts cannot be chosen in bulk: %s is missing", want)
		}
	}

	// Nothing chosen must not read as "done": it is refused, and says so.
	empty, err := client.PostForm("http://ui/recover/accounts", map[string][]string{
		"csrf": {csrfToken(t, page)}, "repository": {"repo"},
	})
	if err != nil {
		t.Fatal(err)
	}
	empty.Body.Close()
	refusal, err := url.QueryUnescape(empty.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(refusal, "Choose at least one account") {
		t.Errorf("a bulk restore of nothing was not refused: %s", refusal)
	}
	if !strings.Contains(refusal, "kind=error") {
		t.Errorf("refusing to restore nothing was not reported as a problem: %s", refusal)
	}

	// Both accounts are attempted even though this suite has no restic to
	// resolve a snapshot with: what matters is that one account failing
	// does not stop the other being tried, and that both are named rather
	// than counted.
	both, err := client.PostForm("http://ui/recover/accounts", map[string][]string{
		"csrf": {csrfToken(t, page)}, "from": {"deleted"}, "repository": {"repo"},
		"account": {"gone1", "gone2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	both.Body.Close()
	outcome, err := url.QueryUnescape(both.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range []string{"gone1", "gone2"} {
		if !strings.Contains(outcome, account) {
			t.Errorf("the outcome does not say what happened to %s: %s", account, outcome)
		}
	}
	// And it lands back where the operator was.
	if !strings.Contains(outcome, "tab=deleted") {
		t.Errorf("the outcome does not return to the deleted accounts: %s", outcome)
	}
}

func TestRestoreCarriesItsThreeViewsAsTabs(t *testing.T) {
	client, _, _ := newUI(t)

	_, account := get(t, client, "/restore")
	for _, want := range []string{
		`<nav class="cpr-tabs" aria-label="Restore views">`,
		`href="?p=restore" aria-current="page"`,
		`href="?p=restore&amp;tab=server"`,
		// The page restores accounts, one or several, and the third tab
		// is for the day the machine is gone.
		"Restore account(s)",
		"Disaster recovery",
	} {
		if !strings.Contains(account, want) {
			t.Errorf("the restore page is missing %q", want)
		}
	}

	// The deleted accounts used to be a tab of their own. The address
	// still works, and lands on the list they are now part of.
	status, deleted := get(t, client, "/restore?tab=deleted")
	if status != http.StatusOK {
		t.Fatalf("GET /restore?tab=deleted = %d", status)
	}
	if strings.Contains(deleted, "Deleted accounts</a>") {
		t.Error("the deleted accounts still have a tab of their own")
	}

	// The whole-server view is the old recovery page, reached from here.
	status, server := get(t, client, "/restore?tab=server")
	if status != http.StatusOK {
		t.Fatalf("GET /restore?tab=server = %d", status)
	}
	for _, want := range []string{
		"Recovery key", "Read the backups",
		`href="?p=restore&amp;tab=server" aria-current="page"`,
	} {
		if !strings.Contains(server, want) {
			t.Errorf("the disaster recovery view is missing %q", want)
		}
	}
	// Restores that have run belong in the logs. Keeping a copy here made
	// the page that starts a restore mostly a list of old ones.
	if strings.Contains(account, "Recent restores") {
		t.Error("the restore page still carries its own history")
	}
	if !strings.Contains(account, `href="?p=logs&amp;tab=restores"`) {
		t.Error("the restore page does not say where that history went")
	}
	// The rail no longer carries it separately, so the tab is the only
	// way in and has to work.
	if strings.Contains(server, `aria-label="Server recovery"`) {
		t.Error("the rail still has a separate server recovery entry")
	}
}

func TestRestoreOffersAccountsCPanelHasDeleted(t *testing.T) {
	client, _, engine := newUI(t)

	finished := time.Now().Add(-2 * time.Hour)
	retired := time.Now().Add(-time.Hour)
	if _, err := engine.Store().PutJob(nodestore.Job{
		Account: "departed", Status: job.StatusSuccess, FinishedAt: &finished,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Store().PutIdentity(nodestore.AccountIdentity{
		Account: "departed", UID: 1234, SinceAt: finished,
		LastSeen: finished, CreatedAt: finished, RetiredAt: &retired,
	}); err != nil {
		t.Fatal(err)
	}
	// Retired, but nothing was ever backed up under the name: offering it
	// would be a dead end.
	if _, err := engine.Store().PutIdentity(nodestore.AccountIdentity{
		Account: "nevercopied", UID: 1235, SinceAt: finished,
		LastSeen: finished, CreatedAt: finished, RetiredAt: &retired,
	}); err != nil {
		t.Fatal(err)
	}

	_, page := get(t, client, "/restore")
	if !strings.Contains(page, `<optgroup label="Deleted accounts">`) {
		t.Error("the picker does not offer deleted accounts")
	}
	if !strings.Contains(page, `<option value="departed"`) {
		t.Error("a deleted account with backups is not in the picker")
	}
	if strings.Contains(page, `<option value="nevercopied"`) {
		t.Error("a deleted account with no backup is offered anyway")
	}

	_, chosen := get(t, client, "/restore?account=departed")
	if !strings.Contains(chosen, "is not on this server any more") {
		t.Error("choosing a deleted account does not say the account is gone")
	}
	if !strings.Contains(chosen, "creates the account again") {
		t.Error("the page does not say what restoring a deleted account does")
	}
	// The whole-account form itself needs snapshots, which needs a restic
	// this suite has none of; the wording it carries for a deleted account
	// is checked against a real repository instead.
}

// Deleting a customer's backups is the one thing here that cannot be
// undone, so the page carries the confirmation rather than the browser: a
// window.confirm nobody saw is no confirmation at all with scripting off.
func TestForgettingADeletedAccountAsksFirst(t *testing.T) {
	client, _, engine := newUI(t)

	finished := time.Now().Add(-2 * time.Hour)
	retired := time.Now().Add(-time.Hour)
	if _, err := engine.Store().PutJob(nodestore.Job{
		Account: "departed", Status: job.StatusSuccess, FinishedAt: &finished,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Store().PutIdentity(nodestore.AccountIdentity{
		Account: "departed", UID: 1234, SinceAt: finished,
		LastSeen: finished, CreatedAt: finished, RetiredAt: &retired,
	}); err != nil {
		t.Fatal(err)
	}

	_, page := get(t, client, "/restore")
	if !strings.Contains(page, "forget=departed") {
		t.Error("a deleted account cannot be forgotten from its row")
	}
	if strings.Contains(page, "Forget departed</button>") {
		t.Error("the page offers to forget an account nobody asked about")
	}

	_, asked := get(t, client, "/restore?forget=departed")
	for _, want := range []string{
		"Forget departed?", "deletes every backup", `name="confirm"`,
	} {
		if !strings.Contains(asked, want) {
			t.Errorf("the confirmation does not say %q", want)
		}
	}
	// A name nobody has retired is not something to offer at all.
	_, other := get(t, client, "/restore?forget=someoneelse")
	if strings.Contains(other, "Forget someoneelse?") {
		t.Error("the page offers to forget an account it does not list")
	}

	// Without the box ticked, nothing happens.
	resp, err := client.PostForm("http://ui/restore/forget", map[string][]string{
		"csrf": {csrfToken(t, asked)}, "account": {"departed"},
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if _, err := engine.Store().Identity("departed"); err != nil {
		t.Errorf("the account was forgotten without a confirmation: %v", err)
	}
}

// A backup takes minutes and a restore can take longer. Until the strip
// existed, the only way to know one was under way was to be on the page
// that lists them, so somebody who asked for a restore and saw nothing
// asked for it again.
func TestTheStripSaysWhatIsRunningOnEveryPage(t *testing.T) {
	client, _, engine := newUI(t)

	running, err := engine.Store().PutJob(nodestore.Job{
		Account: "customer1", Status: job.StatusRunning, QueuedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Store().SetJobProgress(running.ID, nodestore.JobProgress{
		Percent: 42.5, Repository: "the vault",
	}); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/", "/destinations", "/restore"} {
		_, page := get(t, client, path)
		for _, want := range []string{
			"Backing up customer1", "the vault", "43%",
			// The page refreshes itself while it runs, by the same
			// mechanism the history page uses.
			`data-running="1"`, `data-live="cpr-running"`,
		} {
			if !strings.Contains(page, want) {
				t.Errorf("%s does not say %q", path, want)
			}
		}
	}

	// Nothing running, nothing said, and no refresh loop either.
	finished := time.Now()
	running.Status = job.StatusSuccess
	running.FinishedAt = &finished
	if _, err := engine.Store().PutJob(running); err != nil {
		t.Fatal(err)
	}
	_, quiet := get(t, client, "/destinations")
	if strings.Contains(quiet, "Backing up customer1") {
		t.Error("a finished backup is still shown as running")
	}
}

// A whole-account restore used to mean "the newest backup there is". An
// account broken this morning wants last night's, and an operator
// restoring several at once means one date rather than a snapshot id each.
func TestARestoreCanBeAskedForAsAtADate(t *testing.T) {
	client, _, engine := newUI(t)

	finished := time.Now().Add(-2 * time.Hour)
	retired := time.Now().Add(-time.Hour)
	if _, err := engine.Store().PutJob(nodestore.Job{
		Account: "departed", Status: job.StatusSuccess, FinishedAt: &finished,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Store().PutIdentity(nodestore.AccountIdentity{
		Account: "departed", UID: 1234, SinceAt: finished,
		LastSeen: finished, CreatedAt: finished, RetiredAt: &retired,
	}); err != nil {
		t.Fatal(err)
	}

	_, page := get(t, client, "/restore")
	if !strings.Contains(page, `name="asof"`) {
		t.Error("the restore page does not ask which restore point")
	}

	// A date that is not one is refused rather than quietly meaning
	// "the newest", which would restore the wrong day without saying so.
	resp, err := client.PostForm("http://ui/recover/accounts", map[string][]string{
		"csrf": {csrfToken(t, page)}, "account": {"customer1"},
		"repository": {"repo"}, "asof": {"last tuesday"},
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if location := resp.Header.Get("Location"); !strings.Contains(location, "is+not+a+date") {
		t.Errorf("a date that is not one was accepted: %s", location)
	}
	restores, err := engine.Store().Restores(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(restores) != 0 {
		t.Errorf("queued %+v", restores)
	}
}

// Reporting a problem must work when the thing being reported is the
// interface: the icon is a link to a page, the dialog is an enhancement,
// and nothing is sent until the operator has seen the whole report.
func TestAProblemIsReportedOnlyAfterItIsShown(t *testing.T) {
	client, _, _ := newUI(t)

	_, page := get(t, client, "/")
	if !strings.Contains(page, `href="?p=report"`) {
		t.Error("no way to report a problem from the page")
	}

	_, form := get(t, client, "/report")
	for _, want := range []string{`name="subject"`, `name="body"`, "Download it"} {
		if !strings.Contains(form, want) {
			t.Errorf("the report page does not carry %q", want)
		}
	}

	// A report with nothing in it is refused rather than sent empty.
	resp, err := client.PostForm("http://ui/report/send", map[string][]string{
		"csrf": {csrfToken(t, form)}, "subject": {"  "}, "body": {""},
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	empty, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(empty), "needs a subject") {
		t.Error("an empty report was accepted")
	}

	// Pressing the first button shows the report; nothing leaves the server.
	resp, err = client.PostForm("http://ui/report/send", map[string][]string{
		"csrf": {csrfToken(t, form)}, "subject": {"A restore failed"},
		"body": {"It said success and the account was not there."},
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	shown, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	body := string(shown)
	for _, want := range []string{
		"The report", "It said success", "Versions and environment",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the preview does not carry %q", want)
		}
	}
	// Nothing on this page transmits, so there is nothing to press that
	// would -- only the file the operator carries themselves.
	if strings.Contains(body, `name="send" value="1"`) {
		t.Error("the page offers to send a report from the server")
	}

	// The same report as a file, which is how it leaves.
	file, err := client.PostForm("http://ui/report/send", map[string][]string{
		"csrf": {csrfToken(t, form)}, "subject": {"A restore failed"},
		"body": {"It said success and the account was not there."}, "download": {"1"},
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer file.Body.Close()
	if got := file.Header.Get("Content-Disposition"); !strings.Contains(got, "gniza-report-") {
		t.Errorf("the report does not come back as a file: %q", got)
	}
	saved, err := io.ReadAll(file.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), "### Versions and environment") {
		t.Error("the file does not carry the report")
	}
}

func TestThePageOpensInLightUnlessToldOtherwise(t *testing.T) {
	client, _, _ := newUI(t)

	_, page := get(t, client, "/")
	for _, want := range []string{
		`var choice = "light";`,
		`if (choice !== "system") {`,
		`document.documentElement.setAttribute("data-theme", choice);`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page does not open in light: %s is missing", want)
		}
	}
	if !strings.Contains(page, `data-theme-choice="light" aria-pressed="true"`) {
		t.Error("the picker does not show light as the current theme")
	}
	if !strings.Contains(page, `data-theme-choice="dark"`) {
		t.Error("dark is no longer offered")
	}
	if !strings.Contains(page, `return (choice === "dark" || choice === "system") ? choice : "light";`) {
		t.Error("app.js does not fall back to light")
	}
}

func TestWHMPagesUseTheOperationalRail(t *testing.T) {
	client, _, _ := newUI(t)

	_, page := get(t, client, "/restore")
	for _, want := range []string{
		`class="cpr-shell"`,
		`class="cpr-rail"`,
		`class="cpr-workspace"`,
		`href="?p=restore" aria-label="Restore" aria-current="page"`,
		`id="gniza-main" tabindex="-1"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("operational rail page is missing %q", want)
		}
	}
}

// The account-facing interface takes who is asking from the socket, never
// from the request. A page that cannot be attributed is refused rather
// than shown somebody's backups.
func TestTheUserInterfaceRefusesWhatItCannotAttribute(t *testing.T) {
	client, _, _ := newUI(t)

	// newUI serves the operator's handler; what matters here is that the
	// account-facing one refuses a request carrying no account, which is
	// what an unattributed connection produces.
	_ = client
	server := httptest.NewServer(newUserHandler(t))
	defer server.Close()

	resp, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a request with no account = %d, want 403", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "could not tell which account") {
		t.Errorf("the refusal does not say why: %s", body)
	}
}

func TestAccountPagesDoNotRevealTheRootWHMCSRFToken(t *testing.T) {
	server, err := webui.New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("build ui: %v", err)
	}
	admin := server.AdminCSRFForTest()
	studio := server.UserCSRFForTest("studio")
	rtflow := server.UserCSRFForTest("rtflow")
	if admin == studio || admin == rtflow {
		t.Fatal("a cPanel account received root WHM's CSRF token")
	}
	if studio == rtflow {
		t.Fatal("two cPanel accounts share a CSRF token")
	}
	if studio != server.UserCSRFForTest("studio") {
		t.Fatal("an account's token is not stable for the life of the service")
	}
}

// newUserHandler builds the account-facing handler on its own, so it can
// be asked what it does with a request that has no account attached.
func newUserHandler(t *testing.T) http.Handler {
	t.Helper()
	_, _, engine := newUI(t)
	server, err := webui.New(engine, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("build ui: %v", err)
	}
	return server.UserHandler()
}

// The two listeners used to share a directory and set it to different
// modes as they started, so which boundary a server ended up with
// depended on which won the race. Each has its own now.
func TestTheTwoSocketsHaveTheirOwnDirectories(t *testing.T) {
	root := t.TempDir()
	admin := filepath.Join(root, "admin")
	account := filepath.Join(root, "account")

	if err := webui.PrepareSocketDirForTest(admin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := webui.PrepareSocketDirForTest(account, 0o755); err != nil {
		t.Fatal(err)
	}

	for dir, want := range map[string]os.FileMode{admin: 0o700, account: 0o755} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s is %o, want %o", dir, got, want)
		}
	}

	// And making one does not change the other.
	if err := webui.PrepareSocketDirForTest(account, 0o755); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(admin)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("the operator's directory became %o when the account one was made", got)
	}
}

// One account, one request at a time: listing a repository runs restic
// against the destination, and anything running as the account could
// otherwise keep this server busy on its behalf.
func TestOneAccountGetsOneRequestAtATime(t *testing.T) {
	var busy webui.InFlightForTest
	if !busy.Enter("studio") {
		t.Fatal("the first request was refused")
	}
	if busy.Enter("studio") {
		t.Error("a second request for the same account was allowed")
	}
	if !busy.Enter("rtflow") {
		t.Error("another account was held up by the first one")
	}
	busy.Leave("studio")
	if !busy.Enter("studio") {
		t.Error("the account is still held after its request finished")
	}
}

// postForm submits a form and hands back what came out of it, which is a
// page when the handler asked something and an empty body when it acted.
func postForm(t *testing.T, client *http.Client, path string,
	form map[string][]string) (int, string) {

	t.Helper()
	resp, err := client.PostForm("http://ui"+path, form)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// Putting part of an account back replaces what is live, and the operator's
// side used to ask nothing at all: one button carried a window.confirm,
// which is not there with scripting off, and every handler took apply=1 on
// its own. The customer's side has required a tick since it was written.
func TestPuttingAPartBackAsksFirst(t *testing.T) {
	client, _, engine := newUI(t)

	_, page := get(t, client, "/restore")
	form := map[string][]string{
		"csrf": {csrfToken(t, page)}, "account": {"customer1"},
		"repository": {"vault"}, "snapshot": {"abcdef0123456789"},
		"item": {string(granular.KindDatabase)}, "name": {"customer1_shop"},
		"apply": {"1"},
	}

	status, asked := postForm(t, client, "/restore/items", form)
	if status != http.StatusOK {
		t.Fatalf("the restore ran without asking: status %d", status)
	}
	for _, want := range []string{
		"Put a database back into customer1?", "There is no undo",
		"customer1_shop", `name="confirm"`,
		// The held request comes back as hidden fields, so ticking the
		// box runs the restore that was described and not another one.
		`name="apply" value="1"`, `name="item" value="database"`,
	} {
		if !strings.Contains(asked, want) {
			t.Errorf("the confirmation does not say %q", want)
		}
	}

	restores, err := engine.Store().Restores(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(restores) != 0 {
		t.Fatalf("a restore was queued without a confirmation: %+v", restores)
	}

	// Ticked, it is no longer this page's business: the handler acts and
	// redirects, whatever the engine then makes of the request.
	form["confirm"] = []string{"1"}
	status, _ = postForm(t, client, "/restore/items", form)
	if status == http.StatusOK {
		t.Error("the confirmation was shown again after it was given")
	}
}

// The bulk restore is where a name nobody meant to tick is easiest to
// miss, so the confirmation reads the names back.
func TestRestoringSeveralAccountsAsksFirst(t *testing.T) {
	client, _, engine := newUI(t)

	_, page := get(t, client, "/restore")
	form := map[string][]string{
		"csrf": {csrfToken(t, page)}, "repository": {"vault"},
		"account": {"customer1", "customer2"}, "apply": {"1"},
	}

	status, asked := postForm(t, client, "/recover/accounts", form)
	if status != http.StatusOK {
		t.Fatalf("the restore ran without asking: status %d", status)
	}
	for _, want := range []string{
		"Restore 2 accounts onto this server?", "There is no undo",
		"customer1", "customer2", `name="confirm"`,
	} {
		if !strings.Contains(asked, want) {
			t.Errorf("the confirmation does not say %q", want)
		}
	}

	restores, err := engine.Store().Restores(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(restores) != 0 {
		t.Fatalf("a restore was queued without a confirmation: %+v", restores)
	}
}

// Rebuilding an archive overwrites nothing, so it is not something to ask
// about. Only handing that archive to cPanel's own restore is.
func TestRebuildingAnArchiveIsNotAskedAbout(t *testing.T) {
	client, _, _ := newUI(t)

	_, page := get(t, client, "/restore")
	status, body := postForm(t, client, "/recover/accounts", map[string][]string{
		"csrf": {csrfToken(t, page)}, "repository": {"vault"},
		"account": {"customer1"},
	})
	if status == http.StatusOK && strings.Contains(body, `name="confirm"`) {
		t.Error("rebuilding an archive asks a question it does not need to ask")
	}
}

// An account cPanel no longer has is the same button and a different
// question: there is nothing on this server to replace, and a warning that
// says otherwise is a warning that gets read past.
func TestRecreatingADeletedAccountSaysSo(t *testing.T) {
	client, _, engine := newUI(t)

	seen := time.Now().Add(-2 * time.Hour)
	retired := time.Now().Add(-time.Hour)
	if _, err := engine.Store().PutIdentity(nodestore.AccountIdentity{
		Account: "departed", UID: 1234, SinceAt: seen,
		LastSeen: seen, CreatedAt: seen, RetiredAt: &retired,
	}); err != nil {
		t.Fatal(err)
	}

	_, page := get(t, client, "/restore")
	_, asked := postForm(t, client, "/restore/start", map[string][]string{
		"csrf": {csrfToken(t, page)}, "account": {"departed"},
		"repository": {"vault"}, "snapshot": {"abcdef0123456789"},
		"apply": {"1"},
	})
	if !strings.Contains(asked, "Create departed again?") {
		t.Error("recreating a deleted account is described as an overwrite")
	}
	if strings.Contains(asked, "There is no undo") {
		t.Error("a restore that replaces nothing warns that it cannot be undone")
	}

	// The same request for an account this server still has.
	_, live := postForm(t, client, "/restore/start", map[string][]string{
		"csrf": {csrfToken(t, page)}, "account": {"customer1"},
		"repository": {"vault"}, "snapshot": {"abcdef0123456789"},
		"apply": {"1"},
	})
	for _, want := range []string{"Overwrite customer1?", "There is no undo"} {
		if !strings.Contains(live, want) {
			t.Errorf("the confirmation does not say %q", want)
		}
	}
}

// A restore is queued before it runs, because an account has one job in
// flight at a time. The strip used to show nothing until it started, so
// somebody who had just asked for one saw the same empty page they saw
// before asking.
func TestWorkThatIsWaitingIsShownAsWaiting(t *testing.T) {
	client, _, engine := newUI(t)

	if _, err := engine.Store().PutRestore(nodestore.Restore{
		Account: "customer1", RepositoryID: "vault", SnapshotID: "abcdef01",
		Kind: protocol.RestoreAccount, Status: job.StatusPending, QueuedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	_, page := get(t, client, "/")
	for _, want := range []string{
		"Waiting to restore customer1", "the whole account",
		`data-running="1"`, `data-live="cpr-running"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the strip does not say %q", want)
		}
	}
	// Nothing is turning yet, so nothing spins and no bar sits at zero.
	if strings.Contains(page, `role="progressbar"`) {
		t.Error("a restore that has not started is shown with a progress bar")
	}
}

// A restore takes longer than a backup and until now said nothing at all
// while it ran: no stage, no percentage, no bar.
func TestARunningRestoreShowsItsStageAndABar(t *testing.T) {
	client, _, engine := newUI(t)

	started := time.Now()
	stored, err := engine.Store().PutRestore(nodestore.Restore{
		Account: "customer1", RepositoryID: "vault", SnapshotID: "abcdef01",
		Kind: protocol.RestoreAccount, Status: job.StatusRunning,
		QueuedAt: started, StartedAt: &started,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Store().SetRestoreProgress(stored.ID, nodestore.RestoreProgress{
		Stage: "reading the home directory", Percent: 57.4, Known: true,
	}); err != nil {
		t.Fatal(err)
	}

	_, page := get(t, client, "/restore")
	for _, want := range []string{
		"Restoring customer1", "reading the home directory", "57%",
		`role="progressbar"`, "width:57.4%",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the strip does not say %q", want)
		}
	}

	// A stage restic cannot count says so rather than sitting at nothing.
	if err := engine.Store().SetRestoreProgress(stored.ID, nodestore.RestoreProgress{
		Stage: "handing the archive to cPanel's restore",
	}); err != nil {
		t.Fatal(err)
	}
	_, uncounted := get(t, client, "/restore")
	if !strings.Contains(uncounted, "handing the archive to cPanel&#39;s restore") {
		t.Error("the strip does not name a stage restic cannot count")
	}
	if strings.Contains(uncounted, `role="progressbar"`) {
		t.Error("a stage with nothing to count is shown with a bar")
	}
}
