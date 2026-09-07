package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/shukiv/gniza/internal/cpanel"
	"github.com/shukiv/gniza/internal/destination"
	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/protocol"
	"github.com/shukiv/gniza/internal/reassemble"
	"github.com/shukiv/gniza/internal/resticrun"
	"github.com/shukiv/gniza/internal/staging"
)

// RunRestore brings an account, or named files from it, back from a
// snapshot.
//
// It always returns a report. Nothing is handed to cPanel unless the
// operator asked for it: materialising files is safe, overwriting a live
// account is not.
func (a *Agent) RunRestore(ctx context.Context, assignment protocol.RestoreAssignment) protocol.RestoreReport {
	report := protocol.RestoreReport{
		JobID: assignment.JobID,
		// Which attempt this is; the controller refuses a report from
		// an attempt whose lease it has already taken back.
		ClaimToken: assignment.ClaimToken,
		Status:     string(job.StatusFailed),
	}
	log := a.log.With("restore_job_id", assignment.JobID,
		"account", assignment.CPanelUser, "snapshot", assignment.SnapshotID)

	started := time.Now()
	defer func() { report.DurationSecs = time.Since(started).Seconds() }()
	restoreCtx, cancel := context.WithTimeout(ctx, a.TargetTimeout)
	defer cancel()

	repo, err := a.restoreSource(assignment)
	if err != nil {
		log.Error("build restore source", "error", err)
		report.Error = err.Error()
		return report
	}

	// Resolve the selected backup here too: fleet assignments may carry
	// only the current account's size, which can be far smaller (or zero).
	// Do this before superseding any previously collected output.
	snapshot, err := reassemble.FindSnapshot(restoreCtx, a.runner, reassemble.Request{
		Account: assignment.CPanelUser, SnapshotID: assignment.SnapshotID, Repo: repo,
	})
	if err != nil {
		report.Error = err.Error()
		return report
	}
	sourceBytes, err := a.runner.RestoreSize(restoreCtx, repo, snapshot)
	if err != nil {
		report.Error = err.Error()
		return report
	}
	estimate := max(assignment.SizeEstimate, reassemble.ArchiveBytes(sourceBytes))
	// The key carries the kind as well as the account: a granular restore
	// and a whole-account rebuild are different output, and one must not
	// silently replace the other while somebody is downloading it.
	//
	// And it carries this restore's own id. Keying by account alone put
	// every rebuild of an account at the same path, so a second restore
	// overwrote the first one's archive while the first one's record went
	// on pointing there -- and a download asked for by the older restore's
	// id handed over the newer snapshot, under the older one's date.
	//
	// The account and this restore's id are separated by "@" and not by a
	// hyphen: a hyphen is a character an account name may itself contain,
	// so "restore-c1-" is a prefix of every output belonging to an account
	// called "c1-x" as well, and superseding one restore would have thrown
	// away another account's. No cPanel account name contains "@".
	group := "restore-" + assignment.CPanelUser + "@"
	if assignment.Kind == protocol.RestoreItems {
		group = "items-" + assignment.CPanelUser + "@"
	}
	stagingKey := group + assignment.JobID
	// A previous restore of this account has been superseded by this one.
	// Its output is removed rather than allowed to accumulate on a disk
	// that also has to hold tonight's backup; what it does not do any more
	// is take this restore's place on the way.
	superseded, err := a.staging.SupersedeOutputs(group)
	if err != nil {
		log.Error("remove a superseded restore's output", "error", err)
		report.Error = err.Error()
		return report
	}
	for _, key := range superseded {
		log.Warn("removed a superseded restore's output", "key", key)
	}

	dir, err := a.staging.Allocate(stagingKey, estimate)
	if err != nil {
		log.Error("allocate restore staging", "error", err)
		report.Error = err.Error()
		return report
	}
	// Set when the run produced something to collect, which is then kept
	// rather than deleted.
	retain := false
	defer func() {
		if retain {
			return
		}
		if err := a.staging.Release(dir); err != nil {
			log.Error("release restore staging", "error", err)
		}
	}()

	// What the interface shows while this runs. One watch for the whole
	// restore, so every stage of it is reported against the same record.
	watch := a.watchRestore(assignment.JobID)

	switch assignment.Kind {
	case protocol.RestoreFiles:
		return a.restoreFiles(restoreCtx, log, assignment, repo, dir, report, watch)
	case protocol.RestoreItems:
		// A granular restore keeps what it produced, the same way a
		// rebuilt account archive does: it is there to be collected.
		result, keep := a.restoreItems(restoreCtx, log, assignment, repo, dir, report, watch)
		retain = keep
		return result
	case protocol.RestoreAccount, "":
		result, err := a.restoreAccount(restoreCtx, log, assignment, repo, dir, watch)
		if err != nil {
			report.Error = err.Error()
			return report
		}
		report.Status = string(job.StatusSuccess)
		report.BytesRestored = result.BytesRestored
		report.ArchivePath = result.ArchivePath
		report.Applied = assignment.Apply

		if !assignment.Apply {
			// The archive is the deliverable. Retaining it stops it
			// counting as work in progress — an uncollected download used
			// to hold a concurrency slot and block every other account —
			// and lets it survive a restart.
			retained, err := a.staging.Retain(dir)
			if err != nil {
				log.Error("retain the rebuilt archive", "error", err)
				report.Error = err.Error()
				report.Status = string(job.StatusFailed)
				return report
			}
			retain = true
			// Where the archive is inside the directory, not just its
			// name: a monolithic snapshot puts it one level down, in
			// archive/, and taking the base name alone reported a path
			// with nothing at it. The job then said success and the
			// interface had nothing to hand over.
			within, err := filepath.Rel(dir.Path, result.ArchivePath)
			if err != nil {
				within = filepath.Base(result.ArchivePath)
			}
			report.ArchivePath = filepath.Join(retained.Path, within)
			if _, err := os.Stat(report.ArchivePath); err != nil {
				// Reported success is a promise that there is something
				// to collect. Check it rather than make it.
				log.Error("the rebuilt archive is not where it was reported",
					"path", report.ArchivePath, "error", err)
				report.Error = fmt.Sprintf(
					"the rebuilt archive is not where it should be: %s", report.ArchivePath)
				report.Status = string(job.StatusFailed)
				return report
			}
			log.Info("archive ready to collect", "path", report.ArchivePath)
		}
		return report
	default:
		report.Error = fmt.Sprintf("agent: unknown restore kind %q", assignment.Kind)
		return report
	}
}

// restoreAccount rebuilds the account archive and, when asked, hands it to
// cPanel.
func (a *Agent) restoreAccount(ctx context.Context, log *slog.Logger,
	assignment protocol.RestoreAssignment, repo resticrun.Repository,
	dir *staging.Dir, watch *restoreWatch) (reassemble.Result, error) {

	result, err := reassemble.Run(ctx, a.runner, reassemble.Request{
		Account:    assignment.CPanelUser,
		SnapshotID: assignment.SnapshotID,
		WorkDir:    dir.Path,
		Repo:       repo,
		OnStage:    watch.StageFunc(),
		OnProgress: watch.ProgressFunc(),
		// A restore that goes straight to cPanel needs no tar: restorepkg
		// takes the account directory, and copies whatever it is given
		// into a temporary directory of its own either way. Repacking
		// first would put a second full copy of the account on the same
		// disk for nothing. A restore left to collect is the other way
		// round -- one file is what somebody downloads.
		TreeOnly: assignment.Apply,
	})
	if err != nil {
		log.Error("rebuild account archive", "error", err)
		return reassemble.Result{}, err
	}
	log.Info("account archive rebuilt",
		"archive", result.ArchivePath, "account_dir", result.RootDir,
		"mode", result.Mode, "bytes_restored", result.BytesRestored)

	if !assignment.Apply {
		// The default. An operator inspects the archive and applies it
		// themselves, or asks for a job with apply set.
		log.Info("archive left in place; restorepkg not run", "archive", result.ArchivePath)
		return result, nil
	}
	if !result.Complete() {
		return reassemble.Result{}, fmt.Errorf("agent: refusing to apply an incomplete account backup: %s", strings.Join(result.Skipped, ", "))
	}

	// Overwrite means "the account is already on this server, restore
	// into it". Asking for that when the account is gone -- the case
	// this program exists for -- tells cPanel to skip creating it, and
	// nothing is restored into nothing.
	_, lookupErr := a.provider.Account(ctx, assignment.CPanelUser)
	options := cpanel.ApplyOptions{
		Unrestricted: assignment.Unrestricted,
		Overwrite:    lookupErr == nil,
	}
	handOver := handOverPath(result)
	log.Warn("applying restore to the live account",
		"handing_over", handOver, "restricted", !options.Unrestricted)
	// The longest stage restic cannot count: cPanel's own restore reports
	// nothing until it is finished.
	watch.Stage("handing the account to cPanel's restore")
	transcript, err := a.provider.Apply(ctx, handOver, options)
	if err != nil {
		log.Error("restorepkg", "error", err, "transcript", transcript)
		return reassemble.Result{}, err
	}
	// Kept even when cPanel is happy. Most of its restore modules are
	// not fatal: one can fail, be added to a list of skipped items, and
	// the restore still finishes and exits zero. The transcript is then
	// the only account of what was actually put back, and it used to be
	// thrown away.
	log.Info("cPanel finished the restore", "transcript", transcript)

	if err := a.confirmRestored(ctx, log, assignment.CPanelUser, result); err != nil {
		return reassemble.Result{}, err
	}
	log.Info("account restored")
	return result, nil
}

// confirmRestored checks that what cPanel said it restored is actually on
// the server.
//
// cPanel's restore exits zero for things that are not a restored account.
// Seen on a live server: an account terminated a moment before the restore
// started came back "Success." from restorepkg and was not there
// afterwards -- the termination finished after the restore did. A restore
// that reports success and leaves nothing is worse than one that fails,
// because nobody looks again.
//
// The account being there is not enough on its own. Only two of cPanel's
// restore modules treat their own failure as fatal; every other one --
// the databases among them -- is recorded as a skipped item and the
// restore carries on and exits zero. Into an account that already exists,
// which is what a restore over a live account is, that came back as a
// clean success with the databases never put back. So the databases the
// archive holds are looked for on the account afterwards.
//
// What this cannot check is content: a database that was there before and
// was not overwritten looks exactly like one that was. Proving that needs
// something to compare against, and there is nothing here that has it.
func (a *Agent) confirmRestored(ctx context.Context, log *slog.Logger, user string,
	rebuilt reassemble.Result) error {

	account, err := a.provider.Account(ctx, user)
	if err != nil {
		log.Error("restorepkg reported success but the account is not here",
			"account", user, "error", err)
		return fmt.Errorf(
			"agent: cPanel reported the restore of %s as successful, but the "+
				"account is not on this server afterwards: %w", user, err)
	}
	if missing := missingDatabases(rebuilt.Databases(), account.Databases); len(missing) > 0 {
		log.Error("restorepkg reported success but did not put the databases back",
			"account", user, "missing", strings.Join(missing, ","))
		return fmt.Errorf(
			"agent: cPanel reported the restore of %s as successful, but %s "+
				"%s not on the account afterwards -- cPanel carries on when a "+
				"restore module fails, so this restore is not the account back",
			user, strings.Join(missing, ", "), plural(len(missing), "is", "are"))
	}
	return nil
}

// missingDatabases is what the archive holds and the account does not.
//
// cPanel lowercases nothing here: a database is named as it is named, and
// the comparison is exact so a difference is reported rather than
// explained away.
func missingDatabases(archived, present []string) []string {
	if len(archived) == 0 {
		return nil
	}
	have := make(map[string]bool, len(present))
	for _, name := range present {
		have[name] = true
	}
	var missing []string
	for _, name := range archived {
		if !have[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// restoreFiles pulls named paths out of a snapshot, keeping the paths they
// had, and leaves them where the operator asked.
func (a *Agent) restoreFiles(ctx context.Context, log *slog.Logger,
	assignment protocol.RestoreAssignment, repo resticrun.Repository,
	dir *staging.Dir, report protocol.RestoreReport,
	watch *restoreWatch) protocol.RestoreReport {

	if len(assignment.IncludePaths) == 0 {
		report.Error = "agent: a files restore needs at least one path"
		return report
	}

	target := assignment.TargetDir
	if target == "" {
		target = filepath.Join(dir.Path, "files")
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		report.Error = fmt.Sprintf("agent: create restore target: %v", err)
		return report
	}

	watch.Stage("reading the files out of the backup")
	restored, err := a.runner.Restore(ctx, repo, resticrun.RestoreSpec{
		SnapshotID: assignment.SnapshotID,
		Target:     target,
		// Include keeps the original directory structure, so the operator
		// can see where each file came from.
		Include:    assignment.IncludePaths,
		OnProgress: watch.ProgressFunc(),
	})
	if err != nil {
		log.Error("restore files", "error", err)
		report.Error = err.Error()
		return report
	}
	if restored.FilesRestored == 0 {
		report.Error = "agent: no files in the snapshot matched the requested paths"
		return report
	}

	log.Info("files restored", "target", target,
		"files", restored.FilesRestored, "bytes", restored.BytesRestored)
	report.Status = string(job.StatusSuccess)
	report.BytesRestored = restored.BytesRestored
	report.RestoredTo = target
	return report
}

func (a *Agent) restoreSource(assignment protocol.RestoreAssignment) (resticrun.Repository, error) {
	dest, err := destination.Build(assignment.Source.Spec)
	if err != nil {
		return resticrun.Repository{}, err
	}
	return resticrun.Repository{
		Dest:     dest,
		Path:     assignment.Source.RepoPath,
		Password: assignment.Source.RepoPassword,
	}, nil
}

// handOverPath is what cPanel's restore is given: the extracted account
// where there is one, and the archive otherwise.
//
// restorepkg takes either -- its usage lists
// "/path/to/extracted-cpuser-file" beside the archive forms -- and copies
// whatever it is given into a temporary directory of its own. Handing it
// the directory saves building a tar that would be a second full copy of
// the account on the same disk. A monolithic snapshot has no tree: it is
// cPanel's own archive, restored as one file.
func handOverPath(rebuilt reassemble.Result) string {
	if rebuilt.RootDir != "" {
		return rebuilt.RootDir
	}
	return rebuilt.ArchivePath
}
