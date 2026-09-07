// Package directadmin drives the DirectAdmin tooling installed on a host.
//
// It is the second implementation of panel.Provider, and it is not
// finished: what DirectAdmin's own documentation states is implemented
// here, and what it does not state is refused rather than guessed. A
// method that returns ErrUnverified is one whose answer needs a running
// DirectAdmin server, and every one of them is listed in ADR 0019.
//
// Refusing is the point. A backup provider that guesses produces backups
// that look successful and restore into nothing, which is worse than one
// that says it cannot do this yet.
package directadmin

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/shukiv/gniza/internal/layout/dabackup"
	"github.com/shukiv/gniza/internal/panel"
	"github.com/shukiv/gniza/internal/pkgacct"
)

// ErrUnverified is what a method returns when doing the thing would mean
// guessing at DirectAdmin's behaviour.
//
// It is a sentinel so that callers can tell "this panel cannot do it yet"
// apart from "this account could not be backed up", and so that the
// message an operator reads names the decision record rather than a
// stack.
var ErrUnverified = errors.New("directadmin: not established against a running DirectAdmin server")

// unverified builds the error an operator sees, naming what was asked
// for.
func unverified(what string) error {
	return fmt.Errorf("%w: %s (see ADR 0019)", ErrUnverified, what)
}

// Real drives the DirectAdmin installed on this host.
type Real struct {
	// Log is where this provider says what it ran, at debug.
	Log *slog.Logger
	// BinaryPath is the directadmin binary. Empty means the standard
	// location.
	BinaryPath string
	// DataDir holds one directory per account. Empty means the standard
	// location.
	DataDir string
	// HomeRoot is where account home directories live.
	HomeRoot string
	// MysqlPath and MysqldumpPath are the clients used to read and dump
	// databases. Empty means whatever is on PATH.
	MysqlPath     string
	MysqldumpPath string
	// MyCnfPath is the credentials file DirectAdmin keeps for its own
	// MySQL access. Empty means the standard location.
	MyCnfPath string
	// TaskQueuePath is the file DirectAdmin reads work from, and
	// DataskqPath is what executes it. Empty means the standard
	// locations.
	TaskQueuePath string
	DataskqPath   string
}

var _ panel.Provider = (*Real)(nil)

const (
	defaultBinary    = "/usr/local/directadmin/directadmin"
	defaultDataDir   = dabackup.ConfigDir
	defaultHomeRoot  = dabackup.HomeRoot
	defaultMyCnf     = "/usr/local/directadmin/conf/my.cnf"
	defaultTaskQueue = "/usr/local/directadmin/data/task.queue"
	defaultDataskq   = "/usr/local/directadmin/dataskq"
)

func (r *Real) binary() string {
	if r.BinaryPath != "" {
		return r.BinaryPath
	}
	return defaultBinary
}

func (r *Real) dataDir() string {
	if r.DataDir != "" {
		return r.DataDir
	}
	return defaultDataDir
}

func (r *Real) homeRoot() string {
	if r.HomeRoot != "" {
		return r.HomeRoot
	}
	return defaultHomeRoot
}

func (r *Real) mysql() string {
	if r.MysqlPath != "" {
		return r.MysqlPath
	}
	return "mysql"
}

func (r *Real) mysqldump() string {
	if r.MysqldumpPath != "" {
		return r.MysqldumpPath
	}
	return "mysqldump"
}

func (r *Real) myCnf() string {
	if r.MyCnfPath != "" {
		return r.MyCnfPath
	}
	return defaultMyCnf
}

func (r *Real) taskQueue() string {
	if r.TaskQueuePath != "" {
		return r.TaskQueuePath
	}
	return defaultTaskQueue
}

func (r *Real) dataskq() string {
	if r.DataskqPath != "" {
		return r.DataskqPath
	}
	return defaultDataskq
}

func (r *Real) debug(msg string, args ...any) {
	if r.Log != nil {
		r.Log.Debug(msg, args...)
	}
}

// Layout is DirectAdmin's own, and says of itself that it is provisional.
func (r *Real) Layout() panel.Layout { return dabackup.Layout{} }

// NativeExcludes is what DirectAdmin's own backups leave out of an
// account's home directory.
//
// Stated by its documentation, and worth honouring for the same reason
// cPanel's exclude file is: these are directories that are either not the
// customer's data or are copies of backups, and a backup of a backup is
// how a home directory doubles every night.
func (r *Real) NativeExcludes(home string) []string {
	skipped := []string{
		"backups", "user_backups", "admin_backups",
		"usr", "bin", "etc", "lib", "lib64", "tmp", "var", "sbin", "dev",
	}
	excludes := make([]string, 0, len(skipped))
	for _, name := range skipped {
		excludes = append(excludes, filepath.Join(home, name))
	}
	return excludes
}

// Capabilities reports what this host's packaging tool can be told to
// leave out.
//
// The flags are cPanel's pkgacct flags and DirectAdmin has none of them:
// it has admin-backup, whose options for skipping the home directory or
// the databases are not documented. An empty set is the truthful answer
// and is what makes the payload planner report a split payload as
// degraded rather than silently produce a bad one.
func (r *Real) Capabilities(ctx context.Context) (pkgacct.Capabilities, error) {
	return pkgacct.Capabilities{}, nil
}

// Accounts lists every account DirectAdmin knows about, from the one
// directory per account it keeps.
func (r *Real) Accounts(_ context.Context) ([]panel.AccountInfo, error) {
	entries, err := os.ReadDir(r.dataDir())
	if err != nil {
		return nil, fmt.Errorf("directadmin: read %s: %w", r.dataDir(), err)
	}
	accounts := make([]panel.AccountInfo, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		user := entry.Name()
		conf, err := r.userConf(user)
		if err != nil {
			// An account whose configuration cannot be read is still an
			// account, and one left off the page is one nobody notices is
			// not being backed up.
			r.debug("could not read the account configuration",
				"account", user, "error", err.Error())
		}
		info := panel.AccountInfo{
			User:          user,
			HomeDir:       r.homeOf(user, conf),
			PrimaryDomain: conf["domain"],
		}
		if stat, err := os.Stat(info.HomeDir); err != nil || !stat.IsDir() {
			info.Missing = true
		}
		accounts = append(accounts, info)
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].User < accounts[j].User })
	return accounts, nil
}

// Account looks up one account's home directory and databases.
func (r *Real) Account(ctx context.Context, user string) (panel.AccountInfo, error) {
	if err := usableAccountName(user); err != nil {
		return panel.AccountInfo{}, err
	}
	conf, err := r.userConf(user)
	if err != nil {
		return panel.AccountInfo{}, err
	}
	info := panel.AccountInfo{
		User:          user,
		HomeDir:       r.homeOf(user, conf),
		PrimaryDomain: conf["domain"],
	}
	if stat, err := os.Stat(info.HomeDir); err != nil || !stat.IsDir() {
		info.Missing = true
	}
	databases, err := r.databases(ctx, user)
	if err != nil {
		return info, err
	}
	info.Databases = databases
	return info, nil
}

// userConf reads an account's user.conf into its key=value pairs.
func (r *Real) userConf(user string) (map[string]string, error) {
	if err := usableAccountName(user); err != nil {
		return nil, err
	}
	body, err := os.ReadFile(filepath.Join(r.dataDir(), user, dabackup.UserConf))
	if err != nil {
		return nil, fmt.Errorf("directadmin: read the configuration of %s: %w", user, err)
	}
	values := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		key, value, found := strings.Cut(strings.TrimSpace(scanner.Text()), "=")
		if !found {
			continue
		}
		values[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return values, scanner.Err()
}

// homeOf is where the account's files are: what its configuration says,
// or the account's name under the home root.
func (r *Real) homeOf(user string, conf map[string]string) string {
	if home := conf["home"]; home != "" {
		return home
	}
	return filepath.Join(r.homeRoot(), user)
}

// databases lists the account's databases.
//
// DirectAdmin names them <account>_<database>, so the account's own are
// the ones with its prefix. The name is checked before it is used, and
// the pattern is passed as an argument rather than built into a
// statement.
func (r *Real) databases(ctx context.Context, user string) ([]string, error) {
	if err := usableAccountName(user); err != nil {
		return nil, err
	}
	// MySQL's command-line client has no placeholders. The account name
	// has already been checked to be letters, digits, dash and
	// underscore, so it cannot carry a quote; the underscore is still
	// escaped, because to LIKE it means "any character".
	query := "SHOW DATABASES LIKE '" + escapeLike(user) + `\_%` + "'"
	out, err := exec.CommandContext(ctx, r.mysql(),
		"--defaults-file="+r.myCnf(), "--batch", "--skip-column-names", "-e", query).Output()
	if err != nil {
		return nil, fmt.Errorf("directadmin: list the databases of %s: %w", user, err)
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		if !strings.HasPrefix(name, user+"_") {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// escapeLike protects the two characters LIKE treats as patterns.
func escapeLike(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, "%", `\%`)
	return strings.ReplaceAll(value, "_", `\_`)
}

// usableAccountName refuses anything that is not a DirectAdmin username,
// because the name becomes a path and a database prefix.
func usableAccountName(user string) error {
	if user == "" || len(user) > 64 {
		return fmt.Errorf("directadmin: %q is not an account name", user)
	}
	for _, char := range user {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z',
			char >= '0' && char <= '9', char == '-', char == '_':
		default:
			return fmt.Errorf("directadmin: %q is not an account name", user)
		}
	}
	return nil
}

// Stage materialises one account's payload.
//
// Only the whole-account shape is established: DirectAdmin's admin-backup
// writes one archive and its documentation says nothing about telling it
// to leave the home directory or the databases out. Split mode is what
// makes restic deduplicate, so a split payload built by guessing at those
// options is exactly the kind of backup that looks fine until it is
// needed.
func (r *Real) Stage(ctx context.Context, req panel.StageRequest) (pkgacct.Payload, error) {
	if err := usableAccountName(req.Account.User); err != nil {
		return pkgacct.Payload{}, err
	}
	if req.Mode == pkgacct.ModeSplit {
		return pkgacct.Payload{}, unverified(
			"staging an account in parts needs admin-backup options that are not documented")
	}
	if req.SkipHomedir || req.SkipDatabases || req.SkipEmail {
		return pkgacct.Payload{}, unverified(
			"leaving part of an account out of a DirectAdmin backup")
	}
	if err := os.MkdirAll(req.StagingDir, 0o700); err != nil {
		return pkgacct.Payload{}, fmt.Errorf("directadmin: create staging: %w", err)
	}

	started := time.Now()
	cmd := exec.CommandContext(ctx, r.binary(), "admin-backup",
		"--destination="+req.StagingDir, "--user="+req.Account.User)
	output, err := cmd.CombinedOutput()
	r.debug("ran admin-backup", "account", req.Account.User,
		"took", time.Since(started).String(), "error", errorText(err))
	if err != nil {
		return pkgacct.Payload{}, fmt.Errorf("directadmin: admin-backup %s: %w: %s",
			req.Account.User, err, lastLine(output))
	}

	archive, err := soleArchive(req.StagingDir)
	if err != nil {
		return pkgacct.Payload{}, err
	}
	payload := pkgacct.Payload{
		Mode:    pkgacct.ModeMonolithic,
		Account: req.Account.User,
		Parts:   []pkgacct.Part{{Kind: pkgacct.PartArchive, Path: archive}},
	}
	if strings.HasSuffix(archive, ".gz") || strings.HasSuffix(archive, ".zst") {
		payload.Degraded = true
		payload.Reason = "DirectAdmin compressed this archive, so restic deduplication " +
			"will be close to zero and every run stores a full copy"
	}
	return payload, payload.Verify()
}

// StageSystem materialises the server's own configuration.
func (r *Real) StageSystem(ctx context.Context, stagingDir string) (pkgacct.Payload, error) {
	return pkgacct.Payload{}, unverified(
		"what a replacement DirectAdmin server has to be told before accounts mean anything")
}

// Apply hands a rebuilt archive to DirectAdmin's own restore.
//
// DirectAdmin does not restore in the foreground: a line is appended to
// its task queue and dataskq carries it out. Running dataskq here makes
// that synchronous, which is what the interface promises -- but how a
// restore reports that it failed is the second thing ADR 0019 says a real
// host has to answer, so this refuses rather than reporting a success it
// cannot vouch for.
func (r *Real) Apply(ctx context.Context, archivePath string, options panel.ApplyOptions) (string, error) {
	return "", unverified("how a queued DirectAdmin restore reports that it failed")
}

// PutHomeDir copies a restored subtree back into an account's home
// directory, as that account.
func (r *Real) PutHomeDir(ctx context.Context, user, from string) error {
	return unverified("putting files back into a DirectAdmin account")
}

// CreateDatabase makes a database the account does not have.
//
// Creating one straight in MySQL would work and would be wrong: the
// customer would not see it, could not delete it, and DirectAdmin's own
// record of the account would not know it exists. Which record that is,
// and whether it is a file or a query, is the sixth thing ADR 0019 lists.
func (r *Real) CreateDatabase(ctx context.Context, user, database string) error {
	return unverified("creating a database where DirectAdmin can see it")
}

// LoadDatabase replaces the contents of one of the account's databases.
func (r *Real) LoadDatabase(ctx context.Context, user, database, dumpPath string) error {
	return unverified("loading a dump into a DirectAdmin account's database")
}

// PutCrontab replaces the account's cron jobs.
func (r *Real) PutCrontab(ctx context.Context, user, from string) error {
	return unverified("putting back a DirectAdmin account's cron jobs")
}

// PutDatabaseUsers recreates the account's database users.
func (r *Real) PutDatabaseUsers(ctx context.Context, user string, users []panel.DatabaseUser) error {
	return unverified("recreating a DirectAdmin account's database users")
}

// soleArchive finds the single archive admin-backup wrote.
func soleArchive(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("directadmin: read %s: %w", dir, err)
	}
	var archives []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".tar") || strings.HasSuffix(name, ".tar.gz") ||
			strings.HasSuffix(name, ".tar.zst") {
			archives = append(archives, filepath.Join(dir, name))
		}
	}
	switch len(archives) {
	case 1:
		return archives[0], nil
	case 0:
		return "", fmt.Errorf("directadmin: admin-backup wrote no archive")
	default:
		return "", fmt.Errorf("directadmin: admin-backup wrote %d archives, expected one",
			len(archives))
	}
}

// lastLine is the most useful part of a failed command's output.
func lastLine(output []byte) string {
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
