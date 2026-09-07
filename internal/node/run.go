package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/shukiv/gniza/internal/cpanel"
	"github.com/shukiv/gniza/internal/destination"
	"github.com/shukiv/gniza/internal/granular"
	"github.com/shukiv/gniza/internal/human"
	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/notify"
	"github.com/shukiv/gniza/internal/protocol"
	"github.com/shukiv/gniza/internal/reassemble"
	"github.com/shukiv/gniza/internal/resticrun"
	"github.com/shukiv/gniza/internal/vault"
)

// EnsureProvisioned creates any repository that does not exist yet on its
// destination.
//
// The first repository is initialised normally; every later one copies its
// chunker parameters, because those are fixed at creation and cannot be
// changed afterwards. See docs/DESIGN.md §7.
func (e *Engine) EnsureProvisioned(ctx context.Context) (int, error) {
	repos, err := e.store.Repositories()
	if err != nil {
		return 0, err
	}

	var created int
	for _, repo := range repos {
		if repo.InitialisedAt != nil {
			continue
		}
		// Initialisation only creates repository data, which an append-only
		// endpoint permits. The delete-capable endpoint is deliberately not
		// used for normal source-server work.
		target, err := e.OpenRepository(repo.ID, false)
		if err != nil {
			return created, err
		}

		var source *resticrun.Repository
		if repo.ChunkerSourceRepoID != "" {
			opened, err := e.OpenRepository(repo.ChunkerSourceRepoID, false)
			if err != nil {
				return created, fmt.Errorf("node: open chunker source: %w", err)
			}
			source = &opened
		}
		if err := e.runner.Init(ctx, target, source); err != nil {
			if repositoryAlreadyThere(err) {
				// The password for that repository was made a moment ago
				// and is not the one it was created with, so there is
				// nothing to do here but say so. Attaching it is a
				// different operation with a different question: what is
				// its password.
				return created, fmt.Errorf(
					"there is already a repository at %s. Gniza did not make it and "+
						"cannot read it with the password it made for this destination. "+
						"If those are this server's earlier backups, attach them under "+
						"Restore -> Disaster recovery, which asks for the password they "+
						"were made with. If they are not, give this destination a folder "+
						"of its own",
					repo.Path)
			}
			return created, err
		}
		if err := e.store.MarkRepositoryInitialised(repo.ID); err != nil {
			return created, err
		}
		e.log.Info("repository created",
			"repository_id", repo.ID, "path", repo.Path,
			"chunker_source", repo.ChunkerSourceRepoID)
		created++
	}
	return created, nil
}

// repositoryAlreadyThere reports whether restic refused to create a
// repository because one is already at that path.
//
// restic says "create repository at ... failed: config file already
// exists" and exits 1, the same exit code it uses for everything else, so
// the sentence is what there is to go on.
func repositoryAlreadyThere(err error) bool {
	return err != nil && strings.Contains(err.Error(), "config file already exists")
}

// QueueBackup queues a backup of one account under a policy.
// QueueSystemBackup queues the server's own configuration, under the name
// it is stored as. It is queued like any account, so it goes to the same
// destinations, with the same retention and the same reporting.
func (e *Engine) QueueSystemBackup(policyID string) (nodestore.Job, error) {
	return e.QueueBackup(policyID, cpanel.SystemAccount)
}

func (e *Engine) QueueBackup(policyID, account string) (nodestore.Job, error) {
	e.workMu.Lock()
	defer e.workMu.Unlock()
	return e.queueBackup(policyID, account)
}

func (e *Engine) queueBackup(policyID, account string) (nodestore.Job, error) {
	running, err := e.store.RunningJobFor(account)
	if err != nil {
		return nodestore.Job{}, err
	}
	if running {
		// Two runs for one account would stage in the same directory.
		return nodestore.Job{}, fmt.Errorf("node: account %s already has work in flight", account)
	}
	return e.store.PutJob(nodestore.Job{
		PolicyID: policyID, Account: account, Status: job.StatusPending,
	})
}

// QueueCoverageRepair re-runs an enabled policy that actually covers the
// account. It rejects stale or edited forms that name an unrelated policy.
func (e *Engine) QueueCoverageRepair(policyID, account string) (nodestore.Job, error) {
	policy, err := e.store.Policy(policyID)
	if err != nil {
		return nodestore.Job{}, err
	}
	if !policy.Enabled || len(policy.RepositoryIDs) == 0 {
		return nodestore.Job{}, errors.New("node: that schedule is not enabled with a destination")
	}
	covered := policy.AllAccounts()
	for _, selected := range policy.Accounts {
		if selected == account {
			covered = true
			break
		}
	}
	if !covered {
		return nodestore.Job{}, fmt.Errorf("node: schedule %q does not cover %s", policy.Name, account)
	}
	return e.QueueBackup(policy.ID, account)
}

// QueueRestore queues a restore.
func (e *Engine) QueueRestore(restore nodestore.Restore) (nodestore.Restore, error) {
	if restore.SnapshotID == "" {
		return nodestore.Restore{}, errors.New("node: restore needs a snapshot")
	}
	// Which account this belongs to, and not merely which name. What it
	// produces may sit on this server for days, and the name may be
	// somebody else's by then.
	if restore.AccountSince.IsZero() {
		restore.AccountSince = e.AccountSince(restore.Account)
	}
	if restore.Kind == "" {
		restore.Kind = protocol.RestoreAccount
	}
	e.workMu.Lock()
	defer e.workMu.Unlock()
	if restore.Kind == KindVerify {
		// A rehearsal keeps nothing and applies nothing, so the checks
		// that follow do not apply to it.
		running, err := e.store.RunningJobFor(restore.Account)
		if err != nil {
			return nodestore.Restore{}, err
		}
		if running {
			return nodestore.Restore{}, fmt.Errorf(
				"node: account %s already has work in flight", restore.Account)
		}
		return e.store.PutRestore(restore)
	}
	if restore.Kind == protocol.RestoreFiles && len(restore.IncludePaths) == 0 {
		return nodestore.Restore{}, errors.New("node: a files restore needs at least one path")
	}
	// Applying means writing into the live account. A whole account goes
	// through cPanel's own restore; a single part of one is written back
	// only for the parts that can be, which granular decides. Nothing else
	// may carry the flag: a request that reached here asking to apply a
	// zone file or the account's configuration is refused, not downgraded.
	if restore.Apply {
		switch restore.Kind {
		case protocol.RestoreAccount:
		case protocol.RestoreItems:
			// Every part of a basket, not merely the first. One item
			// that cannot be written back makes the whole request a
			// copy to download, because applying the rest and leaving
			// that one out is not what was asked for.
			for _, selection := range restore.Selections() {
				if !granular.Kind(selection.Kind).CanApply() {
					return nodestore.Restore{}, fmt.Errorf(
						"node: a %s restore cannot be written into the live account",
						selection.Kind)
				}
			}
		default:
			return nodestore.Restore{}, errors.New(
				"node: only a whole-account restore can be applied")
		}
	}

	running, err := e.store.RunningJobFor(restore.Account)
	if err != nil {
		return nodestore.Restore{}, err
	}
	if running {
		return nodestore.Restore{}, fmt.Errorf(
			"node: account %s already has work in flight", restore.Account)
	}
	return e.store.PutRestore(restore)
}

// RunOnce performs the oldest piece of pending work, if any, and reports
// whether it did anything.
func (e *Engine) RunOnce(ctx context.Context) (bool, error) {
	restore, backup, err := e.store.PendingWork()
	if err != nil {
		return false, err
	}
	switch {
	case restore != nil:
		return true, e.runRestore(ctx, *restore)
	case backup != nil:
		return true, e.runBackup(ctx, *backup)
	default:
		return false, nil
	}
}

func (e *Engine) runBackup(ctx context.Context, stored nodestore.Job) error {
	// A destination whose repository was never created is not a
	// destination yet. It is created when one is added, but only on the
	// branch where the login was proved in the same request -- a host key
	// confirmed in a second step, or a login that came back with a
	// warning, left the destination saved and the repository missing, and
	// nothing made it again until the service restarted. Between those
	// two, every backup failed with restic's own words for it:
	//
	//	Fatal: repository does not exist: unable to open config file
	//
	// Making it here costs one store read on a run that has nothing to
	// create, and the alternative is a server that says it is backing up
	// and is not.
	e.log.Debug("backup starting",
		"job_id", stored.ID, "account", stored.Account, "policy_id", stored.PolicyID)
	if created, err := e.EnsureProvisioned(ctx); err != nil {
		e.log.Error("create a repository that is not there yet",
			"job_id", stored.ID, "account", stored.Account, "error", err)
	} else if created > 0 {
		e.log.Warn("created a repository that had not been created when its "+
			"destination was added", "count", created)
	}

	policy, err := e.store.Policy(stored.PolicyID)
	if err != nil {
		return e.failJob(stored, fmt.Sprintf("policy: %v", err))
	}
	var account cpanel.AccountInfo
	if stored.Account == cpanel.SystemAccount {
		// Not an account: cPanel has never heard of it, and the worker
		// knows to stage the server's configuration instead.
		account = cpanel.AccountInfo{User: cpanel.SystemAccount}
	} else {
		account, err = e.provider.Account(ctx, stored.Account)
		if err != nil {
			return e.failJob(stored, fmt.Sprintf("account: %v", err))
		}
		// Which unix account this name means, recorded as it is seen. A
		// name that has changed hands since the last backup is a
		// different customer, and this is where that is noticed.
		if _, err := e.noteIdentity(stored.Account); err != nil {
			e.log.Warn("record which account this name means",
				"account", stored.Account, "error", err)
		}
	}
	assignment, err := e.assignmentFor(stored, policy, account)
	if err != nil {
		return e.failJob(stored, err.Error())
	}
	stored.CompleteAccount = stored.Account != cpanel.SystemAccount &&
		!policy.SkipHomedir && !policy.SkipDatabases && !policy.SkipEmail

	now := time.Now().UTC()
	stored.Status = job.StatusRunning
	stored.StartedAt = &now
	if _, err := e.store.PutJob(stored); err != nil {
		return err
	}
	e.Notify(ctx, notify.Message{
		Event: notify.EventStarted, Account: stored.Account,
		Subject: fmt.Sprintf("Backing up %s", stored.Account),
		Body:    "The backup has started. Another message follows when it finishes.",
	})

	report := e.worker.RunJob(ctx, assignment)

	stored.StagingErr = report.StagingError
	stored.Missing = report.Missing
	if len(report.Missing) > 0 {
		// Something the account has is not in this backup. It is worth
		// keeping and worth reporting, but it cannot authorise deleting
		// the account it is a backup of.
		stored.CompleteAccount = false
	}
	stored.Targets = nil
	results := make([]job.TargetResult, 0, len(report.Targets))
	for _, target := range report.Targets {
		stored.Targets = append(stored.Targets, nodestore.JobTarget{
			RepositoryID:   target.RepositoryID,
			Status:         job.TargetStatus(target.Status),
			SnapshotID:     target.SnapshotID,
			BytesAdded:     target.BytesAdded,
			BytesProcessed: target.BytesProcessed,
			DurationSecs:   target.DurationSecs,
			Incomplete:     target.Incomplete,
			Error:          target.Error,
			Detail:         target.Detail,
		})
		results = append(results, job.TargetResult{Status: job.TargetStatus(target.Status), Incomplete: target.Incomplete})
	}
	if report.StagingError != "" {
		// Nothing was uploaded, so no target holds a copy.
		for i := range stored.Targets {
			stored.Targets[i].Status = job.TargetFailed
			stored.Targets[i].Error = report.StagingError
		}
		results = nil
		for _, repositoryID := range policy.RepositoryIDs {
			_ = repositoryID
			results = append(results, job.TargetResult{Status: job.TargetFailed})
		}
	}

	stored.Status = job.Rollup(results)
	// A percentage on a finished job says nothing, and "100%" beside a
	// failure would be a lie.
	stored.Progress = nil
	e.forgetProgress(stored.ID)
	finished := time.Now().UTC()
	stored.FinishedAt = &finished
	if _, err := e.store.PutJob(stored); err != nil {
		return err
	}
	e.log.Info("backup finished",
		"job_id", stored.ID, "account", stored.Account, "status", stored.Status)
	for _, target := range stored.Targets {
		e.log.Debug("backup copy",
			"job_id", stored.ID, "account", stored.Account,
			"repository_id", target.RepositoryID, "status", target.Status,
			"snapshot", target.SnapshotID, "read", target.BytesProcessed,
			"stored", target.BytesAdded, "seconds", target.DurationSecs,
			"error", target.Error)
	}
	e.notifyBackup(ctx, stored)
	return nil
}

// notifyBackup says what a finished backup came to, for whoever asked to
// be told. A backup that fails silently is the same as no backup.
func (e *Engine) notifyBackup(ctx context.Context, stored nodestore.Job) {
	message, send := backupMessage(stored)
	if !send {
		return
	}
	message.Body = backupDetail(stored, e.destinationNames())
	e.Notify(ctx, message)
}

// backupMessage is what a finished run is announced as, without the body,
// which needs the engine to name destinations.
//
// A run that stored everything it was asked for and a run that could not
// take one of the account's databases are both successes as far as the
// targets go, and they must not read the same. The second is announced as
// what it is, every time it happens: nobody goes looking in a job detail
// for a hole they were never told about.
func backupMessage(stored nodestore.Job) (notify.Message, bool) {
	message := notify.Message{Account: stored.Account}
	switch stored.Status {
	case job.StatusSuccess:
		message.Event = notify.EventBackupSucceeded
		message.Subject = fmt.Sprintf("Backed up %s", stored.Account)
	case job.StatusPartialSuccess:
		message.Event = notify.EventBackupPartial
		message.Subject = fmt.Sprintf("Backed up %s, with files it could not read", stored.Account)
	case job.StatusFailed:
		message.Event = notify.EventBackupFailed
		message.Subject = fmt.Sprintf("The backup of %s failed", stored.Account)
	default:
		return notify.Message{}, false
	}
	if len(stored.Missing) > 0 && stored.Status != job.StatusFailed {
		message.Event = notify.EventBackupPartial
		message.Subject = fmt.Sprintf("Backed up %s, without %s",
			stored.Account, countedThings(len(stored.Missing)))
	}
	return message, true
}

// countedThings names how much a backup is short by, for a subject line.
func countedThings(n int) string {
	if n == 1 {
		return "one thing it could not take"
	}
	return fmt.Sprintf("%d things it could not take", n)
}

// destinationNames maps each repository to the destination an operator
// named it, so a message says "Backup server" rather than the first eight
// characters of an identifier nobody has seen before.
func (e *Engine) destinationNames() map[string]string {
	names := map[string]string{}
	destinations, err := e.store.Destinations()
	if err != nil {
		e.log.Error("read destinations", "error", err)
		return names
	}
	byID := map[string]string{}
	for _, dest := range destinations {
		byID[dest.ID] = dest.Name
	}
	repos, err := e.store.Repositories()
	if err != nil {
		e.log.Error("read repositories", "error", err)
		return names
	}
	for _, repo := range repos {
		names[repo.ID] = byID[repo.DestinationID]
	}
	return names
}

// backupDetail is what the message says under its first line: what went
// wrong, or what it cost — per destination, because a backup that reached
// one of two is a different situation from one that reached both.
func backupDetail(stored nodestore.Job, names map[string]string) string {
	if stored.StagingErr != "" {
		return stored.StagingErr
	}
	var lines []string
	// First, because it is the part somebody has to act on. Everything
	// below it is about where the backup went, which is routine.
	for _, missing := range stored.Missing {
		lines = append(lines, "Not in this backup -- "+missing)
	}
	for _, target := range stored.Targets {
		where := names[target.RepositoryID]
		if where == "" {
			where = short(target.RepositoryID)
		}
		switch {
		case target.Error != "":
			lines = append(lines, fmt.Sprintf("%s: %s", where, target.Error))
		case target.Incomplete:
			lines = append(lines, fmt.Sprintf(
				"%s: stored, but some files could not be read", where))
		default:
			lines = append(lines, fmt.Sprintf("%s: %s stored, out of %s read",
				where, human.Bytes(target.BytesAdded), human.Bytes(target.BytesProcessed)))
		}
	}
	if stored.StartedAt != nil && stored.FinishedAt != nil {
		lines = append(lines, fmt.Sprintf("It took %s.",
			roughly(stored.FinishedAt.Sub(*stored.StartedAt))))
	}
	return strings.Join(lines, "\n")
}

// short is an id an operator can recognise without being a whole line.
func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func (e *Engine) failJob(stored nodestore.Job, reason string) error {
	stored.Status = job.StatusFailed
	stored.Progress = nil
	e.forgetProgress(stored.ID)
	stored.StagingErr = reason
	finished := time.Now().UTC()
	stored.FinishedAt = &finished
	e.log.Error("backup failed before starting",
		"job_id", stored.ID, "account", stored.Account, "error", reason)
	_, err := e.store.PutJob(stored)
	return err
}

func (e *Engine) runRestore(ctx context.Context, stored nodestore.Restore) error {
	if stored.Kind == KindVerify {
		return e.runDrill(ctx, stored)
	}
	// Asked for by one account, run for whoever holds the name now: a
	// restore queued before the account was removed and recreated is not
	// this customer's, and neither would be what it produced.
	if !e.BelongsToCurrentHolder(stored) {
		return e.failRestore(stored,
			"the account was removed and made again after this was asked for; "+
				"nothing was restored")
	}

	target, err := e.targetFor(stored.RepositoryID)
	if err != nil {
		return e.failRestore(stored, err.Error())
	}
	var account cpanel.AccountInfo
	if stored.Account == cpanel.SystemAccount {
		// The server's own settings are not an account, and asking cPanel
		// about them fails on the name alone.
		account = cpanel.AccountInfo{User: cpanel.SystemAccount}
	} else if found, lookupErr := e.provider.Account(ctx, stored.Account); lookupErr == nil {
		account = found
	} else {
		// An account that is not on this server is the case this whole
		// program exists for: the machine was lost and is being rebuilt.
		// Refusing to restore one because it is not already here made
		// recovery impossible on the only server that ever needs it.
		//
		// The lookup gives the size estimate and nothing else, so
		// without it the restore proceeds and the estimate is whatever
		// the snapshot turns out to hold.
		e.log.Info("restoring an account this server does not have",
			"account", stored.Account, "detail", lookupErr)
		account = cpanel.AccountInfo{User: stored.Account}
	}

	// The live account is not an estimate of a historical backup. It may
	// have been deleted, emptied after an incident, or simply grown smaller
	// since the requested snapshot. Size the root-owned restore workspace
	// from the snapshot as well, and fail before writing if it cannot even
	// be identified as this account's backup.
	wholeAccountApply := stored.Apply && (stored.Kind == protocol.RestoreAccount || stored.Kind == "")
	snapshotBytes, err := e.snapshotBytes(ctx, stored.RepositoryID, stored.Account, stored.SnapshotID, wholeAccountApply)
	if err != nil {
		return e.failRestore(stored, err.Error())
	}
	account.SizeBytes = restoreStagingEstimate(stored.Kind, account.SizeBytes, snapshotBytes)

	now := time.Now().UTC()
	stored.Status = job.StatusRunning
	stored.StartedAt = &now
	if _, err := e.store.PutRestore(stored); err != nil {
		return err
	}
	doing := "The restore has started"
	if stored.Apply {
		// The one that replaces live data. An operator who did not
		// expect this message is the person who most needs it.
		doing = "A restore that writes into the live account has started"
	}
	e.Notify(ctx, notify.Message{
		Event: notify.EventStarted, Account: stored.Account,
		Subject: fmt.Sprintf("Restoring %s", stored.Account),
		Body:    doing + ". Another message follows when it finishes.",
	})

	report := e.worker.RunRestore(ctx, protocol.RestoreAssignment{
		JobID:        stored.ID,
		AccountID:    stored.Account,
		CPanelUser:   stored.Account,
		SnapshotID:   stored.SnapshotID,
		Kind:         stored.Kind,
		IncludePaths: stored.IncludePaths,
		TargetDir:    stored.TargetDir,
		ItemKind:     stored.ItemKind,
		ItemNames:    stored.ItemNames,
		Items:        restoreSelections(stored),
		Apply:        stored.Apply,
		Unrestricted: stored.Unrestricted,
		Source:       target,
		SizeEstimate: account.SizeBytes,
	})

	stored.Status = job.Status(report.Status)
	// A finished restore carries no progress. A stage and a percentage
	// beside it would read as one still running.
	stored.Progress = nil
	e.forgetProgress(stored.ID)
	stored.BytesRestored = report.BytesRestored
	stored.ArchivePath = report.ArchivePath
	stored.RestoredTo = report.RestoredTo
	if report.Detail != "" {
		stored.Detail = report.Detail
	}
	stored.Hint = report.Hint
	stored.Applied = report.Applied
	stored.Error = report.Error
	finished := time.Now().UTC()
	stored.FinishedAt = &finished
	if _, err := e.store.PutRestore(stored); err != nil {
		return err
	}
	e.log.Info("restore finished",
		"restore_id", stored.ID, "account", stored.Account, "status", stored.Status)
	subject := fmt.Sprintf("Restored %s", stored.Account)
	if stored.Status == job.StatusFailed {
		subject = fmt.Sprintf("The restore of %s failed", stored.Account)
	}
	body := stored.Detail
	if stored.Error != "" {
		body = stored.Error
	}
	e.Notify(ctx, notify.Message{
		Event: notify.EventRestore, Account: stored.Account,
		Subject: subject, Body: body,
	})
	return nil
}

// snapshotBytes returns the amount of source data restic recorded for one
// account snapshot. A short snapshot id is accepted because the restore
// path accepts it too.
func (e *Engine) snapshotBytes(ctx context.Context, repositoryID, account, snapshotID string, requireComplete bool) (uint64, error) {
	snapshots, err := e.Snapshots(ctx, repositoryID, account)
	if err != nil {
		return 0, err
	}
	for _, snapshot := range snapshots {
		if snapshot.ID == snapshotID || snapshot.ShortID == snapshotID {
			if snapshot.Account() != account {
				break
			}
			if requireComplete && !snapshot.Complete() {
				return 0, fmt.Errorf("node: refusing to apply an incomplete account backup: %s", strings.Join(snapshot.Skipped(), ", "))
			}
			repo, err := e.OpenRepository(repositoryID, false)
			if err != nil {
				return 0, err
			}
			return e.runner.RestoreSize(ctx, repo, snapshot)
		}
	}
	return 0, fmt.Errorf("node: snapshot %s does not belong to %s", snapshotID, account)
}

// restoreStagingEstimate accounts for the peak shape of a whole-account
// reassembly. At its fullest, the extracted account tree and the cpmove tar
// built from it coexist. The extra GiB follows cPanel's own pkgacct space
// guidance and gives metadata and filesystem bookkeeping somewhere to go.
func restoreStagingEstimate(_ string, liveBytes, snapshotBytes uint64) uint64 {
	if snapshotBytes == 0 {
		return 0
	}
	return reassemble.StagingBytes(max(liveBytes, snapshotBytes))
}

// runDrill rehearses a restore and records what it proved.
func (e *Engine) runDrill(ctx context.Context, stored nodestore.Restore) error {
	now := time.Now().UTC()
	stored.Status = job.StatusRunning
	stored.StartedAt = &now
	if _, err := e.store.PutRestore(stored); err != nil {
		return err
	}

	checks, skipped, err := e.Drill(ctx, stored.RepositoryID, stored.Account)
	finished := time.Now().UTC()
	stored.FinishedAt = &finished
	// A rehearsal of a backup taken without part of the account proves
	// what that backup holds and nothing about the rest. Recorded so the
	// page can say so rather than show the same tick a whole account gets.
	stored.SkippedParts = skipped
	stored.PartialSource = len(skipped) > 0
	if err != nil {
		stored.Status = job.StatusFailed
		stored.Error = err.Error()
		if len(checks) > 0 {
			stored.Detail = "passed before failing: " + strings.Join(checks, "; ")
		}
		e.log.Error("restore rehearsal failed",
			"account", stored.Account, "error", err, "checks", checks)
	} else {
		stored.Status = job.StatusSuccess
		stored.Detail = strings.Join(checks, "; ")
		e.log.Info("restore rehearsal passed",
			"account", stored.Account, "checks", checks)
	}
	_, storeErr := e.store.PutRestore(stored)
	return storeErr
}

func (e *Engine) failRestore(stored nodestore.Restore, reason string) error {
	stored.Status = job.StatusFailed
	stored.Error = reason
	finished := time.Now().UTC()
	stored.FinishedAt = &finished
	e.log.Error("restore failed before starting", "restore_id", stored.ID, "error", reason)
	_, err := e.store.PutRestore(stored)
	return err
}

// Schedule queues whatever the policies say is due.
//
// A policy that has never run, or has been dormant longer than the catch-up
// window, starts from the edge of that window: a server that was off for a
// week should run tonight's backup, not seven of them.
func (e *Engine) Schedule(ctx context.Context, now time.Time) (int, error) {
	// Cheap — one readdir and a stat per finished output — and it is the
	// only thing that runs regularly on a server that is never restarted.
	if err := e.SweepWorkdir(); err != nil {
		e.log.Error("sweep the work directory", "error", err)
	}
	e.watchForTrouble(ctx, now)
	e.sweepRetention(ctx, now)
	e.sweepDeletedAccounts(ctx, now)
	e.checkForUpdate(ctx, now)
	e.sweepPreparedKeys(now)
	if now.Sub(e.lastReconcile) >= time.Minute {
		if err := e.ReconcileAccounts(ctx); err != nil {
			e.log.Warn("reconcile cPanel accounts", "error", err)
		} else {
			e.lastReconcile = now
		}
	}

	policies, err := e.store.Policies()
	if err != nil {
		return 0, err
	}

	var queued int
	for _, policy := range policies {
		if !policy.Enabled || len(policy.RepositoryIDs) == 0 {
			// Said at debug rather than not at all: "why did nothing run
			// last night" is answered by a schedule that is switched off
			// or has no destination, and neither is visible from a log
			// that only records what did happen.
			e.log.Debug("schedule skipped",
				"policy", policy.Name, "enabled", policy.Enabled,
				"destinations", len(policy.RepositoryIDs))
			continue
		}
		schedule, err := cron.ParseStandard(policy.ScheduleCron)
		if err != nil {
			e.log.Error("policy schedule is not valid",
				"policy", policy.Name, "cron", policy.ScheduleCron, "error", err)
			continue
		}

		last := time.Time{}
		if policy.LastRunAt != nil {
			last = *policy.LastRunAt
		}
		if earliest := now.Add(-catchUpWindow); last.IsZero() || last.Before(earliest) {
			last = earliest
		}
		if next := schedule.Next(last); next.After(now) {
			e.log.Debug("schedule is not due",
				"policy", policy.Name, "cron", policy.ScheduleCron,
				"due", next.UTC().Format(time.RFC3339))
			continue
		}

		accounts, err := e.accountsFor(ctx, policy)
		if err != nil {
			e.log.Error("resolve policy accounts", "policy", policy.Name, "error", err)
			continue
		}
		// The names, not a count: "why did this account not run last
		// night" is answered by whether the schedule resolved to it.
		e.log.Debug("schedule is due",
			"policy", policy.Name, "cron", policy.ScheduleCron,
			"accounts", strings.Join(accounts, ","),
			"include_system", policy.IncludeSystem,
			"destinations", len(policy.RepositoryIDs))
		if err := e.store.SetPolicyLastRun(policy.ID, now); err != nil {
			return queued, err
		}
		if policy.IncludeSystem {
			// The server's own configuration goes first: it is small, and
			// a replacement machine needs it before the accounts on it
			// mean anything.
			accounts = append([]string{cpanel.SystemAccount}, accounts...)
		}
		for _, account := range accounts {
			if _, err := e.QueueBackup(policy.ID, account); err != nil {
				// Usually the previous run for this account is still
				// going, which is a skip rather than a failure.
				e.log.Warn("skipped account", "policy", policy.Name,
					"account", account, "reason", err)
				continue
			}
			queued++
		}
		e.log.Info("policy fired", "policy", policy.Name, "queued", queued)
	}
	return queued, nil
}

// catchUpWindow bounds how far back a restart looks for missed schedules.
const catchUpWindow = 2 * time.Hour

// watchForTrouble raises the two things no run reports, because no run
// happens: an account whose backups have stopped, and a run that started
// and never finished. Each is said once, when it becomes true.
func (e *Engine) watchForTrouble(ctx context.Context, now time.Time) {
	// The scheduler ticks every 15 seconds; nothing here changes that
	// fast, and accountsFor reads the account registry per policy.
	e.alertedMu.Lock()
	if now.Sub(e.lastWatch) < watchEvery {
		e.alertedMu.Unlock()
		return
	}
	e.lastWatch = now
	e.alertedMu.Unlock()

	// Nothing is listening yet. Say nothing rather than marking every
	// current problem as already told, which would leave the operator's
	// first channel silent about the very thing they added it for.
	channels, err := e.store.Channels()
	if err != nil {
		e.log.Error("read notification channels", "error", err)
		return
	}
	listening := false
	for _, channel := range channels {
		if channel.Enabled {
			listening = true
			break
		}
	}
	if !listening {
		return
	}

	e.probeDestinations(ctx, now)

	jobs, err := e.store.Jobs(0)
	if err != nil {
		e.log.Error("read jobs", "error", err)
		return
	}
	policies, err := e.store.Policies()
	if err != nil {
		e.log.Error("read policies", "error", err)
		return
	}

	stuckAfter := 6 * time.Hour
	for _, policy := range policies {
		if policy.AlertRunHours > 0 {
			if asked := time.Duration(policy.AlertRunHours) * time.Hour; asked < stuckAfter {
				stuckAfter = asked
			}
		}
	}

	e.alertedMu.Lock()
	defer e.alertedMu.Unlock()
	if e.alerted == nil {
		e.alerted = map[string]bool{}
	}

	for _, stored := range jobs {
		if stored.Status.Terminal() || stored.StartedAt == nil {
			continue
		}
		key := "stuck:" + stored.ID
		if now.Sub(*stored.StartedAt) < stuckAfter || e.alerted[key] {
			continue
		}
		e.alerted[key] = true
		e.Notify(ctx, notify.Message{
			Event:   notify.EventStuck,
			Account: stored.Account,
			Subject: fmt.Sprintf("The backup of %s has been running for %s",
				stored.Account, roughly(now.Sub(*stored.StartedAt))),
			Body: "A run that long is usually stuck rather than slow. It is holding the " +
				"staging space and the queue behind it.",
		})
	}

	// An account is overdue when its own schedule has come and gone
	// without a good backup, twice over.
	latest := map[string]nodestore.Job{}
	for _, stored := range jobs {
		if stored.Status != job.StatusSuccess {
			continue
		}
		if previous, seen := latest[stored.Account]; !seen || stored.QueuedAt.After(previous.QueuedAt) {
			latest[stored.Account] = stored
		}
	}
	for _, policy := range policies {
		if !policy.Enabled || len(policy.RepositoryIDs) == 0 {
			continue
		}
		schedule, err := cron.ParseStandard(policy.ScheduleCron)
		if err != nil {
			continue
		}
		if policy.AlertNoBackupDays < 0 {
			// The operator said never for this schedule.
			continue
		}
		first := schedule.Next(now)
		overdue := 2 * schedule.Next(first).Sub(first)
		if policy.AlertNoBackupDays > 0 {
			overdue = time.Duration(policy.AlertNoBackupDays) * 24 * time.Hour
		}
		if overdue <= 0 {
			continue
		}

		accounts, err := e.accountsFor(ctx, policy)
		if err != nil {
			continue
		}
		for _, account := range accounts {
			last, backedUp := latest[account]
			if backedUp && now.Sub(last.QueuedAt) < overdue {
				delete(e.alerted, "overdue:"+account)
				continue
			}
			key := "overdue:" + account
			if e.alerted[key] {
				continue
			}
			e.alerted[key] = true
			body := fmt.Sprintf("Nothing has backed it up in the last %s, and %q says it should have.",
				roughly(overdue), policy.Name)
			if !backedUp {
				body = fmt.Sprintf("It has never been backed up successfully, and %q says it should have been.",
					policy.Name)
			}
			e.Notify(ctx, notify.Message{
				Event:   notify.EventOverdue,
				Account: account,
				Subject: fmt.Sprintf("%s has no recent backup", account),
				Body:    body,
			})
		}
	}
}

// probeDestinations reaches each destination on a slow cadence, so that a
// backup server which went away is reported when it goes rather than at
// the next backup — which is the night the operator finds out they have
// none.
func (e *Engine) probeDestinations(ctx context.Context, now time.Time) {
	e.alertedMu.Lock()
	if now.Sub(e.lastProbe) < probeEvery {
		e.alertedMu.Unlock()
		return
	}
	e.lastProbe = now
	e.alertedMu.Unlock()

	destinations, err := e.store.Destinations()
	if err != nil {
		e.log.Error("read destinations", "error", err)
		return
	}
	for _, dest := range destinations {
		// TestDestination records the result and says so if it changed.
		// A probe that succeeded says nothing above debug on purpose: it
		// happens every few minutes, and a log of things that are fine is
		// a log nobody reads. At debug it is how an operator sees the
		// probe is running at all.
		started := time.Now()
		err := e.TestDestination(ctx, dest.ID)
		e.log.Debug("probed a destination", "destination", dest.Name,
			"kind", dest.Type, "took", time.Since(started).Round(time.Millisecond).String(),
			"error", errorText(err))
		if err != nil {
			e.log.Warn("destination unreachable", "destination", dest.Name, "error", err)
		}
	}
}

// roughly says a duration the way a person would, for a sentence rather
// than a table.
func roughly(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d seconds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}

func (e *Engine) accountsFor(ctx context.Context, policy nodestore.Policy) ([]string, error) {
	if !policy.AllAccounts() {
		return policy.Accounts, nil
	}
	// Resolved at run time, so accounts created since the policy was
	// written are backed up without anyone remembering to edit it.
	accounts, err := e.provider.Accounts(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(accounts))
	for _, account := range accounts {
		names = append(names, account.User)
	}
	return names, nil
}

// Run drives the scheduler and the worker until the context is cancelled.
func (e *Engine) Run(ctx context.Context) error {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		if _, err := e.Schedule(ctx, time.Now()); err != nil {
			e.log.Error("scheduler", "error", err)
		}
		// Drain the queue before sleeping, so a policy that queued ten
		// accounts does not take ten ticks to start the second one.
		for {
			did, err := e.RunOnce(ctx)
			if err != nil {
				e.log.Error("run work", "error", err)
				break
			}
			if !did {
				break
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Forget applies a policy's retention to a repository and prunes.
//
// In fleet mode this runs on a separate trusted host. Standalone has no
// such host, so it runs here — which means the credential that can delete
// backups lives on the machine an attacker would compromise. That is the
// trade standalone makes; see docs/adr/0007-standalone-mode.md.
func (e *Engine) Forget(ctx context.Context, repositoryID string, retention nodestore.Retention, prune bool) error {
	repo, err := e.OpenRepository(repositoryID, true)
	if err != nil {
		return err
	}
	incomplete, err := e.incompleteSnapshots(repositoryID)
	if err != nil {
		return err
	}
	return e.runner.Forget(ctx, repo, resticrun.ForgetSpec{
		ProtectedSnapshotIDs: incomplete,
		KeepLast:             retention.KeepLast,
		KeepDaily:            retention.KeepDaily,
		KeepWeekly:           retention.KeepWeekly,
		KeepMonthly:          retention.KeepMonthly,
		KeepYearly:           retention.KeepYearly,
		// A repository holds every account on this server, so retention is
		// per account, and snapshot tags are what identify an account.
		GroupBy: "host,tags",
		Prune:   prune,
	})
}

// Check verifies a repository's integrity.
func (e *Engine) Check(ctx context.Context, repositoryID string, readDataSubsetPercent int) error {
	repo, err := e.OpenRepository(repositoryID, false)
	if err != nil {
		return err
	}
	return e.runner.Check(ctx, repo, resticrun.CheckSpec{
		ReadDataSubsetPercent: readDataSubsetPercent,
	})
}

// Drill rehearses a restore into scratch space and throws the result away.
//
// It never applies anything: the live account is not touched, and nothing
// is kept. What it answers is whether the backup can be turned back into an
// account at all, which is the only question worth asking of a backup
// nightly.
//
// Scratch space is allocated through the staging manager so the same disk
// check that guards a backup guards this too. A rehearsal that filled the
// volume would be worse than no rehearsal.
func (e *Engine) Drill(ctx context.Context, repositoryID, account string) (
	checks []string, skipped []string, err error) {
	repo, err := e.OpenRepository(repositoryID, false)
	if err != nil {
		return nil, nil, err
	}
	snapshots, err := e.Snapshots(ctx, repositoryID, account)
	if err != nil {
		return nil, nil, err
	}
	// The same choice a recovery would make, or the rehearsal answers a
	// question about a snapshot nobody would restore. A partial backup is
	// still rehearsed when it is all there is -- what it does not hold is
	// then part of the answer rather than a silent pass.
	newest, partial, _ := newestForRecovery(snapshots, account, time.Time{})
	if newest.ID == "" {
		newest = partial
	}
	if newest.ID == "" {
		return nil, nil, fmt.Errorf("node: no backup of %s to rehearse", account)
	}
	for _, part := range newest.Skipped() {
		if part == "unverified source reads" {
			return nil, newest.Skipped(), fmt.Errorf("node: the backup's source reads are unverified; a rehearsal cannot certify it as complete")
		}
	}

	sourceBytes, err := e.runner.RestoreSize(ctx, repo, newest)
	if err != nil {
		return nil, nil, err
	}
	dir, err := e.staging.Allocate("drill-"+account, reassemble.StagingBytes(sourceBytes))
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if releaseErr := e.staging.Release(dir); releaseErr != nil {
			e.log.Error("release drill scratch", "path", dir.Path, "error", releaseErr)
		}
	}()

	rebuilt, err := reassemble.Run(ctx, e.runner, reassemble.Request{
		Account:    account,
		SnapshotID: newest.ID,
		WorkDir:    dir.Path,
		Repo:       repo,
	})
	if err != nil {
		return nil, nil, err
	}
	checks, err = reassemble.Verify(rebuilt)
	return checks, rebuilt.Skipped, err
}

// QueueDrill asks for a rehearsal of an account's newest backup.
//
// It goes through the same queue as everything else, so it cannot run
// alongside a backup or restore of the same account and shows up in
// history with what it checked.
func (e *Engine) QueueDrill(ctx context.Context, account string) (nodestore.Restore, error) {
	repositories, err := e.store.Repositories()
	if err != nil {
		return nodestore.Restore{}, err
	}

	var (
		newest     resticrun.Snapshot
		newestRepo string
	)
	for _, repository := range repositories {
		if repository.InitialisedAt == nil {
			continue
		}
		snapshots, err := e.Snapshots(ctx, repository.ID, account)
		if err != nil {
			continue
		}
		for _, snapshot := range snapshots {
			if snapshot.Time.After(newest.Time) {
				newest, newestRepo = snapshot, repository.ID
			}
		}
	}
	if newestRepo == "" {
		return nodestore.Restore{}, fmt.Errorf("node: %s has no backup to rehearse", account)
	}

	return e.QueueRestore(nodestore.Restore{
		Account:      account,
		RepositoryID: newestRepo,
		SnapshotID:   newest.ID,
		Kind:         KindVerify,
	})
}

// AddDestination stores a destination and the repository that will live in
// it, sealing the credentials on the way past.
func (e *Engine) AddDestination(dest nodestore.Destination, secrets map[string]string,
	repositoryPath string) (nodestore.Destination, nodestore.Repository, error) {

	spec := destination.Spec{
		Type: destination.Type(dest.Type), Config: dest.Config, Secrets: secrets,
	}
	// Fail here, with a message an operator can read, rather than at two
	// in the morning on the first scheduled run.
	if _, err := destination.Build(spec); err != nil {
		return nodestore.Destination{}, nodestore.Repository{}, err
	}

	secretID, err := SealCredentials(e.store, e.vault, secrets)
	if err != nil {
		return nodestore.Destination{}, nodestore.Repository{}, err
	}
	dest.CredentialsSecretID = secretID

	stored, err := e.store.PutDestination(dest)
	if err != nil {
		return nodestore.Destination{}, nodestore.Repository{}, err
	}

	repo, err := e.newRepository(stored.ID, repositoryPath)
	if err != nil {
		return nodestore.Destination{}, nodestore.Repository{}, err
	}
	return stored, repo, nil
}

// newRepository creates the repository record for a destination, with its
// own generated password sealed in the vault.
func (e *Engine) newRepository(destinationID, path string) (nodestore.Repository, error) {
	password, err := vault.GenerateMasterKey()
	if err != nil {
		return nodestore.Repository{}, err
	}
	passwordID, err := SealRepositoryPassword(e.store, e.vault, password)
	if err != nil {
		return nodestore.Repository{}, err
	}
	return e.store.PutRepository(nodestore.Repository{
		DestinationID: destinationID, Path: path, PasswordSecretID: passwordID,
	})
}

// RunPolicyNow queues every account a schedule covers, immediately.
//
// It deliberately leaves the schedule's last-fired time alone: running one
// by hand is extra work, not a replacement for tonight's run, and moving
// the marker would skip that.
func (e *Engine) RunPolicyNow(ctx context.Context, policyID string) (queued int, skipped []string, err error) {
	policy, err := e.store.Policy(policyID)
	if err != nil {
		return 0, nil, err
	}
	if len(policy.RepositoryIDs) == 0 {
		return 0, nil, fmt.Errorf(
			"node: %q has no destination, so there is nowhere to send a backup", policy.Name)
	}

	accounts, err := e.accountsFor(ctx, policy)
	if err != nil {
		return 0, nil, err
	}
	if len(accounts) == 0 {
		return 0, nil, fmt.Errorf("node: %q covers no accounts", policy.Name)
	}

	for _, account := range accounts {
		if _, err := e.QueueBackup(policy.ID, account); err != nil {
			// Almost always because that account is already being backed
			// up, which is a skip rather than a failure.
			e.log.Warn("skipped account on a manual run",
				"policy", policy.Name, "account", account, "reason", err)
			skipped = append(skipped, account)
			continue
		}
		queued++
	}
	e.log.Info("schedule run by hand",
		"policy", policy.Name, "queued", queued, "skipped", len(skipped))
	return queued, skipped, nil
}

// ReadyDownload finds an archive already rebuilt for an account and still
// on disk, so pressing Download can hand it over rather than rebuilding
// something that is right there.
func (e *Engine) ReadyDownload(account string) (nodestore.Restore, bool) {
	restores, err := e.store.Restores(0)
	if err != nil {
		return nodestore.Restore{}, false
	}
	for _, restore := range restores {
		if restore.Account != account || restore.Status != job.StatusSuccess {
			continue
		}
		if restore.ArchivePath == "" || restore.Applied {
			continue
		}
		if _, _, _, err := e.statArchive(restore); err != nil {
			continue
		}
		// Restores come back newest first.
		return restore, true
	}
	return nodestore.Restore{}, false
}

// QueueDownload asks for an account's newest backup to be rebuilt into an
// archive that can then be downloaded.
//
// It is a restore that is deliberately not applied: the archive is left on
// this server for someone to fetch, and nothing on the live account is
// touched.
func (e *Engine) QueueDownload(ctx context.Context, account string) (nodestore.Restore, error) {
	repositories, err := e.store.Repositories()
	if err != nil {
		return nodestore.Restore{}, err
	}

	var (
		newest     resticrun.Snapshot
		newestRepo string
		lastErr    error
	)
	for _, repository := range repositories {
		if repository.InitialisedAt == nil {
			continue
		}
		snapshots, err := e.Snapshots(ctx, repository.ID, account)
		if err != nil {
			// One unreachable destination should not stop us looking in
			// the others.
			lastErr = err
			continue
		}
		for _, snapshot := range snapshots {
			if snapshot.Time.After(newest.Time) {
				newest, newestRepo = snapshot, repository.ID
			}
		}
	}

	if newestRepo == "" {
		if lastErr != nil {
			return nodestore.Restore{}, fmt.Errorf(
				"node: no backup of %s could be found: %w", account, lastErr)
		}
		return nodestore.Restore{}, fmt.Errorf("node: %s has no backup yet", account)
	}

	return e.QueueRestore(nodestore.Restore{
		Account:      account,
		RepositoryID: newestRepo,
		SnapshotID:   newest.ID,
		Kind:         protocol.RestoreAccount,
	})
}

// OpenArchiveForDownload opens the archive a finished restore produced.
//
// The path comes from this node's own state file rather than from a
// request, but it still becomes an open() as root on a server whose other
// users are not trusted, so it is checked rather than believed:
//
//   - symlinks are resolved on both the staging root and the target before
//     containment is decided, because a lexical prefix check is satisfied
//     by a symlink pointing anywhere;
//   - containment is decided with filepath.Rel, so a sibling directory
//     whose name merely starts with the root's cannot pass;
//   - the file is opened with O_NOFOLLOW, so a symlink swapped in at the
//     leaf between the check and the open is refused rather than followed.
//
// The caller closes the file.
func (e *Engine) OpenArchiveForDownload(restoreID string) (file *os.File, filename string, size int64, err error) {
	restore, err := e.store.Restore(restoreID)
	if err != nil {
		return nil, "", 0, err
	}
	resolved, filename, size, err := e.statArchive(restore)
	if err != nil {
		return nil, "", 0, err
	}

	file, err = os.OpenFile(resolved, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, "", 0, fmt.Errorf("node: open the archive: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, "", 0, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, "", 0, fmt.Errorf("node: that archive is not a regular file")
	}
	return file, filename, info.Size(), nil
}

// statArchive resolves a restore's archive and checks it is where this node
// puts them, without opening it.
func (e *Engine) statArchive(restore nodestore.Restore) (path, filename string, size int64, err error) {
	if restore.Status != job.StatusSuccess || restore.ArchivePath == "" {
		return "", "", 0, fmt.Errorf("node: that restore produced no archive")
	}

	// Both sides are resolved before containment is decided: a lexical
	// prefix check is satisfied by a symlink pointing anywhere.
	root, err := filepath.EvalSymlinks(e.settings.StagingRoot)
	if err != nil {
		return "", "", 0, fmt.Errorf("node: resolve the staging area: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(restore.ArchivePath)
	if err != nil {
		return "", "", 0, fmt.Errorf(
			"node: the archive is gone — it is replaced when the account is rebuilt "+
				"again: %w", err)
	}

	relative, err := filepath.Rel(root, resolved)
	if err != nil || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) ||
		filepath.IsAbs(relative) {
		return "", "", 0, fmt.Errorf("node: that archive is not in the staging area")
	}

	info, err := os.Lstat(resolved)
	if err != nil {
		return "", "", 0, fmt.Errorf("node: the archive is gone: %w", err)
	}
	return resolved, filepath.Base(resolved), info.Size(), nil
}

const (
	// watchEvery is how often the overdue and stuck checks run. Long
	// enough that reading the account registry costs nothing, short
	// enough that a stuck run is reported the same hour it wedges.
	watchEvery = 5 * time.Minute
	// probeEvery is how often each destination is reached for. A
	// destination that is down is down for hours, not seconds.
	probeEvery = time.Hour
)

// restoreSelections carries a stored restore's parts onto the wire.
func restoreSelections(stored nodestore.Restore) []protocol.RestoreSelection {
	if len(stored.Items) == 0 {
		return nil
	}
	selections := make([]protocol.RestoreSelection, 0, len(stored.Items))
	for _, item := range stored.Items {
		selections = append(selections, protocol.RestoreSelection{
			Kind: item.Kind, Names: item.Names,
		})
	}
	return selections
}

// errorText is an error as a log value, and the empty string for no error.
// A log line whose error field is always present is one that can be read
// the same way whether the thing worked or not.
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
