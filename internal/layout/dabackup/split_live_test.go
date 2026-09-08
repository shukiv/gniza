//go:build directadmin_live

package dabackup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The synthetic round trip is only as good as the fixture it is built
// from, and that fixture was written from a listing. This one is the
// archive itself: point GNIZA_DA_LIVE_ARCHIVE at a real DirectAdmin
// account backup and it is taken apart and put back together, header for
// header, with nothing restored and nothing on the panel touched.
//
// It was run against user.admin.gzv0908a.tar.zst, taken from the
// disposable fixture account on the 1.709 validation host and recorded in
// docs/directadmin-validation-2026-09-08.md.
func TestLiveARealArchiveComesBackTheSame(t *testing.T) {
	archive := os.Getenv("GNIZA_DA_LIVE_ARCHIVE")
	if archive == "" {
		t.Skip("set GNIZA_DA_LIVE_ARCHIVE to a real DirectAdmin account archive")
	}
	info, err := os.Stat(archive)
	if err != nil {
		t.Fatal(err)
	}
	// The comparator holds both archives in memory, which is fine for a
	// fixture and is not fine for somebody's real account.
	if info.Size() > 256<<20 {
		t.Skipf("%s is %d bytes; this comparison reads archives whole", archive, info.Size())
	}
	account, err := ArchiveAccount(filepath.Base(archive))
	if err != nil {
		t.Fatalf("%s is not a DirectAdmin account archive: %v", archive, err)
	}

	dir := t.TempDir()
	if err := (Layout{}).UnpackArchive(context.Background(), archive, account, dir); err != nil {
		t.Fatalf("taking the archive apart: %v", err)
	}
	rebuilt := filepath.Join(t.TempDir(), filepath.Base(archive))
	if err := (Layout{}).PackArchive(context.Background(), dir, account, rebuilt); err != nil {
		t.Fatalf("putting the archive back together: %v", err)
	}
	sameArchive(t, archive, rebuilt)

	// And the archive has to have been the shape this was written for. A
	// comparison that passed because both sides were empty proves
	// nothing, and neither does one on an archive with no second archive
	// inside it.
	members := readSnapshots(t, archive)
	nested, elsewhere := false, false
	for _, member := range members {
		if member.archive == "home" {
			nested = true
		}
		// The reason the headers are kept rather than rebuilt: DirectAdmin
		// writes members owned by groups that are not the account's own.
		if member.gname != "" && member.gname != account && member.gname != "root" {
			elsewhere = true
		}
	}
	if !nested {
		t.Error("this archive has no nested home archive in it, so the nested path was not exercised")
	}
	if !elsewhere {
		t.Error("no member of this archive is owned by a group other than the account's own")
	}
	t.Logf("%d members across both archives of %s came back unchanged",
		len(members), strings.TrimSuffix(filepath.Base(archive), ".tar.zst"))
}
