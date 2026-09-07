// Package maintenance performs repository upkeep from trusted
// infrastructure.
//
// It exists because destinations we control run rest-server with
// --append-only, which rejects deletes: nothing on a cPanel server can
// prune, so without a separate actor holding delete-capable credentials,
// repositories grow without bound. See docs/DESIGN.md §8.
package maintenance

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/shukiv/gniza/internal/destination"
	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/panel"
	"github.com/shukiv/gniza/internal/repobuild"
	"github.com/shukiv/gniza/internal/resticrun"
	"github.com/shukiv/gniza/internal/store"
	"github.com/shukiv/gniza/internal/vault"
)

// Kind names a maintenance operation. The values match the
// maintenance_kind enum.
const (
	KindProvision = "provision"
	KindForget    = "forget"
	KindCheck     = "check"
	KindDrill     = "drill"
)

// Runner performs upkeep on repositories.
type Runner struct {
	store  *store.Store
	vault  *vault.Vault
	restic *resticrun.Runner
	log    *slog.Logger
	// layout is the panel shape a drill rebuilds a snapshot into.
	//
	// The maintenance runner has no panel of its own -- it runs where the
	// repositories are reachable, not where the accounts live -- so it is
	// told which one to expect. One fleet running two panels will have to
	// record the panel against the server a snapshot came from; until a
	// second panel exists there is nothing to record.
	layout panel.Layout
}

// New builds a Runner.
func New(db *store.Store, v *vault.Vault, restic *resticrun.Runner, log *slog.Logger,
	layout panel.Layout) *Runner {
	if log == nil {
		log = slog.Default()
	}
	return &Runner{store: db, vault: v, restic: restic, log: log, layout: layout}
}

// ProvisionPending creates every repository that does not exist yet.
func (r *Runner) ProvisionPending(ctx context.Context) (int, error) {
	repos, err := r.store.RepositoriesNeedingInit(ctx)
	if err != nil {
		return 0, err
	}
	var created int
	for _, repo := range repos {
		if err := r.Provision(ctx, repo.ID); err != nil {
			r.log.Error("provision repository", "repository_id", repo.ID, "error", err)
			continue
		}
		created++
	}
	return created, nil
}

// Provision creates a repository with "restic init".
//
// If the repository has a chunker source, its parameters are copied from
// it. Chunker parameters are fixed at creation and can never change, so
// getting this wrong permanently rules out replicating between a server's
// repositories with "restic copy". See docs/DESIGN.md §7.
func (r *Runner) Provision(ctx context.Context, repositoryID string) error {
	return r.withRun(ctx, repositoryID, KindProvision, func(repo resticrun.Repository) (string, error) {
		stored, _, err := r.store.RepositoryWithDestination(ctx, repositoryID)
		if err != nil {
			return "", err
		}

		var source *resticrun.Repository
		if stored.ChunkerSourceRepoID != "" {
			opened, err := r.openRepository(ctx, stored.ChunkerSourceRepoID)
			if err != nil {
				return "", fmt.Errorf("maintenance: open chunker source: %w", err)
			}
			source = &opened
		}

		if err := r.restic.Init(ctx, repo, source); err != nil {
			return "", err
		}
		if err := r.store.MarkRepositoryInitialised(ctx, repositoryID); err != nil {
			return "", err
		}
		r.log.Info("repository provisioned",
			"repository_id", repositoryID, "path", stored.Path,
			"chunker_source", stored.ChunkerSourceRepoID)
		if stored.ChunkerSourceRepoID == "" {
			return "initialised as this server's chunker source", nil
		}
		return "initialised with chunker parameters from " + stored.ChunkerSourceRepoID, nil
	})
}

// Forget applies a retention policy and prunes.
//
// This is the operation an agent cannot perform: against an append-only
// destination, only these credentials may delete.
func (r *Runner) Forget(ctx context.Context, repositoryID string, retention store.Retention, prune bool) error {
	return r.withRun(ctx, repositoryID, KindForget, func(repo resticrun.Repository) (string, error) {
		incomplete, err := r.store.IncompleteSnapshotIDs(ctx, repositoryID)
		if err != nil {
			return "", err
		}
		return "", r.restic.Forget(ctx, repo, resticrun.ForgetSpec{
			ProtectedSnapshotIDs: incomplete,
			KeepLast:             retention.KeepLast,
			KeepDaily:            retention.KeepDaily,
			KeepWeekly:           retention.KeepWeekly,
			KeepMonthly:          retention.KeepMonthly,
			KeepYearly:           retention.KeepYearly,
			// A repository holds every account on its server, so
			// retention must be applied per account rather than to the
			// repository as a whole. The agent tags each snapshot with
			// its account and nothing job-specific, so grouping by tag
			// gives one group per account per payload mode.
			GroupBy: "host,tags",
			Prune:   prune,
		})
	})
}

// Check verifies repository integrity, optionally re-reading a fraction of
// the pack data. Reading data back costs the same bandwidth the backup did,
// which is why this runs here and not on the cPanel server.
func (r *Runner) Check(ctx context.Context, repositoryID string, readDataSubsetPercent int) error {
	return r.withRun(ctx, repositoryID, KindCheck, func(repo resticrun.Repository) (string, error) {
		if err := r.restic.Check(ctx, repo, resticrun.CheckSpec{
			ReadDataSubsetPercent: readDataSubsetPercent,
		}); err != nil {
			return "", err
		}
		if readDataSubsetPercent > 0 {
			return fmt.Sprintf("structure verified, %d%% of pack data re-read",
				readDataSubsetPercent), nil
		}
		return "structure verified", nil
	})
}

// withRun records a maintenance run around an operation so a failure is
// visible in history rather than only in a log.
//
// The operation may return a line of detail to store alongside the result;
// a drill uses it to record exactly which checks it made.
func (r *Runner) withRun(ctx context.Context, repositoryID, kind string,
	operation func(resticrun.Repository) (string, error)) error {

	repo, err := r.openRepository(ctx, repositoryID)
	if err != nil {
		return err
	}

	runID, err := r.store.StartMaintenanceRun(ctx, repositoryID, kind)
	if err != nil {
		return err
	}

	detail, opErr := operation(repo)
	status, output := string(job.StatusSuccess), detail
	if opErr != nil {
		status, output = string(job.StatusFailed), opErr.Error()
	}
	if err := r.store.FinishMaintenanceRun(ctx, runID, status, output); err != nil {
		r.log.Error("record maintenance run", "run_id", runID, "error", err)
	}
	return opErr
}

func (r *Runner) openRepository(ctx context.Context, repositoryID string) (resticrun.Repository, error) {
	sealed, err := r.store.SealedRepository(ctx, repositoryID)
	if err != nil {
		return resticrun.Repository{}, err
	}
	opened, err := repobuild.OpenForMaintenance(r.vault, repobuild.Sealed{
		DestinationType:    sealed.DestinationType,
		DestinationConfig:  sealed.DestinationConfig,
		CredentialsSealed:  sealed.CredentialsSealed,
		RepoPath:           sealed.RepositoryPath,
		RepoPasswordSealed: sealed.RepoPasswordSealed,
	})
	if err != nil {
		return resticrun.Repository{}, err
	}
	dest, err := destination.Build(opened.Spec)
	if err != nil {
		return resticrun.Repository{}, err
	}
	return resticrun.Repository{Dest: dest, Path: opened.Path, Password: opened.Password}, nil
}
