package reassemble

import (
	"math"
	"testing"

	"github.com/shukiv/gniza/internal/layout/cpmove"
	"github.com/shukiv/gniza/internal/layout/dabackup"
)

// TestScratchIsSizedForWhatEachRestoreActuallyWrites.
//
// One number for every restore was wrong in both directions. A rehearsal
// writes the account tree and nothing else, and was refused for three
// times what it needed; a restore that cPanel applies has a second copy
// made beside it by restorepkg, which the tree-sized figure would not
// cover.
func TestScratchIsSizedForWhatEachRestoreActuallyWrites(t *testing.T) {
	const account = 10 << 30

	tree := TreeBytes(account)
	archive := ArchiveBytes(account)

	if tree <= account {
		t.Errorf("a rehearsal is sized at %d, which does not even hold the tree", tree)
	}
	if tree >= 2*uint64(account) {
		t.Errorf("a rehearsal writes one copy but is sized for two: %d", tree)
	}
	if archive < 2*uint64(account) {
		t.Errorf("a rebuild writes the tree and the archive but is sized at %d", archive)
	}
	if archive >= 3*uint64(account) {
		t.Errorf("a rebuild is sized for a third copy nothing writes: %d", archive)
	}

	for name, got := range map[string]uint64{"TreeBytes": TreeBytes(0), "ArchiveBytes": ArchiveBytes(0)} {
		if got != 0 {
			t.Errorf("%s turned an unknown size into a usable estimate: %d", name, got)
		}
	}
	for name, got := range map[string]uint64{
		"TreeBytes":    TreeBytes(math.MaxUint64),
		"ArchiveBytes": ArchiveBytes(math.MaxUint64),
	} {
		if got != math.MaxUint64 {
			t.Errorf("%s overflowed and made a huge restore look small: %d", name, got)
		}
	}
}

// A rehearsal stops at the tree only where the tree is the account. On a
// panel whose parts are not the account's own files, the archive is, so
// the rehearsal builds it and the tree and the archive are on the disk at
// once. Sizing that one for a tree is how a rehearsal fills a disk.
func TestARehearsalIsSizedForWhatThePanelMakesItWrite(t *testing.T) {
	const account = 10 << 30

	if got, want := RehearsalBytes(account, cpmove.Layout{}), TreeBytes(account); got != want {
		t.Errorf("a cPanel rehearsal is sized at %d, not the tree's %d", got, want)
	}
	if got, want := RehearsalBytes(account, dabackup.Layout{}), PackedBytes(account); got != want {
		t.Errorf("a DirectAdmin rehearsal is sized at %d, not the %d it writes", got, want)
	}
}

// A restore that rebuilds the archive from the parts has a third copy in
// it, and it is not the tree and not the finished archive. DirectAdmin's
// nested home archive carries its own compressed length in a header of
// the outer one, so it has to be finished on disk before the outer one is
// started, and all three are on the filesystem at once. Sized for two,
// a restore of a home directory that does not compress fills the disk at
// the last step of the job.
func TestARestoreThatRebuildsTheArchiveIsSizedForTheCopyInBetween(t *testing.T) {
	const account = 10 << 30

	if got, want := RestoreBytes(account, cpmove.Layout{}), ArchiveBytes(account); got != want {
		t.Errorf("a cPanel restore is sized at %d, not the %d it writes", got, want)
	}
	packed := RestoreBytes(account, dabackup.Layout{})
	if packed < 3*uint64(account) {
		t.Errorf("a DirectAdmin restore holds the tree, the nested archive and the outer one, and is sized at %d", packed)
	}
	if packed >= 4*uint64(account) {
		t.Errorf("a DirectAdmin restore is sized for a fourth copy nothing writes: %d", packed)
	}
	if got := PackedBytes(0); got != 0 {
		t.Errorf("PackedBytes turned an unknown size into a usable estimate: %d", got)
	}
	if got := PackedBytes(math.MaxUint64); got != math.MaxUint64 {
		t.Errorf("PackedBytes overflowed and made a huge restore look small: %d", got)
	}
}
