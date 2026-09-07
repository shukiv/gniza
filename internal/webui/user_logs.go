package webui

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/nodestore"
)

// userBackupRow is one run of this account's backup, as its owner may see
// it.
//
// It is built rather than filtered: a job carries staging paths, target
// errors and the output of whatever command failed, and none of that is
// the customer's to read. What is theirs is when it ran, whether it
// worked, and which of their own things are not in it.
type userBackupRow struct {
	When    time.Time
	Status  job.Status
	Missing []string
	// Whole records that this run stored the entire account, which a run
	// taken under a schedule that excludes parts of it does not.
	Whole bool
}

// Outcome is the run in the words its owner needs, and no others.
func (r userBackupRow) Outcome() string {
	switch {
	case r.Status == job.StatusFailed:
		return "Not backed up"
	case r.Status == job.StatusRunning || r.Status == job.StatusPending:
		return "Running"
	case len(r.Missing) > 0:
		return "Backed up, without everything"
	case r.Status == job.StatusPartialSuccess:
		// The account was stored; one of the places it goes would not
		// take it. That is the host's to fix, and it is not a hole in
		// what was stored -- saying "without everything" beside an empty
		// list of missing things is one row contradicting itself.
		return "Backed up to some destinations"
	default:
		return "Backed up"
	}
}

// Kept reports whether this run is a restore point for the whole account.
//
// Two things stop a run being one, and neither is a failure: something
// the run could not store, and a schedule that was never asked to store
// everything. CompleteAccount is the record of the second, and it is the
// same signal termination safety reads before letting an account be
// deleted.
func (r userBackupRow) Kept() bool { return r.Whole && len(r.Missing) == 0 }

// Bad reports whether the row should read as trouble.
func (r userBackupRow) Bad() bool { return r.Status == job.StatusFailed }

// accountBackupRow makes one job safe to show to the account it belongs
// to.
//
// The reason a thing could not be stored is the server's business: it is
// command output, and it names hosts, paths and software the customer has
// no access to. The thing itself is theirs, so the line is cut at the
// first colon -- "database studio_wp: mysqldump ..." becomes "database
// studio_wp", which is what they would ask their host about.
func accountBackupRow(run nodestore.Job) userBackupRow {
	row := userBackupRow{Status: run.Status, When: runWhen(run), Whole: run.CompleteAccount}
	for _, missing := range run.Missing {
		what := strings.TrimSpace(missing)
		if cut, _, found := strings.Cut(what, ":"); found {
			what = strings.TrimSpace(cut)
		}
		if what != "" {
			row.Missing = append(row.Missing, what)
		}
	}
	return row
}

// runWhen is when a run happened, preferring what it actually did to when
// it was asked for.
func runWhen(run nodestore.Job) time.Time {
	if run.FinishedAt != nil {
		return *run.FinishedAt
	}
	if run.StartedAt != nil {
		return *run.StartedAt
	}
	return run.QueuedAt
}

// accountBackupRows is one account's own runs, newest first.
//
// Filtering by name is not enough on its own -- a recycled username has
// two customers' runs under it -- so the caller passes the ones that
// belong to whoever holds the name now. See BackupBelongsToCurrentHolder.
func accountBackupRows(jobs []nodestore.Job, account string) []userBackupRow {
	rows := make([]userBackupRow, 0, len(jobs))
	for _, run := range jobs {
		if run.Account != account {
			continue
		}
		rows = append(rows, accountBackupRow(run))
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].When.After(rows[j].When) })
	return rows
}

// handleUserLogs draws what has happened to this account: the backups
// taken of it, and the restores asked for from it.
//
// It is the answer to "was my site backed up last night", which is a
// question about one account and is asked far more often than any
// question about the server.
func (s *Server) handleUserLogs(w http.ResponseWriter, r *http.Request) {
	view := userView{Account: accountOf(r), Kinds: userKinds}

	jobs, err := s.engine.Store().Jobs(userHistoryDepth)
	if err != nil {
		s.failUser(w, r, http.StatusInternalServerError, err)
		return
	}
	mine := make([]nodestore.Job, 0, len(jobs))
	for _, run := range jobs {
		// The name is not the account. What the customer before them had
		// backed up -- and the names of the databases it could not store
		// -- is not this one's history.
		if run.Account == view.Account && s.engine.BackupBelongsToCurrentHolder(run) {
			mine = append(mine, run)
		}
	}
	view.Backups = accountBackupRows(mine, view.Account)

	restores, err := s.engine.Store().Restores(userHistoryDepth)
	if err != nil {
		s.failUser(w, r, http.StatusInternalServerError, err)
		return
	}
	for _, restore := range restores {
		if restore.Account != view.Account {
			continue
		}
		// The name is not the account: what the previous holder of it
		// recovered is not this customer's history.
		if !s.engine.BelongsToCurrentHolder(restore) {
			continue
		}
		view.Restores = append(view.Restores, restoreRow{
			Restore:     accountSafeRestore(restore),
			Collectable: restore.ArchivePath != "" && onDisk(restore.ArchivePath),
		})
	}
	s.renderUser(w, r, "user_logs.html", view)
}

// userHistoryDepth is how many runs are read back before one account's
// are picked out of them.
//
// The store keeps every account's runs in one series, so this is shared:
// on a server with twenty accounts backing up nightly it reaches about a
// month of that account's own nights, and on a busier one proportionally
// fewer. It is a page a customer scrolls, not an archive.
const userHistoryDepth = 600
