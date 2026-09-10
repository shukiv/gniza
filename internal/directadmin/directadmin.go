// Package directadmin drives the DirectAdmin tooling installed on a host.
//
// It is the second implementation of panel.Provider, and it is not
// finished: whole-account backup and explicit native overwrite were checked
// against a disposable account on DirectAdmin 1.709. Other operations are
// refused rather than guessed. A
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
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

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
	// NativeRoot is separate from the root-private state and staging tree.
	// Empty selects /var/lib/gniza-directadmin-native. Never put it under
	// an account-writable directory or broaden the service state permissions.
	NativeRoot string
	// ReadHomeInPlace asks this server for the shape ADR 0021 describes:
	// a backup of everything except the account's own files, which restic
	// reads from /home where they already are. It is asked for one server
	// at a time rather than assumed, because the archive a restore is
	// built from is rebuilt out of the restored tree rather than copied
	// out of a manifest, and a server should not be moved onto that shape
	// until a restore drill on it has proved the rebuild.
	ReadHomeInPlace bool
	lookupUser      func(string) (*user.User, error)
	// leanBackups is what this server was found to do with a backup that
	// asks for less than the whole account: 0 not yet known, 1 honoured,
	// -1 ignored. Learned from a real run rather than from a version
	// number, and learned again after a restart.
	leanBackups atomic.Int32
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

// Layout validates native archives; split/granular selectors remain provisional.
func (r *Real) Name() string { return "DirectAdmin" }

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
		// Not DirectAdmin's list, and not the customer's data either:
		// CloudLinux rebuilds .cagefs from the system's own files and
		// LiteSpeed rebuilds lscache from the site. On the validation
		// host .cagefs was 3.2 GiB of one 10.8 GiB account -- read,
		// compressed, written and read again every night, to restore
		// something a single command regenerates.
		".cagefs", "lscache",
		// And JetBackup's own per-account directory, which belongs to
		// root rather than to the account. DirectAdmin extracts the home
		// archive as the account, so carrying a root-owned member takes
		// the whole restore down on the first utime -- found by the first
		// restore drill, see ADR 0021. It was empty on all 142 accounts
		// of the validation host, and JetBackup makes it again.
		".jb-roundcube",
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
	if !info.Missing {
		// The mail is measured in the same walk rather than a second one:
		// on a 19.7 GiB home that walk is the expensive part, and a
		// backup that reads the home in place needs both numbers.
		mail := filepath.Join(info.HomeDir, dabackup.MailDir) + string(os.PathSeparator)
		if err := filepath.Walk(info.HomeDir, func(path string, entry os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.Mode().IsRegular() {
				info.SizeBytes += uint64(entry.Size())
				if strings.HasPrefix(path, mail) {
					info.LeanBytes += uint64(entry.Size())
				}
			}
			return nil
		}); err != nil {
			return info, fmt.Errorf("directadmin: measure account home: %w", err)
		}
	}
	// Database files do not live in the home. Include their measured logical
	// size, otherwise a tiny website with a large database passes preflight.
	// As in databases() below, the account name is put into the statement
	// rather than bound, because MySQL's command-line client has no
	// placeholders. usableAccountName above is what makes that safe: the
	// name is letters, digits, dash and underscore, so it cannot carry a
	// quote. Do not move this query anywhere that check does not run.
	query := "SELECT COALESCE(SUM(data_length+index_length),0) FROM information_schema.tables WHERE table_schema LIKE '" + escapeLike(user) + `\_%` + "'"
	out, err := exec.CommandContext(ctx, r.mysql(), "--defaults-file="+r.myCnf(), "--batch", "--skip-column-names", "-e", query).Output()
	if err != nil {
		return info, fmt.Errorf("directadmin: measure databases: %w", err)
	}
	dbBytes, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return info, fmt.Errorf("directadmin: invalid database size: %w", err)
	}
	if dbBytes > ^uint64(0)-info.SizeBytes {
		return info, fmt.Errorf("directadmin: account size overflow")
	}
	info.SizeBytes += dbBytes
	// The dumps a lean archive carries are written from these tables. On
	// the validation host the dumps came to 546,542,870 bytes against the
	// 756,011,257 information_schema reports, so this is the safe side.
	info.LeanBytes += dbBytes
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
	if user == "" || len(user) > 64 || user[0] == '-' {
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
// DirectAdmin's admin-backup writes one archive and its documentation
// says nothing about telling it to leave the home directory or the
// databases out, so nothing here asks it to. What split mode does instead
// is take the archive apart afterwards -- see stageSplit -- because
// restic cannot deduplicate a compressed archive and a nightly backup of
// one stores close to a full copy every night.
func (r *Real) Stage(ctx context.Context, req panel.StageRequest) (pkgacct.Payload, error) {
	if err := usableAccountName(req.Account.User); err != nil {
		return pkgacct.Payload{}, err
	}
	if req.SkipHomedir || req.SkipDatabases || req.SkipEmail {
		return pkgacct.Payload{}, unverified(
			"leaving part of an account out of a DirectAdmin backup")
	}
	if req.Mode == pkgacct.ModeSplit {
		return r.stageSplit(ctx, req)
	}
	if req.Mode != pkgacct.ModeMonolithic {
		return pkgacct.Payload{}, fmt.Errorf("directadmin: select monolithic mode for a native whole-account backup")
	}
	if _, err := r.userConf(req.Account.User); err != nil {
		return pkgacct.Payload{}, err
	}
	archive, err := r.stageNative(ctx, req.Account.User, req.StagingDir, req.Account.SizeBytes, false)
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

// stageSplit stages the account as files rather than as one compressed
// archive.
//
// DirectAdmin writes the archive; Gniza takes it apart into the two parts
// restic is pointed at, and the archive is removed once it has. What
// comes out is a home directory and the account's own records, at paths
// that are the same tonight as they were last night, which is what lets
// restic store one night's changes rather than one night's archive. The
// same code puts them back before DirectAdmin's own restore sees them:
// see dabackup's split.go.
func (r *Real) stageSplit(ctx context.Context, req panel.StageRequest) (pkgacct.Payload, error) {
	account := req.Account.User
	if _, err := r.userConf(account); err != nil {
		return pkgacct.Payload{}, err
	}
	if err := os.MkdirAll(req.StagingDir, 0o700); err != nil {
		return pkgacct.Payload{}, err
	}
	archive := ""
	// Set below on the run that found this out, and again on every run
	// after it: a server that ignores the selection does so for all 142
	// of its accounts, and one job saying why while the rest say nothing
	// is how a reason gets lost.
	ignored := r.ReadHomeInPlace && r.leanBackups.Load() == -1
	if r.ReadHomeInPlace && r.leanBackups.Load() >= 0 {
		payload, whole, err := r.stageInPlace(ctx, req)
		if err == nil {
			r.leanBackups.Store(1)
			return payload, nil
		}
		if !errors.Is(err, errSelectionIgnored) {
			return pkgacct.Payload{}, err
		}
		// This DirectAdmin read the selection and backed up the whole
		// account anyway. Every run on this server from here on takes the
		// archive apart, which is what it did before ADR 0021, and the
		// operator is told why through the payload's reason.
		//
		// The archive it wrote is the one the old path needs, so it is
		// taken apart rather than asked for again: a server that does
		// this should pay for one backup tonight, not two.
		r.leanBackups.Store(-1)
		r.debug("this DirectAdmin ignored the backup selection", "account", account, "error", err)
		ignored = true
		archive = whole
		if err := clearParts(req.StagingDir); err != nil {
			return pkgacct.Payload{}, err
		}
	}
	if archive == "" {
		// The account is on this disk twice at the peak: the archive
		// DirectAdmin wrote, and the tree it is taken apart into.
		// stageNative checks for one of those; this is the other.
		if err := nativeSpace(req.StagingDir, req.Account.SizeBytes, 2); err != nil {
			return pkgacct.Payload{}, err
		}
		var err error
		archive, err = r.stageNative(ctx, account, req.StagingDir, req.Account.SizeBytes, false)
		if err != nil {
			return pkgacct.Payload{}, err
		}
	}
	// stageNative has already bound the archive's own identity record to
	// this account, so what is taken apart below is known to be theirs.
	if err := (dabackup.Layout{}).UnpackArchive(ctx, archive, account, req.StagingDir); err != nil {
		os.Remove(archive)
		return pkgacct.Payload{}, err
	}
	// Removed rather than kept: leaving it beside the parts is the
	// account twice on a disk that had to fit it once, and restic would
	// store the compressed copy as well -- which is the cost split mode
	// exists to avoid.
	if err := os.Remove(archive); err != nil {
		return pkgacct.Payload{}, fmt.Errorf("directadmin: remove the staged archive: %w", err)
	}
	payload := pkgacct.Payload{
		Mode:    pkgacct.ModeSplit,
		Account: account,
		Parts: []pkgacct.Part{
			{Kind: pkgacct.PartMetadata, Path: dabackup.MetadataPart(req.StagingDir)},
			{Kind: pkgacct.PartHomedir, Path: dabackup.HomedirPart(req.StagingDir)},
		},
	}
	if warning := r.ownershipWarning(account, func(uid int) (foreignOwners, error) {
		return archiveOwners(req.StagingDir, uid)
	}); warning != "" {
		payload.Warnings = append(payload.Warnings, warning)
	}
	if ignored {
		// Only when this server was asked to read the account where it
		// lies and would not. A server that was never asked is doing what
		// it was told, and saying it is degraded would be Gniza
		// complaining about its own configuration.
		payload.Degraded = true
		payload.Reason = "This DirectAdmin backs up the whole account whether or not the " +
			"backup asks for less, so the account is written to disk and taken apart " +
			"again every night instead of being read where it lies."
	}
	return payload, payload.Verify()
}

// ReadsHomeInPlace says whether the next backup on this server will read
// the account's home directory where it lies.
//
// It answers what Stage will do, not only what Stage has already done.
// The two must agree: this is what the staging estimate is built from,
// and an estimate for a shape the run will not take reserves room for an
// account that is never written. A server asked for this shape takes it
// until a run finds that its DirectAdmin ignores the selection -- which
// is exactly the condition Stage tests -- and an agent that has just
// started has found nothing yet. The first account of the night is as
// likely to be the largest as the smallest, and reserving the whole of
// it refuses the backup before the run that would have settled the
// question.
func (r *Real) ReadsHomeInPlace() bool { return r.ReadHomeInPlace && r.leanBackups.Load() >= 0 }

// errSelectionIgnored is a DirectAdmin that produced a whole-account
// archive for a backup that asked for less than one.
var errSelectionIgnored = errors.New("directadmin: the backup selection was ignored")

// stageInPlace stages the account the way cPanel has always staged one:
// DirectAdmin writes its records, and the account's own files are handed
// to restic as the path they are already on.
//
// What that costs is DirectAdmin reading the mail once, because asking
// for the mailboxes' passwords means asking for "email" and "email"
// brings the messages with it. Those are dropped on the way in; they are
// under the home path. See ADR 0021.
//
// When DirectAdmin ignores the selection this reports errSelectionIgnored
// and the archive it wrote, which is a whole-account one the caller can
// take apart rather than ask for a second time.
func (r *Real) stageInPlace(ctx context.Context, req panel.StageRequest) (pkgacct.Payload, string, error) {
	account := req.Account.User
	home := req.Account.HomeDir
	if home == "" {
		conf, err := r.userConf(account)
		if err != nil {
			return pkgacct.Payload{}, "", err
		}
		home = r.homeOf(account, conf)
	}
	if stat, err := os.Stat(home); err != nil || !stat.IsDir() {
		return pkgacct.Payload{}, "", fmt.Errorf(
			"directadmin: %s is not a home directory to read: %w", home, err)
	}
	// The room reserved is the room this archive needs, not the room the
	// account needs. What is left in it is the messages and the database
	// dumps; the account's own files are not written anywhere, and
	// reserving them refuses backups the disk had room for. On the
	// validation host a 19.7 GiB account produced a 296.5 MiB archive of
	// 1.19 GiB of members -- 732,924,695 bytes of mail and 546,542,870 of
	// dumps -- and reserving the account demanded 39 GiB on a disk with
	// 22 GiB free. See ADR 0021.
	//
	// LeanBytes is those two measured uncompressed, so it is already
	// about four times what the archive takes. leanStagingFloor covers
	// the records, which are small and never zero: an account with no
	// mail and no databases measures zero honestly.
	//
	// DirectAdmin's own accounting does not answer it: user.usage records
	// email_quota as what the mailboxes were allotted, not what they hold
	// -- 157,260,176 against 14 MiB of messages on the validation host.
	reserve := req.Account.LeanBytes
	if reserve > ^uint64(0)-leanStagingFloor {
		return pkgacct.Payload{}, "", fmt.Errorf("directadmin: staging estimate overflow")
	}
	reserve += leanStagingFloor
	archive, err := r.stageNative(ctx, account, req.StagingDir, reserve, true)
	if err != nil {
		return pkgacct.Payload{}, "", err
	}
	if err := (dabackup.Layout{}).UnpackLeanArchive(ctx, archive, account, req.StagingDir); err != nil {
		if strings.Contains(err.Error(), "ignored the backup selection") {
			return pkgacct.Payload{}, archive, fmt.Errorf("%w: %w", errSelectionIgnored, err)
		}
		os.Remove(archive)
		return pkgacct.Payload{}, "", err
	}
	if err := os.Remove(archive); err != nil {
		return pkgacct.Payload{}, "", fmt.Errorf("directadmin: remove the staged archive: %w", err)
	}
	payload := pkgacct.Payload{
		Mode:    pkgacct.ModeSplit,
		Account: account,
		Parts: []pkgacct.Part{
			{Kind: pkgacct.PartMetadata, Path: dabackup.MetadataPart(req.StagingDir)},
			{Kind: pkgacct.PartHomedir, Path: home},
		},
	}
	// After the backup, never before it: this walks the home a second
	// time and must not stand between an account and its backup.
	if warning := r.ownershipWarning(account, func(uid int) (foreignOwners, error) {
		return homeOwners(home, uid)
	}); warning != "" {
		payload.Warnings = append(payload.Warnings, warning)
	}
	return payload, "", payload.Verify()
}

// clearParts takes away what an unpack that did not finish left behind,
// so the archive can be taken apart again the other way. Only the two
// directories this program writes, and only under the staging directory
// it was given.
func clearParts(staging string) error {
	for _, part := range []string{dabackup.MetadataPart(staging), dabackup.HomedirPart(staging)} {
		if err := os.RemoveAll(part); err != nil {
			return fmt.Errorf("directadmin: clear %s: %w", part, err)
		}
	}
	return nil
}

// StageSystem materialises the server's own configuration.
func (r *Real) StageSystem(ctx context.Context, stagingDir string) (pkgacct.Payload, error) {
	return pkgacct.Payload{}, unverified(
		"what a replacement DirectAdmin server has to be told before accounts mean anything")
}

// Apply hands a rebuilt archive to DirectAdmin's own restore.
//
// Only an existing ordinary account, explicitly overwritten using the native
// unrestricted restore, is supported. No shared task.queue is read or written.
// New-account, renamed and restricted restore have not been established.
func (r *Real) Apply(ctx context.Context, archivePath string, options panel.ApplyOptions) (string, error) {
	if !options.Unrestricted || options.NewUser != "" || options.SkipDNS || !options.Overwrite {
		return "", unverified("DirectAdmin restore requires explicit native/unrestricted overwrite of an existing account; restricted, renamed and new-account restores are not supported")
	}
	account, err := dabackup.ArchiveAccount(filepath.Base(archivePath))
	if err != nil {
		return "", err
	}
	conf, err := r.userConf(account)
	if err != nil {
		return "", err
	}
	if conf["username"] != account || conf["usertype"] != "user" {
		return "", fmt.Errorf("directadmin: native overwrite requires an existing ordinary user account")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	w, err := r.nativeWorkspace(account)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := w.close(); err != nil {
			r.debug("native workspace cleanup failed", "path", w.path, "error", err)
		}
	}()
	staged := filepath.Join(w.destination, filepath.Base(archivePath))
	if err := copyArchive(ctx, archivePath, staged, uint32(os.Geteuid())); err != nil {
		return "", err
	}
	// What the archive says the account's databases are is what the
	// finished restore is held to. Checking the copy inside the workspace
	// checks the bytes DirectAdmin is about to read, not another file.
	expected, err := dabackup.ArchiveDatabases(ctx, staged, account)
	if err != nil {
		return "", err
	}
	if err := os.Chown(staged, w.adminUID, w.adminGID); err != nil {
		return "", err
	}
	task := url.Values{
		"action": {"restore"}, "ip_choice": {"file"}, "local_path": {w.destination},
		"owner": {"admin"}, "select0": {filepath.Base(staged)}, "type": {"admin"},
		"value": {"multiple"}, "when": {"now"}, "where": {"local"},
	}
	transcript, err := r.runNative(ctx, "restore", filepath.Base(staged), w.destination, "taskq", "--run="+task.Encode())
	if err != nil {
		return transcript, err
	}
	after, err := r.userConf(account)
	if err != nil {
		return transcript, err
	}
	if after["username"] != account || after["usertype"] != "user" {
		return transcript, fmt.Errorf("directadmin: account identity did not survive native restore")
	}
	// A DirectAdmin restore is carried out by several modules, and the
	// account being there afterwards says nothing about whether the
	// databases came back with it. An account whose website answers and
	// whose orders are gone is the failure this check exists for.
	// Nothing to hold it to means no question to ask the database server,
	// and no restore failed because that server could not be reached.
	if len(expected) == 0 {
		return transcript, nil
	}
	present, err := r.databases(ctx, account)
	if err != nil {
		return transcript, err
	}
	if missing := absentFrom(expected, present); len(missing) > 0 {
		verb := "is"
		if len(missing) > 1 {
			verb = "are"
		}
		return transcript, fmt.Errorf(
			"directadmin: the restore of %s reported success, but %s %s not on "+
				"the account afterwards -- DirectAdmin restores an account in "+
				"modules, so this restore is not the account back",
			account, strings.Join(missing, ", "), verb)
	}
	return transcript, nil
}

// absentFrom names what the archive carried and the account does not have.
// A database the account has gained since the backup is not a failure.
func absentFrom(expected, present []string) []string {
	has := make(map[string]bool, len(present))
	for _, name := range present {
		has[name] = true
	}
	var missing []string
	for _, name := range expected {
		if !has[name] {
			missing = append(missing, name)
		}
	}
	return missing
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
		if !entry.Type().IsRegular() {
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
