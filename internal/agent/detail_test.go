package agent

import (
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/layout/cpmove"
	"github.com/shukiv/gniza/internal/layout/dabackup"
	"github.com/shukiv/gniza/internal/panel"
	"github.com/shukiv/gniza/internal/pkgacct"
)

func TestTrimDetailKeepsTheTail(t *testing.T) {
	// restic names every file it could not read, and on a busy account
	// that is thousands of lines. The ones at the end are the summary and
	// the most recent failures, so those are the ones worth keeping.
	var builder strings.Builder
	for i := range 5000 {
		builder.WriteString("error: open /home/customer1/tmp/sess_")
		builder.WriteString(strings.Repeat("a", 12))
		builder.WriteString(": no such file\n")
		_ = i
	}
	builder.WriteString("LAST LINE: 3 files could not be read\n")

	trimmed := trimDetail(builder.String())
	if len(trimmed) > maxDetail+64 {
		t.Errorf("kept %d bytes, want about %d", len(trimmed), maxDetail)
	}
	if !strings.Contains(trimmed, "LAST LINE: 3 files could not be read") {
		t.Error("the tail, which is where restic's summary is, was cut off")
	}
	if !strings.HasPrefix(trimmed, "… earlier output omitted …") {
		t.Error("truncation is not signposted, so the log looks complete when it is not")
	}
	// Truncation must not leave a half line at the top.
	firstBody := strings.SplitN(trimmed, "\n", 3)[1]
	if firstBody != "" && !strings.HasPrefix(firstBody, "error: open") {
		t.Errorf("first kept line is a fragment: %q", firstBody)
	}
}

func TestTrimDetailLeavesShortOutputAlone(t *testing.T) {
	const short = "warning: could not read /home/c1/.cache/x\n"
	if got := trimDetail(short); got != strings.TrimSpace(short) {
		t.Errorf("got %q, want it unchanged", got)
	}
	if got := trimDetail("   \n  "); got != "" {
		t.Errorf("blank output became %q", got)
	}
}

// Split mode never copies the home directory into staging: restic reads it
// where it lies. Reserving the whole account refused backups that would
// have fitted easily — a 6.8 GB account on a volume with 6.4 GB free, for
// a job that stages a few hundred megabytes.
func TestStagingEstimateFollowsWhatIsActuallyStaged(t *testing.T) {
	const gigabyte = 1 << 30

	for _, tc := range []struct {
		what     string
		size     uint64
		lean     uint64
		mode     pkgacct.Mode
		layout   panel.ArchiveLayout
		provider any
		want     uint64
	}{
		{"a monolithic backup stages the whole account", 10 * gigabyte, 0, pkgacct.ModeMonolithic, cpmove.Layout{}, nil, 10 * gigabyte},
		{"an unset mode is monolithic", 10 * gigabyte, 0, "", cpmove.Layout{}, nil, 10 * gigabyte},
		{"a split backup stages a fraction", 10 * gigabyte, 0, pkgacct.ModeSplit, cpmove.Layout{}, nil, 2 * gigabyte},
		{"a small split account still gets room to work", 100 << 20, 0, pkgacct.ModeSplit, cpmove.Layout{}, nil, 512 << 20},
		{"an account of no known size gets the floor", 0, 0, pkgacct.ModeSplit, cpmove.Layout{}, nil, 512 << 20},
		// A panel that will not produce the parts stages the whole
		// account and then takes it apart, so it is on the disk twice at
		// the peak rather than a fifth of it being there at all.
		{"a split backup of an archive panel stages the account twice",
			10 * gigabyte, 0, pkgacct.ModeSplit, dabackup.Layout{}, nil, 20 * gigabyte},
		{"and a small one still gets room to work",
			100 << 20, 0, pkgacct.ModeSplit, dabackup.Layout{}, nil, 512 << 20},
		// Until the same panel is found to produce an archive without
		// the account's own files in it, at which point staging holds
		// what it holds on cPanel and reserving the account twice
		// refuses backups that would have fitted easily.
		// It stages what such a panel writes, which is not a share of
		// the account: the archive without the account's own files in
		// it, and that archive taken apart beside it. An account whose
		// bulk is mail needs more than a fifth of itself; one whose bulk
		// is files on disk needs far less.
		{"an archive panel that reads the home directory in place stages what it writes",
			10 * gigabyte, 4 * gigabyte, pkgacct.ModeSplit, dabackup.Layout{}, inPlace{}, 8 * gigabyte},
		{"a mostly-web account staging its mail and dumps",
			10 * gigabyte, gigabyte, pkgacct.ModeSplit, dabackup.Layout{}, inPlace{}, 2 * gigabyte},
		{"an account with nothing but its records still gets room to work",
			10 * gigabyte, 0, pkgacct.ModeSplit, dabackup.Layout{}, inPlace{}, 512 << 20},
	} {
		if got := stagingEstimate(tc.size, tc.lean, tc.mode, tc.layout, tc.provider); got != tc.want {
			t.Errorf("%s: estimate = %d, want %d", tc.what, got, tc.want)
		}
	}
}

// inPlace is a provider that has been found to read the account's home
// directory where it lies.
type inPlace struct{}

func (inPlace) ReadsHomeInPlace() bool { return true }
