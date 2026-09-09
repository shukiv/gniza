package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// quietLog is a logger that writes nowhere, for a test that cares about
// what a function returns rather than what it says on its way.
func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestLiveCertificationProducesAuditReport(t *testing.T) {
	root := t.TempDir()
	archive := filepath.Join(root, "cpmove-customer1.tar")
	if err := os.WriteFile(archive, []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := runLiveCertification(context.Background(), config{
		certifyArchive: archive, certifyUser: "cprv1234",
		certifyIsolatedHost: true, fakeRoot: filepath.Join(root, "cpanel"),
	}, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed || report.FinishedAt.IsZero() || len(report.Checks) != 4 {
		t.Fatalf("incomplete certification report: %+v", report)
	}
}

func TestLiveCertificationReportRecordsRefusedUnsafeRun(t *testing.T) {
	report, err := runLiveCertification(context.Background(), config{
		certifyArchive: "/tmp/archive.tar", certifyUser: "cprv1234",
	}, quietLog())
	if err == nil || report.Passed || report.Error == "" || report.FinishedAt.IsZero() {
		t.Fatalf("unsafe certification was not recorded as failed: %+v, %v", report, err)
	}
}

// TestAMissingResticDoesNotStopTheServiceFromStarting is a crash loop that
// happened on a live server: the installer replaces /usr/local/bin/restic
// in place, and for the length of that download the file is there but not
// yet executable. The agent probed restic, exited, and was restarted every
// five seconds for three and a half minutes. The interface an operator
// would look at to find out why is the process that keeps exiting.
func TestAMissingResticDoesNotStopTheServiceFromStarting(t *testing.T) {
	log := quietLog()

	version, err := startupRestic(context.Background(), "/nonexistent/restic", false, log)
	if err != nil {
		t.Errorf("a restic that will not run must not stop the service: %v", err)
	}
	if version != "" {
		t.Errorf("version = %q, want empty", version)
	}

	if _, err := startupRestic(context.Background(), "/nonexistent/restic", true, log); err == nil {
		t.Error("preflight exists to answer whether this server is ready, so it must still fail")
	}

	version, err = startupRestic(context.Background(), "/bin/echo", false, log)
	if err != nil {
		t.Fatalf("a restic that runs: %v", err)
	}
	if version != "version" {
		t.Errorf("version = %q, want the probe's output", version)
	}
}
