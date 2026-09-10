package node_test

import (
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/node"
	"github.com/shukiv/gniza/internal/nodestore"
)

// TestABackupWithAHoleInItSaysSoEveryTime.
//
// A run that could not dump one database still stores everything else,
// which is the point -- an account with forty-seven databases and one
// corrupt table should not go a week with no backup at all. But a run like
// that must never pass for a whole one. Whoever is told about the backup
// is told what is not in it, every time, and the run cannot authorise
// deleting the account it is a backup of.
func TestABackupWithAHoleInItSaysSoEveryTime(t *testing.T) {
	stored := nodestore.Job{
		Account: "thelittleprinced",
		Status:  job.StatusSuccess,
		Missing: []string{
			"database thelittleprinced_fwpl1: cpanel: mysqldump thelittleprinced_fwpl1: " +
				"exit status 2: mysqldump: Lost connection to MySQL server during query (2013)",
		},
		Targets: []nodestore.JobTarget{{RepositoryID: "r1", Status: job.TargetSuccess, BytesAdded: 1 << 20}},
	}

	message, send := node.BackupMessage(stored)
	if !send {
		t.Fatal("a backup with something missing from it told nobody")
	}
	if !strings.Contains(message.Subject, "thelittleprinced") {
		t.Errorf("the subject does not name the account: %q", message.Subject)
	}
	if strings.Contains(message.Subject, "Backed up thelittleprinced") &&
		!strings.Contains(strings.ToLower(message.Subject), "without") {
		t.Errorf("the subject reads like an ordinary success: %q", message.Subject)
	}
	for _, want := range []string{"thelittleprinced_fwpl1", "2013"} {
		if !strings.Contains(message.Body, want) {
			t.Errorf("the message does not say what was left out (%q):\n%s", want, message.Body)
		}
	}
}

// TestABackupThatMayNotRestoreSaysSoToo.
//
// Nothing is missing from this run: every byte the account has is in the
// backup. What it cannot do is come back -- DirectAdmin's restore unpacks
// the home as the account and stops at the first file the account does
// not own. The message has to carry that, or the first anyone hears of it
// is a restore that stops a third of the way through.
func TestABackupThatMayNotRestoreSaysSoToo(t *testing.T) {
	stored := nodestore.Job{
		Account: "hayagold",
		Status:  job.StatusSuccess,
		Warnings: []string{
			"12352 files are owned by another account, the first of them " +
				"public_html/wp-config.php, so a restore of this backup stops there.",
		},
		Targets: []nodestore.JobTarget{{RepositoryID: "r1", Status: job.TargetSuccess, BytesAdded: 1 << 20}},
	}

	message, send := node.BackupMessage(stored)
	if !send {
		t.Fatal("a backup that cannot be restored told nobody")
	}
	for _, want := range []string{"public_html/wp-config.php", "12352"} {
		if !strings.Contains(message.Body, want) {
			t.Errorf("the message does not say what will stop the restore (%q):\n%s", want, message.Body)
		}
	}
	if strings.Contains(strings.ToLower(message.Subject), "without") {
		t.Errorf("a backup holding the whole account was announced as short of something: %q", message.Subject)
	}
}
