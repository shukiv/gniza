package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/cpanel"
	"github.com/shukiv/gniza/internal/destination"
	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/pkgacct"
	"github.com/shukiv/gniza/internal/protocol"
	"github.com/shukiv/gniza/internal/resticrun"
	"github.com/shukiv/gniza/internal/staging"
)

// On 182.54.236.143 a backup of the server's configuration was recorded
// as a success with no file in it: every staged path had been left out
// by a list meant for an account. The provider refuses to stage nothing,
// so a snapshot of nothing is never what was meant, whatever the cause.
func TestABackupOfTheServersSettingsWithNoFileInItHasFailed(t *testing.T) {
	for files, want := range map[string]string{
		"0":  string(job.TargetFailed),
		"14": string(job.TargetSuccess),
	} {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "staging"), 0o700); err != nil {
			t.Fatal(err)
		}
		executor := resticrun.ExecFunc(func(_ context.Context, cmd resticrun.Command) (resticrun.CommandResult, error) {
			if !strings.Contains(strings.Join(cmd.Args, " "), "backup") {
				return resticrun.CommandResult{}, nil
			}
			summary := `{"message_type":"summary","files_new":` + files +
				`,"total_files_processed":` + files + `,"total_bytes_processed":0,` +
				`"data_added":2424,"snapshot_id":"926d85bb3d4441a60c0efc31ab5c05f710d2c0e2911ad8c0bd271d0193db4563"}` + "\n"
			if cmd.OnLine != nil {
				cmd.OnLine([]byte(strings.TrimSpace(summary)))
			}
			return resticrun.CommandResult{Stdout: []byte(summary)}, nil
		})
		worker := New(Config{
			Provider: &cpanel.Fake{Root: filepath.Join(root, "cpanel")},
			Staging:  &staging.Manager{Root: filepath.Join(root, "staging")},
			Runner:   resticrun.New(resticrun.Config{RuntimeDir: root}, executor),
			Log:      slog.New(slog.DiscardHandler),
		})
		report := worker.RunJob(t.Context(), protocol.JobAssignment{
			JobID: "job", CPanelUser: cpanel.SystemAccount, PayloadMode: string(pkgacct.ModeSplit),
			SizeEstimate: 1 << 20,
			// What did it on the server: bare names, which restic matches
			// anywhere.
			Excludes: []string{"etc", "var", "usr"},
			Targets: []protocol.Target{{
				RepositoryID: "repo", RepoPath: "backups", RepoPassword: "local-test-only-password",
				Spec: destination.Spec{Type: destination.Type("local"),
					Config: map[string]string{"root": filepath.Join(root, "destination")}},
			}},
		})
		if report.StagingError != "" || len(report.Targets) != 1 {
			t.Fatalf("%s files: the job did not reach its destination: %+v", files, report)
		}
		got := report.Targets[0]
		if got.Status != want {
			t.Errorf("%s files: recorded as %q (%s), want %q", files, got.Status, got.Error, want)
		}
		if files == "0" && !strings.Contains(got.Error, "holds no files") {
			t.Errorf("the failure does not say what is wrong: %q", got.Error)
		}
	}
}
