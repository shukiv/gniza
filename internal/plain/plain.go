// Package plain is the panel of a server that has no panel.
//
// A LAMP server keeps one site per directory under /var/www; a container
// host keeps one stack per directory under /opt and its data in volumes.
// Nothing on such a machine says what an account is, so the operator
// does: a source is one directory, read where it lies, with the MySQL
// and PostgreSQL databases ticked beside it, or the volumes of a
// container; each source is backed up as an account of its own. What is
// under the roots is offered, not assumed. See ADR 0022 and ADR 0025.
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
const Provisional = "backups of chosen directories, MySQL and PostgreSQL databases and container " +
	"volumes have been exercised against fakes and driven from the terminal and the browser; " +
	"a restore of files and databases has not yet been run on a live server (see ADR 0022)"

// PostgresSuffix marks a PostgreSQL database wherever a name has to say
// which client it belongs to: in an account's database list, and in the
// name of its dump. A MySQL "shop" and a PostgreSQL "shop" can both be
// chosen, and "shop.pg.sql" beside "shop.sql" keeps them apart through
// a restore that only knows the dump's name.
const PostgresSuffix = ".pg"

func unverified(what string) error {
	return fmt.Errorf("%w: %s (see ADR 0022)", ErrUnverified, what)
}

// Catalog is where the chosen sources are kept: the node's store, or a
// list in memory under test.
type Catalog interface {
	Sources() ([]panel.Source, error)
	SaveSource(panel.Source) error
	DeleteSource(name string) error
}

// Provider backs up what the operator chose.
type Provider struct {
	// Roots are the directories whose subdirectories are offered as
	// folders to back up: /var/www, /srv, /opt. A root that is not there
	// is skipped, so one list serves a LAMP server and a container host
	// alike. Nothing under them is backed up until chosen.
	Roots []string
	// Catalog holds the choices. Nil means nothing has been chosen and
	// nothing can be.
	Catalog Catalog
	// MySQLPath and MysqldumpPath are the MySQL clients; PsqlPath and
	// PgDumpPath the PostgreSQL ones. Empty means the ones on PATH; a
	// server without a client has no databases of that kind to offer.
	MySQLPath     string
	MysqldumpPath string
	PsqlPath      string
	PgDumpPath    string
	PgDumpallPath string
	// PostgresUser is the unix account the PostgreSQL clients run as
	// when this process is root, because a fresh PostgreSQL trusts that
	// account and nobody else. Empty means postgres; "-" means run them
	// as this process, which is what a test does.
	PostgresUser string
	// DockerPath and PodmanPath are the container engines. Empty means
	// the ones on PATH; an engine that is not there offers nothing.
	DockerPath string
	PodmanPath string
	// Log is where the provider says what it ran, at debug.
	Log *slog.Logger
}

func (p *Provider) Name() string         { return PanelName }
func (p *Provider) Layout() panel.Layout { return Layout{} }

// ReadsHomeInPlace: the source's directory is backed up from where it
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

// Accounts is every chosen source, as an account: its name, its
// directory, and whether the directory is still there.
func (p *Provider) Accounts(ctx context.Context) ([]panel.AccountInfo, error) {
	sources, err := p.Sources(ctx)
	if err != nil {
		return nil, err
	}
	accounts := make([]panel.AccountInfo, 0, len(sources))
	for _, source := range sources {
		info := panel.AccountInfo{User: source.Name, HomeDir: source.Path}
		if source.Path != "" {
			if stat, err := os.Stat(source.Path); err != nil || !stat.IsDir() {
				info.Missing = true
			}
		}
		accounts = append(accounts, info)
	}
	return accounts, nil
}

// usableName is a name that can be a source's: something a snapshot
// tag, a staging directory and a database name can carry.
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

// Account finds one source and measures it: the size of its directory,
// and the databases chosen with it, PostgreSQL ones marked.
func (p *Provider) Account(ctx context.Context, user string) (panel.AccountInfo, error) {
	source, err := p.source(user)
	if err != nil {
		return panel.AccountInfo{}, err
	}
	info := panel.AccountInfo{User: source.Name, HomeDir: source.Path}
	if source.Path != "" {
		if stat, err := os.Stat(source.Path); err != nil || !stat.IsDir() {
			info.Missing = true
		} else {
			info.SizeBytes = dirSize(source.Path)
		}
	}
	if len(source.MySQL) > 0 {
		sizes, err := p.databaseSizes(ctx)
		if err != nil {
			// No MySQL client, or one that cannot connect: said once at
			// warn, and the dumps will say the same, each, when they fail.
			p.log().Warn("the MySQL databases could not be measured", "source", user, "error", err)
		}
		for _, name := range source.MySQL {
			info.Databases = append(info.Databases, name)
			info.LeanBytes += sizes[name]
		}
	}
	if len(source.PostgreSQL) > 0 {
		sizes, err := p.postgresSizes(ctx)
		if err != nil {
			p.log().Warn("the PostgreSQL databases could not be measured", "source", user, "error", err)
		}
		for _, name := range source.PostgreSQL {
			info.Databases = append(info.Databases, name+PostgresSuffix)
			info.LeanBytes += sizes[name]
		}
	}
	return info, nil
}

// AccountIdentity is the directory itself: the moment it was created,
// to the nanosecond. A directory removed and made again under the same
// name is a different account, which is what the identity exists to
// notice; the owner's uid would be the web server's for every site and
// could not tell them apart, and a freed inode number is handed out
// again at once. A filesystem that does not record birth times falls
// back to the inode. A source with no directory is identified by the
// moment it was chosen.
func (p *Provider) AccountIdentity(user string) (int, error) {
	source, err := p.source(user)
	if err != nil {
		return 0, fmt.Errorf("plain: %s is %w", user, panel.ErrNoSuchAccount)
	}
	if source.Path == "" {
		return int(source.AddedAt.UnixNano()), nil
	}
	return directoryIdentity(source.Path)
}

func directoryIdentity(dir string) (int, error) {
	var stat unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, dir, unix.AT_STATX_SYNC_AS_STAT, unix.STATX_BTIME|unix.STATX_INO, &stat); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Chosen, but gone: no account is there to have an identity,
			// and the backup will say so.
			return 0, fmt.Errorf("plain: %s is %w: the directory is gone", dir, panel.ErrNoSuchAccount)
		}
		return 0, fmt.Errorf("plain: identify %s: %w", dir, err)
	}
	if stat.Mask&unix.STATX_BTIME != 0 && (stat.Btime.Sec != 0 || stat.Btime.Nsec != 0) {
		return int(stat.Btime.Sec)*1_000_000_000 + int(stat.Btime.Nsec), nil
	}
	return int(stat.Ino), nil
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

// databaseSizes lists every MySQL database with the bytes it holds. The
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

// Record is what the metadata part says a source is, so a snapshot read
// on another machine, or by a later version, knows where the files came
// from and which dumps belong to them.
type Record struct {
	Account string `json:"account"`
	Path    string `json:"path,omitempty"`
	// Databases names the dumps taken, as the dump files are named
	// without ".sql": a PostgreSQL one carries PostgresSuffix.
	Databases  []string            `json:"databases"`
	MySQL      []string            `json:"mysql,omitempty"`
	PostgreSQL []string            `json:"postgresql,omitempty"`
	Container  *panel.ContainerRef `json:"container,omitempty"`
	// ComposeFiles are the compose files copied beside the record, by
	// their original paths, for a source made from a container.
	ComposeFiles []string `json:"compose_files,omitempty"`
	// Grants names the files beside the record that hold the database
	// accounts' grants (MySQL) or the roles (PostgreSQL), kept for
	// reference and not run on restore.
	Grants   []string  `json:"grants,omitempty"`
	Hostname string    `json:"hostname"`
	TakenAt  time.Time `json:"taken_at"`
}

// Stage writes the dumps and the record into StagingDir and points the
// payload at the source's own directory for the files.
func (p *Provider) Stage(ctx context.Context, req panel.StageRequest) (pkgacct.Payload, error) {
	if req.Mode != pkgacct.ModeSplit {
		return pkgacct.Payload{}, fmt.Errorf("plain: a server without a panel has no %s backup; only split", req.Mode)
	}
	source, err := p.source(req.Account.User)
	if err != nil {
		return pkgacct.Payload{}, err
	}
	if req.SkipHomedir && source.Path != "" {
		return pkgacct.Payload{}, fmt.Errorf("plain: the files are the source; a backup that leaves them out has nothing to hold")
	}
	metadata := filepath.Join(req.StagingDir, "metadata")
	if err := os.MkdirAll(filepath.Join(metadata, RecordDir), 0o700); err != nil {
		return pkgacct.Payload{}, fmt.Errorf("plain: create %s: %w", metadata, err)
	}
	// A source of databases only still has a files part, because a
	// snapshot is read back through one home directory part and one
	// metadata part (internal/reassemble) and nothing else. It holds one
	// note, since a payload with an empty part is refused as incomplete.
	files := source.Path
	if files == "" {
		files = filepath.Join(req.StagingDir, FilesDir)
		if err := os.MkdirAll(files, 0o700); err != nil {
			return pkgacct.Payload{}, fmt.Errorf("plain: create %s: %w", files, err)
		}
		note := "This source backs up databases only. Its dumps are in the metadata part, under databases/.\n"
		if err := os.WriteFile(filepath.Join(files, NoFolderNote), []byte(note), 0o600); err != nil {
			return pkgacct.Payload{}, fmt.Errorf("plain: write %s: %w", NoFolderNote, err)
		}
	}
	payload := pkgacct.Payload{
		Mode:    pkgacct.ModeSplit,
		Account: source.Name,
		Parts: []pkgacct.Part{
			{Kind: pkgacct.PartMetadata, Path: metadata},
			{Kind: pkgacct.PartHomedir, Path: files},
		},
		DumpPaths: map[string]string{},
	}

	var dumped []string
	if !req.SkipDatabases && len(source.MySQL)+len(source.PostgreSQL) > 0 {
		dumps := filepath.Join(metadata, DumpDir)
		if err := os.MkdirAll(dumps, 0o700); err != nil {
			return pkgacct.Payload{}, fmt.Errorf("plain: create %s: %w", dumps, err)
		}
		take := func(shown, name string, dump func() error) {
			path := filepath.Join(dumps, shown+".sql")
			if err := dump(); err != nil {
				// ADR 0017: one table that cannot be dumped does not cost
				// the source its files and its other databases.
				_ = os.Remove(path)
				payload.Missing = append(payload.Missing, pkgacct.Omission{
					What: "database " + name, Why: err.Error()})
				return
			}
			payload.DumpPaths[shown] = path
			dumped = append(dumped, shown)
		}
		for _, name := range source.MySQL {
			path := filepath.Join(dumps, name+".sql")
			take(name, name, func() error { return p.dumpOne(ctx, name, path) })
		}
		for _, name := range source.PostgreSQL {
			shown := name + PostgresSuffix
			path := filepath.Join(dumps, shown+".sql")
			take(shown, name+" (PostgreSQL)", func() error { return p.dumpPostgres(ctx, name, path) })
		}
	}

	// Who used the databases, kept beside the record -- not under the
	// dumps, where every .sql is expected to create something -- so a
	// restore on another machine can make the accounts before loading.
	var grants []string
	keep := func(file, what string, read func() ([]byte, error)) {
		text, err := read()
		if err != nil {
			payload.Missing = append(payload.Missing, pkgacct.Omission{What: what, Why: err.Error()})
			return
		}
		if err := os.WriteFile(filepath.Join(metadata, RecordDir, file), text, 0o600); err != nil {
			payload.Missing = append(payload.Missing, pkgacct.Omission{What: what, Why: err.Error()})
			return
		}
		grants = append(grants, file)
	}
	if !req.SkipDatabases {
		for _, name := range source.MySQL {
			keep(MySQLGrantsFile(name), "grants on "+name, func() ([]byte, error) { return p.mysqlGrants(ctx, name) })
		}
		if len(source.PostgreSQL) > 0 {
			keep(PostgresRolesFile, "PostgreSQL roles", func() ([]byte, error) { return p.postgresRoles(ctx) })
		}
	}

	record := Record{
		Account: source.Name, Path: source.Path, Databases: dumped, Grants: grants,
		MySQL: source.MySQL, PostgreSQL: source.PostgreSQL, Container: source.Container,
		TakenAt: time.Now().UTC(),
	}
	record.Hostname, _ = os.Hostname()
	if source.Container != nil {
		described, err := p.describeContainer(ctx, source, filepath.Join(metadata, RecordDir))
		if err != nil {
			// The volume is still worth having without the description;
			// the omission says what a rebuild would lack.
			payload.Missing = append(payload.Missing, pkgacct.Omission{
				What: "description of container " + source.Container.Name, Why: err.Error()})
		} else {
			record.ComposeFiles = described.composeFiles
			if described.running && source.Container.Mount != "" {
				payload.Warnings = append(payload.Warnings, fmt.Sprintf(
					"container %s was running while %s was read; a database kept inside it "+
						"may not be consistent in this backup -- stop the container for the "+
						"backup, or dump the database itself", source.Container.Name, source.Path))
			}
		}
	}
	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return pkgacct.Payload{}, err
	}
	if err := os.WriteFile(filepath.Join(metadata, RecordDir, RecordFile), append(body, '\n'), 0o600); err != nil {
		return pkgacct.Payload{}, fmt.Errorf("plain: write the source record: %w", err)
	}
	return payload, payload.Verify()
}

// dumpOne writes one MySQL database's dump, uncompressed so restic can
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
// sources restored onto it mean anything: the web, database, container
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
		"containers": commandOutput(ctx, p.docker(), "ps", "-a", "--format", "{{.Names}}\t{{.Image}}\t{{.Status}}"),
		"pods":       commandOutput(ctx, p.podman(), "ps", "-a", "--format", "{{.Names}}\t{{.Image}}\t{{.Status}}"),
		"volumes":    commandOutput(ctx, p.docker(), "volume", "ls", "--format", "{{.Name}}\t{{.Mountpoint}}"),
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
	"/etc/postgresql", "/var/lib/pgsql/data/postgresql.conf", "/var/lib/pgsql/data/pg_hba.conf",
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

// PutHomeDir writes a restored tree over the source's directory. What
// the backup holds replaces what is there file by file; a file added
// since stays, because nothing here knows whether it is wanted.
func (p *Provider) PutHomeDir(ctx context.Context, user, from string) error {
	source, err := p.source(user)
	if err != nil {
		return err
	}
	if source.Path == "" {
		return fmt.Errorf("plain: %s has no directory; it is a source of databases only", user)
	}
	return filepath.Walk(from, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		into := filepath.Join(source.Path, rel)
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

// databaseOf says which client a database of a source belongs to, and
// refuses one the source was not chosen with: a dump is loaded only
// into a database the operator said was this source's.
func (p *Provider) databaseOf(user, database string) (name string, postgres bool, err error) {
	source, err := p.source(user)
	if err != nil {
		return "", false, err
	}
	if trimmed, isPostgres := strings.CutSuffix(database, PostgresSuffix); isPostgres {
		for _, chosen := range source.PostgreSQL {
			if chosen == trimmed {
				return trimmed, true, nil
			}
		}
		return "", false, fmt.Errorf("plain: %s is not a PostgreSQL database of %s; add it to the source first", trimmed, user)
	}
	for _, chosen := range source.MySQL {
		if chosen == database {
			return database, false, nil
		}
	}
	return "", false, fmt.Errorf("plain: %s is not a MySQL database of %s; add it to the source first", database, user)
}

// CreateDatabase makes a database the source was chosen with.
func (p *Provider) CreateDatabase(ctx context.Context, user, database string) error {
	name, postgres, err := p.databaseOf(user, database)
	if err != nil {
		return err
	}
	if !usableName(name) {
		return fmt.Errorf("plain: %q is not a database name", name)
	}
	if postgres {
		return p.createPostgres(ctx, name)
	}
	cmd := exec.CommandContext(ctx, p.mysql(), "-e", "CREATE DATABASE IF NOT EXISTS `"+name+"`")
	var complaint bytes.Buffer
	cmd.Stderr = &complaint
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("plain: create database %s: %w%s", name, err, saidOnStderr(complaint.String()))
	}
	return nil
}

// LoadDatabase feeds a dump to the client it came from, into a database
// the source was chosen with.
func (p *Provider) LoadDatabase(ctx context.Context, user, database, dumpPath string) error {
	name, postgres, err := p.databaseOf(user, database)
	if err != nil {
		return err
	}
	if !usableName(name) {
		return fmt.Errorf("plain: %q is not a database name", name)
	}
	dump, err := os.Open(dumpPath)
	if err != nil {
		return fmt.Errorf("plain: open dump: %w", err)
	}
	defer dump.Close()
	var cmd *exec.Cmd
	if postgres {
		cmd = p.pgCommand(ctx, p.psql(), "-v", "ON_ERROR_STOP=1", "-q", "-d", name)
	} else {
		cmd = exec.CommandContext(ctx, p.mysql(), name)
	}
	cmd.Stdin = dump
	var complaint bytes.Buffer
	cmd.Stderr = &complaint
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("plain: load %s: %w%s", database, err, saidOnStderr(complaint.String()))
	}
	return nil
}

// PutCrontab: a source has no crontab of its own.
func (p *Provider) PutCrontab(context.Context, string, string) error {
	return unverified("cron jobs on a plain server are in the system backup, not in a source")
}

// PutDatabaseUsers: a plain server's backup records no database users.
func (p *Provider) PutDatabaseUsers(context.Context, string, []panel.DatabaseUser) error {
	return unverified("database users are not recorded in a plain server's backup")
}

// sortedCopy is a sorted copy of a list, with blanks and repeats gone.
func sortedCopy(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
