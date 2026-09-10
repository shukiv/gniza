package directadmin

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

// The estimate has to predict the path Stage will take, or a large
// account is refused the room for a shape it was never going to use.
// Stage reads the home in place until a run finds this server ignores
// the selection, and so does this: an agent that has just started knows
// nothing, and the first account of the night is as likely to be the
// largest as the smallest.
func TestTheEstimateAgreesWithThePathAStageWillTake(t *testing.T) {
	r := nativeHost(t)
	if r.ReadsHomeInPlace() {
		t.Error("a server that was not asked for this shape reports it reads the home in place")
	}
	r.ReadHomeInPlace = true
	if !r.ReadsHomeInPlace() {
		t.Error("a server asked for this shape, before any backup has run, is estimated for the old one")
	}
	if _, err := r.Stage(t.Context(), panel.StageRequest{
		Account: panel.AccountInfo{User: "studio"}, StagingDir: privateStaging(t), Mode: pkgacct.ModeSplit,
	}); err != nil {
		t.Fatal(err)
	}
	if !r.ReadsHomeInPlace() {
		t.Error("a server whose backup came back without the account's files reports otherwise")
	}
	// And a server found to ignore the selection is estimated for the
	// shape it does produce, which is the whole account on disk.
	r.leanBackups.Store(-1)
	if r.ReadsHomeInPlace() {
		t.Error("a server that ignores the selection is estimated for a shape it will not produce")
	}
}

// roundcubeScript stands in for DirectAdmin's own scripts/backup_roundcube.php,
// which takes the domain, the account and the output file in its
// environment and writes one XML file. This one writes those three back
// out so the test can see what it was asked for.
func roundcubeScript(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// domainConf records one domain against an account the way DirectAdmin
// does, which is where the provider reads the list of domains from.
func domainConf(t *testing.T, r *Real, account, domain string) {
	t.Helper()
	dir := filepath.Join(r.DataDir, account, "domains")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, domain+".conf"), []byte("domain="+domain+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

const echoingRoundcube = "#!/bin/sh\nprintf '<ROUNDCUBE domain=\"%s\" user=\"%s\"/>\\n' \"$domain\" \"$username\" > \"$xml_file\"\n"

// Leaving email_data out of the selection is what keeps the mailbox out
// of the archive, and it also leaves out roundcube.xml -- DirectAdmin
// files that under the same option. Those are the webmail address books,
// identities and preferences, and DirectAdmin ships the script that
// exports them per domain. It is run for every domain of the account into
// the place DirectAdmin's restore looks for it, so the restore puts them
// back on its own.
func TestALeanBackupExportsTheWebmailDataForEveryDomain(t *testing.T) {
	r := nativeHost(t)
	r.ReadHomeInPlace = true
	r.ScriptsDir = filepath.Join(t.TempDir(), "scripts")
	roundcubeScript(t, r.ScriptsDir, "backup_roundcube.php", echoingRoundcube)
	for _, domain := range []string{"studio.example", "second.example"} {
		domainConf(t, r, "studio", domain)
	}
	staging := privateStaging(t)
	payload, err := r.Stage(t.Context(), panel.StageRequest{
		Account: panel.AccountInfo{User: "studio"}, StagingDir: staging, Mode: pkgacct.ModeSplit,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, domain := range []string{"studio.example", "second.example"} {
		xml := filepath.Join(dabackup.MetadataPart(staging), "backup", domain, "email", "data", "roundcube.xml")
		body, err := os.ReadFile(xml)
		if err != nil {
			t.Errorf("no webmail data for %s where DirectAdmin's restore looks for it: %v", domain, err)
			continue
		}
		want := "<ROUNDCUBE domain=\"" + domain + "\" user=\"studio\"/>\n"
		if string(body) != want {
			t.Errorf("%s: the script was asked for %q, want %q", domain, body, want)
		}
		// And the archive is rebuilt from the manifest, so a file that is
		// only on disk is one the restore never sees.
		manifest, err := os.ReadFile(filepath.Join(dabackup.MetadataPart(staging), dabackup.ManifestFile))
		if err != nil {
			t.Fatal(err)
		}
		if member := "backup/" + domain + "/email/data/roundcube.xml"; !strings.Contains(string(manifest), member) {
			t.Errorf("the manifest does not record %s, so the rebuilt archive will not carry it", member)
		}
	}
	if len(payload.Warnings) != 0 {
		t.Errorf("a backup whose webmail data was exported warns: %v", payload.Warnings)
	}
}

// DirectAdmin runs scripts/custom/backup_roundcube.php ahead of its own
// when an administrator put one there, and so does this.
func TestAnAdministratorsOwnRoundcubeScriptIsTheOneRun(t *testing.T) {
	r := nativeHost(t)
	r.ReadHomeInPlace = true
	r.ScriptsDir = filepath.Join(t.TempDir(), "scripts")
	roundcubeScript(t, r.ScriptsDir, "backup_roundcube.php", echoingRoundcube)
	roundcubeScript(t, filepath.Join(r.ScriptsDir, "custom"), "backup_roundcube.php",
		"#!/bin/sh\nprintf 'custom' > \"$xml_file\"\n")
	domainConf(t, r, "studio", "studio.example")
	staging := privateStaging(t)
	if _, err := r.Stage(t.Context(), panel.StageRequest{
		Account: panel.AccountInfo{User: "studio"}, StagingDir: staging, Mode: pkgacct.ModeSplit,
	}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dabackup.MetadataPart(staging), "backup", "studio.example", "email", "data", "roundcube.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "custom" {
		t.Errorf("the administrator's script was passed over: %q", body)
	}
}

// An address book that could not be exported is a warning on the backup,
// not a backup that did not happen: the account's files, records, mail
// and databases are all there, and the page has to say what is not.
func TestWebmailDataThatCannotBeExportedIsAWarningNotAFailure(t *testing.T) {
	r := nativeHost(t)
	r.ReadHomeInPlace = true
	r.ScriptsDir = filepath.Join(t.TempDir(), "scripts")
	roundcubeScript(t, r.ScriptsDir, "backup_roundcube.php", "#!/bin/sh\necho 'Cannot read mysql configuration file' >&2\nexit 1\n")
	domainConf(t, r, "studio", "studio.example")
	payload, err := r.Stage(t.Context(), panel.StageRequest{
		Account: panel.AccountInfo{User: "studio"}, StagingDir: privateStaging(t), Mode: pkgacct.ModeSplit,
	})
	if err != nil {
		t.Fatalf("a failed webmail export took the whole backup down: %v", err)
	}
	found := false
	for _, w := range payload.Warnings {
		found = found || (strings.Contains(w, "studio.example") && strings.Contains(w, "webmail"))
	}
	if !found {
		t.Errorf("nothing says the webmail data for studio.example is not in the backup: %v", payload.Warnings)
	}
}

// The native workspace holds the compressed archive and nothing else for
// a lean run: DirectAdmin assembles in its own backup_tmpdir, not here.
// Reserving two copies of the dumps for a workspace that holds a
// compressed fraction of one refused a real account.
func TestALeanRunReservesOneCopyInTheNativeWorkspace(t *testing.T) {
	r := nativeHost(t)
	r.ReadHomeInPlace = true
	var fs syscall.Statfs_t
	if err := syscall.Statfs(filepath.Dir(r.NativeRoot), &fs); err != nil {
		t.Fatal(err)
	}
	avail := fs.Bavail * uint64(fs.Bsize)
	const gib = 1 << 30
	if avail < 8*gib {
		t.Skipf("only %d bytes free here; the arithmetic needs 8 GiB", avail)
	}
	// One copy plus the reserve fits; two copies plus the reserve does not.
	lean := avail/2 + gib
	if _, err := r.Stage(t.Context(), panel.StageRequest{
		Account:    panel.AccountInfo{User: "studio", LeanBytes: lean},
		StagingDir: privateStaging(t), Mode: pkgacct.ModeSplit,
	}); err != nil {
		t.Fatalf("a lean backup was refused the room for two copies of its dumps: %v", err)
	}
}

// The per-mailbox send limits are filed under email_data as well, at
// backup/<domain>/email/data/limit/<mailbox>, and DirectAdmin keeps the
// live ones in /etc/virtual/<domain>/limit/. They are copied in from
// there, so a restore puts the limits back with the mailboxes.
func TestALeanBackupKeepsTheMailboxSendLimits(t *testing.T) {
	r := nativeHost(t)
	r.ReadHomeInPlace = true
	r.ScriptsDir = filepath.Join(t.TempDir(), "scripts")
	roundcubeScript(t, r.ScriptsDir, "backup_roundcube.php", echoingRoundcube)
	r.VirtualDir = t.TempDir()
	domainConf(t, r, "studio", "studio.example")
	limits := filepath.Join(r.VirtualDir, "studio.example", "limit")
	if err := os.MkdirAll(limits, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"sales": "1", "studio": "200"} {
		if err := os.WriteFile(filepath.Join(limits, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	staging := privateStaging(t)
	payload, err := r.Stage(t.Context(), panel.StageRequest{
		Account: panel.AccountInfo{User: "studio"}, StagingDir: staging, Mode: pkgacct.ModeSplit,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(dabackup.MetadataPart(staging), dabackup.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"sales": "1", "studio": "200"} {
		member := "backup/studio.example/email/data/limit/" + name
		body, err := os.ReadFile(filepath.Join(dabackup.MetadataPart(staging), filepath.FromSlash(member)))
		if err != nil {
			t.Errorf("no send limit for %s where DirectAdmin's restore looks for it: %v", name, err)
			continue
		}
		if string(body) != want {
			t.Errorf("%s: limit %q, want %q", name, body, want)
		}
		if !strings.Contains(string(manifest), member) {
			t.Errorf("the manifest does not record %s, so the rebuilt archive will not carry it", member)
		}
	}
	if len(payload.Warnings) != 0 {
		t.Errorf("a backup whose limits were copied warns: %v", payload.Warnings)
	}
}
