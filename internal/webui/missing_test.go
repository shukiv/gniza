package webui_test

import (
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/nodestore"
)

// TestWhatABackupCouldNotTakeIsOnTheRow. A run that stored the account but
// could not dump one of its databases looks, from the outside, exactly
// like a run that stored everything: the targets succeeded and the bytes
// are there. What it could not take has to be on the page, or nobody finds
// out until a restore.
func TestWhatABackupCouldNotTakeIsOnTheRow(t *testing.T) {
	client, _, engine := newUI(t)
	if _, err := engine.Store().PutJob(nodestore.Job{
		Account: "thelittleprinced", Status: job.StatusSuccess,
		Missing: []string{"database thelittleprinced_fwpl1: mysqldump: Lost connection (2013)"},
		Targets: []nodestore.JobTarget{{RepositoryID: "r1", Status: job.TargetSuccess}},
	}); err != nil {
		t.Fatal(err)
	}

	_, page := get(t, client, "/logs")
	for _, want := range []string{"not backed up", "thelittleprinced_fwpl1", "2013"} {
		if !strings.Contains(page, want) {
			t.Errorf("the backup row does not say %q", want)
		}
	}
}

// TestWhatABackupCannotRestoreIsOnTheRow. A DirectAdmin home holding a
// file the account does not own backs up completely and restores as far
// as that file and no further. Nothing is missing from the run, so the
// row above says nothing: this is the row that has to.
func TestWhatABackupCannotRestoreIsOnTheRow(t *testing.T) {
	client, _, engine := newUI(t)
	if _, err := engine.Store().PutJob(nodestore.Job{
		Account: "hayagold", Status: job.StatusSuccess,
		Warnings: []string{"12352 files are owned by another account, the first of them public_html/wp-config.php"},
		Targets:  []nodestore.JobTarget{{RepositoryID: "r1", Status: job.TargetSuccess}},
	}); err != nil {
		t.Fatal(err)
	}

	_, page := get(t, client, "/logs")
	for _, want := range []string{"public_html/wp-config.php", "12352"} {
		if !strings.Contains(page, want) {
			t.Errorf("the backup row does not say %q", want)
		}
	}
	if strings.Contains(page, "not backed up") {
		t.Error("a backup that holds everything was shown as one that left something out")
	}
}
