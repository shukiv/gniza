package agent

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/pkgacct"
	"github.com/shukiv/gniza/internal/protocol"
)

// What staging found out is only on this machine's log until it is put on
// the report. A warning nobody reads is the same as no warning.
func TestWhatStagingFoundOutReachesTheReport(t *testing.T) {
	var report protocol.JobReport
	recordPayload(&report, pkgacct.Payload{
		Account:  "hayagold",
		Missing:  []pkgacct.Omission{{What: "database hayagold_wp", Why: "Lost connection (2013)"}},
		Warnings: []string{"12352 files are owned by another account, the first of them public_html/wp-config.php"},
	}, slog.New(slog.DiscardHandler))

	if len(report.Missing) != 1 || !strings.Contains(report.Missing[0], "2013") {
		t.Errorf("what the backup could not take is not on the report: %q", report.Missing)
	}
	if len(report.Warnings) != 1 || !strings.Contains(report.Warnings[0], "public_html/wp-config.php") {
		t.Errorf("what the backup may not restore is not on the report: %q", report.Warnings)
	}
}
