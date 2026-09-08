package directadmin

import (
	"errors"
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
	if kinds[pkgacct.PartHomedir] != dabackup.HomedirPart(staging) {
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
	staging := privateStaging(t)

	if _, err := r.Stage(t.Context(), panel.StageRequest{
		Account: panel.AccountInfo{User: "studio"}, StagingDir: staging, Mode: pkgacct.ModeSplit,
	}); err != nil {
		t.Fatalf("staging in parts: %v", err)
	}
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
