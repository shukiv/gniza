package node

import (
	"testing"

	"github.com/shukiv/gniza/internal/protocol"
	"github.com/shukiv/gniza/internal/resticrun"
)

// TestOneItemIsNotSizedAsAWholeAccount.
//
// Taking one 800 KiB database out of a 48.7 GiB account was refused for
// needing 176.5 GiB of scratch, because every restore was sized as a
// whole-account reassembly whatever it actually did. On any account
// larger than a third of the free disk that made "Restore one thing"
// impossible -- which is exactly the account somebody needs it on.
func TestOneItemIsNotSizedAsAWholeAccount(t *testing.T) {
	const account = 48 << 30
	const oneDatabase = 800 << 10

	whole := restoreStagingEstimate(protocol.RestoreAccount, account, account, 0)
	item := restoreStagingEstimate(protocol.RestoreItems, account, account, oneDatabase)

	if item >= whole {
		t.Errorf("one item needs %d, a whole account %d: the item is not sized by what it takes",
			item, whole)
	}
	if item > 4<<30 {
		t.Errorf("one 800 KiB database reserves %d bytes", item)
	}
	if item <= oneDatabase {
		t.Errorf("an item restore is sized at %d, which does not hold what it restores", item)
	}
}

// TestAnItemOfUnknownSizeFallsBackToTheAccount. Sizing an item needs the
// backup to answer how big it is. When it cannot, guessing small would
// fill the volume, so the whole-account figure stands.
func TestAnItemOfUnknownSizeFallsBackToTheAccount(t *testing.T) {
	const account = 48 << 30
	if got, want := restoreStagingEstimate(protocol.RestoreItems, account, account, 0),
		restoreStagingEstimate(protocol.RestoreAccount, account, account, 0); got != want {
		t.Errorf("an item of unknown size reserves %d, want the account's %d", got, want)
	}
}

// TestAWholeAccountStillCountsCPanelsOwnCopy. restorepkg copies whatever
// it is handed into a temporary directory it makes beside it, so the tree
// and cPanel's copy of it are on the same volume at once.
func TestAWholeAccountStillCountsCPanelsOwnCopy(t *testing.T) {
	const account = 10 << 30
	got := restoreStagingEstimate(protocol.RestoreAccount, account, account, 0)
	if got < 2*uint64(account) {
		t.Errorf("a whole-account restore reserves %d, less than the two copies it makes", got)
	}
}

// TestAFolderIsNotSizedFromItsTopLevelFiles.
//
// "restic ls" without --recursive lists a directory's direct children.
// Asked about public_html it answers with a few files and the folders
// under them -- a few kilobytes standing for thirty gigabytes. Summing
// that would reserve a gigabyte, pass the space check, and then fill the
// volume of a live cPanel server while restic wrote the rest. A directory
// in the listing means the size is not known, and the account's own
// figure stands.
func TestAFolderIsNotSizedFromItsTopLevelFiles(t *testing.T) {
	listing := []resticrun.Entry{
		{Type: "file", Path: "/home/u/public_html/index.php", Size: 2 << 10},
		{Type: "dir", Path: "/home/u/public_html/wp-content", Size: 4 << 10},
	}
	if got := sizeOfEntries(listing); got != 0 {
		t.Errorf("a listing with a folder in it sized at %d, want 0 for unknown", got)
	}
}

// TestFilesAreSizedByWhatTheyHold is the case this exists for: databases,
// DNS zones and certificates are files, and a listing of files is the
// whole truth about them.
func TestFilesAreSizedByWhatTheyHold(t *testing.T) {
	listing := []resticrun.Entry{
		{Type: "file", Path: "/db/u_wp1.sql", Size: 800 << 10},
		{Type: "file", Path: "/db/u_wp2.sql", Size: 200 << 10},
	}
	if got, want := sizeOfEntries(listing), uint64(1000<<10); got != want {
		t.Errorf("two database dumps sized at %d, want %d", got, want)
	}
}

// TestPickedFilesAreNotSizedAsAWholeAccount.
//
// v0.2.8 taught itemBytes to size a files restore and then never asked
// it: the estimate gated on RestoreItems alone, so picking three files
// out of a 48.7 GiB account still reserved the whole account and was
// still refused. The listing was fetched over the network and thrown
// away.
func TestPickedFilesAreNotSizedAsAWholeAccount(t *testing.T) {
	const account = 48 << 30
	const threeFiles = 12 << 20

	whole := restoreStagingEstimate(protocol.RestoreAccount, account, account, 0)
	picked := restoreStagingEstimate(protocol.RestoreFiles, account, account, threeFiles)

	if picked >= whole {
		t.Errorf("picked files need %d, a whole account %d: the files are not sized by what they are",
			picked, whole)
	}
	if picked <= threeFiles {
		t.Errorf("a files restore is sized at %d, which does not hold what it restores", picked)
	}
}
