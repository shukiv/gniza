package reassemble

import (
	"math"

	"github.com/shukiv/gniza/internal/panel"
)

// overhead is room for metadata, filesystem bookkeeping and the small
// pkgacct archive the tree is unpacked from. It follows cPanel's own
// pkgacct space guidance.
const overhead = uint64(1 << 30)

// TreeBytes is what a restore needs when it stops at the account tree.
//
// That is a rehearsal, and it is one copy: restic writes each part
// straight into its slot in the tree, so nothing is downloaded first and
// nothing is repacked afterwards.
func TreeBytes(source uint64) uint64 { return multiple(source, 1) }

// ArchiveBytes is what a restore needs when it also produces the archive.
//
// The tree and the tar built from it coexist, because the tar is built by
// reading the tree. Two copies.
//
// It is also the figure for a restore cPanel will apply, for a different
// reason that comes to the same number: restorepkg copies whatever it is
// given into a temporary directory it creates beside it -- see
// Whostmgr::Transfers::ArchiveManager, which runs "cp --archive" on a
// directory and untars an archive into the same place -- so the tree and
// cPanel's copy of it coexist on the same filesystem.
func ArchiveBytes(source uint64) uint64 { return multiple(source, 2) }

// RehearsalBytes is what a rehearsal needs, which is not the same number
// on every panel.
//
// A rehearsal stops at the tree, and on cPanel the tree is the account:
// one copy. On a panel whose backed-up parts are not the account's own
// files -- DirectAdmin, where the archive is what a restore reads -- the
// only way to check the parts is to build the archive from them, so the
// rehearsal builds it whatever it was asked for. The tree and the archive
// are then on the disk at once: two copies, the same as a real restore.
func RehearsalBytes(source uint64, layout panel.ArchiveLayout) uint64 {
	if _, packs := layout.(panel.ArchivePacker); packs {
		return ArchiveBytes(source)
	}
	return TreeBytes(source)
}

// multiple is n copies of source plus the overhead, saturating rather
// than wrapping: an overflow that made a huge restore look small would
// hand it scratch space it cannot possibly fit in.
func multiple(source uint64, copies uint64) uint64 {
	if source == 0 {
		return 0
	}
	if source > (math.MaxUint64-overhead)/copies {
		return math.MaxUint64
	}
	return source*copies + overhead
}
