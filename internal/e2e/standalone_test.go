//go:build e2e

package e2e_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/cpanel"
	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/layout/cpmove"
	"github.com/shukiv/gniza/internal/node"
	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/testsupport"
	"github.com/shukiv/gniza/internal/vault"
	"github.com/shukiv/gniza/internal/webui"
)

// standalone is one cPanel server running on its own: local state, local
// scheduling, the interface the WHM plugin proxies to, and a real restic.
// No controller, no PostgreSQL.
type standalone struct {
	ctx      context.Context
	engine   *node.Engine
	client   *http.Client
	provider *cpanel.Fake
	root     string
}

func newStandalone(t *testing.T) *standalone {
	t.Helper()
	resticPath := testsupport.RequireBinary(t, "restic")

	root := t.TempDir()
	store, err := nodestore.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	settings := nodestore.DefaultSettings()
	settings.StagingRoot = filepath.Join(root, "staging")
	settings.ResticCache = filepath.Join(root, "cache")
	settings.ResticBinary = resticPath
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

	provider := &cpanel.Fake{
		Root:      filepath.Join(root, "cpanel"),
		Databases: map[string][]string{"customer1": {"customer1_wp"}},
		FileCount: 6, FileSize: 4096,
	}
	engine, err := node.New(node.Config{
		Store: store, Vault: v, Provider: provider, Log: testLogger(t),
	})
	if err != nil {
		t.Fatalf("build engine: %v", err)
	}

	server, err := webui.New(engine, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(root, "ui.sock")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = server.Listen(ctx, socket) }()

	client := &http.Client{
		Timeout: 3 * time.Minute,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.Dial("unix", socket); err == nil {
			_ = conn.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return &standalone{ctx: ctx, engine: engine, client: client, provider: provider, root: root}
}

func (s *standalone) page(t *testing.T, path string) string {
	t.Helper()
	resp, err := s.client.Get("http://ui" + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d\n%s", path, resp.StatusCode, body)
	}
	return string(body)
}

// post submits a form the way the plugin does, carrying the token from the
// page it was rendered on.
func (s *standalone) post(t *testing.T, from, action string, fields map[string]string) string {
	t.Helper()
	page := s.page(t, from)

	values := map[string][]string{"csrf": {token(t, page)}}
	for key, value := range fields {
		values[key] = []string{value}
	}
	resp, err := s.client.PostForm("http://ui"+action, values)
	if err != nil {
		t.Fatalf("POST %s: %v", action, err)
	}
	defer resp.Body.Close()
	if action == "/destinations/add" && resp.StatusCode == http.StatusOK {
		// New destinations finish on the recovery-key page, not a redirect.
		// Exercise that confirmation rather than bypassing it in the fixture.
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		page := string(body)
		if !strings.Contains(page, "destinations/recovery/note") {
			t.Fatal("adding a destination did not present its recovery-key confirmation")
		}
		_, rest, found := strings.Cut(page, `name="repository" value="`)
		if !found {
			t.Fatal("recovery-key confirmation has no repository")
		}
		repositoryID, _, _ := strings.Cut(rest, `"`)
		confirmation, err := s.client.PostForm("http://ui/destinations/recovery/note", map[string][]string{
			"csrf": {token(t, page)}, "repository": {repositoryID},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer confirmation.Body.Close()
		if confirmation.StatusCode != http.StatusSeeOther {
			t.Fatalf("confirming the recovery key returned %d", confirmation.StatusCode)
		}
		return confirmation.Header.Get("Location")
	}
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST %s = %d\n%s", action, resp.StatusCode, body)
	}
	return resp.Header.Get("Location")
}

func token(t *testing.T, page string) string {
	t.Helper()
	const marker = `name="csrf" value="`
	start := strings.Index(page, marker)
	if start < 0 {
		t.Fatal("no csrf token on the page")
	}
	rest := page[start+len(marker):]
	return rest[:strings.Index(rest, `"`)]
}

// TestStandaloneBackupAndRestoreThroughTheInterface is the plugin product
// end to end: an operator adds a destination, schedules a backup, runs one,
// and restores it — all through the interface WHM proxies to, against a
// real restic repository.
func TestStandaloneBackupAndRestoreThroughTheInterface(t *testing.T) {
	s := newStandalone(t)

	// 1. Add a destination. The interface creates its repository and
	//    checks it can be reached before saying so.
	destinationRoot := filepath.Join(s.root, "backup-server")
	if err := os.MkdirAll(destinationRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	location := s.post(t, "/destinations", "/destinations/add", map[string]string{
		"name": "Backup disk", "type": "local",
		"root": destinationRoot, "repo_path": "cp01",
	})
	if !strings.Contains(location, "kind=ok") {
		t.Fatalf("adding a destination reported: %s", location)
	}
	repositories, err := s.engine.Store().Repositories()
	if err != nil {
		t.Fatal(err)
	}
	if len(repositories) != 1 || repositories[0].InitialisedAt == nil {
		t.Fatalf("repositories = %+v", repositories)
	}
	repositoryID := repositories[0].ID

	// 2. Schedule it.
	s.post(t, "/schedule", "/schedule/save", map[string]string{
		"name": "Nightly", "cron": "0 2 * * *", "mode": "split",
		"repository": repositoryID, "scope": "all", "enabled": "1",
		"keep_daily": "7", "keep_weekly": "4", "keep_monthly": "6",
	})
	policies, err := s.engine.Store().Policies()
	if err != nil || len(policies) != 1 {
		t.Fatalf("policies = %+v (%v)", policies, err)
	}

	// 3. Back up now, from the accounts page.
	s.post(t, "/accounts", "/accounts/backup", map[string]string{
		"account": "customer1", "policy": policies[0].ID,
	})
	runQueue(t, s)

	jobs, err := s.engine.Store().Jobs(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %+v", jobs)
	}
	if jobs[0].Status != job.StatusSuccess {
		t.Fatalf("backup status = %q (%s)", jobs[0].Status, jobs[0].StagingErr)
	}
	if jobs[0].Targets[0].SnapshotID == "" {
		t.Error("the backup recorded no snapshot")
	}

	// 4. The restore page offers that snapshot.
	restorePage := s.page(t, "/restore?account=customer1&repository="+repositoryID)
	if !strings.Contains(restorePage, jobs[0].Targets[0].SnapshotID) {
		t.Fatalf("the restore page does not list the snapshot just taken")
	}

	// The same page lists what the destination itself holds, read from the
	// backups rather than from cPanel, so an account this server no longer
	// has can still be chosen. Several can be ticked and restored at once.
	for _, want := range []string{
		"Accounts in this destination",
		`<input type="checkbox" name="account" value="customer1"`,
		`action="?p=recover/accounts"`,
	} {
		if !strings.Contains(restorePage, want) {
			t.Errorf("the restore page is missing %q", want)
		}
	}

	// 5. Restore the whole account. Nothing is applied unless asked.
	original := readTree(t, filepath.Join(s.provider.Root, "home", "customer1"))
	s.post(t, "/restore?account=customer1&repository="+repositoryID, "/restore/start",
		map[string]string{
			"account": "customer1", "repository": repositoryID,
			"snapshot": jobs[0].Targets[0].SnapshotID,
		})
	runQueue(t, s)

	restores, err := s.engine.Store().Restores(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(restores) != 1 || restores[0].Status != job.StatusSuccess {
		t.Fatalf("restores = %+v", restores)
	}
	if restores[0].Applied || len(s.provider.Applied) != 0 {
		t.Error("the restore was applied to the live account without being asked")
	}

	// 6. What came back is what went in.
	tree := filepath.Join(filepath.Dir(restores[0].ArchivePath), "tree")
	accountRoot, err := soleDir(tree)
	if err != nil {
		t.Fatalf("restored tree: %v", err)
	}
	compareTrees(t, original, readTree(t, filepath.Join(accountRoot, cpmove.HomedirDir)))

	dump, err := os.ReadFile(filepath.Join(accountRoot, cpmove.DatabaseDir, "customer1_wp.sql"))
	if err != nil {
		t.Fatalf("restored database dump: %v", err)
	}
	if !strings.Contains(string(dump), "CREATE TABLE") {
		t.Errorf("the restored dump does not look like SQL: %q", dump)
	}

	// 7. The logs show both, and what the backup actually cost.
	history := s.page(t, "/logs")
	for _, want := range []string{"customer1", "success", "new of"} {
		if !strings.Contains(history, want) {
			t.Errorf("the history page is missing %q", want)
		}
	}
}

// TestStandaloneRetentionPrunes checks that the property three fleet-mode
// bugs were about still holds here: two runs of one account land in the
// same retention group, so retention can actually remove one.
func TestStandaloneRetentionPrunes(t *testing.T) {
	s := newStandalone(t)

	destinationRoot := filepath.Join(s.root, "backup-server")
	if err := os.MkdirAll(destinationRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	s.post(t, "/destinations", "/destinations/add", map[string]string{
		"name": "Backup disk", "type": "local",
		"root": destinationRoot, "repo_path": "cp01",
	})
	repositories, err := s.engine.Store().Repositories()
	if err != nil || len(repositories) != 1 {
		t.Fatalf("repositories = %+v (%v)", repositories, err)
	}
	repositoryID := repositories[0].ID

	s.post(t, "/schedule", "/schedule/save", map[string]string{
		"name": "Nightly", "cron": "0 2 * * *", "mode": "split",
		"repository": repositoryID, "scope": "all", "enabled": "1",
	})
	policies, _ := s.engine.Store().Policies()

	for range 2 {
		if _, err := s.engine.QueueBackup(policies[0].ID, "customer1"); err != nil {
			t.Fatalf("queue backup: %v", err)
		}
		runQueue(t, s)
	}

	snapshots, err := s.engine.Snapshots(s.ctx, repositoryID, "customer1")
	if err != nil {
		t.Fatalf("list snapshots: %v", err)
	}
	if len(snapshots) != 2 {
		t.Fatalf("expected 2 snapshots, got %d", len(snapshots))
	}

	if err := s.engine.Forget(s.ctx, repositoryID,
		nodestore.Retention{KeepLast: 1}, true); err != nil {
		t.Fatalf("forget: %v", err)
	}
	remaining, err := s.engine.Snapshots(s.ctx, repositoryID, "customer1")
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 {
		t.Errorf("retention kept %d snapshots, want 1", len(remaining))
	}
}

// TestStandaloneDrill rehearses a restore the way scheduled upkeep would.
func TestStandaloneDrill(t *testing.T) {
	s := newStandalone(t)

	destinationRoot := filepath.Join(s.root, "backup-server")
	if err := os.MkdirAll(destinationRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	s.post(t, "/destinations", "/destinations/add", map[string]string{
		"name": "Backup disk", "type": "local",
		"root": destinationRoot, "repo_path": "cp01",
	})
	repositories, _ := s.engine.Store().Repositories()
	s.post(t, "/schedule", "/schedule/save", map[string]string{
		"name": "Nightly", "cron": "0 2 * * *", "mode": "split",
		"repository": repositories[0].ID, "scope": "all", "enabled": "1",
	})
	policies, _ := s.engine.Store().Policies()
	if _, err := s.engine.QueueBackup(policies[0].ID, "customer1"); err != nil {
		t.Fatal(err)
	}
	runQueue(t, s)

	// Scratch space comes from the staging manager now, so the same disk
	// check that guards a backup guards the rehearsal.
	checks, skipped, err := s.engine.Drill(s.ctx, repositories[0].ID, "customer1")
	if err != nil {
		t.Fatalf("drill: %v", err)
	}
	if len(checks) < 3 {
		t.Errorf("the drill made only %d checks: %v", len(checks), checks)
	}
	// This schedule leaves nothing out, so the rehearsal has nothing to
	// say about parts that were not in the snapshot.
	if len(skipped) != 0 {
		t.Errorf("the drill reported parts as skipped for a full backup: %v", skipped)
	}
}

// runQueue drains whatever the interface queued.
func runQueue(t *testing.T, s *standalone) {
	t.Helper()
	for range 10 {
		did, err := s.engine.RunOnce(s.ctx)
		if err != nil {
			t.Fatalf("run work: %v", err)
		}
		if !did {
			return
		}
	}
	t.Fatal("the queue did not drain")
}
