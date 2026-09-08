// Command gniza-maintenance performs repository upkeep from trusted
// infrastructure.
//
// It exists because destinations we control run rest-server with
// --append-only, which rejects deletes: nothing running on a cPanel server
// can prune, so without this component repositories grow without bound. It
// holds the only delete-capable credentials in the system and must not run
// on a cPanel server. See docs/DESIGN.md §8.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/shukiv/gniza/internal/layout/cpmove"
	"github.com/shukiv/gniza/internal/layout/dabackup"
	"github.com/shukiv/gniza/internal/maintenance"
	"github.com/shukiv/gniza/internal/panel"
	"github.com/shukiv/gniza/internal/resticrun"
	"github.com/shukiv/gniza/internal/store"
	"github.com/shukiv/gniza/internal/vault"
)

func main() {
	databaseURL := flag.String("database-url", os.Getenv("GNIZA_DATABASE_URL"),
		"PostgreSQL connection string")
	masterKeyPath := flag.String("master-key", os.Getenv("GNIZA_MASTER_KEY"),
		"vault master key file")
	kind := flag.String("kind", "provision",
		"work to perform: provision, forget, check, drill")
	repositoryID := flag.String("repository", "",
		"repository to act on; empty means every eligible repository")
	readDataSubset := flag.Int("read-data-subset", 5,
		"percent of pack data to verify during check")
	prune := flag.Bool("prune", true, "remove unreferenced data after forget")
	account := flag.String("account", "",
		"account to rehearse for -kind drill; empty picks the newest snapshot")
	panelName := flag.String("panel", "cpanel",
		"the hosting panel whose servers wrote these snapshots: cpanel or directadmin")
	resticBinary := flag.String("restic", "restic", "path to the restic binary")
	runtimeDir := flag.String("runtime-dir", os.TempDir(),
		"directory for the transient restic password file")
	cacheDir := flag.String("restic-cache", "", "restic cache directory")
	caCert := flag.String("restic-cacert", "",
		"CA bundle restic should trust, for a destination behind a private CA")
	logLevel := flag.String("log-level", "info", "debug, info, warn or error")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, runConfig{
		databaseURL: *databaseURL, masterKeyPath: *masterKeyPath, kind: *kind,
		repositoryID: *repositoryID, readDataSubset: *readDataSubset, prune: *prune,
		resticBinary: *resticBinary, runtimeDir: *runtimeDir, cacheDir: *cacheDir,
		caCert: *caCert, account: *account, logLevel: *logLevel,
		panelName: *panelName,
	}); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "gniza-maintenance: %v\n", err)
		os.Exit(1)
	}
}

type runConfig struct {
	databaseURL    string
	masterKeyPath  string
	kind           string
	repositoryID   string
	readDataSubset int
	prune          bool
	resticBinary   string
	runtimeDir     string
	cacheDir       string
	caCert         string
	account        string
	logLevel       string
	panelName      string
}

// layoutFor is the layout of the panel this maintenance run is about.
//
// Maintenance runs away from the servers it serves and never talks to a
// panel, so unlike the agent it cannot ask a provider which one wrote a
// snapshot. It is told, and an empty answer means cPanel, which is what
// every deployment that predates the flag is running.
//
// A name nobody wrote a layout for is refused rather than defaulted:
// checking a DirectAdmin snapshot against cPanel's layout fails on every
// account, with an error about a missing cpmove tree that says nothing
// about the actual mistake.
func layoutFor(name string) (panel.Layout, error) {
	switch name {
	case "", "cpanel":
		return cpmove.Layout{}, nil
	case "directadmin":
		return dabackup.Layout{}, nil
	default:
		return nil, fmt.Errorf("unknown -panel %q: cpanel or directadmin", name)
	}
}

func run(ctx context.Context, cfg runConfig) error {
	switch cfg.kind {
	case maintenance.KindProvision, maintenance.KindForget,
		maintenance.KindCheck, maintenance.KindDrill:
	default:
		return fmt.Errorf("unknown -kind %q", cfg.kind)
	}
	layout, err := layoutFor(cfg.panelName)
	if err != nil {
		return err
	}
	if cfg.databaseURL == "" {
		return errors.New("-database-url is required")
	}
	if cfg.masterKeyPath == "" {
		return errors.New("-master-key is required")
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.logLevel)); err != nil {
		level = slog.LevelInfo
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	db, err := store.Open(ctx, cfg.databaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	key, err := vault.LoadMasterKey(cfg.masterKeyPath)
	if err != nil {
		return err
	}
	v, err := vault.New(key)
	if err != nil {
		return err
	}

	restic := resticrun.New(resticrun.Config{
		Binary:     cfg.resticBinary,
		RuntimeDir: cfg.runtimeDir,
		CacheDir:   cfg.cacheDir,
		CACertPath: cfg.caCert,
	}, nil)
	runner := maintenance.New(db, v, restic, log, layout)

	switch cfg.kind {
	case maintenance.KindProvision:
		return provision(ctx, db, runner, cfg.repositoryID)
	case maintenance.KindForget:
		return forget(ctx, db, runner, cfg.repositoryID, cfg.prune)
	case maintenance.KindDrill:
		return drill(ctx, db, runner, cfg.repositoryID, cfg.account)
	default:
		return check(ctx, db, runner, cfg.repositoryID, cfg.readDataSubset)
	}
}

func provision(ctx context.Context, db *store.Store, runner *maintenance.Runner, repositoryID string) error {
	if repositoryID != "" {
		return runner.Provision(ctx, repositoryID)
	}
	created, err := runner.ProvisionPending(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("provisioned %d repositories\n", created)
	return nil
}

func forget(ctx context.Context, db *store.Store, runner *maintenance.Runner,
	repositoryID string, prune bool) error {

	policies, err := db.ListPolicies(ctx)
	if err != nil {
		return err
	}
	if len(policies) == 0 {
		return errors.New("no policies exist, so no retention is defined")
	}
	// Retention lives on the policy; a repository can be a target of
	// several, so the most generous keep wins. Deleting a snapshot one
	// policy still wants would be unrecoverable.
	retention := widestRetention(policies)

	repositories, err := targetRepositories(ctx, db, repositoryID)
	if err != nil {
		return err
	}
	var failures int
	for _, id := range repositories {
		if err := runner.Forget(ctx, id, retention, prune); err != nil {
			fmt.Fprintf(os.Stderr, "repository %s: %v\n", id, err)
			failures++
		}
	}
	if failures > 0 {
		return fmt.Errorf("%d of %d repositories failed", failures, len(repositories))
	}
	fmt.Printf("applied retention to %d repositories\n", len(repositories))
	return nil
}

func check(ctx context.Context, db *store.Store, runner *maintenance.Runner,
	repositoryID string, readDataSubset int) error {

	repositories, err := targetRepositories(ctx, db, repositoryID)
	if err != nil {
		return err
	}
	var failures int
	for _, id := range repositories {
		if err := runner.Check(ctx, id, readDataSubset); err != nil {
			fmt.Fprintf(os.Stderr, "repository %s: %v\n", id, err)
			failures++
		}
	}
	if failures > 0 {
		return fmt.Errorf("%d of %d repositories failed the check", failures, len(repositories))
	}
	fmt.Printf("checked %d repositories\n", len(repositories))
	return nil
}

// drill rehearses a restore. An untested backup is not a backup.
func drill(ctx context.Context, db *store.Store, runner *maintenance.Runner,
	repositoryID, account string) error {

	repositories, err := targetRepositories(ctx, db, repositoryID)
	if err != nil {
		return err
	}
	var failures int
	for _, id := range repositories {
		result, err := runner.Drill(ctx, maintenance.DrillRequest{
			RepositoryID: id, Account: account,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "repository %s: %v\n", id, err)
			failures++
			continue
		}
		fmt.Printf("repository %s: restored %s from %s (%s) - %s\n",
			id, result.Account, result.SnapshotID[:min(12, len(result.SnapshotID))],
			result.Mode, strings.Join(result.Checks, "; "))
	}
	if failures > 0 {
		return fmt.Errorf("%d of %d drills failed", failures, len(repositories))
	}
	return nil
}

func targetRepositories(ctx context.Context, db *store.Store, repositoryID string) ([]string, error) {
	if repositoryID != "" {
		return []string{repositoryID}, nil
	}
	repos, err := db.ListRepositories(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, repo := range repos {
		// An unprovisioned repository has nothing to prune or verify.
		if repo.InitialisedAt != nil {
			ids = append(ids, repo.ID)
		}
	}
	return ids, nil
}

func widestRetention(policies []store.Policy) store.Retention {
	var widest store.Retention
	for _, policy := range policies {
		widest.KeepLast = max(widest.KeepLast, policy.Retention.KeepLast)
		widest.KeepDaily = max(widest.KeepDaily, policy.Retention.KeepDaily)
		widest.KeepWeekly = max(widest.KeepWeekly, policy.Retention.KeepWeekly)
		widest.KeepMonthly = max(widest.KeepMonthly, policy.Retention.KeepMonthly)
		widest.KeepYearly = max(widest.KeepYearly, policy.Retention.KeepYearly)
	}
	return widest
}
