package agent

import (
	"testing"

	"github.com/shukiv/gniza/internal/layout/cpmove"
	"github.com/shukiv/gniza/internal/layout/dabackup"
	"github.com/shukiv/gniza/internal/protocol"
)

// The finding from the fleet check of 2026-09-22: the node sized a
// restore of one database by the database, and the agent then took the
// larger of that and the whole snapshot, so the fix never reached a
// server. On 182.54.236.148 three restores of one item out of a
// 48.7 GiB account were refused for wanting 176.5 GiB.
func TestARestoreOfOnePartIsGivenRoomForThatPart(t *testing.T) {
	const gib = uint64(1) << 30
	snapshot := 49 * gib
	for _, c := range []struct {
		name       string
		assignment protocol.RestoreAssignment
		want       uint64
	}{
		{"one database, measured", protocol.RestoreAssignment{
			Kind: protocol.RestoreItems, SizeEstimate: 3 * gib, ItemBytes: gib}, 3 * gib},
		{"named files, measured", protocol.RestoreAssignment{
			Kind: protocol.RestoreFiles, SizeEstimate: 5 * gib, ItemBytes: 2 * gib}, 5 * gib},
		{"one database, nobody asked the backup", protocol.RestoreAssignment{
			Kind: protocol.RestoreItems, SizeEstimate: 1024}, 99 * gib},
		{"the whole account", protocol.RestoreAssignment{
			Kind: protocol.RestoreAccount, SizeEstimate: 1024, ItemBytes: gib}, 99 * gib},
		{"the whole account, and the assignment asked for more", protocol.RestoreAssignment{
			Kind: protocol.RestoreAccount, SizeEstimate: 120 * gib}, 120 * gib},
	} {
		if got := restoreEstimate(c.assignment, snapshot, cpmove.Layout{}); got != c.want {
			t.Errorf("%s: %d GiB, want %d", c.name, got/gib, c.want/gib)
		}
	}
	// A panel whose archive is packed pays for the copy in between, for
	// a whole account and not for one part of it.
	whole := protocol.RestoreAssignment{Kind: protocol.RestoreAccount}
	if got := restoreEstimate(whole, snapshot, dabackup.Layout{}); got != 148*gib {
		t.Errorf("a packed whole account: %d GiB, want 148", got/gib)
	}
	part := protocol.RestoreAssignment{Kind: protocol.RestoreItems, ItemBytes: gib}
	if got := restoreEstimate(part, snapshot, dabackup.Layout{}); got != 3*gib {
		t.Errorf("a part on a packed panel: %d GiB, want 3", got/gib)
	}
}
