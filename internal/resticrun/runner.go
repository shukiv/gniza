package resticrun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/shukiv/gniza/internal/destination"
)

// Repository is a restic repository: a path inside a destination, plus the
// password that encrypts it. Destination credentials and repository
// password are separate secrets with separate lifecycles.
type Repository struct {
	Dest destination.Destination
	// Path is the repository's location inside the destination, e.g. "cp01".
	Path string
	// Password is the restic repository password, resolved from the vault
	// for the duration of one job and never written to the agent's disk
	// except as the transient mode-0600 file below.
	Password string
}

// Config holds runner-wide settings.
type Config struct {
	// Binary is the restic executable. Empty means "restic" on PATH.
	Binary string
	// RuntimeDir holds the transient password file. It must be on a
	// filesystem only root can read. Empty means the OS temp directory.
	RuntimeDir string
	// CacheDir is restic's cache. A persistent cache markedly speeds up
	// repeat backups; empty leaves restic's default in place.
	CacheDir string
	// Compression is RESTIC_COMPRESSION: "auto", "max" or "off".
	// Empty leaves restic's default.
	Compression string
	// CACertPath is a CA bundle restic should trust in addition to the
	// system roots, for a rest-server or S3 endpoint using a private CA.
	CACertPath string
	// PathEnv is the PATH given to restic. Empty uses a fixed safe default
	// rather than inheriting the agent's, so a poisoned PATH in the
	// agent's environment cannot redirect the backend's helper binaries.
	PathEnv string
	// Log is where each restic invocation is written down. Everything it
	// writes is at debug. The environment is never logged: it is where the
	// backend credentials are, and the password file's path is in there
	// too. Nil says nothing.
	Log *slog.Logger
}

// Runner executes restic against a repository.
type Runner struct {
	cfg  Config
	exec Execer
}

// New returns a Runner. Passing a fake Execer lets callers test without a
// restic binary present.
func New(cfg Config, execer Execer) *Runner {
	if execer == nil {
		execer = &OSExec{}
	}
	return &Runner{cfg: cfg, exec: execer}
}

// BackupResult reports one completed backup to one repository.
type BackupResult struct {
	Summary BackupSummary
	// Incomplete is true when restic reported that some source files could
	// not be read. A snapshot exists but does not cover everything.
	Incomplete bool
	Stderr     string
}

// Backup runs "restic backup" and returns the parsed summary.
func (r *Runner) Backup(ctx context.Context, repo Repository, spec BackupSpec) (BackupResult, error) {
	if spec.RecordCompletion {
		spec.Tags = append(append([]string(nil), spec.Tags...), NeedsCompletionTag)
	}
	args, err := BackupArgs(spec)
	if err != nil {
		return BackupResult{}, err
	}
	result, err := r.run(ctx, repo, args, secondary{}, progressReader(spec.OnProgress))
	if err != nil {
		return BackupResult{}, err
	}
	// Only "backup" forgives exit 3: it still produced a snapshot.
	if err := classifyExit(result.ExitCode, result.Stderr, true); err != nil {
		return BackupResult{}, err
	}
	summary, err := ParseBackupSummary(result.Stdout)
	if err != nil {
		return BackupResult{}, err
	}
	if summary.SnapshotID == "" {
		return BackupResult{}, fmt.Errorf("resticrun: restic reported no snapshot id")
	}
	backup := BackupResult{
		Summary:    summary,
		Incomplete: result.ExitCode == exitIncompleteRead,
		Stderr:     string(result.Stderr),
	}
	if spec.RecordCompletion && !backup.Incomplete {
		if err := r.recordCompletion(ctx, repo, spec.Host, summary.SnapshotID); err != nil {
			backup.Incomplete = true
			return backup, fmt.Errorf("resticrun: snapshot was written but completion could not be recorded: %w", err)
		}
	}
	return backup, nil
}

// Init creates a repository.
//
// chunkerSource, when non-nil, is an existing repository whose chunker
// parameters the new repository must copy. Every repository after a
// server's first must pass one: the parameters are immutable after
// creation, and mismatched ones permanently rule out "restic copy".
// See docs/DESIGN.md §7.
func (r *Runner) Init(ctx context.Context, repo Repository, chunkerSource *Repository) error {
	var sourceURI string
	if chunkerSource != nil {
		uri, err := chunkerSource.Dest.URI(chunkerSource.Path)
		if err != nil {
			return fmt.Errorf("resticrun: chunker source uri: %w", err)
		}
		sourceURI = uri
	}
	// restic opens both repositories in one process, so the chunker
	// source's backend options, credentials and password must be in
	// effect too.
	var extra secondary
	if chunkerSource != nil {
		sourceOptions, err := chunkerSource.Dest.Options()
		if err != nil {
			return fmt.Errorf("resticrun: chunker source options: %w", err)
		}
		sourceEnv, err := chunkerSource.Dest.Env()
		if err != nil {
			return fmt.Errorf("resticrun: chunker source environment: %w", err)
		}
		if chunkerSource.Password == "" {
			return fmt.Errorf("resticrun: chunker source password is empty")
		}

		passwordFile, cleanup, err := writePasswordFile(r.cfg.RuntimeDir, chunkerSource.Password)
		if err != nil {
			return err
		}
		defer cleanup()

		extra.options = sourceOptions
		extra.env = map[string]string{"RESTIC_FROM_PASSWORD_FILE": passwordFile}
		for key, value := range sourceEnv {
			extra.env[key] = value
		}
	}

	result, err := r.run(ctx, repo, InitArgs(sourceURI), extra, nil)
	if err != nil {
		return err
	}
	return classifyExit(result.ExitCode, result.Stderr, false)
}

// secondary carries the settings of a second repository opened in the same
// restic invocation, as "init --from-repo" needs.
type secondary struct {
	options map[string]string
	env     map[string]string
}

// Check verifies repository integrity. It is run by the maintenance runner,
// not by agents: reading pack data back costs the same bandwidth as the
// backup did.
func (r *Runner) Check(ctx context.Context, repo Repository, spec CheckSpec) error {
	args, err := CheckArgs(spec)
	if err != nil {
		return err
	}
	result, err := r.run(ctx, repo, args, secondary{}, nil)
	if err != nil {
		return err
	}
	return classifyExit(result.ExitCode, result.Stderr, false)
}

// Forget applies a retention policy and optionally prunes.
//
// Against an append-only destination this fails by design when called with
// agent credentials; only the maintenance runner holds credentials that may
// delete.
func (r *Runner) Forget(ctx context.Context, repo Repository, spec ForgetSpec) error {
	_, err := r.ForgetPlanned(ctx, repo, spec)
	return err
}

// run assembles the environment, writes the transient password file and
// executes restic.
//
// extra carries the settings of a second repository opened in the same
// invocation, as "restic init --from-repo" needs.
func (r *Runner) run(ctx context.Context, repo Repository, args []string, extra secondary,
	onLine func([]byte), stream ...io.Writer) (CommandResult, error) {
	if repo.Dest == nil {
		return CommandResult{}, fmt.Errorf("resticrun: repository has no destination")
	}
	if repo.Password == "" {
		return CommandResult{}, fmt.Errorf("resticrun: repository password is empty")
	}

	uri, err := repo.Dest.URI(repo.Path)
	if err != nil {
		return CommandResult{}, fmt.Errorf("resticrun: repository uri: %w", err)
	}
	backendEnv, err := repo.Dest.Env()
	if err != nil {
		return CommandResult{}, fmt.Errorf("resticrun: backend environment: %w", err)
	}
	backendOptions, err := repo.Dest.Options()
	if err != nil {
		return CommandResult{}, fmt.Errorf("resticrun: backend options: %w", err)
	}
	options, err := mergeOptions(backendOptions, extra.options)
	if err != nil {
		return CommandResult{}, err
	}

	passwordFile, cleanup, err := writePasswordFile(r.cfg.RuntimeDir, repo.Password)
	if err != nil {
		return CommandResult{}, err
	}
	defer cleanup()

	env := map[string]string{
		"PATH":                 r.pathEnv(),
		"RESTIC_REPOSITORY":    uri,
		"RESTIC_PASSWORD_FILE": passwordFile,
	}
	for key, value := range backendEnv {
		env[key] = value
	}
	for key, value := range extra.env {
		// restic reads a secondary repository's backend credentials from
		// the same variables as the primary's, so two repositories on the
		// same backend type cannot use different credentials in one
		// invocation. Failing beats silently using the wrong ones.
		if existing, clash := env[key]; clash && existing != value {
			return CommandResult{}, fmt.Errorf(
				"resticrun: conflicting environment variable %q; restic shares backend "+
					"credentials between the primary and secondary repository", key)
		}
		env[key] = value
	}
	if r.cfg.CacheDir != "" {
		env["RESTIC_CACHE_DIR"] = r.cfg.CacheDir
	}
	if r.cfg.Compression != "" {
		env["RESTIC_COMPRESSION"] = r.cfg.Compression
	}

	globals := optionArgs(options)
	if r.cfg.CACertPath != "" {
		globals = append(globals, "--cacert", r.cfg.CACertPath)
	}
	command := Command{
		Path:   r.binary(),
		Args:   append(globals, args...),
		Env:    envSlice(env),
		OnLine: onLine,
	}
	if len(stream) == 1 {
		command.Stdout = stream[0]
	}
	started := time.Now()
	result, err := r.exec.Exec(ctx, command)
	r.logRun(command, uri, started, result, err)
	return result, err
}

// logRun writes down what restic was asked to do and what came back.
//
// Two things are kept out of it deliberately. The environment, because
// that is where the backend credentials and the password file's path are.
// And the values of the -o options, because a backend option can carry a
// path or a token and none of them is worth a leak -- the key alone says
// which option was set, which is what a reader needs.
func (r *Runner) logRun(command Command, uri string, started time.Time,
	result CommandResult, err error) {
	if r.Log() == nil {
		return
	}
	args := []any{
		"restic", command.Path,
		"args", strings.Join(elideOptionValues(command.Args), " "),
		"repository", safeURI(uri),
		"took", time.Since(started).Round(time.Millisecond).String(),
		"exit", result.ExitCode,
	}
	if err != nil {
		args = append(args, "error", err.Error())
	}
	r.Log().Debug("ran restic", args...)
}

// Log is the runner's logger, or nil.
func (r *Runner) Log() *slog.Logger { return r.cfg.Log }

// elideOptionValues keeps "-o key" and drops what it was set to.
func elideOptionValues(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		out = append(out, args[i])
		if args[i] != "-o" || i+1 >= len(args) {
			continue
		}
		key, _, _ := strings.Cut(args[i+1], "=")
		out = append(out, key+"=[set]")
		i++
	}
	return out
}

// safeURI removes a user and password some backends allow in the
// repository address. A repository nobody can name is no use in a log, and
// a password in one is a leak, so the host and path stay and the rest goes.
func safeURI(uri string) string {
	at := strings.LastIndex(uri, "@")
	if at < 0 {
		return uri
	}
	scheme := strings.Index(uri, ":")
	if scheme < 0 || scheme > at {
		return uri
	}
	return uri[:scheme+1] + "[credentials]@" + uri[at+1:]
}

// mergeOptions combines two sets of restic extended options. Restic applies
// "-o" globally rather than per repository, so two repositories in one
// invocation cannot disagree about the same key.
func mergeOptions(primary, extra map[string]string) (map[string]string, error) {
	merged := make(map[string]string, len(primary)+len(extra))
	for key, value := range primary {
		merged[key] = value
	}
	for key, value := range extra {
		if existing, clash := merged[key]; clash && existing != value {
			return nil, fmt.Errorf(
				"resticrun: conflicting backend option %q; restic applies -o globally, "+
					"so these two repositories cannot be used in one invocation", key)
		}
		merged[key] = value
	}
	return merged, nil
}

// optionArgs renders extended options as global "-o key=value" flags, which
// must precede the subcommand.
func optionArgs(options map[string]string) []string {
	if len(options) == 0 {
		return nil
	}
	keys := make([]string, 0, len(options))
	for key := range options {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	args := make([]string, 0, 2*len(keys))
	for _, key := range keys {
		args = append(args, "-o", key+"="+options[key])
	}
	return args
}

func (r *Runner) binary() string {
	if r.cfg.Binary != "" {
		return r.cfg.Binary
	}
	return "restic"
}

// pathEnv returns a fixed PATH rather than inheriting the agent's, so the
// backend's helper binaries cannot be redirected by a poisoned environment.
func (r *Runner) pathEnv() string {
	if r.cfg.PathEnv != "" {
		return r.cfg.PathEnv
	}
	return "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
}

// passwordDirPrefix names the directory each invocation's password file
// lives in. It is a prefix rather than a fixed name so two runs at once do
// not share a file, and it is a constant so the sweep below and the
// directories it looks for cannot drift apart.
const passwordDirPrefix = "gniza-run-"

// SweepPasswordDirs removes password directories that no process owns any
// more, and reports how many it removed.
//
// Each invocation cleans up its own directory when it returns. A process
// that is killed -- a service restarted mid-backup, an installer replacing
// the binary -- never returns, and the file it wrote stays where it is: a
// repository password on disk with nothing left to use it. This is called
// once at startup, where the only writer of these directories on this
// server is the process doing the calling and it has not written one yet.
func SweepPasswordDirs(runtimeDir string) (int, error) {
	entries, err := os.ReadDir(runtimeDir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("resticrun: read runtime dir: %w", err)
	}
	removed := 0
	var failures []error
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), passwordDirPrefix) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(runtimeDir, entry.Name())); err != nil {
			failures = append(failures, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(failures...)
}

// writePasswordFile stores the repository password in a private file.
//
// The password is passed to restic by file rather than by argument
// (/proc/<pid>/cmdline is world-readable) and by file rather than by
// RESTIC_PASSWORD so it is not inherited by any grandchild process restic's
// backends may spawn.
func writePasswordFile(runtimeDir, password string) (path string, cleanup func(), err error) {
	dir, err := os.MkdirTemp(runtimeDir, passwordDirPrefix)
	if err != nil {
		return "", func() {}, fmt.Errorf("resticrun: create runtime dir: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(dir) }

	path = filepath.Join(dir, "repo.pass")
	if err := os.WriteFile(path, []byte(password), 0o600); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("resticrun: write password file: %w", err)
	}
	return path, cleanup, nil
}
