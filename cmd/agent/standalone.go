package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/shukiv/gniza/internal/directadmin"
	"github.com/shukiv/gniza/internal/node"
	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/plain"
	"github.com/shukiv/gniza/internal/vault"
	"github.com/shukiv/gniza/internal/webui"
)

// runStandalone serves one cPanel server with no controller: local state,
// local scheduling, and an interface behind the WHM plugin.
// See docs/adr/0007-standalone-mode.md.
func runStandalone(ctx context.Context, cfg config, log *slog.Logger) error {
	store, err := nodestore.Open(cfg.statePath)
	if err != nil {
		return err
	}
	defer store.Close()

	// A server that was installed when this program was called cprest has
	// the old directory names written into its state file. The installer
	// renames the directories; this renames what the state file says about
	// them, before anything reads a path out of it.
	if moved, err := store.MigrateLegacyPaths(); err != nil {
		return err
	} else if moved > 0 {
		log.Info("moved stored paths to the new directory names", "records", moved)
	}

	v, err := openOrCreateVault(cfg.masterKeyPath, log)
	if err != nil {
		return err
	}

	settings, err := store.Settings()
	if err != nil {
		return err
	}
	// Command-line values win over stored ones, so an operator can correct
	// a bad setting without being able to reach the interface.
	if cfg.stagingRoot != "" {
		settings.StagingRoot = cfg.stagingRoot
	}
	if cfg.resticBinary != "" {
		settings.ResticBinary = cfg.resticBinary
	}
	if cfg.resticCache != "" {
		settings.ResticCache = cfg.resticCache
	}
	if cfg.resticCACert != "" {
		settings.ResticCACert = cfg.resticCACert
	}
	if settings.Hostname == "" {
		settings.Hostname = cfg.hostname
	}
	if cfg.maxConcurrent > 0 {
		settings.MaxConcurrent = cfg.maxConcurrent
	}
	settings.SafetyMargin = cfg.safetyMargin
	if err := store.SaveSettings(settings); err != nil {
		return err
	}

	provider, err := buildProvider(cfg, log)
	if err != nil {
		return err
	}
	// A plain server keeps what the operator chose to back up in the
	// store, beside everything else, so the pages can change it.
	if chooser, ok := provider.(*plain.Provider); ok {
		chooser.Catalog = store
	}

	engine, err := node.New(node.Config{
		Store: store, Vault: v, Provider: provider, Log: log,
		HookSpool: cfg.hookSpoolDir, LogLevel: cfg.level,
	})
	if err != nil {
		return err
	}
	// The stored level is what the operator last chose, and the flag is
	// what the unit file says. The stored one wins: it is the newer of the
	// two, and it is the one they can change without editing a unit.
	engine.ApplyStoredLogLevel()
	if err := engine.ProbeCapabilities(ctx); err != nil {
		// A host whose pkgacct cannot be probed can still be configured;
		// the interface shows the gap.
		log.Error("probe pkgacct", "error", err)
	}
	// What the hooks could not deliver while this service was down, put
	// back before anything reads the account list. An account created or
	// removed in that window cannot be recovered from the list itself,
	// and recording it late is the whole point of the spool.
	if err := engine.ReplayHookSpool(); err != nil {
		log.Error("replay the cPanel events left by hooks", "error", err)
	}
	// Which unix account each cPanel name means, recorded before anything
	// serves a customer. Everything that decides whether a name has
	// changed hands leans on this having happened.
	if err := engine.BackfillIdentities(ctx); err != nil {
		log.Warn("record which unix account each cPanel name means", "error", err)
	}
	if created, err := engine.EnsureProvisioned(ctx); err != nil {
		log.Error("create repositories", "error", err)
	} else if created > 0 {
		log.Info("created repositories", "count", created)
	}

	uiOptions, daSocket, err := directAdminUI(cfg)
	if err != nil {
		return err
	}
	if cfg.webListen != "" {
		uiOptions = append(uiOptions, webui.WithBrowser(webui.BrowserConfig{
			Listen: cfg.webListen, PasswordFile: cfg.webPasswordFile,
			CertFile: cfg.webCertFile, KeyFile: cfg.webKeyFile,
			Hostname: cfg.hostname,
		}))
	}
	ui, err := webui.New(engine, log, uiOptions...)
	if err != nil {
		return err
	}

	errs := make(chan error, 5)
	go func() { errs <- ui.Listen(ctx, cfg.socketPath) }()
	// The browser door, on a server with no panel. A port that cannot be
	// opened -- no password set yet, the address in use -- is said in the
	// log and costs the backups nothing: the socket and the terminal
	// still work, and a service that exited over its own front door
	// would restart into the same refusal forever.
	if cfg.webListen != "" {
		go func() {
			if err := ui.ListenBrowser(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Error("the browser interface is not available", "address", cfg.webListen, "error", err)
			}
		}()
	}
	// DirectAdmin's own page. It is a third socket rather than the
	// root-only one above widened: that one's mode is its authorization,
	// and this one's owner is a service account that is nobody in
	// particular, so a verified DirectAdmin session takes its place.
	if daSocket != "" {
		go func() { errs <- ui.ListenDirectAdmin(ctx, daSocket, cfg.daPluginUser) }()
	}
	// The account-facing interface, which cPanel users reach through
	// their own plugin. It is a separate socket because it answers a
	// different question: not "what is on this server" but "what is mine".
	if cfg.userSocketPath != "" {
		go func() { errs <- ui.ListenUser(ctx, cfg.userSocketPath) }()
	}
	if cfg.lifecycleSocketPath != "" {
		go func() { errs <- ui.ListenLifecycle(ctx, cfg.lifecycleSocketPath) }()
	}
	go func() { errs <- engine.Run(ctx) }()

	log.Info("standalone node running",
		"state", cfg.statePath, "socket", cfg.socketPath,
		"staging_root", settings.StagingRoot)

	err = <-errs
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// openOrCreateVault loads the local master key, creating one on first run.
//
// Without it there is nowhere safe to put the credentials an operator types
// into the interface, so the node cannot start.
func openOrCreateVault(path string, log *slog.Logger) (*vault.Vault, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		generated, err := vault.GenerateMasterKey()
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(generated+"\n"), 0o600); err != nil {
			return nil, fmt.Errorf("write master key %s: %w", path, err)
		}
		log.Warn("generated a new encryption key; back it up, "+
			"because without it the stored destination credentials cannot be read",
			"path", path)
	}

	key, err := vault.LoadMasterKey(path)
	if err != nil {
		return nil, err
	}
	return vault.New(key)
}

// directAdminUI is how the interface is built and where DirectAdmin's
// plugin reaches it. On a cPanel server it is nothing at all.
//
// The panel's address is read from DirectAdmin's own configuration
// rather than assumed: what travels to it is a live session credential,
// its certificate is verified, and that certificate carries the name
// written there. A server that cannot say where its panel is does not
// start, because the alternative is a socket that refuses every request
// for a reason no operator would guess.
func directAdminUI(cfg config) ([]webui.Option, string, error) {
	if cfg.panelName != "directadmin" || cfg.daSocketPath == "" || cfg.daPluginUser == "" {
		return nil, "", nil
	}
	panelURL, err := directadmin.PanelURL(cfg.daConfPath)
	if err != nil {
		return nil, "", err
	}
	return []webui.Option{webui.WithDirectAdmin(panelURL)}, cfg.daSocketPath, nil
}
