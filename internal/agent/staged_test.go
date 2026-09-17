package agent

import (
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/pkgacct"
	"github.com/shukiv/gniza/internal/protocol"
)

// A run writes down what it staged, so the history can say what a backup
// was a backup of: the parts in the order restic was given them, the
// paths, the databases by name and what the schedule left out, each list
// in a fixed order so two nights' rows read the same.
func TestARunRecordsWhatItStaged(t *testing.T) {
	var report protocol.JobReport
	recordStaged(&report, pkgacct.Payload{
		Mode: pkgacct.ModeSplit,
		Parts: []pkgacct.Part{
			{Kind: pkgacct.PartMetadata, Path: "/stage/c1/metadata"},
			{Kind: pkgacct.PartHomedir, Path: "/home/c1"},
		},
		DumpPaths: map[string]string{"c1_wp": "/stage/c1/metadata/mysql/c1_wp.sql", "c1_shop": "/stage/c1/metadata/mysql/c1_shop.sql"},
	}, protocol.JobAssignment{SkipEmail: true, SkipHomedir: false})

	staged := report.Staged
	if staged == nil {
		t.Fatal("nothing was recorded")
	}
	got := staged.Mode + " | " + strings.Join(staged.Parts, ",") + " | " + strings.Join(staged.Paths, ",") +
		" | " + strings.Join(staged.Databases, ",") + " | " + strings.Join(staged.Skipped, ",")
	want := "split | metadata,homedir | /stage/c1/metadata,/home/c1 | c1_shop,c1_wp | email"
	if got != want {
		t.Errorf("staged = %q, want %q", got, want)
	}
}
