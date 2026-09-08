package webui

import (
	"path/filepath"

	"context"
	"errors"
	"github.com/shukiv/gniza/internal/job"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/granular"
	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/protocol"
	"github.com/shukiv/gniza/internal/resticrun"
)

func TestFeatureResponseMustExplicitlyAllowTheAccount(t *testing.T) {
	for name, response := range map[string]string{
		"enabled":  `{"data":{"has_feature":1},"metadata":{"result":1,"reason":"OK"}}`,
		"disabled": `{"data":{"has_feature":0},"metadata":{"result":1,"reason":"OK"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			allowed, err := parseFeatureResponse([]byte(response))
			if err != nil {
				t.Fatalf("parseFeatureResponse: %v", err)
			}
			if allowed != (name == "enabled") {
				t.Fatalf("allowed = %v", allowed)
			}
		})
	}
	for _, response := range []string{
		`not json`,
		`{"data":{"has_feature":1},"metadata":{"result":0,"reason":"bad query"}}`,
		`{"data":{"has_feature":1}}`,
	} {
		if allowed, err := parseFeatureResponse([]byte(response)); err == nil || allowed {
			t.Fatalf("unsafe feature response accepted: %s", response)
		}
	}
}

func TestAccountGuardRejectsTheRootTokenAndOtherAccountsTokens(t *testing.T) {
	server := &Server{
		log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		csrfToken:   "root-whm-secret",
		userCSRFKey: []byte("a separate account-facing derivation key"),
	}
	called := false
	handler := server.userGuard(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	for name, token := range map[string]struct {
		token  string
		status int
	}{
		"root token":           {server.csrfToken, http.StatusForbidden},
		"another account":      {server.userCSRFToken("rtflow"), http.StatusForbidden},
		"this account's token": {server.userCSRFToken("studio"), http.StatusOK},
	} {
		t.Run(name, func(t *testing.T) {
			called = false
			body := url.Values{"csrf": {token.token}}.Encode()
			request := httptest.NewRequest(http.MethodPost, "/restore", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request = request.WithContext(context.WithValue(request.Context(), accountKey{}, "studio"))
			response := httptest.NewRecorder()
			handler(response, request)
			if response.Code != token.status || called != (token.status == http.StatusOK) {
				t.Fatalf("status=%d called=%v; want status=%d", response.Code, called, token.status)
			}
		})
	}
}

func TestPublicSocketEnforcesFeatureManagerBehindThePHPFrontend(t *testing.T) {
	for _, test := range []struct {
		name       string
		check      func(context.Context, string) (bool, error)
		wantStatus int
		wantCalled bool
	}{
		{"enabled", func(context.Context, string) (bool, error) { return true, nil }, 200, true},
		{"disabled", func(context.Context, string) (bool, error) { return false, nil }, 403, false},
		{"unverifiable", func(context.Context, string) (bool, error) {
			return false, errors.New("cpanel unavailable")
		}, 503, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil)), userFeatures: accountFeatureGate{
				decisions: map[string]featureDecision{}, check: test.check, slots: make(chan struct{}, 1),
			}}
			called := false
			handler := server.userPage(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusOK)
			})
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request = request.WithContext(context.WithValue(request.Context(), accountKey{}, "studio"))
			response := httptest.NewRecorder()
			handler(response, request)
			if response.Code != test.wantStatus || called != test.wantCalled {
				t.Fatalf("status=%d called=%v; want %d, %v",
					response.Code, called, test.wantStatus, test.wantCalled)
			}
			if test.name == "disabled" && !strings.Contains(response.Body.String(), "not enabled") {
				t.Fatalf("disabled response did not explain Feature Manager: %s", response.Body.String())
			}
		})
	}
}

func TestAccountRestoreHistoryDoesNotExposeRootDiagnostics(t *testing.T) {
	restore := accountSafeRestore(nodestore.Restore{
		Error:       "restic: unable to open sftp:backup-admin@internal.example:/root/archive",
		Detail:      "/var/lib/gniza/staging/restore-studio",
		ArchivePath: "/var/lib/gniza/staging/restore-studio/account.tar",
		RestoredTo:  "/var/lib/gniza/staging/restore-studio/tree",
	})
	if strings.Contains(restore.Error, "internal.example") || restore.Detail != "" ||
		restore.ArchivePath != "" || restore.RestoredTo != "" {
		t.Fatalf("account-visible restore still carries root diagnostics: %+v", restore)
	}

	// A hint is the one part of a failure written for the customer, so it
	// replaces the generic message rather than being swept away with the
	// operator's.
	withHint := accountSafeRestore(nodestore.Restore{
		Error: "agent: c1 no longer has the database(s) c1_shop",
		Hint:  "The database c1_shop is not on the account any more. Create it again first.",
	})
	if !strings.Contains(withHint.Error, "Create it again first") {
		t.Fatalf("the customer lost the one thing they could act on: %+v", withHint)
	}
	if strings.Contains(withHint.Error, "agent:") {
		t.Fatalf("the operator's wording reached the customer: %+v", withHint)
	}
}

func TestAccountRecoveryRequestsKeepTheirBoundaries(t *testing.T) {
	full, err := userRestoreRequest("studio", "repo", "snapshot", userKindAccount, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if full.Kind != protocol.RestoreAccount || full.Apply || full.ItemKind != "" {
		t.Fatalf("full account request crossed the safe-download boundary: %+v", full)
	}

	database, err := userRestoreRequest("studio", "repo", "snapshot",
		granular.KindDatabase, []string{" studio_wp ", ""}, false)
	if err != nil {
		t.Fatal(err)
	}
	if database.Kind != protocol.RestoreItems || database.Apply ||
		database.ItemKind != string(granular.KindDatabase) ||
		len(database.ItemNames) != 1 || database.ItemNames[0] != "studio_wp" {
		t.Fatalf("database request = %+v", database)
	}

	if _, err := userRestoreRequest("studio", "repo", "snapshot",
		granular.KindDatabase, nil, false); !errors.Is(err, errUserRestoreNeedsNames) {
		t.Fatalf("database without a selection was accepted: %v", err)
	}
	if _, err := userRestoreRequest("studio", "repo", "snapshot",
		granular.KindSettings, nil, false); err == nil {
		t.Fatal("raw panel settings were exposed to the account interface")
	}
}

// An account can ask for a restore to be written into its own account, but
// only for the parts that can be written back. Asking for it on any other
// part is refused rather than quietly turned into a download: the request
// said "replace this", and handing back a copy while reporting success
// would tell the customer their site was fixed when it was not.
//
// The whole-account archive is the one that matters most here. It is what
// cPanel's restorepkg takes, it runs as root, and the archive holds the
// customer's own home directory.
func TestAnAccountCanApplyOnlyWhatCanBeWrittenBack(t *testing.T) {
	writable := map[granular.Kind]bool{
		granular.KindFiles:    true,
		granular.KindMailbox:  true,
		granular.KindDatabase: true,
		granular.KindDBUsers:  true,
		granular.KindCron:     true,
	}
	for _, kind := range userKinds {
		names := []string{"something"}
		request, err := userRestoreRequest("studio", "repo", "snapshot", kind, names, true)
		if !writable[kind] {
			if err == nil {
				t.Errorf("%s was accepted for writing into the live account: %+v",
					kind, request)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s should be writable into the account: %v", kind, err)
			continue
		}
		if !request.Apply {
			t.Errorf("%s lost the request to write it back: %+v", kind, request)
		}
	}
}

// A customer sees their own account's work and nobody else's: the strip is
// drawn from the same account the socket confined the request to.
func TestTheRunningStripIsConfinedToOneAccount(t *testing.T) {
	store, err := nodestore.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	backup, err := store.PutJob(nodestore.Job{
		Account: "studio", Status: job.StatusRunning, QueuedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetJobProgress(backup.ID, nodestore.JobProgress{
		Percent: 42.5, Repository: "the vault",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutRestore(nodestore.Restore{
		Account: "other", Status: job.StatusRunning, Kind: protocol.RestoreItems,
		ItemKind: "database", ItemNames: []string{"other_shop"},
	}); err != nil {
		t.Fatal(err)
	}
	// One that has finished says nothing: the strip is what is happening.
	if _, err := store.PutJob(nodestore.Job{
		Account: "studio", Status: job.StatusSuccess, QueuedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	theirs, err := runningWorkFor(store, "studio")
	if err != nil {
		t.Fatal(err)
	}
	if len(theirs) != 1 {
		t.Fatalf("the customer is shown %d runs: %+v", len(theirs), theirs)
	}
	if label := theirs[0].Label(); !strings.Contains(label, "Backing up studio") ||
		!strings.Contains(label, "the vault") || !strings.Contains(label, "43%") {
		t.Errorf("label = %q", label)
	}

	everything, err := runningWorkFor(store, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(everything) != 2 {
		t.Fatalf("the operator is shown %d runs: %+v", len(everything), everything)
	}
	if label := everything[1].Label(); !strings.Contains(label, "Restoring other") {
		t.Errorf("label = %q", label)
	}
}

func TestAccountRecoveryPagesRenderTheGuidedFlow(t *testing.T) {
	server, err := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0).UTC()
	request := httptest.NewRequest(http.MethodGet, "/browse", nil)
	request = request.WithContext(context.WithValue(request.Context(), accountKey{}, "studio"))
	response := httptest.NewRecorder()
	server.renderUser(response, request, "user_browse.html", userView{
		Account: "studio", Kinds: userKinds, Repository: "repo", Snapshot: "abcdef",
		SnapshotAt: now, Snapshots: []resticrun.Snapshot{{ID: "abcdef", Time: now}},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("render status = %d: %s", response.Code, response.Body.String())
	}
	page := response.Body.String()
	for _, want := range []string{
		"Restore &amp; download", "Restore point", "Full account", "Home directory",
		"Cron jobs", "Databases", "Database users", "Domains", "SSL certificates",
		"Email accounts", "FTP accounts", `aria-label="Use system theme"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("account recovery page is missing %q", want)
		}
	}
}

// The two things a customer can do with a backup are on the same page and
// on the same form. Which of them is offered depends on the part of the
// account chosen, and the one that replaces live data asks first.
func TestTheRecoveryPageOffersPuttingBackOnlyWhereItIsPossible(t *testing.T) {
	server, err := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0).UTC()
	render := func(kind granular.Kind, view userView) string {
		t.Helper()
		view.Account = "studio"
		view.Kinds = userKinds
		view.Repository = "repo"
		view.Snapshot = "abcdef"
		view.SnapshotAt = now
		view.Snapshots = []resticrun.Snapshot{{ID: "abcdef", Time: now}}
		view.Kind = kind
		request := httptest.NewRequest(http.MethodGet, "/browse", nil)
		request = request.WithContext(context.WithValue(request.Context(), accountKey{}, "studio"))
		response := httptest.NewRecorder()
		server.renderUser(response, request, "user_browse.html", view)
		if response.Code != http.StatusOK {
			t.Fatalf("render %s = %d: %s", kind, response.Code, response.Body.String())
		}
		return response.Body.String()
	}

	databases := render(granular.KindDatabase, userView{
		Root: "/var/lib/gniza/staging/stage-studio/databases",
		Path: "/var/lib/gniza/staging/stage-studio/databases",
		Entries: []browseEntry{{
			Name: "studio_wp.sql", Item: "studio_wp",
			Path: "/var/lib/gniza/staging/stage-studio/databases/studio_wp.sql",
		}},
	})
	for _, want := range []string{
		`name="action" value="restore"`,
		`name="action" value="download"`,
		`name="confirm" value="1" required`,
		"Restore these databases",
	} {
		if !strings.Contains(databases, want) {
			t.Errorf("the databases page is missing %q", want)
		}
	}
	// The download has to stay reachable without ticking the box, which
	// means it must not be held up by the box being required.
	if !strings.Contains(databases, `value="download" formnovalidate`) {
		t.Error("downloading a copy is blocked by the confirmation the other button needs")
	}
	// Where the backup happens to be staged is root's business.
	if strings.Contains(databases, "/var/lib/gniza") {
		t.Error("the account page shows the server's staging directory")
	}

	// Database users are chosen whole rather than item by item, so they
	// go through the other form on the same page. That form has to offer
	// the same two choices, and a database restored without the user that
	// reads it is the reason it matters.
	users := render(granular.KindDBUsers, userView{})
	for _, want := range []string{
		`name="action" value="restore"`,
		`name="action" value="download"`,
		`name="confirm" value="1" required`,
		"Restore these database users",
	} {
		if !strings.Contains(users, want) {
			t.Errorf("the database users page is missing %q", want)
		}
	}

	// DNS is put back by the host, so the page offers only the copy.
	dns := render(granular.KindDNS, userView{})
	if strings.Contains(dns, `value="restore"`) || strings.Contains(dns, `name="confirm"`) {
		t.Error("DNS records were offered for writing into the live account")
	}
	if !strings.Contains(dns, `name="action" value="download"`) {
		t.Error("DNS records cannot be downloaded")
	}
}

func TestAccountRedirectUsesTheCPanelLivePHPEntryPoint(t *testing.T) {
	response := httptest.NewRecorder()
	redirectUser(response, "/browse?repository=repo&snapshot=abc&item=database",
		"error", "Choose an item")
	if response.Code != http.StatusSeeOther {
		t.Fatalf("redirect status = %d", response.Code)
	}
	location := response.Header().Get("Location")
	if !strings.HasPrefix(location, "browse.live.php?") || strings.Contains(location, "p=") {
		t.Fatalf("account redirect escaped the .live.php route: %q", location)
	}
	for _, want := range []string{"repository=repo", "snapshot=abc", "item=database", "kind=error"} {
		if !strings.Contains(location, want) {
			t.Errorf("redirect %q is missing %q", location, want)
		}
	}
}

// A basket is one restore, so a database and the users that open it arrive
// together rather than as two jobs with a gap between them.
func TestABasketBecomesOneRestore(t *testing.T) {
	basket := nodestore.Basket{
		Account: "studio", RepositoryID: "repo", SnapshotID: "snapshot",
		Items: []nodestore.RestoreSelection{
			{Kind: "database", Names: []string{"studio_shop"}},
			{Kind: "dbusers", Names: []string{"studio_shop"}},
		},
	}
	restore, err := userBasketRestore("studio", basket, true)
	if err != nil {
		t.Fatal(err)
	}
	if restore.Kind != protocol.RestoreItems || len(restore.Items) != 2 || !restore.Apply {
		t.Fatalf("restore = %+v", restore)
	}
	if len(restore.Selections()) != 2 {
		t.Errorf("selections = %+v", restore.Selections())
	}
}

// One part that cannot be written back makes the whole basket a download.
// Putting back the rest and leaving that one out is not what was asked
// for, and would only be discovered afterwards.
func TestABasketCarryingADownloadOnlyPartCannotBePutBack(t *testing.T) {
	basket := nodestore.Basket{
		Account: "studio", RepositoryID: "repo", SnapshotID: "snapshot",
		Items: []nodestore.RestoreSelection{
			{Kind: "database", Names: []string{"studio_shop"}},
			{Kind: "dns"},
		},
	}
	if _, err := userBasketRestore("studio", basket, true); err == nil {
		t.Error("a basket carrying DNS was accepted for putting back")
	}
	// It is still a download, which is the whole point of offering it.
	restore, err := userBasketRestore("studio", basket, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(restore.Items) != 2 || restore.Apply {
		t.Errorf("restore = %+v", restore)
	}
}

// The parts of an account a customer may ask for are the ones the page
// offers. The whole account is not one of them here: it is everything, and
// it goes to cPanel's restorepkg as root over a home directory the customer
// controls.
func TestABasketRefusesWhatTheRecoveryCentreDoesNotOffer(t *testing.T) {
	for _, kind := range []string{"account", "system", "settings", "anything"} {
		basket := nodestore.Basket{
			Account: "studio", RepositoryID: "repo", SnapshotID: "snapshot",
			Items: []nodestore.RestoreSelection{{Kind: kind}},
		}
		if _, err := userBasketRestore("studio", basket, false); err == nil {
			t.Errorf("a basket of %q was accepted", kind)
		}
	}
	if _, err := userBasketRestore("studio", nodestore.Basket{}, false); err == nil {
		t.Error("an empty basket was accepted")
	}
}

// A basket of one reads on the history page the way a single restore
// always has, rather than as a new shape nothing else knows.
func TestABasketOfOneStillNamesItsPart(t *testing.T) {
	restore, err := userBasketRestore("studio", nodestore.Basket{
		Account: "studio", RepositoryID: "repo", SnapshotID: "snapshot",
		Items: []nodestore.RestoreSelection{
			{Kind: "database", Names: []string{"studio_shop"}},
		},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if restore.ItemKind != "database" || len(restore.ItemNames) != 1 {
		t.Fatalf("restore = %+v", restore)
	}
}

// A server with hundreds of accounts queues hundreds of backups, and the
// strip is at the top of every page. Drawn a line per account it was the
// page: on a real DirectAdmin host one account was being backed up and
// the three hundred behind it filled the screen, so the one line that
// said what was actually happening was the only one nobody could see.
//
// What is running keeps its line and its bar. What is waiting becomes one
// line with a count, and the names are still there for whoever opens it.
func TestAQueueOfAccountsIsOneLineAndNotThreeHundred(t *testing.T) {
	store, err := nodestore.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	started, err := store.PutJob(nodestore.Job{
		Account: "clinicco", Status: job.StatusRunning, QueuedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetJobProgress(started.ID, nodestore.JobProgress{Percent: 99}); err != nil {
		t.Fatal(err)
	}
	for _, account := range []string{"coilco", "com2com", "compninjas", "connectivity", "dad"} {
		if _, err := store.PutJob(nodestore.Job{
			Account: account, Status: job.StatusPending, QueuedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	running, err := runningWorkFor(store, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(running.Now()) != 1 || running.Now()[0].Account != "clinicco" {
		t.Errorf("what is happening now is %+v", running.Now())
	}
	if len(running.Queue()) != 5 {
		t.Errorf("the queue holds %d", len(running.Queue()))
	}
	if !running.Folded() {
		t.Error("five accounts behind the one running are still five lines")
	}
	if summary := running.QueueSummary(); !strings.Contains(summary, "5") ||
		!strings.Contains(summary, "waiting to back up") {
		t.Errorf("the queue summary is %q", summary)
	}
}

// One account behind the one running is not a queue, and hiding it behind
// a count somebody has to open would be worse than the line it replaces.
func TestAShortQueueIsStillJustItsLines(t *testing.T) {
	store, err := nodestore.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.PutJob(nodestore.Job{
		Account: "studio", Status: job.StatusRunning, QueuedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutJob(nodestore.Job{
		Account: "coilco", Status: job.StatusPending, QueuedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	running, err := runningWorkFor(store, "")
	if err != nil {
		t.Fatal(err)
	}
	if running.Folded() {
		t.Error("one account waiting was folded away behind a count")
	}
}

// A queue with both kinds in it says both, because "312 accounts waiting"
// over a restore somebody is waiting on is the wrong thing to read.
func TestAQueueOfBothKindsSaysBoth(t *testing.T) {
	store, err := nodestore.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	for _, account := range []string{"a", "b", "c", "d"} {
		if _, err := store.PutJob(nodestore.Job{
			Account: account, Status: job.StatusPending, QueuedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.PutRestore(nodestore.Restore{
		Account: "e", Status: job.StatusPending, Kind: protocol.RestoreAccount,
	}); err != nil {
		t.Fatal(err)
	}
	running, err := runningWorkFor(store, "")
	if err != nil {
		t.Fatal(err)
	}
	summary := running.QueueSummary()
	if !strings.Contains(summary, "4") || !strings.Contains(summary, "back up") {
		t.Errorf("the summary does not count the backups: %q", summary)
	}
	if !strings.Contains(summary, "restore") {
		t.Errorf("the summary does not mention the restore waiting behind them: %q", summary)
	}
}

// The strip said which copy was being written by its repository id --
// "Backing up clinicco — 08d0a595086a4a984a2737a6341d3788" -- which is
// not a name anybody gave anything. The destination has a name, and that
// is what an operator recognises.
func TestTheStripNamesTheDestinationRatherThanItsIdentifier(t *testing.T) {
	store, err := nodestore.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	destination, err := store.PutDestination(nodestore.Destination{Name: "the vault"})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := store.PutRepository(nodestore.Repository{
		DestinationID: destination.ID, Path: "uscp",
	})
	if err != nil {
		t.Fatal(err)
	}
	backup, err := store.PutJob(nodestore.Job{
		Account: "clinicco", Status: job.StatusRunning, QueuedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetJobProgress(backup.ID, nodestore.JobProgress{
		Percent: 99, Repository: repository.ID,
	}); err != nil {
		t.Fatal(err)
	}

	running, err := runningWorkFor(store, "")
	if err != nil {
		t.Fatal(err)
	}
	label := running.Now()[0].Label()
	if strings.Contains(label, repository.ID) {
		t.Errorf("the strip shows the repository id: %q", label)
	}
	if !strings.Contains(label, "the vault") {
		t.Errorf("the strip does not name the destination: %q", label)
	}
}
