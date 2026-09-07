package node

import (
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
func RestoreStagingEstimateForTest(kind string, liveBytes, snapshotBytes uint64) uint64 {
	return restoreStagingEstimate(kind, liveBytes, snapshotBytes)
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
