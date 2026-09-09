package webui

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/cpanel"
	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/node"
	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/panel"
	"github.com/shukiv/gniza/internal/vault"
)

func lifecycleTestServer(t *testing.T) (*Server, *nodestore.Store) {
	t.Helper()
	root := t.TempDir()
	store, err := nodestore.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	settings := nodestore.DefaultSettings()
	settings.StagingRoot = filepath.Join(root, "staging")
	settings.ResticCache = filepath.Join(root, "cache")
	settings.ConfigDir = filepath.Join(root, "config")
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	v, err := vault.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine, err := node.New(node.Config{
		Store: store, Vault: v,
		Provider: &cpanel.Fake{Root: filepath.Join(root, "cpanel")}, Log: log,
		AccountUID: func(string) (int, error) { return 1500, nil },
		HookSpool:  filepath.Join(root, "hooks"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &Server{engine: engine, log: log}, store
}

func TestLifecycleAccountReadsStandardizedHookEnvelope(t *testing.T) {
	raw := []byte(`{"context":{"event":"Accounts::Remove"},"data":{"user":"customer1"}}`)
	if got := lifecycleAccount(raw); got != "customer1" {
		t.Fatalf("account = %q", got)
	}
}

func TestLifecycleAccountRejectsCommandLikeUser(t *testing.T) {
	raw := []byte(`{"data":{"user":"customer1;rm"}}`)
	if got := lifecycleAccount(raw); got != "" {
		t.Fatalf("unsafe account accepted: %q", got)
	}
}

func TestLifecycleEventTitlesAreExplicit(t *testing.T) {
	tests := []struct {
		event, title, outcome string
		ok                    bool
	}{
		{event: "suspend", title: "Suspended", outcome: "Handled", ok: true},
		{event: "unsuspend", title: "Unsuspended", outcome: "Handled", ok: true},
		{event: "remove-pre", title: "Removal check", outcome: "Blocked"},
	}
	for _, test := range tests {
		event := nodestore.LifecycleEvent{Event: test.event, OK: test.ok}
		if event.Title() != test.title || event.Outcome() != test.outcome {
			t.Errorf("%s = %q/%q", test.event, event.Title(), event.Outcome())
		}
	}
}

func TestSuspensionHookQueuesPreservationWithoutRetainingRawPayload(t *testing.T) {
	server, store := lifecycleTestServer(t)
	settings, _ := store.Settings()
	settings.BackupOnSuspension = true
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	policy, err := store.PutPolicy(nodestore.Policy{
		Name: "full off-site", Enabled: true, RepositoryIDs: []string{"repo-1"},
	})
	if err != nil {
		t.Fatal(err)
	}

	payload := `{"data":{"user":"customer1","reason":"private billing note","rawout":"sensitive command output"}}`
	request := httptest.NewRequest(http.MethodPost, "/event?event=suspend", strings.NewReader(payload))
	response := httptest.NewRecorder()
	server.handleLifecycleEvent(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("suspend status = %d: %s", response.Code, response.Body.String())
	}
	jobs, err := store.Jobs(0)
	if err != nil || len(jobs) != 1 || jobs[0].PolicyID != policy.ID {
		t.Fatalf("preservation jobs = %+v, %v", jobs, err)
	}
	events, err := store.LifecycleEvents(0)
	if err != nil || len(events) != 1 {
		t.Fatalf("lifecycle events = %+v, %v", events, err)
	}
	if events[0].Account != "customer1" || events[0].Event != "suspend" || !events[0].OK {
		t.Fatalf("suspension event = %+v", events[0])
	}
	if strings.Contains(events[0].Detail, "private billing note") ||
		strings.Contains(events[0].Detail, "sensitive command output") {
		t.Fatalf("raw cPanel payload was retained: %+v", events[0])
	}

	request = httptest.NewRequest(http.MethodPost, "/event?event=unsuspend", strings.NewReader(payload))
	response = httptest.NewRecorder()
	server.handleLifecycleEvent(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("unsuspend status = %d: %s", response.Code, response.Body.String())
	}
	jobs, _ = store.Jobs(0)
	if len(jobs) != 1 {
		t.Fatalf("unsuspend queued another backup: %+v", jobs)
	}
}

func TestCoverageCountsAnOldOneOffBackupAsUnscheduled(t *testing.T) {
	finished := time.Now().Add(-time.Hour)
	view := dashboardView{}
	addCoverage(&view, []accountView{{
		LastBackup: &finished, LastStatus: job.StatusSuccess,
	}})
	if view.Unscheduled != 1 || view.Protected != 0 || view.Stale != 1 {
		t.Fatalf("coverage = unscheduled %d, protected %d, stale %d",
			view.Unscheduled, view.Protected, view.Stale)
	}
}

func TestCoverageDoesNotHideMissingSecondDestination(t *testing.T) {
	finished := time.Now().Add(-time.Hour)
	account := accountView{
		LastBackup: &finished, LastStatus: job.StatusSuccess,
		ExpectedEvery: 24 * time.Hour,
		FreshCopies:   []string{"local"}, MissingCopies: []string{"off-site"},
	}
	if account.State() != StateCopyGap {
		t.Fatalf("state = %q, want copy gap", account.State())
	}
	view := dashboardView{}
	addCoverage(&view, []accountView{account})
	if view.CopyGaps != 1 || view.Protected != 0 || view.Stale != 1 {
		t.Fatalf("coverage hid destination gap: %+v", view)
	}
}

func TestTargetCoverageUsesEachDestinationsOwnFreshnessWindow(t *testing.T) {
	now := time.Now()
	fresh, missing := targetCoverage(
		map[string]time.Duration{"daily": 48 * time.Hour, "weekly": 14 * 24 * time.Hour, "never": 24 * time.Hour},
		map[string]time.Time{"daily": now.Add(-3 * 24 * time.Hour), "weekly": now.Add(-7 * 24 * time.Hour)},
		map[string]string{"daily": "Local", "weekly": "Off-site", "never": "Archive"}, now)
	if len(fresh) != 1 || fresh[0] != "Off-site" {
		t.Fatalf("fresh = %v", fresh)
	}
	if len(missing) != 2 || missing[0] != "Archive" || missing[1] != "Local" {
		t.Fatalf("missing = %v", missing)
	}
}

func TestRepairPolicyChoosesPolicyThatRepairsMostMissingCopies(t *testing.T) {
	policies := []nodestore.Policy{
		{ID: "named-wrong", Name: "wrong", Enabled: true,
			Accounts: []string{"customer2"}, RepositoryIDs: []string{"a", "b"}},
		{ID: "one", Name: "one", Enabled: true, RepositoryIDs: []string{"a"}},
		{ID: "two", Name: "two", Enabled: true, RepositoryIDs: []string{"a", "b"}},
	}
	id, _ := repairPolicy(policies, "customer1", map[string]bool{"a": true, "b": true})
	if id != "two" {
		t.Fatalf("repair policy = %q, want widest relevant policy", id)
	}
}

func TestPreferredBackupPolicyIsEnabledCoveringAndComplete(t *testing.T) {
	policies := []nodestore.Policy{
		{ID: "disabled", Name: "a", Enabled: false, RepositoryIDs: []string{"a", "b", "c"}},
		{ID: "other", Name: "b", Enabled: true, Accounts: []string{"customer2"}, RepositoryIDs: []string{"a", "b", "c"}},
		{ID: "partial", Name: "c", Enabled: true, SkipEmail: true, RepositoryIDs: []string{"a", "b", "c"}},
		{ID: "full", Name: "d", Enabled: true, RepositoryIDs: []string{"a"}},
	}
	selected, ok := preferredBackupPolicy(policies, "customer1", false)
	if !ok || selected.ID != "full" {
		t.Fatalf("preferred policy = %+v, %v", selected, ok)
	}
	selected, ok = preferredBackupPolicy([]nodestore.Policy{
		{ID: "named", Enabled: true, Accounts: []string{"customer1"}, RepositoryIDs: []string{"a", "b"}},
		{ID: "all", Enabled: true, RepositoryIDs: []string{"a"}},
	}, "", true)
	if !ok || selected.ID != "all" {
		t.Fatalf("run-all policy = %+v, %v", selected, ok)
	}
}

func TestCoverageAndLifecycleTemplatesParse(t *testing.T) {
	templates, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	when := time.Now().UTC()
	// Every optional part of the overview filled in, because a template
	// that only ever renders the empty case is a template whose other
	// half has never been executed: the page is assembled in a buffer and
	// a failure halfway through is an internal error, not a half-drawn
	// page.
	run := runSummary{
		PolicyID: "nightly", Policy: "nightly", QueuedAt: when, StartedAt: when,
		FinishedAt: when.Add(42 * time.Minute), Accounts: 19, Succeeded: 18,
		Failed: 1, BytesAdded: 3 << 30,
	}
	fired := when.Add(-time.Hour)
	err = templates["dashboard.html"].ExecuteTemplate(&output, "layout", page{
		Data: dashboardView{
			Accounts: []accountView{{
				AccountInfo: panel.AccountInfo{User: "customer1", SizeBytes: 1 << 30},
				Runs:        9, Succeeded: 6,
			}},
			Destinations: []destinationView{{Endpoint: "sftp://backup", Status: "ok"}},
			Protected:    1, Verified: 1, Managed: 1 << 30,
			Largest: 1 << 30, LargestAccount: "customer1",
			Runs: []runSummary{run}, LastRun: &run, BackedUp: 18,
			Feed: feedOf([]runSummary{run}, nil, []nodestore.LifecycleEvent{{
				Event: "create", Account: "customer1", OK: true, At: when,
			}}, feedShown),
			Schedules: scheduleRows([]nodestore.Policy{{
				ID: "nightly", Name: "nightly", Enabled: true, PayloadMode: "split",
				ScheduleCron: "0 2 * * *", RepositoryIDs: []string{"a"}, LastRunAt: &fired,
				Retention: nodestore.Retention{KeepDaily: 7},
			}}, []runSummary{run}, 19, when),
			Weakest: []accountView{{
				AccountInfo: panel.AccountInfo{User: "customer1"}, Runs: 9, Succeeded: 6,
			}},
			Lifecycle: []nodestore.LifecycleEvent{{
				Event: "create", Account: "customer1", OK: true, At: when,
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Scheduled backup finished", "1 failed", "nightly", "split, deduplicating",
		"Accounts with the worst record", "Under management",
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("the overview is missing %q", want)
		}
	}
	output.Reset()
	safety := node.RemovalSafety{
		Enforced: true, Detail: "no complete copy at off-site",
		MissingRepositoryIDs: []string{"off-site"},
	}
	err = templates["accounts.html"].ExecuteTemplate(&output, "layout", page{
		Data: struct {
			Accounts    []accountView
			RunAll      *nodestore.Policy
			Warnings    []string
			Protected   int
			Unprotected int
		}{Accounts: []accountView{{RemovalSafety: &safety}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output.Bytes(), []byte("Prepare removal")) {
		t.Fatal("blocked account has no removal preparation action")
	}
}

// TestLifecycleAccountIsDeterministic covers a nondeterminism that decides
// which account a blocking removal hook evaluates. Go randomises map
// iteration, so an envelope carrying a usable name in more than one branch
// answered differently from one run to the next -- and the hook would then
// check one account's backup coverage before deleting another.
func TestLifecycleAccountIsDeterministic(t *testing.T) {
	raw := []byte(`{"alpha":{"user":"aaccount"},"zeta":{"user":"zaccount"},` +
		`"context":{"event":"Accounts::Remove"}}`)
	first := lifecycleAccount(raw)
	if first == "" {
		t.Fatal("no account was found in an envelope that names one")
	}
	for i := 0; i < 200; i++ {
		if got := lifecycleAccount(raw); got != first {
			t.Fatalf("the same envelope named %q and then %q", first, got)
		}
	}
	// data still wins outright, whatever else the envelope carries.
	withData := []byte(`{"zeta":{"user":"zaccount"},"data":{"user":"realaccount"}}`)
	if got := lifecycleAccount(withData); got != "realaccount" {
		t.Fatalf("account = %q, want the one cPanel put in data", got)
	}
}
