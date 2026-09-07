package reassemble

import (
	"math"
	"testing"
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
