package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/shukiv/gniza/internal/job"
)

// ErrNoWork means no job is ready for this agent right now.
var ErrNoWork = errors.New("store: no work available")

// CreateJob queues a backup of one account under one policy, with a target
// row per repository the policy writes to.
//
// It returns ErrNoWork if the account already has an open job for this
// policy: a slow nightly run must not be joined by the next tick's copy.
// An account on two policies may have two jobs queued; ClaimNextJob then
// runs them one at a time.
func (s *Store) CreateJob(ctx context.Context, accountID, policyID string) (string, error) {
	var jobID string
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var existing int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM backup_jobs
			 WHERE account_id = $1 AND policy_id = $2
			   AND status IN ('pending', 'running')`, accountID, policyID).Scan(&existing); err != nil {
			return fmt.Errorf("store: count open jobs: %w", err)
		}
		if existing > 0 {
			return ErrNoWork
		}

		if err := tx.QueryRow(ctx, `
			INSERT INTO backup_jobs (id, account_id, policy_id, status)
			VALUES (gen_random_uuid(), $1, $2, 'pending')
			RETURNING id::text`, accountID, policyID).Scan(&jobID); err != nil {
			return fmt.Errorf("store: create job: %w", err)
		}

		tag, err := tx.Exec(ctx, `
			INSERT INTO backup_job_targets (id, job_id, repository_id)
			SELECT gen_random_uuid(), $1, pr.repository_id
			  FROM policy_repositories pr
			 WHERE pr.policy_id = $2`, jobID, policyID)
		if err != nil {
			return fmt.Errorf("store: create job targets: %w", err)
		}
		if tag.RowsAffected() == 0 {
			// A policy with no repositories would produce a job that can
			// only ever roll up to "failed". Refuse it at creation so the
			// misconfiguration surfaces immediately.
			return fmt.Errorf("store: policy %s has no repositories", policyID)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return jobID, nil
}

// ClaimNextJob leases one pending job for a server.
//
// The row is selected FOR UPDATE SKIP LOCKED so several controller
// instances can claim concurrently without blocking each other or handing
// the same job to two agents.
func (s *Store) ClaimNextJob(ctx context.Context, serverID string, lease time.Duration) (ClaimedJob, error) {
	var claimed ClaimedJob
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var running, limit int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FILTER (WHERE bj.status = 'running'),
			       max(sv.max_concurrency)
			  FROM servers sv
			  LEFT JOIN accounts a ON a.server_id = sv.id
			  LEFT JOIN backup_jobs bj ON bj.account_id = a.id AND bj.status = 'running'
			 WHERE sv.id = $1`, serverID).Scan(&running, &limit); err != nil {
			return fmt.Errorf("store: count running jobs: %w", err)
		}
		if limit > 0 && running >= limit {
			// The agent stages a full account copy per job; more at once
			// than the operator sized the volume for fills the disk.
			return ErrNoWork
		}

		row := tx.QueryRow(ctx, `
			UPDATE backup_jobs SET
			    status = 'running',
			    started_at = coalesce(started_at, now()),
			    lease_expires_at = now() + $2::interval,
			    claim_token = gen_random_uuid()
			 WHERE id = (
			     SELECT bj.id
			       FROM backup_jobs bj
			       JOIN accounts a ON a.id = bj.account_id
			      WHERE a.server_id = $1 AND bj.status = 'pending'
			        -- One running job per account, whatever the policy.
			        -- The agent stages an account under its own name so
			        -- that snapshot paths repeat between runs, so two
			        -- concurrent jobs for one account would collide on
			        -- the same staging directory.
			        AND NOT EXISTS (
			            SELECT 1 FROM backup_jobs other
			             WHERE other.account_id = bj.account_id
			               AND other.status = 'running')
			      ORDER BY bj.created_at
			      FOR UPDATE OF bj SKIP LOCKED
			      LIMIT 1)
			 RETURNING id::text, claim_token::text, lease_expires_at,
			           account_id::text, policy_id::text`,
			serverID, lease.String())

		var accountID, policyID string
		err := row.Scan(&claimed.JobID, &claimed.ClaimToken, &claimed.LeaseExpiresAt,
			&accountID, &policyID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoWork
		}
		if err != nil {
			return fmt.Errorf("store: claim job: %w", err)
		}

		if err := tx.QueryRow(ctx, `
			SELECT id::text, server_id::text, cpanel_user, coalesce(primary_domain, ''),
			       coalesce(size_estimate, 0), active
			  FROM accounts WHERE id = $1`, accountID).Scan(
			&claimed.Account.ID, &claimed.Account.ServerID, &claimed.Account.CPanelUser,
			&claimed.Account.PrimaryDomain, &claimed.Account.SizeEstimate,
			&claimed.Account.Active); err != nil {
			return fmt.Errorf("store: read claimed account: %w", err)
		}

		var retention []byte
		if err := tx.QueryRow(ctx, `
			SELECT id::text, name, schedule_cron, payload_mode::text, retention,
			       compression, limit_upload_kib
			  FROM policies WHERE id = $1`, policyID).Scan(
			&claimed.Policy.ID, &claimed.Policy.Name, &claimed.Policy.ScheduleCron,
			&claimed.Policy.PayloadMode, &retention, &claimed.Policy.Compression,
			&claimed.Policy.LimitUploadKiB); err != nil {
			return fmt.Errorf("store: read claimed policy: %w", err)
		}
		if len(retention) > 0 {
			if err := json.Unmarshal(retention, &claimed.Policy.Retention); err != nil {
				return fmt.Errorf("store: decode retention: %w", err)
			}
		}

		targets, err := claimTargets(ctx, tx, claimed.JobID)
		if err != nil {
			return err
		}
		claimed.Targets = targets
		return nil
	})
	if err != nil {
		return ClaimedJob{}, err
	}
	return claimed, nil
}

// claimTargets loads the repositories a job must write to, together with
// the still-sealed credentials for each. Decryption happens one layer up,
// so nothing here can log a plaintext secret.
func claimTargets(ctx context.Context, tx pgx.Tx, jobID string) ([]ClaimedTarget, error) {
	rows, err := tx.Query(ctx, `
		SELECT t.repository_id::text, r.path, d.type::text, d.config,
		       coalesce(cs.ciphertext, ''::bytea), ps.ciphertext, t.attempt
		  FROM backup_job_targets t
		  JOIN repositories r  ON r.id = t.repository_id
		  JOIN destinations d  ON d.id = r.destination_id
		  JOIN secrets ps      ON ps.id = r.password_secret_id
		  LEFT JOIN secrets cs ON cs.id = d.credentials_secret_id
		 WHERE t.job_id = $1 AND t.status IN ('pending', 'failed')
		 ORDER BY d.name, r.path`, jobID)
	if err != nil {
		return nil, fmt.Errorf("store: read job targets: %w", err)
	}
	defer rows.Close()

	var targets []ClaimedTarget
	for rows.Next() {
		var target ClaimedTarget
		if err := rows.Scan(&target.RepositoryID, &target.RepositoryPath,
			&target.DestinationType, &target.DestinationConfig,
			&target.CredentialsSealed, &target.RepoPasswordSealed,
			&target.Attempt); err != nil {
			return nil, fmt.Errorf("store: scan job target: %w", err)
		}
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("store: job %s has no outstanding targets", jobID)
	}
	return targets, nil
}

// TargetReport is one repository's outcome as reported by an agent.
type TargetReport struct {
	RepositoryID   string
	Status         job.TargetStatus
	SnapshotID     string
	BytesAdded     uint64
	BytesProcessed uint64
	DurationSecs   float64
	Incomplete     bool
	Error          string
}

// JobOutcome is what an agent says about the run as a whole, as opposed
// to about one repository.
type JobOutcome struct {
	// StagingError describes a failure before any target was attempted,
	// such as insufficient disk for pkgacct.
	StagingError string
	// Missing is what the payload could not include, one line each.
	Missing []string
	// Warnings is what it holds and a restore may not put back.
	Warnings []string
}

// JobNotes is what a finished job recorded about its payload.
type JobNotes struct {
	Missing  []string
	Warnings []string
}

// ApplyReport records target outcomes and rolls the job up.
//
// The rollup is computed here from the stored rows rather than taken from
// the agent: a compromised or buggy agent must not be able to declare a
// job successful.
func (s *Store) ApplyReport(ctx context.Context, serverID, jobID, claimToken string, reports []TargetReport, outcome JobOutcome) (job.Status, error) {
	stagingError := outcome.StagingError
	var status job.Status
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		// Whose job this is, and whether that server is running it now.
		// An agent's certificate says which server it is; without this,
		// the job id was the only thing that decided whose results these
		// were, and a job id is not a secret one server keeps from
		// another.
		if err := jobIsRunningOn(ctx, tx, serverID, jobID, claimToken); err != nil {
			return err
		}
		for _, report := range reports {
			_, err := tx.Exec(ctx, `
				UPDATE backup_job_targets SET
				    status = $3::job_target_status,
				    snapshot_id = nullif($4, ''),
				    bytes_added = $5,
				    bytes_processed = $6,
				    duration_seconds = $7,
				    incomplete = $8,
				    error = nullif($9, ''),
				    attempt = attempt + 1
				 WHERE job_id = $1 AND repository_id = $2`,
				jobID, report.RepositoryID, string(report.Status), report.SnapshotID,
				int64(report.BytesAdded), int64(report.BytesProcessed),
				report.DurationSecs, report.Incomplete, report.Error)
			if err != nil {
				return fmt.Errorf("store: update job target: %w", err)
			}
		}

		if stagingError != "" {
			// Nothing was uploaded, so every target that never ran fails
			// with the staging error rather than being left pending.
			if _, err := tx.Exec(ctx, `
				UPDATE backup_job_targets
				   SET status = 'failed', error = $2, attempt = attempt + 1
				 WHERE job_id = $1 AND status IN ('pending', 'running')`,
				jobID, stagingError); err != nil {
				return fmt.Errorf("store: fail targets after staging error: %w", err)
			}
		}

		stored, err := targetStatuses(ctx, tx, jobID)
		if err != nil {
			return err
		}
		status = job.Rollup(stored)

		// The attempt that just reported replaces what an earlier one
		// said rather than adding to it: a retry that found the database
		// again must not leave the account still listed as missing it.
		if _, err := tx.Exec(ctx, `
			UPDATE backup_jobs SET
			    status = $2::job_status,
			    lease_expires_at = NULL,
			    claim_token = NULL,
			    missing = coalesce($4::text[], '{}'),
			    warnings = coalesce($5::text[], '{}'),
			    finished_at = CASE WHEN $3 THEN now() ELSE finished_at END
			 WHERE id = $1`, jobID, string(status), status.Terminal(),
			outcome.Missing, outcome.Warnings); err != nil {
			return fmt.Errorf("store: update job status: %w", err)
		}
		return nil
	})
	return status, err
}

// jobIsRunningOn checks that a backup job belongs to a server and is
// leased to it now, and locks the row for the rest of the transaction.
//
// A job that has been reclaimed, finished, or never claimed is not one
// this server is entitled to report on: an outcome for it would be a
// result nobody produced. The claim token narrows that to the attempt
// that is running: the same server can hold two attempts at one job over
// time, and the earlier one's result is not the current one's.
func jobIsRunningOn(ctx context.Context, tx pgx.Tx, serverID, jobID, claimToken string) error {
	var found string
	err := tx.QueryRow(ctx, `
		SELECT bj.id::text
		  FROM backup_jobs bj
		  JOIN accounts a ON a.id = bj.account_id
		 WHERE bj.id = $1 AND a.server_id = $2 AND bj.status = 'running'
		   AND bj.claim_token = $3::uuid
		 FOR UPDATE OF bj`, jobID, serverID, claimToken).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: check who owns job %s: %w", jobID, err)
	}
	return nil
}

func targetStatuses(ctx context.Context, tx pgx.Tx, jobID string) ([]job.TargetResult, error) {
	rows, err := tx.Query(ctx,
		`SELECT status::text, incomplete FROM backup_job_targets WHERE job_id = $1`, jobID)
	if err != nil {
		return nil, fmt.Errorf("store: read target statuses: %w", err)
	}
	defer rows.Close()

	var results []job.TargetResult
	for rows.Next() {
		var status string
		var incomplete bool
		if err := rows.Scan(&status, &incomplete); err != nil {
			return nil, fmt.Errorf("store: scan target status: %w", err)
		}
		results = append(results, job.TargetResult{Status: job.TargetStatus(status), Incomplete: incomplete})
	}
	return results, rows.Err()
}

// ReclaimExpiredLeases returns jobs whose agent stopped reporting to the
// pending queue.
//
// Restic tolerates a partially written repository: the orphaned pack data
// is unreferenced and a later prune removes it. So a reclaimed job can
// simply be retried.
func (s *Store) ReclaimExpiredLeases(ctx context.Context) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE backup_jobs
		   SET status = 'pending', lease_expires_at = NULL, claim_token = NULL
		 WHERE status = 'running'
		   AND lease_expires_at IS NOT NULL
		   AND lease_expires_at < now()`)
	if err != nil {
		return 0, fmt.Errorf("store: reclaim leases: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// JobStatus reads a job's current rolled-up status.
func (s *Store) JobStatus(ctx context.Context, jobID string) (job.Status, error) {
	var status string
	err := s.pool.QueryRow(ctx,
		`SELECT status::text FROM backup_jobs WHERE id = $1`, jobID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: read job status: %w", err)
	}
	return job.Status(status), nil
}

// JobNotes reads what a job recorded about its payload: what the backup
// left out, and what it holds that a restore may not put back.
func (s *Store) JobNotes(ctx context.Context, jobID string) (JobNotes, error) {
	var notes JobNotes
	err := s.pool.QueryRow(ctx,
		`SELECT missing, warnings FROM backup_jobs WHERE id = $1`, jobID).
		Scan(&notes.Missing, &notes.Warnings)
	if errors.Is(err, pgx.ErrNoRows) {
		return notes, ErrNotFound
	}
	if err != nil {
		return notes, fmt.Errorf("store: read job notes: %w", err)
	}
	return notes, nil
}

// JobTargets reads the recorded outcome of every target on a job.
func (s *Store) JobTargets(ctx context.Context, jobID string) ([]job.TargetResult, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT repository_id::text, status::text, coalesce(snapshot_id, ''),
		       coalesce(bytes_added, 0), coalesce(bytes_processed, 0),
		       attempt, incomplete, coalesce(error, '')
		  FROM backup_job_targets WHERE job_id = $1 ORDER BY repository_id`, jobID)
	if err != nil {
		return nil, fmt.Errorf("store: read job targets: %w", err)
	}
	defer rows.Close()

	var results []job.TargetResult
	for rows.Next() {
		var (
			result                 job.TargetResult
			status                 string
			bytesAdded, bytesProcd int64
		)
		if err := rows.Scan(&result.RepositoryID, &status, &result.SnapshotID,
			&bytesAdded, &bytesProcd, &result.Attempt, &result.Incomplete,
			&result.Err); err != nil {
			return nil, fmt.Errorf("store: scan job target: %w", err)
		}
		result.Status = job.TargetStatus(status)
		result.BytesAdded = uint64(bytesAdded)
		result.BytesProcessed = uint64(bytesProcd)
		results = append(results, result)
	}
	return results, rows.Err()
}

// IncompleteSnapshotIDs preserves known failures from before completion
// receipts were introduced. This does not infer success from missing history.
func (s *Store) IncompleteSnapshotIDs(ctx context.Context, repositoryID string) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT snapshot_id FROM backup_job_targets WHERE repository_id = $1 AND incomplete AND snapshot_id IS NOT NULL`, repositoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if id != "" {
			ids[id] = true
		}
	}
	return ids, rows.Err()
}
