//go:build directadmin_live

package directadmin

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/destination"
	"github.com/shukiv/gniza/internal/panel"
	"github.com/shukiv/gniza/internal/pkgacct"
	"github.com/shukiv/gniza/internal/reassemble"
	"github.com/shukiv/gniza/internal/resticrun"
)

// This deliberately narrow, opt-in test is for the disposable fixture recorded
// in docs/directadmin-validation-2026-09-08.md. It never selects arbitrary
// accounts, changes panel configuration, installs hooks or runs a shared queue.
// Successful output is retained for inspection. Run only after checking the
// production host for conflicting jobs and verifying the fixture baseline.
func TestLiveDirectAdminProviderRoundTrip(t *testing.T) {
	if os.Getenv("GNIZA_DA_LIVE_CONFIRM") != "restore-gzv0908a-on-182.54.236.10" {
		t.Skip("requires explicit confirmation of the disposable .10 fixture")
	}
	if os.Geteuid() != 0 {
		t.Fatal("root required")
	}
	const account = "gzv0908a"
	const domain = "gzv0908a.gniza-test.invalid"
	r := &Real{}
	conf, err := r.userConf(account)
	if err != nil || conf["domain"] != domain || conf["username"] != account || conf["usertype"] != "user" {
		t.Fatalf("fixture identity mismatch: %v", err)
	}
	work := os.Getenv("GNIZA_DA_LIVE_WORK")
	if !strings.HasPrefix(work, "/root/gniza-da-validation.") || filepath.Clean(work) != work {
		t.Fatal("root-private evidence directory required")
	}
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal("evidence directory must be new: ", err)
	}
	t.Log("evidence retained in", work)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	r.NativeRoot, err = os.MkdirTemp("/var/tmp", "gniza-da-fixture-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(r.NativeRoot, 0o711); err != nil {
		t.Fatal(err)
	}
	t.Log("isolated native workspace root", r.NativeRoot)
	private := filepath.Join(work, "stage")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	home := "/home/" + account
	page := filepath.Join(home, "domains", domain, "public_html/index.html")
	before, err := os.ReadFile(page)
	if err != nil || !bytes.Contains(before, []byte(account)) {
		t.Fatal("fixture website does not match the recorded account")
	}
	var fixtureBytes int64
	if err := filepath.Walk(home, func(_ string, entry os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if entry.Mode().IsRegular() {
			fixtureBytes += entry.Size()
		}
		if fixtureBytes > 20<<20 {
			return fmt.Errorf("disposable home exceeded 20 MiB; stop")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var disk syscall.Statfs_t
	if err := syscall.Statfs(work, &disk); err != nil || disk.Bavail*uint64(disk.Bsize) < 2<<30 {
		t.Fatal("need at least 2 GiB free for the fixture test")
	}
	info, err := r.Account(ctx, account)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Databases) != 1 || info.Databases[0] != account+"_shop" {
		t.Fatal("fixture database list changed")
	}
	payload, err := r.Stage(ctx, panel.StageRequest{Account: info, StagingDir: private, Mode: pkgacct.ModeMonolithic})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("provider staged and validated a native archive")
	archiveBytes, err := os.ReadFile(payload.Parts[0].Path)
	if err != nil || len(archiveBytes) > 10<<20 {
		t.Fatal("fixture archive unexpectedly large or unreadable")
	}
	password := make([]byte, 32)
	if _, err := rand.Read(password); err != nil {
		t.Fatal(err)
	}
	// Retained evidence includes its own private test-only repository key.
	if err := os.WriteFile(filepath.Join(work, "repository.key"), []byte(hex.EncodeToString(password)), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := resticrun.New(resticrun.Config{Binary: os.Getenv("GNIZA_DA_LIVE_RESTIC"), RuntimeDir: work, CacheDir: filepath.Join(work, "cache")}, nil)
	repo := resticrun.Repository{Dest: &destination.Local{Root: work}, Path: "repository", Password: hex.EncodeToString(password)}
	if err := runner.Init(ctx, repo, nil); err != nil {
		t.Fatal(err)
	}
	backup, err := runner.Backup(ctx, repo, resticrun.BackupSpec{Paths: payload.Paths(), Tags: []string{"account:" + account}, RecordCompletion: true})
	if err != nil || backup.Incomplete {
		t.Fatalf("repository backup failed: %v", err)
	}
	if err := runner.Check(ctx, repo, resticrun.CheckSpec{}); err != nil {
		t.Fatal(err)
	}
	result, err := reassemble.Run(ctx, runner, reassemble.Request{Account: account, SnapshotID: backup.Summary.SnapshotID, Layout: r.Layout(), WorkDir: filepath.Join(work, "restore"), Repo: repo})
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := os.ReadFile(result.ArchivePath)
	if err != nil || sha256.Sum256(archiveBytes) != sha256.Sum256(rebuilt) {
		t.Fatal("archive from repository differs")
	}
	t.Log("restic backup/check/reassembly passed; archive bytes match")
	// Mutate exactly one disposable file, preserving it first. Leave all other
	// synthetic fixture data intact, then independently check it after this test.
	held := filepath.Join(filepath.Dir(page), "gniza-provider-test-held-index.html")
	if _, err := os.Lstat(held); !os.IsNotExist(err) {
		t.Fatal("rollback file already exists")
	}
	if err := os.Rename(page, held); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := os.Lstat(page); os.IsNotExist(err) {
			_ = os.Rename(held, page)
		}
	}()
	transcript, applyErr := r.Apply(ctx, result.ArchivePath, panel.ApplyOptions{Overwrite: true, Unrestricted: true})
	if err := os.WriteFile(filepath.Join(work, "restore.log"), []byte(transcript), 0o600); err != nil {
		t.Error(err)
	}
	if applyErr != nil {
		t.Fatal(applyErr)
	}
	after, err := os.ReadFile(page)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("native provider restore did not restore the exact website content")
	}
	// Keep the rollback aid outside the account so future backups do not carry
	// an extra website copy. No existing customer file is deleted.
	if err := os.Rename(held, filepath.Join(work, "held-index.html")); err != nil {
		t.Fatal(err)
	}
	t.Log("PASS: provider -> restic -> reassembly -> provider restored the fixture website")
}
