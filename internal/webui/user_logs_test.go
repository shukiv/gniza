package webui

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/nodestore"
)

// TestAccountBackupHistoryDoesNotExposeRootDiagnostics is the backup-side
// counterpart to the same rule for restores. A customer is told what
// happened to their own account and nothing about the machine it happened
// on: no paths, no destination addresses, no command output.
func TestAccountBackupHistoryDoesNotExposeRootDiagnostics(t *testing.T) {
	row := accountBackupRow(nodestore.Job{
		Account:    "studio",
		Status:     job.StatusFailed,
		StagingErr: "staging: not enough room in /var/lib/gniza/staging: 3.2 GiB free",
		Targets: []nodestore.JobTarget{{
			Error: "restic: unable to open sftp:backup-admin@internal.example:/srv/backups",
		}},
		Missing: []string{
			"database studio_wp: cpanel: mysqldump studio_wp: exit status 2: " +
				"mysqldump: Couldn't execute 'SELECT * FROM `wp_cache`': " +
				"Lost connection to MySQL server during query (2013)",
		},
	})

	said := row.Outcome() + " " + strings.Join(row.Missing, " ")
	for _, secret := range []string{
		"internal.example", "/var/lib/gniza", "mysqldump", "restic", "sftp", "2013",
	} {
		if strings.Contains(said, secret) {
			t.Errorf("the customer is shown %q: %s", secret, said)
		}
	}

	// What they can act on survives: which of their databases is not in
	// the backup. Only the reason, which is the server's business, goes.
	if len(row.Missing) != 1 || !strings.Contains(row.Missing[0], "studio_wp") {
		t.Errorf("the customer cannot tell what is missing: %+v", row.Missing)
	}
}

// TestAnIncompleteBackupSaysSoToTheAccount. A run that stored everything
// but one database is not a failure and must not read as one -- but it is
// not a clean backup either, and the customer whose database it is has
// the most reason to know.
func TestAnIncompleteBackupSaysSoToTheAccount(t *testing.T) {
	clean := accountBackupRow(nodestore.Job{
		Account: "studio", Status: job.StatusSuccess, CompleteAccount: true,
	})
	short := accountBackupRow(nodestore.Job{
		Account: "studio", Status: job.StatusSuccess,
		Missing: []string{"database studio_wp: whatever the server saw"},
	})
	failed := accountBackupRow(nodestore.Job{Account: "studio", Status: job.StatusFailed})

	if strings.EqualFold(clean.Outcome(), short.Outcome()) {
		t.Errorf("a backup missing a database reads the same as a clean one: %q", clean.Outcome())
	}
	if strings.EqualFold(short.Outcome(), failed.Outcome()) {
		t.Errorf("a backup that stored almost everything reads as a failure: %q", short.Outcome())
	}
	if !clean.Kept() {
		t.Error("a clean backup does not say it is a restore point")
	}
	if short.Kept() {
		t.Error("an incomplete backup claims to be a whole-account restore point")
	}

	// A schedule that leaves the databases out succeeds and misses
	// nothing it was asked for, and is still not a copy of the whole
	// account. CompleteAccount is what knows the difference.
	partial := accountBackupRow(nodestore.Job{
		Account: "studio", Status: job.StatusSuccess, CompleteAccount: false,
	})
	if partial.Kept() {
		t.Error("a backup taken with parts excluded claims to be a whole-account restore point")
	}
}

// TestOneDestinationFailingIsNotAMissingDatabase. A partly successful run
// stored the whole account and could not put it everywhere. Saying
// "without everything" beside an empty list of missing things is two
// cells of one row contradicting each other.
func TestOneDestinationFailingIsNotAMissingDatabase(t *testing.T) {
	row := accountBackupRow(nodestore.Job{
		Account: "studio", Status: job.StatusPartialSuccess, CompleteAccount: true,
	})
	if strings.Contains(row.Outcome(), "without") {
		t.Errorf("a destination that failed reads as a hole in the backup: %q", row.Outcome())
	}
	if row.Bad() {
		t.Errorf("a backup that was stored somewhere reads as a failure: %q", row.Outcome())
	}
}

// TestTheLogsPageIsOneAccountsOwnHistory. The store holds every account's
// runs. This page is served over the account socket, so it must show the
// one account the socket attributed and no other.
func TestTheLogsPageIsOneAccountsOwnHistory(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	rows := accountBackupRows([]nodestore.Job{
		{Account: "studio", Status: job.StatusSuccess, QueuedAt: now},
		{Account: "rtflow", Status: job.StatusFailed, QueuedAt: now.Add(-time.Hour)},
		{Account: "studio", Status: job.StatusFailed, QueuedAt: now.Add(-2 * time.Hour)},
	}, "studio")

	if len(rows) != 2 {
		t.Fatalf("the account is shown %d runs, want its own 2: %+v", len(rows), rows)
	}
	// Newest first: the run somebody came to read about is the last one.
	if !rows[0].When.After(rows[1].When) {
		t.Errorf("the history is not newest first: %v then %v", rows[0].When, rows[1].When)
	}
}

// TestTheLogsPageRenders draws the page the tab leads to, with the nav
// entry that reaches it.
func TestTheLogsPageRenders(t *testing.T) {
	server, err := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0).UTC()
	request := httptest.NewRequest(http.MethodGet, "/logs", nil)
	request = request.WithContext(context.WithValue(request.Context(), accountKey{}, "studio"))
	response := httptest.NewRecorder()
	server.renderUser(response, request, "user_logs.html", userView{
		Account: "studio",
		Backups: []userBackupRow{{
			When: now, Status: job.StatusSuccess,
			Missing: []string{"database studio_wp"},
		}},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("render status = %d: %s", response.Code, response.Body.String())
	}
	page := response.Body.String()
	for _, want := range []string{"logs.live.php", "Logs", "studio_wp"} {
		if !strings.Contains(page, want) {
			t.Errorf("the logs page is missing %q", want)
		}
	}
}
