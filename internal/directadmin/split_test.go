package directadmin

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/layout/dabackup"
	"github.com/shukiv/gniza/internal/panel"
	"github.com/shukiv/gniza/internal/pkgacct"
)

// Split mode is what makes restic deduplicate. DirectAdmin will not
// produce the parts, so the archive it does produce is taken apart into
// them, and what restic is pointed at is files rather than one compressed
// blob that is a full copy every night.
func TestSplitStagingLeavesTheAccountAsFilesResticCanDeduplicate(t *testing.T) {
	r := nativeHost(t)
	r.ReadHomeInPlace = true
	staging := privateStaging(t)

	payload, err := r.Stage(t.Context(), panel.StageRequest{
		Account: panel.AccountInfo{User: "studio"}, StagingDir: staging, Mode: pkgacct.ModeSplit,
	})
	if err != nil {
		t.Fatalf("staging in parts: %v", err)
	}
	if payload.Mode != pkgacct.ModeSplit {
		t.Errorf("the payload reports mode %q", payload.Mode)
	}
	// Not degraded: this is the shape that deduplicates, and saying it
	// stores badly would be telling an operator to turn it off.
	if payload.Degraded {
		t.Errorf("a split payload reports itself degraded: %s", payload.Reason)
	}
	kinds := map[pkgacct.PartKind]string{}
	for _, part := range payload.Parts {
		kinds[part.Kind] = part.Path
	}
	if kinds[pkgacct.PartMetadata] != dabackup.MetadataPart(staging) {
		t.Errorf("the account's records are at %q", kinds[pkgacct.PartMetadata])
	}
	// The home directory is the one the account is on. Copying it into
	// staging first is what this stopped doing: see ADR 0021.
	if kinds[pkgacct.PartHomedir] != filepath.Join(r.HomeRoot, "studio") {
		t.Errorf("the home directory is at %q", kinds[pkgacct.PartHomedir])
	}
	if kinds[pkgacct.PartArchive] != "" {
		t.Errorf("a split payload still carries a whole archive at %q", kinds[pkgacct.PartArchive])
	}

	// The archive itself is gone. Leaving it beside the parts would be
	// the account twice on a disk that had to fit it once, and restic
	// would store the compressed copy too.
	entries, err := os.ReadDir(staging)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tar.zst") || strings.HasSuffix(entry.Name(), ".tar.gz") {
			t.Errorf("the compressed archive was left in staging: %s", entry.Name())
		}
	}
}

// And what was staged goes back into the archive DirectAdmin's own
// restore reads. A backup that could be taken apart and not put back
// together is a backup nothing can restore.
func TestASplitStagedAccountGoesBackIntoItsOwnArchive(t *testing.T) {
	r := nativeHost(t)
	r.ReadHomeInPlace = true
	staging := privateStaging(t)

	if _, err := r.Stage(t.Context(), panel.StageRequest{
		Account: panel.AccountInfo{User: "studio"}, StagingDir: staging, Mode: pkgacct.ModeSplit,
	}); err != nil {
		t.Fatalf("staging in parts: %v", err)
	}
	// What restic restores beside the metadata part: a copy of the home
	// directory it read where it lay.
	restoreHome(t, staging)
	rebuilt, err := (dabackup.Layout{}).PackArchive(t.Context(), staging, "studio", t.TempDir())
	if err != nil {
		t.Fatalf("putting the account back into an archive: %v", err)
	}
	if filepath.Base(rebuilt) != "user.admin.studio.tar.zst" {
		t.Errorf("the rebuilt archive is called %s", filepath.Base(rebuilt))
	}
	if err := r.Layout().ValidateArchive(t.Context(), rebuilt, "studio"); err != nil {
		t.Fatalf("the rebuilt archive does not say whose account it is: %v", err)
	}
}

// Leaving part of an account out still needs an answer only a running
// DirectAdmin can give, and it is refused in split mode as in any other:
// a backup that silently dropped the databases is worse than one that
// did not run.
func TestSplitStagingStillRefusesToLeavePartOfTheAccountOut(t *testing.T) {
	r := nativeHost(t)
	for what, req := range map[string]panel.StageRequest{
		"the home directory": {SkipHomedir: true},
		"the databases":      {SkipDatabases: true},
		"the email":          {SkipEmail: true},
	} {
		req.Account = panel.AccountInfo{User: "studio"}
		req.StagingDir = privateStaging(t)
		req.Mode = pkgacct.ModeSplit
		if _, err := r.Stage(t.Context(), req); !errors.Is(err, ErrUnverified) {
			t.Errorf("leaving out %s: err = %v, want it to say this is not established yet", what, err)
		}
	}
}

// restoreHome puts back what restic stored for the home part, which is
// the tree a rebuilt archive is made out of.
func restoreHome(t *testing.T, staging string) {
	t.Helper()
	home := dabackup.HomedirPart(staging)
	if err := os.MkdirAll(filepath.Join(home, "domains", "studio.example", "public_html"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(home, "domains", "studio.example", "public_html", "index.html"),
		[]byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".bashrc"), []byte("umask 022\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The backup asks DirectAdmin for everything except the account's own
// files, through the task line its own backup page posts. Asking for all
// of it is what made a DirectAdmin server read, compress, write, unpack
// and re-read every account every night.
func TestTheBackupAsksForEverythingExceptTheAccountsOwnFiles(t *testing.T) {
	r := nativeHost(t)
	r.ReadHomeInPlace = true
	if _, err := r.Stage(t.Context(), panel.StageRequest{
		Account: panel.AccountInfo{User: "studio"}, StagingDir: privateStaging(t),
		Mode: pkgacct.ModeSplit,
	}); err != nil {
		t.Fatalf("staging in parts: %v", err)
	}
	body, err := os.ReadFile(os.Getenv("GNIZA_NATIVE_CAPTURE"))
	if err != nil {
		t.Fatal(err)
	}
	task, err := url.ParseQuery(string(body))
	if err != nil {
		t.Fatal(err)
	}
	if task.Get("what") != "select" {
		t.Errorf("the backup asked for %q rather than a chosen set", task.Get("what"))
	}
	asked := map[string]bool{}
	for key, values := range task {
		if strings.HasPrefix(key, "option") {
			asked[values[0]] = true
		}
	}
	if asked["domain"] {
		t.Error("the backup asked for the account's own files, which restic reads where they lie")
	}
	// The mailboxes' passwords, quotas and webmail settings come with
	// "email" and are nowhere in the home directory.
	if !asked["email"] {
		t.Error("the backup did not ask for the mailboxes")
	}
	for _, want := range []string{"database", "database_data", "ftp", "subdomain"} {
		if !asked[want] {
			t.Errorf("the backup did not ask for %s", want)
		}
	}
}

// A DirectAdmin that reads the selection and backs up the whole account
// anyway is one this cannot read in place: what it wrote is the account,
// and unpacking it as though it were records alone would store the files
// twice. It goes back to taking the archive apart, and says so.
func TestAServerThatIgnoresTheSelectionGoesBackToTheArchive(t *testing.T) {
	r := nativeHost(t)
	r.ReadHomeInPlace = true
	t.Setenv("GNIZA_NATIVE_SCENARIO", "ignores-selection")
	staging := privateStaging(t)

	payload, err := r.Stage(t.Context(), panel.StageRequest{
		Account: panel.AccountInfo{User: "studio"}, StagingDir: staging, Mode: pkgacct.ModeSplit,
	})
	if err != nil {
		t.Fatalf("staging in parts: %v", err)
	}
	if !payload.Degraded || payload.Reason == "" {
		t.Error("a server that writes the whole account every night does not say so")
	}
	kinds := map[pkgacct.PartKind]string{}
	for _, part := range payload.Parts {
		kinds[part.Kind] = part.Path
	}
	if kinds[pkgacct.PartHomedir] != dabackup.HomedirPart(staging) {
		t.Errorf("the home directory is at %q, not the tree the archive was taken apart into",
			kinds[pkgacct.PartHomedir])
	}
	// And it does not ask again: the next account on this server takes
	// the whole archive without a wasted attempt at a selective one.
	if r.leanBackups.Load() != -1 {
		t.Errorf("the server was not remembered as one that ignores the selection")
	}
	// It still says why, though. A server that does this does it to every
	// account it has, and one job carrying the reason while the rest
	// carry nothing is how an operator ends up with a page full of
	// backups that look fine.
	next, err := r.Stage(t.Context(), panel.StageRequest{
		Account: panel.AccountInfo{User: "studio"}, StagingDir: privateStaging(t), Mode: pkgacct.ModeSplit,
	})
	if err != nil {
		t.Fatalf("staging the next account: %v", err)
	}
	if !next.Degraded || next.Reason == "" {
		t.Error("only the account that found this out is told why")
	}
}

// Reading the account where it lies changes the shape of what a restore
// is built from, and ADR 0021 says nothing ships on that shape until a
// restore drill on a real archive proves it can be rebuilt. So the
// server has to be told to do it, one server at a time, and a server
// that was not told does what it did before.
func TestReadingTheHomeDirectoryInPlaceWaitsToBeAskedFor(t *testing.T) {
	r := nativeHost(t)
	staging := privateStaging(t)
	payload, err := r.Stage(t.Context(), panel.StageRequest{
		Account: panel.AccountInfo{User: "studio"}, StagingDir: staging, Mode: pkgacct.ModeSplit,
	})
	if err != nil {
		t.Fatalf("staging in parts: %v", err)
	}
	kinds := map[pkgacct.PartKind]string{}
	for _, part := range payload.Parts {
		kinds[part.Kind] = part.Path
	}
	if kinds[pkgacct.PartHomedir] != dabackup.HomedirPart(staging) {
		t.Errorf("the home directory is at %q, and this server was never asked to read one in place",
			kinds[pkgacct.PartHomedir])
	}
	body, err := os.ReadFile(os.Getenv("GNIZA_NATIVE_CAPTURE"))
	if err != nil {
		t.Fatal(err)
	}
	task, err := url.ParseQuery(string(body))
	if err != nil {
		t.Fatal(err)
	}
	// No chosen set at all: "admin-backup", which is the command
	// DirectAdmin documents for a whole account.
	if task.Has("what") {
		t.Errorf("the backup asked for the chosen set %q", task.Get("what"))
	}
	if r.ReadsHomeInPlace() {
		t.Error("the server reports that it reads home directories in place")
	}
}

// A backup that reads the home directory in place never writes the home
// directory anywhere. Reserving room for it refuses backups that would
// have fitted: on the validation host a 19.7 GiB account produced a
// 296.5 MiB lean archive, and reserving the account refused it on a disk
// with 22 GiB free. See ADR 0021.
func TestABackupThatReadsTheHomeInPlaceReservesWhatItWrites(t *testing.T) {
	r := nativeHost(t)
	r.ReadHomeInPlace = true
	staging := privateStaging(t)

	// An account too large for any disk, whose lean archive is not.
	if _, err := r.Stage(t.Context(), panel.StageRequest{
		Account:    panel.AccountInfo{User: "studio", SizeBytes: ^uint64(0), LeanBytes: 4096},
		StagingDir: staging, Mode: pkgacct.ModeSplit,
	}); err != nil {
		t.Fatalf("a lean backup was refused the room for a whole account: %v", err)
	}
}
