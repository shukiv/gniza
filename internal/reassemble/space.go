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

// PackedBytes is what a restore needs on a panel whose archive is built
// from the parts rather than restored beside them.
//
// Two copies, and it takes care to stay at two. The tree is one and the
// archive built from it is the other, as anywhere else. Between them is
// a third that is easy to miss: DirectAdmin's home directory arrives as
// an archive inside the account archive, and its compressed length is a
// header in the outer one, so it has to be finished on disk before the
// outer one can be started -- see dabackup.PackArchive. Each of the
// account's files is therefore removed from the tree once it is in that
// inner archive, so the two of them together are never more than one
// copy and the largest file; and the inner archive is removed as soon as
// it has been copied into the outer one. A home directory that does not
// compress, which is most of the large ones, makes each copy about the
// size of the account.
//
// This was three until 2026-09-28, when the tree was left whole until
// the restore had finished. On a server with 42.7 GiB free that refused
// the restore of an account of 17.
func PackedBytes(source uint64) uint64 { return multiple(source, 2) }

// RehearsalBytes is what a rehearsal needs, which is not the same number
// on every panel.
//
// A rehearsal stops at the tree, and on cPanel the tree is the account:
// one copy. On a panel whose backed-up parts are not the account's own
// files -- DirectAdmin, where the archive is what a restore reads -- the
// only way to check the parts is to build the archive from them, so the
// rehearsal builds it whatever it was asked for, and pays what a restore
// pays.
func RehearsalBytes(source uint64, layout panel.ArchiveLayout) uint64 {
	if _, packs := layout.(panel.ArchivePacker); packs {
		return PackedBytes(source)
	}
	return TreeBytes(source)
}

// RestoreBytes is what a restore that produces the archive needs, which
// is also not the same number on every panel.
func RestoreBytes(source uint64, layout panel.ArchiveLayout) uint64 {
	if _, packs := layout.(panel.ArchivePacker); packs {
		return PackedBytes(source)
	}
	return ArchiveBytes(source)
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
