// Package plain is the panel of a server that has no panel.
//
// A LAMP server keeps one site per directory under /var/www; a container
// host keeps one stack per directory under /opt, and its volumes under
// /var/lib/docker/volumes. Those directories are the accounts here: the
// operator names the roots, and every directory directly under one of
// them is backed up as an account of its own, read where it lies, with
// the MySQL databases named after it dumped beside it. See ADR 0022.
//
// What a panel would do on restore -- create the account, put the
// archive back through its own tool -- has no counterpart here, and is
// refused rather than imitated. Files go back with PutHomeDir; dumps go
// back with LoadDatabase. That is the whole of a plain server's restore.
package plain

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/shukiv/gniza/internal/panel"
	"github.com/shukiv/gniza/internal/pkgacct"
)

// ErrUnverified marks what a plain server cannot do: there is no panel
// to hand an archive to, no crontab of the account's own, no record of
// database users to put back.
var ErrUnverified = errors.New("plain: a server without a panel has no native restore")

// Provisional is said once where the service starts, so a server set up
// today knows what has and has not been proved of this provider.
const Provisional = "backups of the directories under the roots and of the databases named " +
	"after them have been exercised against a fake MySQL and driven from the terminal " +
	"interface; a restore of files and databases has not yet been run on a live server " +
	"(see ADR 0022)"

func unverified(what string) error {
	return fmt.Errorf("%w: %s (see ADR 0022)", ErrUnverified, what)
}

// Provider backs up the directories under its roots.
type Provider struct {
	// Roots are the directories whose subdirectories are the accounts:
	// /var/www, /srv, /opt. A root that is not there is skipped, so one
	// list serves a LAMP server and a container host alike.
	Roots []string
	// MySQLPath and MysqldumpPath are the clients. Empty means the ones
	// on PATH; a server without MySQL has accounts with no databases.
	MySQLPath     string
	MysqldumpPath string
	// Log is where the provider says what it ran, at debug.
	Log *slog.Logger
}

func (p *Provider) Name() string         { return PanelName }
func (p *Provider) Layout() panel.Layout { return Layout{} }

// ReadsHomeInPlace: the account's directory is backed up from where it
// is. Only the dumps and the record are written to staging.
func (p *Provider) ReadsHomeInPlace() bool { return true }

// NativeExcludes: there is no panel backup to defer to.
func (p *Provider) NativeExcludes(string) []string { return nil }

// Capabilities: there is no pkgacct here; nothing is asked of it.
func (p *Provider) Capabilities(context.Context) (pkgacct.Capabilities, error) {
	return pkgacct.Capabilities{}, nil
}

func (p *Provider) log() *slog.Logger {
	if p.Log == nil {
		return slog.Default()
	}
	return p.Log
}

func (p *Provider) mysql() string {
	if p.MySQLPath != "" {
		return p.MySQLPath
	}
	return "mysql"
}

func (p *Provider) mysqldump() string {
	if p.MysqldumpPath != "" {
		return p.MysqldumpPath
	}
	return "mysqldump"
}

// Accounts lists every directory directly under a root, by name. A name
// that appears under two roots is taken from the first, and said so.
func (p *Provider) Accounts(context.Context) ([]panel.AccountInfo, error) {
	seen := map[string]string{}
	var accounts []panel.AccountInfo
	for _, root := range p.Roots {
		entries, err := os.ReadDir(root)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("plain: list %s: %w", root, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if !entry.IsDir() || strings.HasPrefix(name, ".") || !usableName(name) {
				continue
			}
			if first, dup := seen[name]; dup {
				p.log().Warn("two roots hold a directory of the same name; the first is the account",
					"account", name, "taken", first, "ignored", filepath.Join(root, name))
				continue
			}
			seen[name] = filepath.Join(root, name)
			accounts = append(accounts, panel.AccountInfo{User: name, HomeDir: seen[name]})
		}
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].User < accounts[j].User })
	return accounts, nil
}

// usableName is a directory name that can be an account name: something
// a snapshot tag, a staging directory and a database name can carry.
func usableName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// Account finds one account under the roots and measures it: the size
// of its directory, and the databases named after it.
func (p *Provider) Account(ctx context.Context, user string) (panel.AccountInfo, error) {
	accounts, err := p.Accounts(ctx)
	if err != nil {
		return panel.AccountInfo{}, err
	}
	var info panel.AccountInfo
	found := false
	for _, a := range accounts {
		if a.User == user {
			info, found = a, true
			break
		}
	}
	if !found {
		return panel.AccountInfo{}, fmt.Errorf("plain: %s is not a directory under any of the roots %s", user, strings.Join(p.Roots, ", "))
	}
	info.SizeBytes = dirSize(info.HomeDir)
	sizes, err := p.databaseSizes(ctx)
	if err != nil {
		// No MySQL client, or one that cannot connect: an account with
		// no databases, said once at warn so a server that does have
		// them is not silently backed up without.
		p.log().Warn("the databases could not be listed; backing up files only", "account", user, "error", err)
		return info, nil
	}
	for name, size := range sizes {
		if owns(user, name) {
			info.Databases = append(info.Databases, name)
			info.LeanBytes += size
		}
	}
	sort.Strings(info.Databases)
	return info, nil
}

// AccountIdentity is the directory itself: the moment it was created,
// to the nanosecond. A directory removed and made again under the same
// name is a different account, which is what the identity exists to
// notice; the owner's uid would be the web server's for every site and
// could not tell them apart, and a freed inode number is handed out
// again at once. A filesystem that does not record birth times falls
// back to the inode.
func (p *Provider) AccountIdentity(user string) (int, error) {
	accounts, err := p.Accounts(context.Background())
	if err != nil {
		return 0, err
	}
	for _, a := range accounts {
		if a.User != user {
			continue
		}
		return directoryIdentity(a.HomeDir)
	}
	return 0, fmt.Errorf("plain: %s is %w", user, panel.ErrNoSuchAccount)
}

func directoryIdentity(dir string) (int, error) {
	var stat unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, dir, unix.AT_STATX_SYNC_AS_STAT, unix.STATX_BTIME|unix.STATX_INO, &stat); err != nil {
		return 0, fmt.Errorf("plain: identify %s: %w", dir, err)
	}
	if stat.Mask&unix.STATX_BTIME != 0 && (stat.Btime.Sec != 0 || stat.Btime.Nsec != 0) {
		return int(stat.Btime.Sec)*1_000_000_000 + int(stat.Btime.Nsec), nil
	}
	return int(stat.Ino), nil
}

// owns says whether a database belongs to an account by name: the
// account's own name, or that name, an underscore and anything.
func owns(account, database string) bool {
	return database == account || strings.HasPrefix(database, account+"_")
}

func dirSize(root string) uint64 {
	var total uint64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil && info.Mode().IsRegular() {
			total += uint64(info.Size())
		}
		return nil
	})
	return total
}

// databaseSizes lists every database with the bytes it holds. The
// system schemas are not anybody's.
func (p *Provider) databaseSizes(ctx context.Context) (map[string]uint64, error) {
	cmd := exec.CommandContext(ctx, p.mysql(), "-N", "-B", "-e",
		"SELECT table_schema, COALESCE(SUM(data_length+index_length),0) FROM information_schema.tables GROUP BY table_schema")
	var out, complaint bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &complaint
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("plain: mysql: %w%s", err, saidOnStderr(complaint.String()))
	}
	sizes := map[string]uint64{}
	scanner := bufio.NewScanner(&out)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "information_schema", "performance_schema", "mysql", "sys":
			continue
		}
		var size uint64
		if len(fields) > 1 {
			size, _ = strconv.ParseUint(fields[1], 10, 64)
		}
		sizes[fields[0]] = size
	}
	return sizes, nil
}

func saidOnStderr(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	return ": " + text
}

// Record is what the metadata part says an account is, so a snapshot
// read on another machine, or by a later version, knows where the files
// came from and which dumps belong to them.
type Record struct {
	Account   string    `json:"account"`
	Path      string    `json:"path"`
	Databases []string  `json:"databases"`
	Hostname  string    `json:"hostname"`
	TakenAt   time.Time `json:"taken_at"`
}

// Stage writes the dumps and the record into StagingDir and points the
// payload at the account's own directory for the files.
func (p *Provider) Stage(ctx context.Context, req panel.StageRequest) (pkgacct.Payload, error) {
	if req.Mode != pkgacct.ModeSplit {
		return pkgacct.Payload{}, fmt.Errorf("plain: a server without a panel has no %s backup; only split", req.Mode)
	}
	if req.SkipHomedir {
		return pkgacct.Payload{}, fmt.Errorf("plain: the files are the account; a backup that leaves them out has nothing to hold")
	}
	metadata := filepath.Join(req.StagingDir, "metadata")
	if err := os.MkdirAll(filepath.Join(metadata, RecordDir), 0o700); err != nil {
		return pkgacct.Payload{}, fmt.Errorf("plain: create %s: %w", metadata, err)
	}
	payload := pkgacct.Payload{
		Mode:    pkgacct.ModeSplit,
		Account: req.Account.User,
		Parts: []pkgacct.Part{
			{Kind: pkgacct.PartMetadata, Path: metadata},
			{Kind: pkgacct.PartHomedir, Path: req.Account.HomeDir},
		},
		DumpPaths: map[string]string{},
	}

	var dumped []string
	if !req.SkipDatabases && len(req.Account.Databases) > 0 {
		dumps := filepath.Join(metadata, DumpDir)
		if err := os.MkdirAll(dumps, 0o700); err != nil {
			return pkgacct.Payload{}, fmt.Errorf("plain: create %s: %w", dumps, err)
		}
		for _, name := range req.Account.Databases {
			path := filepath.Join(dumps, name+".sql")
			if err := p.dumpOne(ctx, name, path); err != nil {
				// ADR 0017: one table that cannot be dumped does not cost
				// the account its files and its other databases.
				_ = os.Remove(path)
				payload.Missing = append(payload.Missing, pkgacct.Omission{
					What: "database " + name, Why: err.Error()})
				continue
			}
			payload.DumpPaths[name] = path
			dumped = append(dumped, name)
		}
	}

	hostname, _ := os.Hostname()
	record, err := json.MarshalIndent(Record{
		Account: req.Account.User, Path: req.Account.HomeDir,
		Databases: dumped, Hostname: hostname, TakenAt: time.Now().UTC(),
	}, "", "  ")
	if err != nil {
		return pkgacct.Payload{}, err
	}
	if err := os.WriteFile(filepath.Join(metadata, RecordDir, RecordFile), append(record, '\n'), 0o600); err != nil {
		return pkgacct.Payload{}, fmt.Errorf("plain: write the account record: %w", err)
	}
	return payload, payload.Verify()
}

// dumpOne writes one database's dump, uncompressed so restic can
// deduplicate it between nights, and says what mysqldump said when it
// could not.
func (p *Provider) dumpOne(ctx context.Context, name, path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("plain: create dump %s: %w", path, err)
	}
	cmd := exec.CommandContext(ctx, p.mysqldump(),
		"--single-transaction", "--quick", "--routines", "--events", name)
	cmd.Stdout = file
	var complaint bytes.Buffer
	cmd.Stderr = &complaint
	err = cmd.Run()
	closeErr := file.Close()
	p.log().Debug("dumped a database", "database", name, "error", err)
	if err != nil {
		return fmt.Errorf("plain: mysqldump %s: %w%s", name, err, saidOnStderr(complaint.String()))
	}
	return closeErr
}

// StageSystem copies what a replacement machine needs before the
// accounts restored onto it mean anything: the web, database, container
// and cron configuration, the certificates, and a list of the packages.
func (p *Provider) StageSystem(ctx context.Context, stagingDir string) (pkgacct.Payload, error) {
	root := filepath.Join(stagingDir, "system")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return pkgacct.Payload{}, fmt.Errorf("plain: create %s: %w", root, err)
	}
	var copied []string
	for _, path := range SystemPaths {
		info, err := os.Lstat(path)
		if err != nil {
			continue
		}
		if err := copyPath(path, filepath.Join(root, path), info); err != nil {
			return pkgacct.Payload{}, err
		}
		copied = append(copied, path)
	}
	manifest := map[string]any{
		"taken_at": time.Now().UTC(), "copied": copied,
		"packages":   commandOutput(ctx, "rpm", "-qa", "--qf", "%{NAME}-%{VERSION}-%{RELEASE}.%{ARCH}\n"),
		"dpkg":       commandOutput(ctx, "dpkg-query", "-W", "-f", "${Package} ${Version}\n"),
		"containers": commandOutput(ctx, "docker", "ps", "-a", "--format", "{{.Names}}\t{{.Image}}\t{{.Status}}"),
		"pods":       commandOutput(ctx, "podman", "ps", "-a", "--format", "{{.Names}}\t{{.Image}}\t{{.Status}}"),
		"volumes":    commandOutput(ctx, "docker", "volume", "ls", "--format", "{{.Name}}\t{{.Mountpoint}}"),
	}
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return pkgacct.Payload{}, err
	}
	if err := os.WriteFile(filepath.Join(root, "gniza-system.json"), append(body, '\n'), 0o600); err != nil {
		return pkgacct.Payload{}, fmt.Errorf("plain: write the system manifest: %w", err)
	}
	payload := pkgacct.Payload{
		Mode: pkgacct.ModeSystem, Account: "@system",
		Parts: []pkgacct.Part{{Kind: pkgacct.PartSystem, Path: root}},
	}
	return payload, payload.Verify()
}

// SystemPaths is what the system backup of a plain server copies. A path
// that is not there is skipped: one list serves Debian and RHEL, Apache
// and nginx, docker and podman.
var SystemPaths = []string{
	"/etc/hostname", "/etc/hosts", "/etc/fstab", "/etc/crontab", "/etc/cron.d",
	"/etc/cron.daily", "/etc/cron.hourly", "/etc/cron.weekly", "/etc/cron.monthly",
	"/var/spool/cron",
	"/etc/apache2", "/etc/httpd", "/etc/nginx", "/etc/php", "/etc/php.ini", "/etc/php.d",
	"/etc/opt/remi", "/etc/mysql", "/etc/my.cnf", "/etc/my.cnf.d",
	"/etc/docker", "/etc/containers", "/etc/systemd/system",
	"/etc/letsencrypt", "/etc/ssl/certs", "/etc/pki/tls",
	"/etc/ssh/sshd_config", "/etc/ssh/sshd_config.d", "/etc/sudoers", "/etc/sudoers.d",
	"/etc/environment", "/etc/profile.d", "/etc/logrotate.d",
	"/root/.docker", "/root/.config/containers",
}

func commandOutput(ctx context.Context, name string, args ...string) string {
	if _, err := exec.LookPath(name); err != nil {
		return ""
	}
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// Apply: there is no panel to hand an archive to.
func (p *Provider) Apply(context.Context, string, panel.ApplyOptions) (string, error) {
	return "", unverified("restore the files with a files restore and the databases with a database restore")
}

// PutHomeDir writes a restored tree over the account's directory. What
// the backup holds replaces what is there file by file; a file added
// since stays, because nothing here knows whether it is wanted.
func (p *Provider) PutHomeDir(ctx context.Context, user, from string) error {
	account, err := p.Account(ctx, user)
	if err != nil {
		return err
	}
	return filepath.Walk(from, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		into := filepath.Join(account.HomeDir, rel)
		switch {
		case info.IsDir():
			if err := os.MkdirAll(into, info.Mode().Perm()); err != nil {
				return err
			}
			return os.Chmod(into, info.Mode().Perm())
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_ = os.Remove(into)
			return os.Symlink(link, into)
		case !info.Mode().IsRegular():
			return nil
		}
		if err := copyFile(path, into, info.Mode().Perm()); err != nil {
			return err
		}
		return chownLike(into, info)
	})
}

// CreateDatabase makes a database the account owns by name.
func (p *Provider) CreateDatabase(ctx context.Context, user, database string) error {
	if !owns(user, database) {
		return fmt.Errorf("plain: %s is not a database of %s: an account's databases are named %s or %s_...", database, user, user, user)
	}
	if !usableName(database) {
		return fmt.Errorf("plain: %q is not a database name", database)
	}
	cmd := exec.CommandContext(ctx, p.mysql(), "-e", "CREATE DATABASE IF NOT EXISTS `"+database+"`")
	var complaint bytes.Buffer
	cmd.Stderr = &complaint
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("plain: create database %s: %w%s", database, err, saidOnStderr(complaint.String()))
	}
	return nil
}

// LoadDatabase feeds a dump to mysql, into a database the account owns
// by name.
func (p *Provider) LoadDatabase(ctx context.Context, user, database, dumpPath string) error {
	if !owns(user, database) {
		return fmt.Errorf("plain: %s is not a database of %s: an account's databases are named %s or %s_...", database, user, user, user)
	}
	if !usableName(database) {
		return fmt.Errorf("plain: %q is not a database name", database)
	}
	dump, err := os.Open(dumpPath)
	if err != nil {
		return fmt.Errorf("plain: open dump: %w", err)
	}
	defer dump.Close()
	cmd := exec.CommandContext(ctx, p.mysql(), database)
	cmd.Stdin = dump
	var complaint bytes.Buffer
	cmd.Stderr = &complaint
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("plain: load %s: %w%s", database, err, saidOnStderr(complaint.String()))
	}
	return nil
}

// PutCrontab: an account directory has no crontab of its own.
func (p *Provider) PutCrontab(context.Context, string, string) error {
	return unverified("cron jobs on a plain server are in the system backup, not in an account")
}

// PutDatabaseUsers: a plain server's backup records no database users.
func (p *Provider) PutDatabaseUsers(context.Context, string, []panel.DatabaseUser) error {
	return unverified("database users are not recorded in a plain server's backup")
}
