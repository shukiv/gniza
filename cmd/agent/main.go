// Command gniza-agent runs on a cPanel server. It long-polls the
// controller for backup jobs, stages a payload once, and uploads it to
// every repository the job names.
//
// The agent never receives delete-capable credentials: retention and
// pruning belong to gniza-maintenance. See docs/DESIGN.md §8.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/shukiv/gniza/internal/agent"
	"github.com/shukiv/gniza/internal/cpanel"
	"github.com/shukiv/gniza/internal/directadmin"
	"github.com/shukiv/gniza/internal/hookspool"
	"github.com/shukiv/gniza/internal/layout/dabackup"
	"github.com/shukiv/gniza/internal/panel"
	"github.com/shukiv/gniza/internal/resticrun"
	"github.com/shukiv/gniza/internal/staging"
)

type config struct {
	controllerURL string
	clientCert    string
	clientKey     string
	caBundle      string
	stagingRoot   string
	runtimeDir    string
	maxConcurrent int
	safetyMargin  float64
	resticBinary  string
	resticCache   string
	resticCACert  string
	pollInterval  time.Duration
	targetTimeout time.Duration
	hostname      string
	logLevel      string
	fakeRoot      string
	panelName     string
	preflightOnly bool

	standalone          bool
	statePath           string
	socketPath          string
	userSocketPath      string
	masterKeyPath       string
	lifecycleSocketPath string
	daSocketPath        string
	daPluginUser        string
	daConfPath          string
	daReadHomeInPlace   bool
	cpanelHookEvent     string
	panelHookEvent      string
	hookSpoolDir        string
	cpanelHookDescribe  bool
	certifyArchive      string
	certifyUser         string
	certifyIsolatedHost bool
	// level is the running log level, shared with the handler the logger
	// was built on so the interface can move it.
	level *slog.LevelVar
}

func main() {
	cfg := parseFlags()
	log, logLevel := newLogger(cfg.logLevel)
	cfg.level = logLevel
	if cfg.cpanelHookDescribe {
		if err := writeCPanelHookDescription(os.Stdout); err != nil {
			log.Error("describe cPanel lifecycle hooks", "error", err)
			os.Exit(1)
		}
		return
	}
	if cfg.cpanelHookEvent == "" {
		cfg.cpanelHookEvent = cfg.panelHookEvent
	}
	if cfg.cpanelHookEvent != "" {
		// Read before the call, not after it: reaching an unreachable
		// service takes the socket timeout, and the event happened when
		// cPanel ran this, not when it gave up.
		happenedAt := time.Now().UTC()
		payload, err := runCPanelHook(cfg.lifecycleSocketPath, cfg.cpanelHookEvent)
		if err != nil {
			if cfg.cpanelHookEvent == "remove-pre" {
				if detail, denied := blockingHookFailure(err); denied {
					fmt.Printf("0 BAILOUT Gniza blocked account removal: %s\n", hookMessage(detail))
					log.Warn("cPanel account removal blocked", "reason", err)
					os.Exit(1)
				}
				// Do not wedge WHM account administration merely because the
				// backup service is restarting or unavailable. A reachable
				// service makes policy failures explicit above; infrastructure
				// failures are logged and deliberately fail open.
				fmt.Println("1 Gniza termination check unavailable; account removal allowed")
				log.Error("cPanel account removal check unavailable; allowed removal", "error", err)
				return
			}
			// The same reasoning as the blocking hook above, and for
			// the same reason: a stopped or restarting service must
			// not make every account create, modify or suspend on
			// this server report a failed hook. What was missed is
			// reconciled the next time the service looks.
			//
			// Except that two of these cannot be worked out again by
			// looking. A username deleted and recreated while the
			// service is down, onto the same uid, leaves an account
			// list identical to the one that was there before -- so
			// the create and the remove are written down here, and
			// the service replays them before it reconciles
			// anything. Failing to write one down is the one case
			// that has to be reported: it is the difference between
			// deferred and lost.
			//
			// A service that answered with a 5xx is in the same
			// position as one that did not answer at all: it did not
			// record the event, and it did not decide anything. Only
			// its own decision -- a 4xx -- is final enough to drop an
			// account boundary on.
			if recordableFailure(cfg.cpanelHookEvent, err) {
				path, spoolErr := spoolCPanelHook(
					cfg.hookSpoolDir, cfg.cpanelHookEvent, payload, happenedAt)
				if spoolErr != nil {
					fmt.Println("0 Gniza could not record this account event; " +
						"back up and restore for this account may show the wrong owner")
					log.Error("could not record a cPanel account event for replay",
						"event", cfg.cpanelHookEvent, "error", spoolErr)
					os.Exit(1)
				}
				fmt.Println("1 Gniza lifecycle recorded; " +
					"the backup service could not take it now and will replay it")
				log.Warn("recorded a cPanel account event for replay",
					"event", cfg.cpanelHookEvent, "spooled", path, "error", err)
				return
			}
			if !serviceAnswered(err) {
				fmt.Println("1 Gniza lifecycle deferred; the backup service is unavailable")
				log.Error("cPanel lifecycle hook could not reach the service; deferred",
					"event", cfg.cpanelHookEvent, "error", err)
				return
			}
			fmt.Println("0 Gniza lifecycle reconciliation failed")
			log.Error("cPanel lifecycle hook failed", "error", err)
			os.Exit(1)
		}
		fmt.Println("1 Gniza lifecycle reconciled")
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if cfg.certifyArchive != "" {
		report, err := runLiveCertification(ctx, cfg, log)
		if encodeErr := json.NewEncoder(os.Stdout).Encode(report); encodeErr != nil {
			log.Error("write live certification report", "error", encodeErr)
			os.Exit(1)
		}
		if err != nil {
			log.Error("live restore certification failed", "error", err)
			os.Exit(1)
		}
		log.Info("live restore certification passed", "archive", cfg.certifyArchive)
		return
	}

	if err := run(ctx, cfg, log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("agent stopped", "error", err)
		os.Exit(1)
	}
}

func parseFlags() config {
	var cfg config
	hostname, _ := os.Hostname()

	flag.StringVar(&cfg.controllerURL, "controller", "", "controller base URL (https)")
	flag.StringVar(&cfg.clientCert, "client-cert", "", "mTLS client certificate")
	flag.StringVar(&cfg.clientKey, "client-key", "", "mTLS client private key")
	flag.StringVar(&cfg.caBundle, "ca-bundle", "", "CA bundle used to verify the controller")
	flag.StringVar(&cfg.stagingRoot, "staging-root", "/var/lib/gniza/staging",
		"directory pkgacct stages into; size this volume deliberately")
	flag.StringVar(&cfg.runtimeDir, "runtime-dir", "/run/gniza",
		"directory for the transient restic password file")
	flag.IntVar(&cfg.maxConcurrent, "max-concurrent", 1, "accounts staged at once")
	flag.Float64Var(&cfg.safetyMargin, "safety-margin", 0.2,
		"extra free space required beyond the payload estimate, as a fraction")
	flag.StringVar(&cfg.resticBinary, "restic", "restic", "path to the restic binary")
	flag.StringVar(&cfg.resticCache, "restic-cache", "/var/cache/gniza/restic",
		"restic cache directory; a warm cache markedly speeds up repeat backups")
	flag.StringVar(&cfg.resticCACert, "restic-cacert", "",
		"CA bundle restic should trust, for a destination behind a private CA")
	flag.DurationVar(&cfg.pollInterval, "poll-interval", 5*time.Second,
		"pause after an empty poll")
	flag.DurationVar(&cfg.targetTimeout, "target-timeout", 4*time.Hour,
		"how long one repository upload may take before it is abandoned")
	flag.StringVar(&cfg.hostname, "hostname", hostname, "hostname reported to the controller")
	flag.StringVar(&cfg.logLevel, "log-level", "info", "debug, info, warn or error")
	flag.StringVar(&cfg.panelName, "panel", "cpanel",
		"the hosting panel this server runs: cpanel, or directadmin, which is "+
			"unfinished and refuses what it cannot do (see ADR 0019)")
	flag.StringVar(&cfg.fakeRoot, "fake-cpanel-root", "",
		"use a synthetic cPanel provider rooted here, for development without cPanel")
	flag.BoolVar(&cfg.preflightOnly, "preflight", false, "check local prerequisites and exit")
	flag.BoolVar(&cfg.standalone, "standalone", false,
		"run this server on its own, with local state and the WHM interface, and no controller")
	flag.StringVar(&cfg.statePath, "state", "/var/lib/gniza/state.db",
		"standalone: where this server keeps its own configuration and history")
	flag.StringVar(&cfg.userSocketPath, "user-socket", "/var/run/gniza/account/user.sock",
		"unix socket the cPanel account interface listens on")
	flag.StringVar(&cfg.socketPath, "socket", "/var/run/gniza/admin/ui.sock",
		"standalone: unix socket the WHM plugin connects to")
	flag.StringVar(&cfg.masterKeyPath, "master-key", "/etc/gniza/master.key",
		"standalone: key that encrypts stored destination credentials")
	flag.StringVar(&cfg.lifecycleSocketPath, "lifecycle-socket", "/var/run/gniza/hooks/lifecycle.sock",
		"root-only socket used by cPanel account lifecycle hooks")
	// DirectAdmin will not run a plugin as root, so its page cannot open
	// the root-only socket above. It gets one of its own, owned by the
	// account named in plugin.conf's admin_run_as, where a DirectAdmin
	// session takes the place of the uid. See docs/adr/0020.
	flag.StringVar(&cfg.daSocketPath, "directadmin-socket", "/var/run/gniza/directadmin/plugin.sock",
		"directadmin: unix socket the DirectAdmin plugin connects to; empty serves no plugin")
	flag.StringVar(&cfg.daPluginUser, "directadmin-plugin-user", "gniza-plugin",
		"directadmin: the account plugin.conf runs the page as, which owns that socket")
	flag.StringVar(&cfg.daConfPath, "directadmin-conf", directadmin.DefaultConfPath,
		"directadmin: where DirectAdmin records the name and port its own panel answers on")
	// Off until a restore drill on this server has proved the rebuild.
	// See ADR 0021: a backup in this shape is put back together out of
	// the tree restic restored rather than copied out of a manifest, and
	// a server should be moved onto it one at a time and knowingly.
	flag.BoolVar(&cfg.daReadHomeInPlace, "directadmin-read-home-in-place", false,
		"directadmin: back up everything except the account's own files and read "+
			"/home where it lies, instead of writing the whole account to disk and "+
			"taking it apart again every night (see ADR 0021)")
	flag.StringVar(&cfg.hookSpoolDir, "hook-spool", hookspool.DefaultDir,
		"where a cPanel lifecycle hook leaves an account event this service was not running to hear")
	flag.StringVar(&cfg.cpanelHookEvent, "cpanel-hook", "",
		"internal: forward a create, modify, suspend, unsuspend, remove-pre or remove hook")
	// The same thing under a name that is not one panel's. The original
	// spelling stays because installed cPanel hooks name it, and a hook
	// registered on a live server is not a string this program gets to
	// change.
	flag.StringVar(&cfg.panelHookEvent, "panel-hook", "",
		"internal: forward an account lifecycle event from the panel's own hook")
	flag.BoolVar(&cfg.cpanelHookDescribe, "describe", false,
		"internal: describe cPanel Standardized Hooks")
	flag.StringVar(&cfg.certifyArchive, "certify-live-archive", "",
		"restore an archive into a disposable cPanel account and remove it")
	flag.StringVar(&cfg.certifyUser, "certify-user", "",
		"disposable cPanel username for live certification")
	flag.BoolVar(&cfg.certifyIsolatedHost, "certify-isolated-host", false,
		"confirm live certification is running on an isolated cPanel host")
	flag.Parse()
	return cfg
}

type liveCertificationReport struct {
	Archive        string    `json:"archive"`
	DisposableUser string    `json:"disposable_user"`
	StartedAt      time.Time `json:"started_at"`
	FinishedAt     time.Time `json:"finished_at"`
	Passed         bool      `json:"passed"`
	Checks         []string  `json:"checks,omitempty"`
	Error          string    `json:"error,omitempty"`
}

func runLiveCertification(ctx context.Context, cfg config, log *slog.Logger) (report liveCertificationReport, returnErr error) {
	report.Archive = filepath.Clean(cfg.certifyArchive)
	report.DisposableUser = cfg.certifyUser
	report.StartedAt = time.Now().UTC()
	defer func() {
		report.FinishedAt = time.Now().UTC()
		report.Passed = returnErr == nil
		if returnErr != nil {
			report.Error = returnErr.Error()
		}
	}()
	if !cfg.certifyIsolatedHost {
		return report, errors.New("-certify-isolated-host is required: certification creates and removes a cPanel account")
	}
	if cfg.certifyUser == "" {
		return report, errors.New("-certify-user is required")
	}
	provider, err := buildProvider(cfg, log)
	if err != nil {
		return report, err
	}
	certifier, ok := provider.(panel.Certifier)
	if !ok {
		return report, errors.New("this cPanel provider cannot certify restores")
	}
	if err := certifier.Certify(ctx, cfg.certifyArchive, cfg.certifyUser); err != nil {
		return report, err
	}
	report.Checks = []string{
		"restricted restorepkg accepted the archive",
		"DNS zone updates were disabled",
		"cPanel registered the disposable account and its home directory",
		"removeacct removed the disposable account",
	}
	return report, nil
}

func run(ctx context.Context, cfg config, log *slog.Logger) error {
	resticVersion, err := startupRestic(ctx, cfg.resticBinary, cfg.preflightOnly, log)
	if err != nil {
		return err
	}
	if err := ensureDir(cfg.stagingRoot); err != nil {
		return err
	}
	if err := ensureDir(cfg.runtimeDir); err != nil {
		return err
	}
	// See node.New: startup is where a password file left by a killed
	// process can be removed without racing a live one.
	if swept, err := resticrun.SweepPasswordDirs(cfg.runtimeDir); err != nil {
		log.Warn("clear password files left by an interrupted run", "error", err)
	} else if swept > 0 {
		log.Info("cleared password files left by an interrupted run", "count", swept)
	}

	stagingManager := &staging.Manager{
		Root:              cfg.stagingRoot,
		SafetyMarginRatio: cfg.safetyMargin,
		MaxConcurrent:     cfg.maxConcurrent,
	}
	available, err := staging.AvailableBytes(cfg.stagingRoot)
	if err != nil {
		return err
	}

	provider, err := buildProvider(cfg, log)
	if err != nil {
		return err
	}
	capabilities, err := provider.Capabilities(ctx)
	if err != nil {
		return err
	}

	if cfg.standalone && !cfg.preflightOnly {
		return runStandalone(ctx, cfg, log)
	}

	if cfg.preflightOnly {
		fmt.Printf("restic:       %s\n", resticVersion)
		fmt.Printf("staging root: %s (%d bytes available)\n", cfg.stagingRoot, available)
		fmt.Printf("pkgacct:      nocompress=%q skiphomedir=%q skipdb=%q\n",
			capabilities.NoCompressFlag, capabilities.SkipHomedirFlag, capabilities.SkipDBFlag)
		fmt.Println("preflight ok")
		return nil
	}

	if cfg.controllerURL == "" {
		return errors.New("-controller is required")
	}
	client, err := agent.NewClient(agent.ClientConfig{
		BaseURL:        cfg.controllerURL,
		ClientCertPath: cfg.clientCert,
		ClientKeyPath:  cfg.clientKey,
		CABundlePath:   cfg.caBundle,
		// The controller holds a job request open, so polling needs a
		// deadline longer than its long-poll window.
		Timeout: 5 * time.Minute,
	})
	if err != nil {
		return err
	}

	runner := resticrun.New(resticrun.Config{
		Log:        log,
		Binary:     cfg.resticBinary,
		RuntimeDir: cfg.runtimeDir,
		CacheDir:   cfg.resticCache,
		CACertPath: cfg.resticCACert,
	}, nil)

	worker := agent.New(agent.Config{
		Client: client, Provider: provider, Staging: stagingManager,
		Runner: runner, Log: log, Hostname: cfg.hostname,
		ResticVersion: resticVersion, PollInterval: cfg.pollInterval,
		TargetTimeout: cfg.targetTimeout,
	})

	if err := worker.Enrol(ctx); err != nil {
		return fmt.Errorf("enrol: %w", err)
	}
	if err := worker.CleanStaleStaging(); err != nil {
		log.Error("clean stale staging", "error", err)
	}

	log.Info("agent running", "hostname", cfg.hostname,
		"controller", cfg.controllerURL, "staging_root", cfg.stagingRoot)
	return worker.Run(ctx)
}

func buildProvider(cfg config, log *slog.Logger) (panel.Provider, error) {
	if cfg.panelName == "directadmin" {
		if cfg.fakeRoot != "" {
			return nil, fmt.Errorf("there is no synthetic DirectAdmin to run against")
		}
		// Said once, loudly, where an operator setting this up will see
		// it: the layout was read out of documentation, and the restore
		// path has never been run against a DirectAdmin server.
		log.Warn("the DirectAdmin provider is unfinished", "detail", dabackup.Provisional)
		provider := &directadmin.Real{Log: log, ReadHomeInPlace: cfg.daReadHomeInPlace}
		// A run killed mid-stage leaves the account's archive behind, and
		// what it leaves is measured in gigabytes on the disk the next
		// night needs. The sweep takes each account's own lock, so a run
		// in flight keeps its workspace.
		if swept, err := provider.SweepNativeWorkspaces(); err != nil {
			log.Warn("clear native workspaces left by an interrupted run", "error", err)
		} else if swept > 0 {
			log.Info("cleared native workspaces left by an interrupted run", "count", swept)
		}
		return provider, nil
	}
	if cfg.panelName != "" && cfg.panelName != "cpanel" {
		return nil, fmt.Errorf("unknown panel %q: cpanel or directadmin", cfg.panelName)
	}
	if cfg.fakeRoot == "" {
		// The provider writes only at debug, so this costs nothing until
		// somebody turns the level up -- and then it is the commands
		// themselves, which is what a failure on a real cPanel host needs.
		return &cpanel.Real{Log: log}, nil
	}
	// A synthetic provider makes the agent runnable on a developer
	// machine and in the end-to-end suite, where no cPanel exists.
	if err := ensureDir(cfg.fakeRoot); err != nil {
		return nil, err
	}
	return &cpanel.Fake{Root: cfg.fakeRoot}, nil
}

// startupRestic probes restic, and decides whether one that will not run
// should stop the process.
//
// It should not. A live server spent three and a half minutes restarting
// every five seconds because its installer was replacing
// /usr/local/bin/restic in place: for the length of that download the file
// was there but not yet executable, and the agent that probed it exited.
// Nothing said why on the panel, because the interface the panel serves is
// this process. A server whose restic is missing can still show its
// history, its destinations and the job that is about to fail; only
// -preflight, whose whole job is to answer whether this server is ready,
// still refuses.
func startupRestic(ctx context.Context, binary string, preflight bool,
	log *slog.Logger) (string, error) {
	version, err := resticVersion(ctx, binary)
	if err == nil {
		return version, nil
	}
	if preflight {
		return "", err
	}
	log.Error("restic did not run; backups and restores will fail until it does",
		"binary", binary, "error", err)
	return "", nil
}

func resticVersion(ctx context.Context, binary string) (string, error) {
	cmd := exec.CommandContext(ctx, binary, "version")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("run %s version: %w", binary, err)
	}
	return strings.TrimSpace(string(output)), nil
}

func ensureDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Clean(path), err)
	}
	return nil
}

// newLogger builds the process's logger on a level that can be moved
// later. The variable is handed to the engine, so turning the log up in the
// interface changes what this handler writes rather than what the next
// process would write.
func newLogger(level string) (*slog.Logger, *slog.LevelVar) {
	var parsed slog.Level
	if err := parsed.UnmarshalText([]byte(level)); err != nil {
		parsed = slog.LevelInfo
	}
	current := new(slog.LevelVar)
	current.Set(parsed)
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: current})), current
}
