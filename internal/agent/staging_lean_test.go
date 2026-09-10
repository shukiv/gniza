package agent

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/cpanel"
	"github.com/shukiv/gniza/internal/layout/dabackup"
	"github.com/shukiv/gniza/internal/panel"
	"github.com/shukiv/gniza/internal/pkgacct"
	"github.com/shukiv/gniza/internal/protocol"
	"github.com/shukiv/gniza/internal/resticrun"
	"github.com/shukiv/gniza/internal/staging"
)

// leanPanel is a provider whose archives carry everything -- so the agent
// would ordinarily stage the account and take it apart -- but which has
// been found to leave the account's own files out and read them where
// they lie. What it stages is then the mail and the database dumps, and
// the account it belongs to says nothing about how large those are.
type leanPanel struct{ *cpanel.Fake }

func (leanPanel) Layout() panel.Layout   { return dabackup.Layout{} }
func (leanPanel) ReadsHomeInPlace() bool { return true }
func (leanPanel) Account(context.Context, string) (panel.AccountInfo, error) {
	// A petabyte of files on disk; eight megabytes of mail and dumps.
	return panel.AccountInfo{User: "c1", HomeDir: "/home/c1", SizeBytes: 1 << 50, LeanBytes: 8 << 20}, nil
}

// The preflight reserves what the panel writes, not a share of what the
// account holds. Reserving a share of the account refused a backup on the
// validation host that needed a fraction of a percent of it.
func TestABackupReservesWhatThePanelWritesNotAShareOfTheAccount(t *testing.T) {
	root := t.TempDir()
	executor := resticrun.ExecFunc(func(context.Context, resticrun.Command) (resticrun.CommandResult, error) {
		return resticrun.CommandResult{}, nil
	})
	worker := New(Config{Provider: leanPanel{&cpanel.Fake{}}, Staging: &staging.Manager{Root: root},
		Runner: resticrun.New(resticrun.Config{RuntimeDir: root}, executor), Log: slog.New(slog.DiscardHandler)})
	report := worker.RunJob(t.Context(), protocol.JobAssignment{
		JobID: "job", CPanelUser: "c1", PayloadMode: string(pkgacct.ModeSplit),
	})
	if strings.Contains(report.StagingError, "not enough room") || strings.Contains(report.StagingError, "staging") {
		t.Fatalf("a backup of what the panel writes was refused the room for the account: %q", report.StagingError)
	}
}
