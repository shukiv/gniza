package node

import (
	"context"
	"time"

	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/notify"
)

// RetentionIsThrottledForTest reports whether this repository is inside
// the window that keeps retention from taking the repository lock again.
func (e *Engine) RetentionIsThrottledForTest(repo nodestore.Repository) bool {
	last := lastRetentionAttempt(repo.Retention)
	return !last.IsZero() && time.Since(last) < retentionEvery
}

// RestoreStagingEstimateForTest exposes the pure sizing rule to the
// external-package tests without making it part of the production API.
func RestoreStagingEstimateForTest(kind string, snapshotBytes uint64) uint64 {
	return restoreStagingEstimate(kind, snapshotBytes, 0)
}

// BackupMessage is what a finished run would be announced as, with the
// body it would carry.
func BackupMessage(stored nodestore.Job) (notify.Message, bool) {
	message, send := backupMessage(stored)
	if send {
		message.Body = backupDetail(stored, nil)
	}
	return message, send
}

// TakeCensusForTest measures the repositories on the caller's goroutine,
// so a test can read the result instead of waiting for one.
func (e *Engine) TakeCensusForTest(ctx context.Context, now time.Time) {
	e.takeCensus(ctx, now)
}

// InstallerWrapperForTest is the script the transient unit runs, so a
// test can run it the way systemd would rather than a copy of it.
const InstallerWrapperForTest = installerWrapper

// CheckForTest checks one repository on the caller's goroutine, so a test
// can read what was recorded instead of waiting for it.
func (e *Engine) CheckForTest(ctx context.Context, repositoryID string) {
	e.checkOne(ctx, repositoryID, checkSubsetPercent)
}

// CheckDueForTest exposes the rule that decides when a repository is
// checked.
func CheckDueForTest(repo nodestore.Repository, now time.Time, quiet bool) bool {
	return checkDueFor(repo, now, quiet)
}

// SweepLocksForTest looks at every repository for stale locks on the
// caller's goroutine.
func (e *Engine) SweepLocksForTest(ctx context.Context) { e.sweepLocksOnce(ctx) }

// SetCheckingForTest says a check is, or is no longer, running.
func (e *Engine) SetCheckingForTest(running bool) { e.checking.Store(running) }
