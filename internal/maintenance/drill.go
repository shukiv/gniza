package maintenance

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/pkgacct"
	"github.com/shukiv/gniza/internal/reassemble"
	"github.com/shukiv/gniza/internal/resticrun"
	"github.com/shukiv/gniza/internal/staging"
)

// DrillRequest asks for a restore rehearsal.
type DrillRequest struct {
	RepositoryID string
	// Account is the cPanel user to rehearse. Empty picks the account of
	// the repository's most recent snapshot.
	Account string
	// WorkDir is scratch space; it is emptied when the drill finishes.
	WorkDir string
}

// DrillResult summarises a rehearsal.
type DrillResult struct {
	Account       string
	SnapshotID    string
	Mode          pkgacct.Mode
	BytesRestored uint64
	// Checks are the structural assertions that passed, in order, so a
	// recorded drill says what was actually verified.
	Checks []string
}

// Drill restores a snapshot into scratch space and checks that what comes
// out looks like an account, then throws it away.
//
// The checks are structural. Nothing here can tell you cPanel would accept
// the archive — only a real restorepkg on a real host can — but a drill
// that fails means the backup certainly cannot be restored, which is the
// question worth answering nightly. See docs/DESIGN.md §10.
func (r *Runner) Drill(ctx context.Context, req DrillRequest) (DrillResult, error) {
	var result DrillResult

	err := r.withRun(ctx, req.RepositoryID, KindDrill, func(repo resticrun.Repository) (string, error) {
		account := req.Account
		snapshotID := ""

		filter := resticrun.SnapshotFilter{Latest: 1}
		if account != "" {
			filter.Tags = []string{"account:" + account}
		}
		snapshots, err := r.restic.Snapshots(ctx, repo, filter)
		if err != nil {
			return "", err
		}
		if len(snapshots) == 0 {
			return "", fmt.Errorf("maintenance: repository holds no snapshot to rehearse")
		}
		newest := snapshots[len(snapshots)-1]
		incomplete, err := r.store.IncompleteSnapshotIDs(ctx, req.RepositoryID)
		if err != nil {
			return "", err
		}
		newest.ReadFailed = incomplete[newest.ID]
		if !newest.Complete() {
			return "", fmt.Errorf("maintenance: latest snapshot is partial or its source reads are unverified")
		}
		snapshotID = newest.ID
		if account == "" {
			account = newest.Account()
		}
		if account == "" {
			return "", fmt.Errorf("maintenance: snapshot %s has no account tag", newest.ShortID)
		}
		sourceBytes, err := r.restic.RestoreSize(ctx, repo, newest)
		if err != nil {
			return "", err
		}

		workDir := req.WorkDir
		if workDir == "" {
			workDir, err = os.MkdirTemp("", "gniza-drill-")
			if err != nil {
				return "", fmt.Errorf("maintenance: create drill scratch: %w", err)
			}
		} else if err := os.MkdirAll(workDir, 0700); err != nil {
			return "", fmt.Errorf("maintenance: create drill scratch: %w", err)
		}
		// A drill's output is proof, not a deliverable.
		defer func() { _ = os.RemoveAll(workDir) }()
		available, err := staging.AvailableBytes(workDir)
		if err != nil {
			return "", err
		}
		// A drill stops at the tree, so it is one copy rather than two --
		// unless the panel's parts are not the account's own files, and
		// checking them means building the archive as well.
		if required := reassemble.RehearsalBytes(sourceBytes, r.layout); available < required {
			return "", &staging.ErrInsufficientSpace{Required: required, Available: available}
		}

		rebuilt, err := reassemble.Run(ctx, r.restic, reassemble.Request{
			Account:    account,
			SnapshotID: snapshotID,
			WorkDir:    workDir,
			TreeOnly:   true,
			Repo:       repo,
			Layout:     r.layout,
		})
		if err != nil {
			return "", err
		}

		checks, err := reassemble.Verify(ctx, rebuilt)
		result = DrillResult{
			Account:       account,
			SnapshotID:    snapshotID,
			Mode:          rebuilt.Mode,
			BytesRestored: rebuilt.BytesRestored,
			Checks:        checks,
		}
		if err != nil {
			return "", err
		}
		r.log.Info("restore drill passed",
			"repository_id", req.RepositoryID, "account", account,
			"snapshot", snapshotID, "checks", strings.Join(checks, ", "))
		_, detail := drillOutcome(result, nil)
		return detail, nil
	})
	return result, err
}

// drillOutcome renders a drill result for the maintenance_runs row.
func drillOutcome(result DrillResult, err error) (string, string) {
	if err != nil {
		return string(job.StatusFailed), err.Error()
	}
	return string(job.StatusSuccess), fmt.Sprintf(
		"account=%s snapshot=%s mode=%s bytes=%d checks=%s",
		result.Account, result.SnapshotID, result.Mode, result.BytesRestored,
		strings.Join(result.Checks, "; "))
}
