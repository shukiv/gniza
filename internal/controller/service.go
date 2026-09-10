// Package controller is the control plane: it turns database state into
// job assignments for agents and records what comes back.
//
// Backup data never passes through here. The controller only schedules,
// hands out credentials for the destinations an agent must reach, and
// stores results. See docs/DESIGN.md §2.
package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/protocol"
	"github.com/shukiv/gniza/internal/repobuild"
	"github.com/shukiv/gniza/internal/store"
	"github.com/shukiv/gniza/internal/vault"
)

// Service holds the controller's dependencies.
type Service struct {
	store *store.Store
	vault *vault.Vault
	log   *slog.Logger

	// LeaseDuration is how long a claimed job stays leased before another
	// agent may take it. It must exceed the longest plausible backup.
	LeaseDuration time.Duration
	// PollInterval is what agents are told to wait between polls.
	PollInterval time.Duration
}

// New builds a Service.
func New(db *store.Store, v *vault.Vault, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		store:         db,
		vault:         v,
		log:           log,
		LeaseDuration: 6 * time.Hour,
		PollInterval:  protocol.DefaultPoll,
	}
}

// Enrol records what an agent reports about its host.
func (s *Service) Enrol(ctx context.Context, serverID string, req protocol.EnrolRequest) (protocol.EnrolResponse, error) {
	if err := s.store.RecordEnrolment(ctx, serverID, req.PkgacctFlags, req.StagingRoot); err != nil {
		return protocol.EnrolResponse{}, err
	}
	s.log.Info("agent enrolled",
		"server_id", serverID, "hostname", req.Hostname,
		"agent_version", req.AgentVersion, "restic_version", req.ResticVer,
		"pkgacct_flags", req.PkgacctFlags)
	return protocol.EnrolResponse{ServerID: serverID, PollInterval: s.PollInterval}, nil
}

// validClaimToken checks the shape of the token a report carries before
// it reaches SQL, where a uuid cast on anything else is a database error
// rather than a refusal. It says nothing about whether the token is the
// right one for this attempt; only the store can answer that.
func validClaimToken(token string) bool {
	if len(token) != 36 {
		return false
	}
	for i, char := range token {
		switch i {
		case 8, 13, 18, 23:
			if char != '-' {
				return false
			}
		default:
			hex := char >= '0' && char <= '9' ||
				char >= 'a' && char <= 'f' || char >= 'A' && char <= 'F'
			if !hex {
				return false
			}
		}
	}
	return true
}

// NextWork leases whatever this server should do next.
//
// Restores are offered before backups: someone is usually waiting for a
// restore, while a backup can start a minute later without anyone noticing.
func (s *Service) NextWork(ctx context.Context, serverID string) (protocol.Assignment, error) {
	restore, err := s.NextRestore(ctx, serverID)
	switch {
	case err == nil:
		return protocol.Assignment{Kind: protocol.KindRestore, Restore: &restore}, nil
	case !errors.Is(err, store.ErrNoWork):
		return protocol.Assignment{}, err
	}

	backup, err := s.NextJob(ctx, serverID)
	if err != nil {
		return protocol.Assignment{}, err
	}
	return protocol.Assignment{Kind: protocol.KindBackup, Backup: &backup}, nil
}

// NextRestore leases a restore and resolves its source repository.
func (s *Service) NextRestore(ctx context.Context, serverID string) (protocol.RestoreAssignment, error) {
	claimed, err := s.store.ClaimNextRestore(ctx, serverID, s.LeaseDuration)
	if err != nil {
		return protocol.RestoreAssignment{}, err
	}

	source, err := s.buildTarget(claimed.Source)
	if err != nil {
		return protocol.RestoreAssignment{}, fmt.Errorf(
			"controller: build restore source %s: %w", claimed.Source.RepositoryID, err)
	}

	return protocol.RestoreAssignment{
		JobID:        claimed.JobID,
		ClaimToken:   claimed.ClaimToken,
		AccountID:    claimed.Account.ID,
		CPanelUser:   claimed.Account.CPanelUser,
		SnapshotID:   claimed.SnapshotID,
		Kind:         claimed.Kind,
		IncludePaths: claimed.IncludePaths,
		TargetDir:    claimed.TargetDir,
		Apply:        claimed.Apply,
		Source:       source,
		// A restore writes roughly what the account occupies, so the
		// backup estimate is the right input to the space preflight.
		SizeEstimate:   uint64(claimed.Account.SizeEstimate),
		LeaseExpiresAt: claimed.LeaseExpiresAt,
	}, nil
}

// ReportRestore records a restore's outcome.
func (s *Service) ReportRestore(ctx context.Context, serverID string, report protocol.RestoreReport) error {
	if report.JobID == "" {
		return errors.New("controller: restore report is missing a job id")
	}
	if !validClaimToken(report.ClaimToken) {
		return errors.New("controller: restore report is missing the claim token it was assigned")
	}
	status := job.Status(report.Status)
	switch status {
	case job.StatusSuccess, job.StatusFailed:
	default:
		return fmt.Errorf("controller: restore report has invalid status %q", report.Status)
	}

	if err := s.store.ApplyRestoreReport(ctx, serverID, report.JobID, report.ClaimToken, store.RestoreOutcome{
		Status:        status,
		BytesRestored: report.BytesRestored,
		ArchivePath:   report.ArchivePath,
		Error:         report.Error,
		Detail:        report.Detail,
		Applied:       report.Applied,
	}); err != nil {
		return err
	}

	level := slog.LevelInfo
	if status == job.StatusFailed {
		level = slog.LevelError
	}
	s.log.Log(ctx, level, "restore reported",
		"server_id", serverID, "job_id", report.JobID, "status", status,
		"bytes_restored", report.BytesRestored, "applied", report.Applied,
		"archive_path", report.ArchivePath, "error", report.Error)
	return nil
}

// RenewLease extends the lease on work an agent says it is still doing.
//
// The lease exists so that an agent that died does not hold a job for
// ever. It is not a deadline for the work: a large account on a slow link
// can outlast any span that is also short enough to be useful, and a job
// taken back while its agent is still running it is done twice. For a
// restore that writes into a live account, twice is the harm.
//
// The claim token is checked here as it is on a report, and for the same
// reason: only the attempt that holds the job may extend it.
func (s *Service) RenewLease(ctx context.Context, serverID string,
	req protocol.LeaseRenewal) (protocol.LeaseRenewed, error) {

	if !validClaimToken(req.ClaimToken) {
		return protocol.LeaseRenewed{}, fmt.Errorf(
			"controller: lease renewal has no valid claim token")
	}
	renew := s.store.RenewBackupLease
	if req.Restore {
		renew = s.store.RenewRestoreLease
	}
	expires, err := renew(ctx, serverID, req.JobID, req.ClaimToken, s.LeaseDuration)
	if err != nil {
		return protocol.LeaseRenewed{}, err
	}
	return protocol.LeaseRenewed{LeaseExpiresAt: expires}, nil
}

// NextJob leases a backup job for a server and builds the assignment,
// decrypting only the credentials that job actually needs.
func (s *Service) NextJob(ctx context.Context, serverID string) (protocol.JobAssignment, error) {
	claimed, err := s.store.ClaimNextJob(ctx, serverID, s.LeaseDuration)
	if err != nil {
		return protocol.JobAssignment{}, err
	}

	assignment := protocol.JobAssignment{
		JobID:          claimed.JobID,
		ClaimToken:     claimed.ClaimToken,
		AccountID:      claimed.Account.ID,
		CPanelUser:     claimed.Account.CPanelUser,
		SizeEstimate:   uint64(claimed.Account.SizeEstimate),
		PayloadMode:    claimed.Policy.PayloadMode,
		Compression:    claimed.Policy.Compression,
		LimitUploadKiB: claimed.Policy.LimitUploadKiB,
		LeaseExpiresAt: claimed.LeaseExpiresAt,
	}

	for _, target := range claimed.Targets {
		built, err := s.buildTarget(target)
		if err != nil {
			// Failing the whole assignment is deliberate: handing an agent
			// a partial target list would silently reduce the number of
			// copies a policy promises.
			return protocol.JobAssignment{}, fmt.Errorf(
				"controller: build target %s: %w", target.RepositoryID, err)
		}
		assignment.Targets = append(assignment.Targets, built)
	}
	return assignment, nil
}

func (s *Service) buildTarget(target store.ClaimedTarget) (protocol.Target, error) {
	opened, err := repobuild.Open(s.vault, repobuild.Sealed{
		DestinationType:    target.DestinationType,
		DestinationConfig:  target.DestinationConfig,
		CredentialsSealed:  target.CredentialsSealed,
		RepoPath:           target.RepositoryPath,
		RepoPasswordSealed: target.RepoPasswordSealed,
	})
	if err != nil {
		return protocol.Target{}, err
	}
	return protocol.Target{
		RepositoryID: target.RepositoryID,
		Spec:         opened.Spec,
		RepoPath:     opened.Path,
		RepoPassword: opened.Password,
	}, nil
}

// Report records the outcome of a job.
//
// The rolled-up status is computed from stored rows, not taken from the
// agent: an agent must not be able to declare its own job successful.
func (s *Service) Report(ctx context.Context, serverID string, report protocol.JobReport) (job.Status, error) {
	if report.JobID == "" {
		return "", errors.New("controller: report is missing a job id")
	}
	if !validClaimToken(report.ClaimToken) {
		return "", errors.New("controller: report is missing the claim token it was assigned")
	}

	targets := make([]store.TargetReport, 0, len(report.Targets))
	for _, target := range report.Targets {
		status := job.TargetStatus(target.Status)
		switch status {
		case job.TargetSuccess, job.TargetFailed, job.TargetSkipped:
		default:
			return "", fmt.Errorf("controller: target %s has invalid status %q",
				target.RepositoryID, target.Status)
		}
		targets = append(targets, store.TargetReport{
			RepositoryID:   target.RepositoryID,
			Status:         status,
			SnapshotID:     target.SnapshotID,
			BytesAdded:     target.BytesAdded,
			BytesProcessed: target.BytesProcessed,
			DurationSecs:   target.DurationSecs,
			Incomplete:     target.Incomplete,
			Error:          target.Error,
		})
	}

	status, err := s.store.ApplyReport(ctx, serverID, report.JobID, report.ClaimToken, targets,
		store.JobOutcome{
			StagingError: report.StagingError,
			Missing:      report.Missing,
			Warnings:     report.Warnings,
		})
	if err != nil {
		return "", err
	}

	level := slog.LevelInfo
	switch status {
	case job.StatusFailed:
		level = slog.LevelError
	case job.StatusPartialSuccess:
		// Two good copies out of three is a warning, not an incident.
		level = slog.LevelWarn
	}
	s.log.Log(ctx, level, "job reported",
		"server_id", serverID, "job_id", report.JobID, "status", status,
		"targets", len(report.Targets), "staging_error", report.StagingError)
	// One line each, and at a level an operator watching for trouble
	// will see: a job that stored everything but one database is not a
	// failure, and the count alone does not say which database it was.
	for _, omission := range report.Missing {
		s.log.Warn("left out of the backup",
			"server_id", serverID, "job_id", report.JobID, "what", omission)
	}
	for _, warning := range report.Warnings {
		s.log.Warn("this backup may not restore in full",
			"server_id", serverID, "job_id", report.JobID, "why", warning)
	}
	return status, nil
}

// ReclaimLeases returns work whose agent stopped reporting to the queue.
func (s *Service) ReclaimLeases(ctx context.Context) (int, error) {
	backups, err := s.store.ReclaimExpiredLeases(ctx)
	if err != nil {
		return 0, err
	}
	restores, err := s.store.ReclaimExpiredRestoreLeases(ctx)
	if err != nil {
		return backups, err
	}
	if total := backups + restores; total > 0 {
		s.log.Warn("reclaimed expired leases", "backups", backups, "restores", restores)
		return total, nil
	}
	return 0, nil
}
