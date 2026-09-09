package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/shukiv/gniza/internal/cpanel"
	"github.com/shukiv/gniza/internal/destination"
	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/panel"
	"github.com/shukiv/gniza/internal/pkgacct"
	"github.com/shukiv/gniza/internal/protocol"
	"github.com/shukiv/gniza/internal/resticrun"
	"github.com/shukiv/gniza/internal/staging"
)

// Version identifies the agent build to the controller, and is what an
// update check compares against a published release. It is set at build time
// from the git tag; a build from a working tree says so rather than claiming
// a version it is not.
var Version = "dev"

// BuiltAt is the commit this build was made from, as an RFC 3339 time. It
// is what says whether one build is later than another when neither is a
// released version -- a branch has no version numbers to compare, and a
// name like v0.1.0-18-g39bbd5b puts builds in no order at all.
//
// It is the commit's own time rather than the moment of compilation, so
// two builds of the same commit agree.
var BuiltAt = ""

// Built is BuiltAt as a time, and whether this build has one.
func Built() (time.Time, bool) {
	if BuiltAt == "" {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, BuiltAt)
	if err != nil {
		return time.Time{}, false
	}
	return at.UTC(), true
}

// Agent executes backup jobs on one cPanel server.
type Agent struct {
	client   *Client
	provider panel.Provider
	staging  *staging.Manager
	runner   *resticrun.Runner
	log      *slog.Logger

	// PollInterval is the pause after an empty poll. The controller holds
	// the request open, so this is a backstop, not the polling rate.
	PollInterval time.Duration
	// Hostname is reported at enrolment.
	Hostname string
	// ResticVersion is reported at enrolment for fleet-wide version
	// tracking: repository features depend on it.
	ResticVersion string
	// LeaseRenewEvery is how often work in progress tells the controller
	// it is still going. Zero derives it from the lease the assignment
	// carries, which is what a running agent does; it is set explicitly
	// by tests that cannot wait for a heartbeat measured in minutes.
	LeaseRenewEvery time.Duration
	// TargetTimeout bounds one repository upload. restic retries a
	// failing backend for a long time; without a bound, one unreachable
	// destination consumes the window the reachable ones needed.
	TargetTimeout time.Duration
	// OnProgress, when set, is called as a backup runs. It is called from
	// the goroutine reading restic's output, roughly once a second per
	// repository, so it must not block.
	OnProgress func(jobID, repositoryID string, progress resticrun.Progress)
	// OnRestoreStage, when set, is called as a restore moves through its
	// stages. progress is nil for a stage restic cannot count -- unpacking
	// an archive, handing one to cPanel, writing a database back -- so the
	// interface shows the stage on its own rather than a bar that does not
	// move. Called from the goroutine reading restic's output as well as
	// from the restore itself, so it must not block.
	OnRestoreStage func(restoreID, stage string, progress *resticrun.RestoreProgress)
}

// Config assembles an Agent.
type Config struct {
	Client        *Client
	Provider      panel.Provider
	Staging       *staging.Manager
	Runner        *resticrun.Runner
	Log           *slog.Logger
	Hostname      string
	ResticVersion string
	PollInterval  time.Duration
	TargetTimeout time.Duration
}

// New builds an Agent.
func New(cfg Config) *Agent {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	interval := cfg.PollInterval
	if interval == 0 {
		interval = 5 * time.Second
	}
	targetTimeout := cfg.TargetTimeout
	if targetTimeout == 0 {
		targetTimeout = 4 * time.Hour
	}
	return &Agent{
		client:        cfg.Client,
		provider:      cfg.Provider,
		staging:       cfg.Staging,
		runner:        cfg.Runner,
		log:           log,
		Hostname:      cfg.Hostname,
		ResticVersion: cfg.ResticVersion,
		PollInterval:  interval,
		TargetTimeout: targetTimeout,
	}
}

// Enrol reports this host's capabilities to the controller.
func (a *Agent) Enrol(ctx context.Context) error {
	caps, err := a.provider.Capabilities(ctx)
	if err != nil {
		return err
	}
	flags := map[string]string{}
	if caps.NoCompressFlag != "" {
		flags["nocompress"] = caps.NoCompressFlag
	}
	if caps.SkipHomedirFlag != "" {
		flags["skiphomedir"] = caps.SkipHomedirFlag
	}
	if caps.SkipDBFlag != "" {
		flags["skipdb"] = caps.SkipDBFlag
	}
	if caps.NoCompressFlag == "" {
		// Worth saying out loud at startup: without it, every monolithic
		// backup stores a full copy.
		a.log.Warn("pkgacct on this host cannot disable compression; " +
			"monolithic payloads will deduplicate poorly")
	}

	_, err = a.client.Enrol(ctx, protocol.EnrolRequest{
		Hostname:     a.Hostname,
		AgentVersion: Version,
		ResticVer:    a.ResticVersion,
		PkgacctFlags: flags,
		StagingRoot:  a.staging.Root,
	})
	return err
}

// CleanStaleStaging removes staging directories left by a previous process.
//
// Cleaning up only on the success path is not enough: the failure path is
// exactly when the volume is already under pressure.
func (a *Agent) CleanStaleStaging() error {
	dirs, err := a.staging.List()
	if err != nil {
		return err
	}
	for _, dir := range dirs {
		if err := a.staging.Release(&dir); err != nil {
			a.log.Error("remove stale staging", "path", dir.Path, "error", err)
			continue
		}
		a.log.Warn("removed stale staging directory", "key", dir.Key, "path", dir.Path)
	}
	return nil
}

// Run polls for jobs until the context is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		assignment, err := a.client.NextWork(ctx)
		switch {
		case errors.Is(err, ErrNoWork):
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return err
		case err != nil:
			a.log.Error("poll for work", "error", err)
		default:
			a.execute(ctx, assignment)
			continue
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(a.PollInterval):
		}
	}
}

// reportBudget is how long a finished job has to say what happened. A
// variable so a test does not have to take a minute to prove the
// deadline is not already spent.
var reportBudget = time.Minute

// execute performs one assignment and reports it.
//
// Reporting gets its own deadline. Work that consumed its whole budget must
// still be able to say what happened, otherwise the lease expires and it is
// all repeated.
func (a *Agent) execute(ctx context.Context, assignment protocol.Assignment) {
	switch assignment.Kind {
	case protocol.KindBackup:
		job := *assignment.Backup
		held, release := a.holdLease(ctx, job.JobID, job.ClaimToken, false, job.LeaseExpiresAt)
		report := a.RunJob(held, job)
		release()
		// Taken now rather than before the work: a backup runs for
		// minutes or hours, and a deadline started when the assignment
		// arrived has already passed by the time there is anything to
		// say.
		reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reportBudget)
		defer cancel()
		if err := a.client.Report(reportCtx, report); err != nil {
			// The lease will expire and the controller will re-queue it;
			// restic tolerates the partial write.
			a.log.Error("report backup", "job_id", report.JobID, "error", err)
		}
	case protocol.KindRestore:
		restore := *assignment.Restore
		held, release := a.holdLease(ctx, restore.JobID, restore.ClaimToken, true,
			restore.LeaseExpiresAt)
		report := a.RunRestore(held, restore)
		release()
		// The same, and it matters more here: a restore that has already
		// written into a live account and cannot say so is a restore the
		// controller queues again.
		reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reportBudget)
		defer cancel()
		if err := a.client.ReportRestore(reportCtx, report); err != nil {
			a.log.Error("report restore", "job_id", report.JobID, "error", err)
		}
	default:
		a.log.Error("unknown work kind", "kind", assignment.Kind)
	}
}

// leaseRenewBounds keep the heartbeat sensible whatever the lease is: often
// enough that a job outliving its lease is renewed well before it expires,
// rarely enough that a long job is not chattering at the controller.
const (
	leaseRenewAtLeastEvery = 30 * time.Second
	leaseRenewAtMostEvery  = 15 * time.Minute
)

// holdLease keeps a claim alive while the work runs, and takes the work's
// context away when the claim is gone.
//
// The lease is a fixed span; the work is not. A restore of a large account
// over a slow link outlasts it, and the job is then handed to another
// attempt while this one is still writing -- so the same destructive
// restore runs twice, over an account the first one has half-rewritten.
// Saying "still working" is what stops that.
//
// Losing the lease cancels the work. Continuing to write into a live
// account with no claim on it is the thing the claim token was added to
// forbid, and stopping is no worse than any other failure partway through:
// what was already written is reported with the failure.
//
// Standalone has no controller and no lease, so this does nothing there.
func (a *Agent) holdLease(ctx context.Context, jobID, claimToken string, restore bool,
	expires time.Time) (context.Context, func()) {

	if a.client == nil || jobID == "" || claimToken == "" {
		return ctx, func() {}
	}
	every := a.LeaseRenewEvery
	if every <= 0 {
		every = leaseRenewAtMostEvery
		if !expires.IsZero() {
			every = time.Until(expires) / 2
		}
		every = min(max(every, leaseRenewAtLeastEvery), leaseRenewAtMostEvery)
	}

	held, cancel := context.WithCancel(ctx)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-held.Done():
				return
			case <-ticker.C:
			}
			// Not held's context: a renewal has to be able to run even
			// as the work is finishing, and it must not be cancelled by
			// the cancellation it is deciding about.
			renewCtx, cancelRenew := context.WithTimeout(
				context.WithoutCancel(ctx), 30*time.Second)
			_, err := a.client.RenewLease(renewCtx, protocol.LeaseRenewal{
				JobID: jobID, ClaimToken: claimToken, Restore: restore,
			})
			cancelRenew()
			switch {
			case errors.Is(err, ErrLeaseLost):
				a.log.Error("this job is no longer ours; stopping",
					"job_id", jobID, "restore", restore)
				cancel()
				return
			case err != nil:
				// A controller that cannot be reached is not the same as
				// one that refused. Keep working and try again: the lease
				// has not been taken from us, it has only not been
				// extended yet.
				a.log.Warn("could not extend the lease on this job",
					"job_id", jobID, "restore", restore, "error", err)
			}
		}
	}()
	return held, func() {
		close(stop)
		cancel()
		<-done
	}
}

// RunJob stages one account and uploads it to every target.
//
// It always returns a report. A failure to stage fails the whole job; a
// failure to reach one repository fails only that target, because the
// copies that did land are still good.
func (a *Agent) RunJob(ctx context.Context, assignment protocol.JobAssignment) protocol.JobReport {
	// The token says which attempt this is. The controller refuses a
	// report that does not carry the token of the attempt it is running,
	// so a job whose lease was reclaimed cannot close out its successor.
	report := protocol.JobReport{
		JobID: assignment.JobID, ClaimToken: assignment.ClaimToken,
	}
	log := a.log.With("job_id", assignment.JobID, "account", assignment.CPanelUser)

	system := assignment.CPanelUser == cpanel.SystemAccount
	var account panel.AccountInfo
	if !system {
		found, err := a.provider.Account(ctx, assignment.CPanelUser)
		if err != nil {
			log.Error("read account", "error", err)
			report.StagingError = err.Error()
			return report
		}
		account = found
	}
	// Prefer what the host reports now over the controller's stored
	// estimate, which may predate the account growing.
	size := account.SizeBytes
	if assignment.SizeEstimate > size {
		size = assignment.SizeEstimate
	}
	estimate := stagingEstimate(size, pkgacct.Mode(assignment.PayloadMode), a.provider.Layout(), a.provider)
	if system {
		// Configuration files and an EasyApache profile: megabytes, and
		// the same every night.
		estimate = systemStagingEstimate
	}

	// Staged under the account, not the job: the paths restic records
	// must be identical every night, or each run becomes its own
	// retention group and nothing is ever pruned.
	log.Debug("staging an account",
		"size", size, "estimate", estimate, "mode", assignment.PayloadMode,
		"targets", len(assignment.Targets))
	dir, err := a.staging.Allocate(assignment.CPanelUser, estimate)
	if err != nil {
		log.Error("allocate staging", "error", err)
		report.StagingError = err.Error()
		return report
	}
	defer func() {
		if err := a.staging.Release(dir); err != nil {
			log.Error("release staging", "error", err)
		}
	}()

	mode := pkgacct.Mode(assignment.PayloadMode)
	var payload pkgacct.Payload
	if system {
		// The server's own configuration, not an account: no pkgacct, no
		// databases, no home directory.
		mode = pkgacct.ModeSystem
		payload, err = a.provider.StageSystem(ctx, dir.Path)
	} else {
		payload, err = a.provider.Stage(ctx, panel.StageRequest{
			Account: account, StagingDir: dir.Path, Mode: mode,
			SkipHomedir:   assignment.SkipHomedir,
			SkipDatabases: assignment.SkipDatabases,
			SkipEmail:     assignment.SkipEmail,
		})
	}
	if err != nil {
		log.Error("stage payload", "error", err)
		report.StagingError = err.Error()
		return report
	}
	if payload.Degraded {
		// Not only about deduplication: this is also where a skip that
		// the host cannot honour in full is said out loud.
		log.Warn("the payload is not quite what the schedule asked for",
			"reason", payload.Reason)
	}
	for _, omission := range payload.Missing {
		// At warn, not debug: this is the operator's business on every
		// run, not something to go looking for.
		log.Warn("left out of the backup", "what", omission.What, "why", omission.Why)
		report.Missing = append(report.Missing, omission.String())
	}
	log.Debug("staged an account", "dir", dir.Path, "mode", mode,
		"parts", len(payload.Parts), "dumps", len(payload.DumpPaths),
		"degraded", payload.Degraded, "missing", len(payload.Missing))
	// restic treats a path it cannot read as a warning and carries on, so
	// a missing part would become a snapshot that looks fine and restores
	// an incomplete account. It has to stop the job instead.
	if err := payload.Verify(); err != nil {
		log.Error("staged payload is incomplete", "error", err)
		report.StagingError = err.Error()
		return report
	}

	// pkgacct runs once; the staged payload is uploaded to every target.
	for _, target := range assignment.Targets {
		report.Targets = append(report.Targets,
			a.backupTarget(ctx, log, assignment, target, payload))
	}

	// A destination that failed gets one more attempt while the payload is
	// still staged: an upload rather than another pkgacct. Most of what
	// fails here is a network that was briefly not there.
	if assignment.RetryFailed {
		for i, result := range report.Targets {
			if result.Status != string(job.TargetFailed) {
				continue
			}
			target, found := targetFor(assignment, result.RepositoryID)
			if !found {
				continue
			}
			log.Warn("retrying a destination that failed",
				"repository_id", result.RepositoryID, "first_error", result.Error)
			retried := a.backupTarget(ctx, log, assignment, target, payload)
			if retried.Status == string(job.TargetSuccess) {
				report.Targets[i] = retried
				continue
			}
			// Keep the second error, and say that both attempts failed:
			// "it failed twice" is worth more than either message alone.
			report.Targets[i].Error = fmt.Sprintf(
				"failed twice: %s (and on retry: %s)", result.Error, retried.Error)
		}
	}
	return report
}

// progressFor adapts the hook to what the runner expects, and returns nil
// when nobody is watching so restic's status lines are not even parsed.
func (a *Agent) progressFor(jobID, repositoryID string) func(resticrun.Progress) {
	if a.OnProgress == nil {
		return nil
	}
	return func(progress resticrun.Progress) {
		a.OnProgress(jobID, repositoryID, progress)
	}
}

// restoreWatch adapts the hook to what a restore reports, and keeps the
// stage beside the percentage.
//
// restic counts each part of a split snapshot from zero, so a percentage
// without the stage it belongs to reads as a bar that goes backwards. The
// stage is set on the goroutine running the restore and read on the one
// parsing restic's output, so it is held under a lock.
type restoreWatch struct {
	agent     *Agent
	restoreID string
	mu        sync.Mutex
	stage     string
}

// watchRestore returns nil when nobody is listening, so restic's status
// lines are not even parsed.
func (a *Agent) watchRestore(restoreID string) *restoreWatch {
	if a.OnRestoreStage == nil {
		return nil
	}
	return &restoreWatch{agent: a, restoreID: restoreID}
}

// Stage records that the restore has moved on, and says so with no
// percentage: whatever the last part reached does not describe this one.
func (w *restoreWatch) Stage(stage string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.stage = stage
	w.mu.Unlock()
	w.agent.OnRestoreStage(w.restoreID, stage, nil)
}

// Progress passes restic's own figure on, against the stage it belongs to.
func (w *restoreWatch) Progress(progress resticrun.RestoreProgress) {
	if w == nil {
		return
	}
	w.mu.Lock()
	stage := w.stage
	w.mu.Unlock()
	w.agent.OnRestoreStage(w.restoreID, stage, &progress)
}

// StageFunc and ProgressFunc are the hooks in the shape reassemble takes,
// and are nil when nobody is listening.
func (w *restoreWatch) StageFunc() func(string) {
	if w == nil {
		return nil
	}
	return w.Stage
}

func (w *restoreWatch) ProgressFunc() func(resticrun.RestoreProgress) {
	if w == nil {
		return nil
	}
	return w.Progress
}

// Staging holds what pkgacct writes and what mysqldump writes. In split
// mode that is the account's configuration and its database dumps — the
// home directory is backed up where it lies and is never copied — so
// reserving the whole account refuses backups that would have fitted
// easily. It refused a 6.8 GB account on a volume with 6.4 GB free, for a
// job that would have staged a few hundred megabytes.
//
// A fraction is still a guess. It is a deliberate one: too small a
// reservation costs a run that fails partway, which is the same outcome as
// refusing it, while too large costs backups that never run at all.
const (
	splitStagingShare = 0.20
	splitStagingFloor = 512 << 20
	// The server's own configuration is small and roughly constant.
	systemStagingEstimate = 256 << 20
)

// stagingEstimate is how much room a payload of this shape needs.
//
// The layout is asked because split does not mean the same thing on every
// panel. cPanel is asked for the parts and backs the home directory up
// where it lies, so staging holds a fraction of the account. A panel that
// will not produce the parts has its whole archive written into staging
// and taken apart there, so at the peak the account is on that disk
// twice -- once compressed and once not -- and a fifth of it is the
// estimate that lets a run fill the disk instead of being refused.
func stagingEstimate(size uint64, mode pkgacct.Mode, layout panel.ArchiveLayout, provider any) uint64 {
	if mode != pkgacct.ModeSplit {
		// One archive of the whole account, written into staging.
		return size
	}
	share := uint64(float64(size) * splitStagingShare)
	inPlace, asked := provider.(panel.InPlaceReader)
	if _, packs := layout.(panel.ArchivePacker); packs && !(asked && inPlace.ReadsHomeInPlace()) {
		share = 2 * size
	}
	if share < splitStagingFloor {
		return splitStagingFloor
	}
	return share
}

// targetFor finds the destination a report line came from.
func targetFor(assignment protocol.JobAssignment, repositoryID string) (protocol.Target, bool) {
	for _, target := range assignment.Targets {
		if target.RepositoryID == repositoryID {
			return target, true
		}
	}
	return protocol.Target{}, false
}

// skipTags names the parts of an account a schedule asked to leave out,
// in a fixed order: the tag set decides the retention group, and a set
// that varied between runs of the same schedule would split it.
func skipTags(assignment protocol.JobAssignment) []string {
	var tags []string
	if assignment.SkipHomedir {
		tags = append(tags, resticrun.SkipTagPrefix+"homedir")
	}
	if assignment.SkipDatabases {
		tags = append(tags, resticrun.SkipTagPrefix+"databases")
	}
	if assignment.SkipEmail {
		tags = append(tags, resticrun.SkipTagPrefix+"email")
	}
	return tags
}

func (a *Agent) backupTarget(ctx context.Context, log *slog.Logger,
	assignment protocol.JobAssignment, target protocol.Target,
	payload pkgacct.Payload) protocol.TargetReport {

	result := protocol.TargetReport{
		RepositoryID: target.RepositoryID,
		Status:       string(job.TargetFailed),
	}

	dest, err := destination.Build(target.Spec)
	if err != nil {
		result.Error = err.Error()
		log.Error("build destination", "repository_id", target.RepositoryID, "error", err)
		return result
	}

	targetCtx, cancel := context.WithTimeout(ctx, a.TargetTimeout)
	defer cancel()

	log.Debug("uploading to a destination",
		"repository_id", target.RepositoryID, "path", target.RepoPath,
		"timeout", a.TargetTimeout.String())
	started := time.Now()
	backup, err := a.runner.Backup(targetCtx, resticrun.Repository{
		Dest:     dest,
		Path:     target.RepoPath,
		Password: target.RepoPassword,
	}, resticrun.BackupSpec{
		RecordCompletion: true,
		Paths:            payload.Paths(),
		Host:             a.Hostname,
		OnProgress:       a.progressFor(assignment.JobID, target.RepositoryID),
		// Tags identify the account, and must be stable: retention groups
		// by them, so a per-job tag would put every run in a group of its
		// own and exempt it from pruning. The job a snapshot came from is
		// recorded in the database against its snapshot id.
		//
		// What the schedule left out is tagged too, and for two reasons.
		// A restore has to be able to tell a backup of the whole account
		// from one taken without its databases, and retention groups by
		// tags -- so a partial run lands in its own group instead of
		// counting towards the keeps and evicting a complete backup.
		Tags: append([]string{
			"account:" + assignment.CPanelUser,
			"mode:" + string(payload.Mode),
		}, skipTags(assignment)...),
		Exclude:        assignment.Excludes,
		LimitUploadKiB: assignment.LimitUploadKiB,
	})
	result.DurationSecs = time.Since(started).Seconds()
	result.SnapshotID = backup.Summary.SnapshotID
	result.BytesAdded = backup.Summary.DataAdded
	result.BytesProcessed = backup.Summary.TotalBytesProcessed
	result.Incomplete = backup.Incomplete
	result.Detail = trimDetail(backup.Stderr)

	if err != nil {
		result.Error = err.Error()
		// One unreachable destination must not invalidate the copies that
		// succeeded, so this is recorded and the loop continues.
		log.Error("backup target", "repository_id", target.RepositoryID, "error", err)
		return result
	}

	result.Status = string(job.TargetSuccess)
	if backup.Incomplete {
		result.Status = string(job.TargetFailed)
		result.Error = "backup is incomplete: some source files could not be read"
		log.Warn("snapshot is incomplete: some source files could not be read",
			"repository_id", target.RepositoryID, "snapshot_id", result.SnapshotID)
	}
	log.Info("target complete",
		"repository_id", target.RepositoryID, "snapshot_id", result.SnapshotID,
		"bytes_added", result.BytesAdded, "bytes_processed", result.BytesProcessed)
	return result
}

// maxDetail bounds what is kept from restic's error stream. A backup of a
// busy account can name thousands of transient files, and the last of them
// are the ones worth reading.
const maxDetail = 16 << 10

// trimDetail keeps the tail of restic's error output, which is where its
// summary of what it could not read appears.
func trimDetail(stderr string) string {
	trimmed := strings.TrimSpace(stderr)
	if len(trimmed) <= maxDetail {
		return trimmed
	}
	cut := trimmed[len(trimmed)-maxDetail:]
	if newline := strings.IndexByte(cut, '\n'); newline >= 0 {
		cut = cut[newline+1:]
	}
	return "… earlier output omitted …\n" + cut
}
