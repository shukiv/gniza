package staging

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAllocateRefusesWithoutEstimate(t *testing.T) {
	manager := &Manager{Root: t.TempDir()}
	if _, err := manager.Allocate("customer1", 0); err == nil {
		t.Error("a zero estimate must not bypass the space check")
	}
}

func TestAllocateKeyIsStableAcrossRuns(t *testing.T) {
	root := t.TempDir()
	manager := &Manager{Root: root}

	// The same account must stage to the same path every night: restic
	// records those paths in the snapshot, and paths that change per run
	// give every run its own retention group, which is then never pruned.
	first, err := manager.Allocate("customer1", 1024)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if err := manager.Release(first); err != nil {
		t.Fatalf("Release: %v", err)
	}
	second, err := manager.Allocate("customer1", 1024)
	if err != nil {
		t.Fatalf("second Allocate: %v", err)
	}
	if first.Path != second.Path {
		t.Errorf("paths differ between runs: %q then %q", first.Path, second.Path)
	}
}

func TestAllocateRefusesADirectoryAlreadyInUse(t *testing.T) {
	manager := &Manager{Root: t.TempDir(), MaxConcurrent: 4}
	if _, err := manager.Allocate("customer1", 1024); err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	// Either a concurrent run, which the controller prevents, or crash
	// debris the startup sweep missed. Both deserve a loud failure.
	if _, err := manager.Allocate("customer1", 1024); err == nil {
		t.Error("staging the same account twice should be refused")
	}
}

func TestAllocateRejectsUnsafeKey(t *testing.T) {
	manager := &Manager{Root: t.TempDir()}
	for _, key := range []string{"", "../escape", "a/b", "a b", "a;rm"} {
		if _, err := manager.Allocate(key, 1024); err == nil {
			t.Errorf("key %q should be rejected", key)
		}
	}
}

func TestAllocateInsufficientSpace(t *testing.T) {
	manager := &Manager{Root: t.TempDir(), SafetyMarginRatio: 0.2}
	// Ask for more than any test filesystem can offer.
	_, err := manager.Allocate("job1", 1<<62)
	var spaceErr *ErrInsufficientSpace
	if !errors.As(err, &spaceErr) {
		t.Fatalf("err = %v, want ErrInsufficientSpace", err)
	}
	if spaceErr.Required <= 1<<62 {
		t.Errorf("Required = %d, safety margin was not applied", spaceErr.Required)
	}
}

func TestAllocateListRelease(t *testing.T) {
	root := t.TempDir()
	manager := &Manager{Root: root}

	dir, err := manager.Allocate("customer1", 1024)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if want := filepath.Join(root, "stage-customer1"); dir.Path != want {
		t.Errorf("Path = %q, want %q", dir.Path, want)
	}
	info, err := os.Stat(dir.Path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("mode = %04o, want 0700", perm)
	}

	// List must find directories left behind by a crashed agent, not just
	// ones this process allocated.
	listed, err := manager.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 1 || listed[0].Key != "customer1" {
		t.Fatalf("List = %+v, want one entry for customer1", listed)
	}

	if err := manager.Release(dir); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Stat(dir.Path); !os.IsNotExist(err) {
		t.Error("staging directory should be gone after Release")
	}
}

func TestConcurrencyCap(t *testing.T) {
	manager := &Manager{Root: t.TempDir(), MaxConcurrent: 2}
	for i, jobID := range []string{"a", "b"} {
		if _, err := manager.Allocate(jobID, 1024); err != nil {
			t.Fatalf("Allocate %d: %v", i, err)
		}
	}
	if _, err := manager.Allocate("c", 1024); err == nil {
		t.Error("allocation beyond MaxConcurrent should be rejected")
	}
}

func TestReleaseRefusesOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	manager := &Manager{Root: root}
	if err := manager.Release(&Dir{Path: outside, Key: "x"}); err == nil {
		t.Error("Release outside the staging root should be refused")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Error("Release must not have removed the outside directory")
	}
}

func TestReclaim(t *testing.T) {
	root := t.TempDir()
	manager := &Manager{Root: root}

	if reclaimed, err := manager.Reclaim("restore-customer1"); err != nil || reclaimed {
		t.Errorf("Reclaim on a clean root = %v, %v; want false, nil", reclaimed, err)
	}

	dir, err := manager.Allocate("restore-customer1", 1024)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir.Path, "cpmove.tar"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A second restore of the account supersedes the first one's archive,
	// rather than failing on the directory that is already there.
	reclaimed, err := manager.Reclaim("restore-customer1")
	if err != nil {
		t.Fatalf("Reclaim: %v", err)
	}
	if !reclaimed {
		t.Error("Reclaim did not report removing the directory")
	}
	if _, err := manager.Allocate("restore-customer1", 1024); err != nil {
		t.Errorf("a reclaimed key should be allocatable again: %v", err)
	}

	if _, err := manager.Reclaim("../escape"); err == nil {
		t.Error("an unsafe key should be rejected")
	}
}

// TestRetainedOutputDoesNotHoldAConcurrencySlot covers the failure that
// blocked a live server: a rebuilt archive left for collection counted as
// work in progress, so with a limit of one, a single uncollected download
// stopped every other account being backed up or restored.
func TestRetainedOutputDoesNotHoldAConcurrencySlot(t *testing.T) {
	root := t.TempDir()
	manager := &Manager{Root: root, MaxConcurrent: 1}

	dir, err := manager.Allocate("restore-customer1", 1024)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir.Path, "cpmove.tar"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	// While it is still work in progress, the limit applies.
	if _, err := manager.Allocate("customer2", 1024); err == nil {
		t.Error("the concurrency limit was not applied to work in progress")
	}

	retained, err := manager.Retain(dir)
	if err != nil {
		t.Fatalf("Retain: %v", err)
	}
	if !retained.Retained {
		t.Error("Retain did not mark the directory as output")
	}
	if _, err := os.Stat(filepath.Join(retained.Path, "cpmove.tar")); err != nil {
		t.Errorf("retaining lost the archive: %v", err)
	}

	// Once it is finished output, another account can be worked on.
	if _, err := manager.Allocate("customer2", 1024); err != nil {
		t.Errorf("retained output still blocks other accounts: %v", err)
	}

	// It is still listed, so a sweep can find it, but not as active work.
	all, err := manager.List()
	if err != nil {
		t.Fatal(err)
	}
	active, err := manager.Active()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || len(active) != 1 {
		t.Errorf("list=%d active=%d, want 2 and 1", len(all), len(active))
	}
	if active[0].Key != "customer2" {
		t.Errorf("active = %+v, want only the account being worked on", active)
	}
}

func TestRetainReplacesPreviousOutput(t *testing.T) {
	root := t.TempDir()
	manager := &Manager{Root: root}

	for _, body := range []string{"first", "second"} {
		dir, err := manager.Allocate("restore-customer1", 1024)
		if err != nil {
			t.Fatalf("Allocate %s: %v", body, err)
		}
		if err := os.WriteFile(filepath.Join(dir.Path, "cpmove.tar"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.Retain(dir); err != nil {
			t.Fatalf("Retain %s: %v", body, err)
		}
	}

	// A newer rebuild supersedes the older one rather than piling up.
	got, err := os.ReadFile(filepath.Join(root, "keep-restore-customer1", "cpmove.tar"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "second" {
		t.Errorf("kept %q, want the newer rebuild", got)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 {
		t.Errorf("root holds %d directories, want just the retained one", len(entries))
	}
}

func TestReclaimRemovesRetainedOutputToo(t *testing.T) {
	root := t.TempDir()
	manager := &Manager{Root: root}

	dir, err := manager.Allocate("restore-customer1", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Retain(dir); err != nil {
		t.Fatal(err)
	}

	reclaimed, err := manager.Reclaim("restore-customer1")
	if err != nil {
		t.Fatalf("Reclaim: %v", err)
	}
	if !reclaimed {
		t.Error("Reclaim did not report removing the retained output")
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("root still holds %d directories", len(entries))
	}
}

// Finished output is kept so it can be collected, but not for ever: a
// server where every account had been restored once kept every one of
// those trees, on a disk that also has to hold tonight's backup.
func TestRetainedListsWhatIsWaitingWithItsSizeAndAge(t *testing.T) {
	root := t.TempDir()
	manager := &Manager{Root: root, MaxConcurrent: 4}

	dir, err := manager.Allocate("restore-customer1", 1<<10)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir.Path, "cpmove.tar"), make([]byte, 2048), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Retain(dir); err != nil {
		t.Fatal(err)
	}
	// Work in progress is not output and must never be listed as such.
	if _, err := manager.Allocate("customer2", 1<<10); err != nil {
		t.Fatal(err)
	}

	outputs, err := manager.Retained()
	if err != nil {
		t.Fatal(err)
	}
	if len(outputs) != 1 {
		t.Fatalf("retained = %+v, want only the finished output", outputs)
	}
	if outputs[0].Key != "restore-customer1" {
		t.Errorf("key = %q", outputs[0].Key)
	}
	if outputs[0].Bytes != 2048 {
		t.Errorf("bytes = %d, want 2048", outputs[0].Bytes)
	}
	if time.Since(outputs[0].At) > time.Minute {
		t.Errorf("produced at %v, which is not now", outputs[0].At)
	}
}

// The number an operator has to act on is a size, not a byte count.
func TestSpaceErrorReadsAsSizes(t *testing.T) {
	err := &ErrInsufficientSpace{Required: 8151213721, Available: 6778830848}
	want := "not enough room to stage this account: it needs 7.6 GiB free and there is 6.3 GiB"
	if err.Error() != want {
		t.Errorf("error = %q,\n want %q", err.Error(), want)
	}
}

// The server's own configuration is staged under a name no cPanel account
// can have. Everything else that is not a plain name is still refused: a
// key becomes a directory under the staging root.
func TestKeysAllowTheSystemNameAndNothingDangerous(t *testing.T) {
	root := t.TempDir()
	manager := &Manager{Root: root, MaxConcurrent: 4}

	if _, err := manager.Allocate("@system", 1<<10); err != nil {
		t.Errorf("the system key was refused: %v", err)
	}
	for _, key := range []string{"../escape", "a/b", "a b", "a;rm -rf /", ".", ""} {
		if _, err := manager.Allocate(key, 1<<10); err == nil {
			t.Errorf("staging accepted %q as a key", key)
		}
	}
}

// With MaxConcurrent above one, two accounts stage at the same time, and
// each was checked against the whole free volume as though it were the
// only one: two accounts estimated at 30 GiB each both passed on a disk
// with 53 GiB free, and together they needed more than it holds. What a
// directory has committed to and not yet written has to count against
// the next request.
func TestASecondAccountIsCheckedAgainstWhatTheFirstReserved(t *testing.T) {
	root := t.TempDir()
	free, err := AvailableBytes(root)
	if err != nil {
		t.Fatal(err)
	}
	manager := &Manager{Root: root, MaxConcurrent: 4}

	// Three fifths each: one fits on its own, two do not fit together.
	tooBigTogether := free / 5 * 3
	first, err := manager.Allocate("first", tooBigTogether)
	if err != nil {
		t.Fatalf("the first account could not be staged on an empty volume: %v", err)
	}
	var full *ErrInsufficientSpace
	if _, err := manager.Allocate("second", tooBigTogether); !errors.As(err, &full) {
		t.Errorf("two accounts needing more than the volume holds were both staged: %v", err)
	}

	// What the first one gave back is free again.
	if err := manager.Release(first); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Allocate("second", tooBigTogether); err != nil {
		t.Errorf("the space the first account gave back was not offered to the second: %v", err)
	}
}

// And the reservation is not a second concurrency limit: accounts that
// do fit together still stage together.
func TestAccountsThatFitTogetherStageTogether(t *testing.T) {
	root := t.TempDir()
	free, err := AvailableBytes(root)
	if err != nil {
		t.Fatal(err)
	}
	manager := &Manager{Root: root, MaxConcurrent: 4}
	small := free / 8
	for _, key := range []string{"one", "two", "three"} {
		if _, err := manager.Allocate(key, small); err != nil {
			t.Fatalf("staging %s: %v", key, err)
		}
	}
}

// A staging directory is being written into while its reservation is
// read: pkgacct creates, renames and unlinks under it, and a subtree the
// walk cannot enter is an answer of "I do not know how much is there",
// not "there is nothing there". Forgetting the reservation on any such
// answer puts back the bug it was made for.
func TestAReservationSurvivesADirectoryThatCannotBeMeasured(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory whatever its mode says")
	}
	root := t.TempDir()
	free, err := AvailableBytes(root)
	if err != nil {
		t.Fatal(err)
	}
	manager := &Manager{Root: root, MaxConcurrent: 4}

	tooBigTogether := free / 5 * 3
	first, err := manager.Allocate("first", tooBigTogether)
	if err != nil {
		t.Fatal(err)
	}
	closed := filepath.Join(first.Path, "unreadable")
	if err := os.Mkdir(closed, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(closed, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(closed, 0o700) })

	var full *ErrInsufficientSpace
	if _, err := manager.Allocate("second", tooBigTogether); !errors.As(err, &full) {
		t.Errorf("a directory that could not be measured gave its reservation up: %v", err)
	}
}

// And a file that goes between the listing and the stat is ordinary: the
// staging directory is being written into the whole time it is measured.
func TestAFileThatVanishesDuringTheWalkIsNotAnError(t *testing.T) {
	var total uint64
	count := addSize(&total)
	if err := count("stays", fakeFile(7), nil); err != nil {
		t.Fatal(err)
	}
	if err := count("gone", nil, fs.ErrNotExist); err != nil {
		t.Errorf("a file that went while the walk was running stopped it: %v", err)
	}
	if err := count("unreadable", nil, fs.ErrPermission); err == nil {
		t.Error("a directory that could not be read was measured as empty")
	}
	if total != 7 {
		t.Errorf("the walk counted %d bytes", total)
	}
}

// fakeFile is one regular file of the given size, as Walk would hand it over.
type fakeFile int64

func (f fakeFile) Name() string       { return "file" }
func (f fakeFile) Size() int64        { return int64(f) }
func (f fakeFile) Mode() fs.FileMode  { return 0o600 }
func (f fakeFile) ModTime() time.Time { return time.Time{} }
func (f fakeFile) IsDir() bool        { return false }
func (f fakeFile) Sys() any           { return nil }
