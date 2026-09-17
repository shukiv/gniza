package node

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/nodestore"
)

// TestTheRunsThatAreMoreThanALog: of a history, a clearing keeps the
// last run of each account under each schedule, whatever it came to, and
// the last good copy of each account at each destination -- any copy,
// a complete one, and a complete one under each schedule, since those are
// the three things asked of the history by the pages and by termination
// protection. A run with unreadable files is not a good copy.
func TestTheRunsThatAreMoreThanALog(t *testing.T) {
	at := time.Date(2026, 9, 17, 2, 0, 0, 0, time.UTC)
	run := func(id, account, policy string, daysAgo int, status job.Status, complete bool, targets ...nodestore.JobTarget) nodestore.Job {
		return nodestore.Job{ID: id, Account: account, PolicyID: policy, Status: status, CompleteAccount: complete,
			QueuedAt: at.AddDate(0, 0, -daysAgo), Targets: targets}
	}
	good := func(repo string) nodestore.JobTarget {
		return nodestore.JobTarget{RepositoryID: repo, Status: job.TargetSuccess, SnapshotID: "s"}
	}
	bad := func(repo string) nodestore.JobTarget {
		return nodestore.JobTarget{RepositoryID: repo, Status: job.TargetFailed}
	}
	holed := nodestore.JobTarget{RepositoryID: "usb", Status: job.TargetSuccess, Incomplete: true, SnapshotID: "h"}
	jobs := []nodestore.Job{ // newest first, as the store lists them
		run("going", "alice", "nightly", 0, job.StatusRunning, true),
		run("failed-last", "alice", "nightly", 1, job.StatusFailed, true, bad("usb"), bad("s3")),
		run("files-only", "alice", "files", 2, job.StatusSuccess, false, good("usb")),
		run("holed", "alice", "nightly", 3, job.StatusPartialSuccess, true, holed, good("s3")),
		run("complete", "alice", "nightly", 4, job.StatusSuccess, true, good("usb"), good("s3")),
		run("older", "alice", "nightly", 5, job.StatusSuccess, true, good("usb"), good("s3")),
		run("weekly", "alice", "weekly", 6, job.StatusSuccess, true, good("usb")),
		run("bob-only", "bob", "nightly", 7, job.StatusSuccess, true, good("usb")),
		run("bob-older", "bob", "nightly", 8, job.StatusSuccess, true, good("usb")),
	}
	var kept []string
	for id := range evidenceRuns(jobs) {
		kept = append(kept, id)
	}
	sort.Strings(kept)
	// failed-last: alice's last nightly run. files-only: her last files
	// run and the newest copy at usb. holed: the newest copy at s3.
	// complete: the newest complete copy at both. weekly: the newest
	// complete copy under that schedule. bob-only: all of bob's answers.
	want := "bob-only,complete,failed-last,files-only,holed,weekly"
	if got := strings.Join(kept, ","); got != want {
		t.Errorf("kept %s, want %s", got, want)
	}
}
