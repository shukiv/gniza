// Package node runs Gniza on a single cPanel server with no controller.
//
// It is the same machinery as fleet mode with the control plane removed:
// state lives in a local bbolt file instead of PostgreSQL, scheduling
// happens here instead of on a controller, and configuration comes from the
// WHM plugin instead of a CLI. The parts that make backups correct —
// payload planning, staging, the restic runner, reassembly — are the fleet
// implementations, reused rather than rewritten.
// See docs/adr/0007-standalone-mode.md.
package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shukiv/gniza/internal/agent"
	"github.com/shukiv/gniza/internal/destination"
	"github.com/shukiv/gniza/internal/granular"
	"github.com/shukiv/gniza/internal/hookspool"
	"github.com/shukiv/gniza/internal/inventory"
	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/notify"
	"github.com/shukiv/gniza/internal/panel"
	"github.com/shukiv/gniza/internal/protocol"
	"github.com/shukiv/gniza/internal/reassemble"
	"github.com/shukiv/gniza/internal/resticrun"
	"github.com/shukiv/gniza/internal/staging"
	"github.com/shukiv/gniza/internal/vault"
)

// Engine is the standalone node.
type Engine struct {
	store    *nodestore.Store
	vault    *vault.Vault
	provider panel.Provider
	runner   *resticrun.Runner
	worker   *agent.Agent
	// workMu makes the "is this account busy?" check and enqueue one
	// operation. Without it, two simultaneous WHM requests could both see
	// an idle account and stage work into the same directory.
	workMu sync.Mutex

	// progressMu guards lastProgress, which throttles how often a running
	// job's percentage is written.
	progressMu   sync.Mutex
	lastProgress map[string]progressMark

	// alertedMu guards alerted, which remembers what has already been
	// said so a server that is down for a week does not send a week of
	// identical messages.
	alertedMu     sync.Mutex
	alerted       map[string]bool
	lastWatch     time.Time
	lastProbe     time.Time
	lastReconcile time.Time
	// lastDeletedSweep is when accounts gone longer than this server
	// keeps them were last looked for.
	lastDeletedSweep time.Time
	// checkingUpdate is set while the daily ask about a newer release is
	// in flight, so a slow answer does not start a second one.
	checkingUpdate atomic.Bool
	// upgrading is set while a newer release is being fetched and
	// installed, so a second click does not start a second one.
	upgrading atomic.Bool
	// censusing is set while the repositories are being measured. That
	// walks every repository's index over the network and can take
	// minutes, so it runs beside the scheduler rather than in it, and a
	// second tick does not start a second one.
	censusing atomic.Bool
	// lastKeySweep is when prepared SFTP keys nobody used were last
	// looked for.
	lastKeySweep time.Time
	staging      *staging.Manager
	log          *slog.Logger

	// items remembers what a snapshot holds in the parts of an account
	// that are not files, so browsing between them does not stream the
	// same archive out of the repository once per click.
	items inventory.Cache

	settings nodestore.Settings
	// accountUID is replaceable in tests; on a cPanel host it resolves the
	// Unix identity behind a username.
	accountUID func(string) (int, error)
	// hookSpool is where a cPanel lifecycle hook left an event this
	// service was not running to hear. It is drained before anything
	// infers who owns a name from the account list.
	hookSpool string
	// logLevel is what the service is logging at now. It is shared with
	// the handler the loggers were built on, so changing it here changes
	// what they write.
	logLevel *slog.LevelVar
}

// Config assembles an Engine.
type Config struct {
	Store    *nodestore.Store
	Vault    *vault.Vault
	Provider panel.Provider
	Log      *slog.Logger
	// Exec runs restic. Nil means real child processes; a test
	// substitutes one so the paths that only happen when restic fails
	// can be exercised at all.
	Exec resticrun.Execer
	// AccountUID overrides Unix account lookup in tests. Production leaves
	// it nil and resolves identities through the operating system.
	AccountUID func(string) (int, error)
	// HookSpool is the directory cPanel lifecycle hooks write to when
	// this service is not there to answer. Empty means hookspool.DefaultDir.
	HookSpool string
	// LogLevel is the level variable the process's logger was built on.
	// Nil gets a private one, which is what a test wants; the service
	// passes the one its own handler reads.
	LogLevel *slog.LevelVar
}

// New builds an Engine from stored settings.
func New(cfg Config) (*Engine, error) {
	if cfg.Store == nil || cfg.Vault == nil || cfg.Provider == nil {
		return nil, errors.New("node: store, vault and provider are required")
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}

	settings, err := cfg.Store.Settings()
	if err != nil {
		return nil, err
	}
	if settings.ConfigDir == "" {
		// State written before this setting existed.
		settings.ConfigDir = nodestore.DefaultSettings().ConfigDir
	}
	for _, dir := range []string{settings.StagingRoot, settings.ResticCache, settings.ConfigDir} {
		if dir == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("node: create %s: %w", dir, err)
		}
	}

	// A process that was killed mid-run left its repository password
	// where it wrote it. This is the one moment where nothing on this
	// server owns one of those directories: the previous process is gone
	// and this one has not written its first yet.
	if swept, err := resticrun.SweepPasswordDirs(settings.StagingRoot); err != nil {
		log.Warn("clear password files left by an interrupted run", "error", err)
	} else if swept > 0 {
		log.Info("cleared password files left by an interrupted run", "count", swept)
	}

	runner := resticrun.New(resticrun.Config{
		Log:        log,
		Binary:     settings.ResticBinary,
		RuntimeDir: settings.StagingRoot,
		CacheDir:   settings.ResticCache,
		CACertPath: settings.ResticCACert,
	}, cfg.Exec)

	stagingManager := &staging.Manager{
		Root:              settings.StagingRoot,
		SafetyMarginRatio: settings.SafetyMargin,
		MaxConcurrent:     settings.MaxConcurrent,
	}

	// The worker is the fleet agent with no controller attached: RunJob and
	// RunRestore never touch its client, so the same tested code path runs
	// here.
	worker := agent.New(agent.Config{
		Provider:      cfg.Provider,
		Staging:       stagingManager,
		Runner:        runner,
		Log:           log,
		Hostname:      settings.Hostname,
		ResticVersion: "",
	})

	uidLookup := cfg.AccountUID
	if uidLookup == nil {
		uidLookup = accountUID
	}
	spoolDir := cfg.HookSpool
	if spoolDir == "" {
		spoolDir = hookspool.DefaultDir
	}
	engine := &Engine{
		store: cfg.Store, vault: cfg.Vault, provider: cfg.Provider,
		runner: runner, worker: worker, staging: stagingManager,
		log: log, settings: settings, lastProgress: map[string]progressMark{},
		accountUID: uidLookup,
		hookSpool:  spoolDir,
		logLevel:   cfg.LogLevel,
	}
	if engine.logLevel == nil {
		engine.logLevel = new(slog.LevelVar)
	}
	// A backup of a large account takes minutes, and an operator watching
	// it deserves to see it move. restic reports about once a second per
	// repository; the engine writes far less often than that.
	worker.OnProgress = engine.recordProgress
	// And a restore, which is the longer of the two and until now said
	// nothing at all while it ran.
	worker.OnRestoreStage = engine.recordRestoreStage
	if err := engine.RecoverFromRestart(); err != nil {
		return nil, err
	}
	return engine, nil
}

// RecoverFromRestart closes out work that was in flight when the process
// died, and clears what it left on disk.
//
// Fleet mode has lease expiry for this. Standalone has nothing: a job stuck
// in "running" would make the account look permanently busy, so every later
// backup of it would be skipped — silently, every night — and its staging
// directory would block the next attempt anyway.
func (e *Engine) RecoverFromRestart() error {
	jobs, err := e.store.Jobs(0)
	if err != nil {
		return err
	}
	var interrupted int
	for _, stored := range jobs {
		// Only work that was actually in flight. Something merely queued
		// is still perfectly good work, and failing it would mean a
		// restart quietly emptied the queue.
		if stored.Status != job.StatusRunning {
			continue
		}
		stored.Status = job.StatusFailed
		stored.StagingErr = "interrupted by a service restart"
		finished := time.Now().UTC()
		stored.FinishedAt = &finished
		if _, err := e.store.PutJob(stored); err != nil {
			return err
		}
		interrupted++
	}

	restores, err := e.store.Restores(0)
	if err != nil {
		return err
	}
	for _, stored := range restores {
		if stored.Status != job.StatusRunning {
			continue
		}
		stored.Status = job.StatusFailed
		stored.Error = "interrupted by a service restart"
		finished := time.Now().UTC()
		stored.FinishedAt = &finished
		if _, err := e.store.PutRestore(stored); err != nil {
			return err
		}
		interrupted++
	}

	// Whatever those left behind is debris. Finished output is kept: a
	// rebuilt archive somebody was told to download should still be there
	// after a restart.
	dirs, err := e.staging.Active()
	if err != nil {
		return err
	}
	for _, dir := range dirs {
		if err := e.staging.Release(&dir); err != nil {
			e.log.Error("remove stale staging", "path", dir.Path, "error", err)
			continue
		}
		e.log.Warn("removed a staging directory left by a previous run",
			"key", dir.Key, "path", dir.Path)
	}

	if interrupted > 0 {
		e.log.Warn("marked interrupted work as failed", "count", interrupted)
	}
	if err := e.SweepWorkdir(); err != nil {
		e.log.Error("sweep the work directory", "error", err)
	}
	return nil
}

// SweepWorkdir removes collected output nobody came back for.
//
// A restore leaves its result in the work directory on purpose: it is
// there to be downloaded, and it survives a restart. Nothing expired it,
// so a server where every account had been restored once kept every one of
// those trees for ever, on a disk that also has to hold tonight's backup.
//
// Work in progress is never touched here — only finished output, and only
// once it is older than the retention this server is configured with.
func (e *Engine) SweepWorkdir() error {
	ttl := e.settings.KeepOutputFor()
	if ttl <= 0 {
		return nil
	}
	outputs, err := e.staging.Retained()
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-ttl)
	for _, output := range outputs {
		if output.At.After(cutoff) {
			continue
		}
		dir := output.Dir
		if err := e.staging.Release(&dir); err != nil {
			e.log.Error("remove collected output", "key", output.Key, "error", err)
			continue
		}
		e.log.Info("removed collected output nobody came back for",
			"key", output.Key, "bytes", output.Bytes, "age_hours", time.Since(output.At).Hours())
	}
	return nil
}

// Store exposes the state file to the UI.
func (e *Engine) Store() *nodestore.Store { return e.store }

// Vault exposes the credential vault to the UI, which seals what an
// operator types before it is written.
func (e *Engine) Vault() *vault.Vault { return e.vault }

// Settings returns the node's configuration as loaded at startup.
func (e *Engine) Settings() nodestore.Settings { return e.settings }

// Accounts lists the cPanel accounts on this server.
func (e *Engine) Accounts(ctx context.Context) ([]panel.AccountInfo, error) {
	return e.provider.Accounts(ctx)
}

// ProbeCapabilities reports which pkgacct flags this host supports, and
// records them so the UI can warn about a host that cannot disable
// compression.
func (e *Engine) ProbeCapabilities(ctx context.Context) error {
	caps, err := e.provider.Capabilities(ctx)
	if err != nil {
		return err
	}
	flags := map[string]string{}
	for name, flag := range map[string]string{
		"nocompress":     caps.NoCompressFlag,
		"skiphomedir":    caps.SkipHomedirFlag,
		"skipdb":         caps.SkipDBFlag,
		"skipmail":       caps.SkipMailFlag,
		"skipmailconfig": caps.SkipMailConfigFlag,
	} {
		if flag != "" {
			flags[name] = flag
		}
	}
	e.settings.PkgacctFlags = flags
	return e.store.SaveSettings(e.settings)
}

// OpenRepository resolves a stored repository into something restic can be
// pointed at.
//
// forMaintenance addresses the endpoint that permits deletes, where the
// destination declares one.
func (e *Engine) OpenRepository(repositoryID string, forMaintenance bool) (resticrun.Repository, error) {
	repo, err := e.store.Repository(repositoryID)
	if err != nil {
		return resticrun.Repository{}, err
	}
	dest, err := e.store.Destination(repo.DestinationID)
	if err != nil {
		return resticrun.Repository{}, err
	}

	spec, err := e.buildSpec(dest)
	if err != nil {
		return resticrun.Repository{}, err
	}
	if forMaintenance {
		spec = destination.ForMaintenance(spec)
	}
	built, err := destination.Build(spec)
	if err != nil {
		return resticrun.Repository{}, err
	}

	password, err := e.openSecret(repo.PasswordSecretID)
	if err != nil {
		return resticrun.Repository{}, fmt.Errorf("node: repository password: %w", err)
	}
	return resticrun.Repository{Dest: built, Path: repo.Path, Password: string(password)}, nil
}

// buildSpec combines a destination's stored settings with its unsealed
// credentials.
func (e *Engine) buildSpec(dest nodestore.Destination) (destination.Spec, error) {
	secrets := map[string]string{}
	if dest.CredentialsSecretID != "" {
		plaintext, err := e.openSecret(dest.CredentialsSecretID)
		if err != nil {
			return destination.Spec{}, fmt.Errorf("node: destination credentials: %w", err)
		}
		if err := decodeSecrets(plaintext, &secrets); err != nil {
			return destination.Spec{}, err
		}
	}
	config := make(map[string]string, len(dest.Config))
	for key, value := range dest.Config {
		config[key] = value
	}
	return destination.Spec{
		Type: destination.Type(dest.Type), Config: config, Secrets: secrets,
	}, nil
}

func (e *Engine) openSecret(id string) ([]byte, error) {
	secret, err := e.store.Secret(id)
	if err != nil {
		return nil, err
	}
	return e.vault.Open(secret.Ciphertext)
}

// notifyDestination reports a destination that could not be reached, and
// says so only when the state changes: a nightly check against a server
// that is down for a week should not be a week of identical messages.
func (e *Engine) notifyDestination(ctx context.Context, dest nodestore.Destination, reachErr error) {
	wasDown := dest.LastCheckError != ""
	if reachErr == nil {
		if wasDown {
			e.Notify(ctx, notify.Message{
				Event:   notify.EventDestinationDown,
				Level:   notify.SeverityInfo,
				Subject: fmt.Sprintf("%s can be reached again", dest.Name),
				Body:    "Backups to it will run as scheduled.",
			})
		}
		return
	}
	if wasDown {
		return
	}
	e.Notify(ctx, notify.Message{
		Event:   notify.EventDestinationDown,
		Subject: fmt.Sprintf("%s could not be reached", dest.Name),
		Body:    reachErr.Error() + "\n\nNothing can be backed up there until it answers.",
	})
}

// TestDestination checks that a destination is reachable and configured,
// without touching any repository.
func (e *Engine) TestDestination(ctx context.Context, destinationID string) error {
	dest, err := e.store.Destination(destinationID)
	if err != nil {
		return err
	}
	spec, err := e.buildSpec(dest)
	if err != nil {
		return err
	}
	built, err := destination.Build(spec)
	if err != nil {
		return err
	}

	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	checkErr := built.Preflight(checkCtx)

	// Read before it is overwritten: whether this is news depends on what
	// the destination was doing last time.
	e.notifyDestination(ctx, dest, checkErr)

	now := time.Now().UTC()
	dest.LastCheckedAt = &now
	dest.LastCheckError = ""
	if checkErr != nil {
		dest.LastCheckError = checkErr.Error()
	}
	// How much room is left is worth knowing before the night it runs out,
	// and asking costs one statfs or one df. It is only asked when the
	// destination answered at all: a df against a machine that is not
	// there would fail slowly for a reason already recorded above.
	if checkErr == nil {
		dest.Space = measureSpace(ctx, built)
	}
	if _, err := e.store.PutDestination(dest); err != nil {
		return err
	}
	return checkErr
}

// measureSpace asks a destination how much room it has, if its kind can
// say. An S3 bucket has no size and a restic REST server does not report
// the disk under it; those record that they cannot rather than a zero,
// which would read on the page as a disk that is full.
func measureSpace(ctx context.Context, built destination.Destination) nodestore.DestinationSpace {
	now := time.Now().UTC()
	sizer, ok := built.(destination.Sizer)
	if !ok {
		return nodestore.DestinationSpace{Unsupported: true, MeasuredAt: &now}
	}
	space, err := sizer.Space(ctx)
	if err != nil {
		return nodestore.DestinationSpace{MeasuredAt: &now, Error: err.Error()}
	}
	return nodestore.DestinationSpace{
		TotalBytes: space.TotalBytes,
		FreeBytes:  space.FreeBytes,
		MeasuredAt: &now,
	}
}

// Snapshots lists an account's snapshots in a repository.
func (e *Engine) Snapshots(ctx context.Context, repositoryID, account string) ([]resticrun.Snapshot, error) {
	// Listing needs read access only. Using the maintenance endpoint here
	// needlessly gives ordinary WHM and account-facing browsing a route to
	// the endpoint that can delete snapshots, and breaks browsing entirely
	// when that endpoint is correctly isolated from the cPanel server.
	repo, err := e.OpenRepository(repositoryID, false)
	if err != nil {
		return nil, err
	}
	filter := resticrun.SnapshotFilter{}
	if account != "" {
		filter.Tags = []string{"account:" + account}
	}
	snapshots, err := e.runner.Snapshots(ctx, repo, filter)
	if err != nil {
		return nil, err
	}
	incomplete, err := e.incompleteSnapshots(repositoryID)
	if err != nil {
		return nil, err
	}
	for i := range snapshots {
		snapshots[i].ReadFailed = incomplete[snapshots[i].ID]
	}
	return snapshots, nil
}

func (e *Engine) incompleteSnapshots(repositoryID string) (map[string]bool, error) {
	jobs, err := e.store.Jobs(0)
	if err != nil {
		return nil, err
	}
	incomplete := map[string]bool{}
	for _, stored := range jobs {
		for _, target := range stored.Targets {
			if target.RepositoryID == repositoryID && target.Incomplete && target.SnapshotID != "" {
				incomplete[target.SnapshotID] = true
			}
		}
	}
	return incomplete, nil
}

// Browse lists the contents of a snapshot, so an operator can pick out the
// one file they need.
func (e *Engine) Browse(ctx context.Context, repositoryID, snapshotID string, subpaths ...string) ([]resticrun.Entry, error) {
	repo, err := e.OpenRepository(repositoryID, false)
	if err != nil {
		return nil, err
	}
	return e.runner.Ls(ctx, repo, snapshotID, subpaths...)
}

// Items says what one part of an account a snapshot holds: which DNS
// zones, which certificates, which cron jobs, which database users.
//
// The parts of an account that are files are listed by Browse, out of the
// snapshot's own paths. These are inside a single archive or a single SQL
// file, so saying what is in them means reading them.
func (e *Engine) Items(ctx context.Context, repositoryID, snapshotID string,
	parts reassemble.Parts, kind granular.Kind) ([]inventory.Item, error) {

	repo, err := e.OpenRepository(repositoryID, false)
	if err != nil {
		return nil, err
	}
	return e.items.Items(ctx, e.runner, inventory.Source{
		Key:        repositoryID,
		Repo:       repo,
		SnapshotID: snapshotID,
		Parts:      parts,
	}, kind)
}

// assignmentFor turns a stored job into the assignment the fleet agent
// already knows how to execute.
func (e *Engine) assignmentFor(j nodestore.Job, policy nodestore.Policy,
	account panel.AccountInfo) (protocol.JobAssignment, error) {

	assignment := protocol.JobAssignment{
		JobID:          j.ID,
		AccountID:      j.Account,
		CPanelUser:     j.Account,
		SizeEstimate:   account.SizeBytes,
		PayloadMode:    policy.PayloadMode,
		Compression:    policy.Compression,
		LimitUploadKiB: policy.LimitUploadKiB,
		Excludes:       excludesFor(policy, account, e.provider.NativeExcludes(account.HomeDir)),
		SkipHomedir:    policy.SkipHomedir,
		SkipEmail:      policy.SkipEmail,
		SkipDatabases:  policy.SkipDatabases,
		RetryFailed:    policy.RetryFailed,
	}
	for _, repositoryID := range policy.RepositoryIDs {
		target, err := e.targetFor(repositoryID)
		if err != nil {
			// A partial target list would silently reduce the number of
			// copies the policy promises.
			return protocol.JobAssignment{}, err
		}
		assignment.Targets = append(assignment.Targets, target)
	}
	if len(assignment.Targets) == 0 {
		return protocol.JobAssignment{}, fmt.Errorf("node: policy %s has no repositories", policy.Name)
	}
	return assignment, nil
}

// excludesFor is what this job should not store: the patterns the
// operator gave, plus the account's mail when the schedule leaves email
// out.
//
// Leaving email out means two directories, not one. The messages are
// under ~/mail; the mail accounts themselves are under ~/etc, one
// directory per domain, holding passwd, shadow and quota — shadow being
// every mailbox password on the account, as a crypt hash. A schedule told
// to leave email out was shipping all of them, so a backup an operator
// believed held no email held the credentials to all of it.
//
// The whole of ~/etc goes, rather than the credential files by name: a
// pattern that misses one ships the hashes, and what else lives there is
// cPanel's own bookkeeping — cacheid, ftpquota, the webmail databases —
// which is either regenerated or mail data in its own right.
//
// And this is still only half of it. These keep the files out of the file
// backup; the same configuration is inside pkgacct's own archive, where
// no exclude here can reach, so the schedule's choice is passed to
// pkgacct as well.
func excludesFor(policy nodestore.Policy, account panel.AccountInfo, native []string) []string {
	excludes := append([]string(nil), policy.Excludes...)
	if policy.SkipEmail && account.HomeDir != "" {
		excludes = append(excludes,
			filepath.Join(account.HomeDir, "mail"),
			filepath.Join(account.HomeDir, "etc"))
	}
	// What cPanel's own backups would leave out. An operator who wrote a
	// path into cpbackup-exclude.conf has said it must not leave the
	// server; ignoring that file uploaded the very files they excluded.
	return append(excludes, native...)
}

func (e *Engine) targetFor(repositoryID string) (protocol.Target, error) {
	repo, err := e.store.Repository(repositoryID)
	if err != nil {
		return protocol.Target{}, err
	}
	dest, err := e.store.Destination(repo.DestinationID)
	if err != nil {
		return protocol.Target{}, err
	}
	spec, err := e.buildSpec(dest)
	if err != nil {
		return protocol.Target{}, err
	}
	if _, err := destination.Build(spec); err != nil {
		return protocol.Target{}, err
	}
	password, err := e.openSecret(repo.PasswordSecretID)
	if err != nil {
		return protocol.Target{}, err
	}
	return protocol.Target{
		RepositoryID: repositoryID,
		Spec:         spec,
		RepoPath:     repo.Path,
		RepoPassword: string(password),
	}, nil
}

// SaveDestination stores changes to an existing destination, after checking
// that what was typed still produces something restic can be pointed at.
func (e *Engine) SaveDestination(dest nodestore.Destination) error {
	spec, err := e.buildSpec(dest)
	if err != nil {
		return err
	}
	if _, err := destination.Build(spec); err != nil {
		return err
	}
	_, err = e.store.PutDestination(dest)
	return err
}

// KindVerify is a restore that is rehearsed and thrown away: it proves the
// backup can be rebuilt without touching the account or keeping anything.
const KindVerify = "verify"

// progressFloor is how much has to change before a running job's progress
// is written again: a percentage point, or two seconds. restic reports
// once a second per repository and a fleet backup runs several at once, so
// writing every line would rewrite the same record for no visible gain.
const (
	progressFloor    = 1.0
	progressInterval = 2 * time.Second
)

// recordProgress stores how far a running backup has got.
//
// It is called from the goroutine reading restic's output, so it holds its
// own lock, keeps the work small, and never fails the backup: a job that
// cannot write its progress is still a job that is running.
func (e *Engine) recordProgress(jobID, repositoryID string, progress resticrun.Progress) {
	percent := progress.PercentDone * 100
	if percent > 100 {
		percent = 100
	}

	e.progressMu.Lock()
	last, seen := e.lastProgress[jobID]
	now := time.Now()
	if seen && percent-last.percent < progressFloor && now.Sub(last.at) < progressInterval {
		e.progressMu.Unlock()
		return
	}
	if e.lastProgress == nil {
		e.lastProgress = map[string]progressMark{}
	}
	e.lastProgress[jobID] = progressMark{percent: percent, at: now}
	e.progressMu.Unlock()

	err := e.store.SetJobProgress(jobID, nodestore.JobProgress{
		Percent:    percent,
		BytesDone:  progress.BytesDone,
		TotalBytes: progress.TotalBytes,
		FilesDone:  progress.FilesDone,
		TotalFiles: progress.TotalFiles,
		Repository: repositoryID,
		At:         now.UTC(),
	})
	if err != nil {
		e.log.Debug("record backup progress", "job_id", jobID, "error", err)
	}
}

// recordRestoreStage stores what a running restore is doing, and how far
// through it is where restic can say.
//
// It is called from the goroutine reading restic's output as well as from
// the restore itself, so it keeps the work small and never fails the
// restore: one that cannot write its progress is still one that is running.
//
// A change of stage is always written. Only the percentage inside a stage
// is throttled, because a stage is the part an operator is waiting to see
// change.
func (e *Engine) recordRestoreStage(restoreID, stage string, progress *resticrun.RestoreProgress) {
	record := nodestore.RestoreProgress{Stage: stage, At: time.Now().UTC()}
	if progress != nil {
		percent := progress.PercentDone * 100
		if percent > 100 {
			percent = 100
		}
		record.Percent, record.Known = percent, true
		record.BytesRestored = progress.BytesRestored
		record.TotalBytes = progress.TotalBytes

		e.progressMu.Lock()
		last, seen := e.lastProgress[restoreID]
		now := time.Now()
		if seen && last.stage == stage &&
			percent-last.percent < progressFloor && now.Sub(last.at) < progressInterval {
			e.progressMu.Unlock()
			return
		}
		if e.lastProgress == nil {
			e.lastProgress = map[string]progressMark{}
		}
		e.lastProgress[restoreID] = progressMark{percent: percent, at: now, stage: stage}
		e.progressMu.Unlock()
	}

	if err := e.store.SetRestoreProgress(restoreID, record); err != nil {
		e.log.Debug("record restore progress", "restore_id", restoreID, "error", err)
	}
}

// forgetProgress drops what was remembered about a job that has finished,
// so the map does not grow for the life of the process.
func (e *Engine) forgetProgress(jobID string) {
	e.progressMu.Lock()
	delete(e.lastProgress, jobID)
	e.progressMu.Unlock()
}

// progressMark is the last progress written for one job.
type progressMark struct {
	percent float64
	at      time.Time
	// stage is set for a restore, which counts each part of an account
	// from zero: the throttle must let a new stage through even when its
	// percentage is lower than what the last one reached.
	stage string
}

// RetainedOutput lists what finished restores have left in the work
// directory, so an operator can see what is using the disk.
func (e *Engine) RetainedOutput() ([]staging.Output, error) { return e.staging.Retained() }

// DeleteOutput removes one piece of collected output.
//
// Only finished output can be removed this way: work in progress is not
// listed, and Release refuses any path outside the work directory.
func (e *Engine) DeleteOutput(key string) error {
	outputs, err := e.staging.Retained()
	if err != nil {
		return err
	}
	for _, output := range outputs {
		if output.Key != key {
			continue
		}
		dir := output.Dir
		if err := e.staging.Release(&dir); err != nil {
			return err
		}
		e.log.Info("removed collected output", "key", key, "bytes", output.Bytes)
		return nil
	}
	return fmt.Errorf("node: there is no collected output called %q", key)
}

// RepositoryPassword is the password restic needs to read a repository.
//
// It exists to be written down. The destination holds nothing but
// ciphertext, and the key that unlocks it lives on this server: lose the
// server and lose the key, and the backups it made are unreadable by
// anyone, including this program. An operator who has this password and
// access to the destination can restore with restic alone.
func (e *Engine) RepositoryPassword(repositoryID string) (string, error) {
	repo, err := e.store.Repository(repositoryID)
	if err != nil {
		return "", err
	}
	password, err := e.openSecret(repo.PasswordSecretID)
	if err != nil {
		return "", err
	}
	return string(password), nil
}

// NoteRecoveryKey records that the password for a repository has been
// written down somewhere else, which is what stops the interface asking.
func (e *Engine) NoteRecoveryKey(repositoryID string) error {
	repo, err := e.store.Repository(repositoryID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	repo.RecoveryNotedAt = &now
	_, err = e.store.PutRepository(repo)
	return err
}

// RecoveryCard is everything needed to read a repository without this
// program: where it is, what unlocks it, and the commands that do it.
type RecoveryCard struct {
	Destination string
	Repository  string
	URI         string
	Password    string
	Hostname    string
	// ResticOptions is what restic needs to reach the destination as this
	// server does — for SFTP, the key and the pinned host key. Empty when
	// the destination needs nothing beyond its URI.
	ResticOptions string
	// SSHIdentityPath and SSHKnownHostsPath are where those files live on
	// this server, for a restore run from here.
	SSHIdentityPath   string
	SSHKnownHostsPath string
	// SSHPrivateKey and SSHHostKey are their contents, for a restore run
	// from a machine that has never seen this one. Empty for destinations
	// that are not reached over SSH.
	SSHPrivateKey string
	SSHHostKey    string
	SSHUser       string
	SSHHost       string
}

// Recovery assembles what an operator has to keep somewhere else.
func (e *Engine) Recovery(repositoryID string) (RecoveryCard, error) {
	repo, err := e.store.Repository(repositoryID)
	if err != nil {
		return RecoveryCard{}, err
	}
	dest, err := e.store.Destination(repo.DestinationID)
	if err != nil {
		return RecoveryCard{}, err
	}
	opened, err := e.OpenRepository(repositoryID, false)
	if err != nil {
		return RecoveryCard{}, err
	}
	uri, err := opened.Dest.URI(repo.Path)
	if err != nil {
		return RecoveryCard{}, err
	}
	password, err := e.RepositoryPassword(repositoryID)
	if err != nil {
		return RecoveryCard{}, err
	}
	card := RecoveryCard{
		Destination: dest.Name,
		Repository:  repo.Path,
		URI:         uri,
		Password:    password,
		Hostname:    e.settings.Hostname,
	}

	// An SFTP destination is reached with the key this server generated
	// for it, and with the host key it pinned when it first connected.
	// Recovery instructions that leave those out do not work.
	if options, err := opened.Dest.Options(); err == nil {
		card.ResticOptions = options["sftp.args"]
	}
	card.SSHIdentityPath = dest.Config["identity_file"]
	card.SSHKnownHostsPath = dest.Config["known_hosts_file"]
	card.SSHUser, card.SSHHost = dest.Config["user"], dest.Config["host"]
	if card.SSHIdentityPath != "" {
		if key, err := os.ReadFile(card.SSHIdentityPath); err == nil {
			card.SSHPrivateKey = strings.TrimSpace(string(key))
		}
	}
	if card.SSHKnownHostsPath != "" {
		if hosts, err := os.ReadFile(card.SSHKnownHostsPath); err == nil {
			card.SSHHostKey = strings.TrimSpace(string(hosts))
		}
	}
	return card, nil
}
