package cpanel

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	osuser "os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/shukiv/gniza/internal/granular"
	"github.com/shukiv/gniza/internal/pkgacct"
)

// Real drives the cPanel tooling installed on the host.
//
// This is the one part of the agent that cannot be exercised without a
// cPanel server. Everything it depends on — flag probing, payload planning,
// staging, restic execution — is tested independently.
type Real struct {
	// Log is where this provider says what it ran. Everything it writes is
	// at debug, so a server at the default level pays nothing for it, and
	// a server being debugged gets the commands themselves -- which is the
	// difference between "exit status 2" and something reproducible by
	// hand. Nil is a provider that says nothing.
	Log *slog.Logger
	// PkgacctPath is the pkgacct script. Empty means the standard location.
	PkgacctPath string
	// MysqldumpPath is the dump utility. Empty means "mysqldump" on PATH.
	MysqldumpPath string
	// RestorepkgPath applies an account archive. Empty means the standard
	// location.
	RestorepkgPath string
	// RemoveacctPath removes a disposable certification account. Empty
	// means the standard cPanel script.
	RemoveacctPath string
	// HomeRoot is where account home directories live.
	HomeRoot string
	// UsersDir holds one file per cPanel account. Empty means the standard
	// location.
	UsersDir string
	// MysqlPath is the client used to read database users and their
	// grants. Empty means whatever is on PATH.
	MysqlPath string
	// EasyApachePath is the tool that writes an EasyApache profile.
	// Empty means the standard location.
	EasyApachePath string
	// SuspendedDir holds one file per suspended account. Empty means the
	// standard location.
	SuspendedDir string
	// DatabasesDir holds cPanel's own record of which databases belong to
	// which account. Empty means the standard location.
	DatabasesDir string
	// DBOwnersPath is the server-wide database-to-owner map. Empty means
	// the standard location.
	DBOwnersPath string
	// PostgresPaths are where PostgreSQL would be if this server had it.
	// Empty means the standard locations.
	PostgresPaths []string
	// ServerExcludeConf is the server-wide cpbackup-exclude.conf. Empty
	// means the standard location.
	ServerExcludeConf string
	// DBMapTool records which database users and databases belong to an
	// account. Empty means the standard cPanel script.
	DBMapTool string
	// UapiPath calls cPanel's user API as a named account. Empty means
	// the standard cPanel script.
	UapiPath string
	// Crontab writes an account's cron jobs. Empty means "crontab" on
	// PATH.
	Crontab string
	// ReplacedDir is where a copy of what a restore wrote over is kept.
	// Empty means the standard location under /var/lib/gniza.
	ReplacedDir string
}

var _ Provider = (*Real)(nil)

func (r *Real) pkgacct() string {
	if r.PkgacctPath != "" {
		return r.PkgacctPath
	}
	return "/scripts/pkgacct"
}

// debug writes one line, if anybody is listening. Every caller passes
// arguments it already has, so a provider with no logger costs a nil check.
func (r *Real) debug(msg string, args ...any) {
	if r.Log == nil {
		return
	}
	r.Log.Debug(msg, args...)
}

// ranCommand is the line every external command gets at debug: what was
// run, with its arguments, and how long it took. It is what makes a
// failure reproducible -- an operator can copy the command out of the log
// and run it themselves, which is how the first real MySQL corruption on
// one of these servers was found.
// The account is on the line because that is what the service log is
// filtered by: a line about one account's dump that does not name the
// account is not in that account's story when somebody goes looking.
func (r *Real) ranCommand(what, account string, cmd *exec.Cmd, started time.Time, err error) {
	if r.Log == nil {
		return
	}
	args := []any{"account", account,
		"command", cmd.Path, "args", strings.Join(cmd.Args[1:], " "),
		"took", time.Since(started).Round(time.Millisecond).String()}
	if err != nil {
		args = append(args, "error", err.Error())
	}
	r.Log.Debug(what, args...)
}

func (r *Real) mysqldump() string {
	if r.MysqldumpPath != "" {
		return r.MysqldumpPath
	}
	return "mysqldump"
}

func (r *Real) mysql() string {
	if r.MysqlPath != "" {
		return r.MysqlPath
	}
	return "mysql"
}

func (r *Real) uapi() string {
	if r.UapiPath != "" {
		return r.UapiPath
	}
	return "/usr/local/cpanel/bin/uapi"
}

func (r *Real) homeRoot() string {
	if r.HomeRoot != "" {
		return r.HomeRoot
	}
	return "/home"
}

// Capabilities probes the installed pkgacct rather than assuming flag
// names, which have moved between cPanel versions.
func (r *Real) Capabilities(ctx context.Context) (pkgacct.Capabilities, error) {
	cmd := exec.CommandContext(ctx, r.pkgacct(), "--help")
	// pkgacct --help exits non-zero on some versions; the text is what
	// matters, so only a failure to run at all is fatal.
	output, err := cmd.CombinedOutput()
	if len(output) == 0 && err != nil {
		return pkgacct.Capabilities{}, fmt.Errorf("cpanel: run pkgacct --help: %w", err)
	}
	return pkgacct.ProbeCapabilities(string(output)), nil
}

// Accounts lists the cPanel accounts on this host.
//
// This is deliberately cheap: it reads the account names and their home
// directories and nothing else. Measuring sizes means walking every home
// directory, and listing databases means a MySQL round trip per account —
// on a real server with twenty accounts that was nineteen seconds, for a
// page that only needed the names.
//
// It also never drops an account because some subsystem is unhappy. An
// account whose databases cannot be listed is still an account, and hiding
// it would tell an operator they have nothing to back up.
func (r *Real) Accounts(_ context.Context) ([]AccountInfo, error) {
	entries, err := os.ReadDir(r.usersDir())
	if err != nil {
		return nil, fmt.Errorf("cpanel: list accounts: %w", err)
	}

	var accounts []AccountInfo
	for _, entry := range entries {
		user := entry.Name()
		if entry.IsDir() || strings.HasPrefix(user, ".") || user == "system" {
			continue
		}
		if err := validateUser(user); err != nil {
			continue
		}
		// Where cPanel says this account lives, which is not always
		// /home/<name>: an account moved to another partition kept its
		// name and changed its path, and reconstructing the path from
		// the name dropped it out of every backup without an error.
		home := r.homeFor(user)
		if info, err := os.Stat(home); err != nil || !info.IsDir() {
			// Still listed. An account cPanel knows about whose home is
			// missing is a problem to be shown, not one to be hidden by
			// leaving it off the page that says what gets backed up.
			accounts = append(accounts, AccountInfo{User: user, HomeDir: home, Missing: true})
			continue
		}
		accounts = append(accounts, AccountInfo{User: user, HomeDir: home})
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].User < accounts[j].User })
	return accounts, nil
}

func (r *Real) usersDir() string {
	if r.UsersDir != "" {
		return r.UsersDir
	}
	return "/var/cpanel/users"
}

// Account reads an account's home directory and database list.
func (r *Real) Account(ctx context.Context, user string) (AccountInfo, error) {
	if err := validateUser(user); err != nil {
		return AccountInfo{}, err
	}
	home := r.homeFor(user)
	info, err := os.Stat(home)
	if err != nil {
		return AccountInfo{}, fmt.Errorf("cpanel: account home %s: %w", home, err)
	}
	if !info.IsDir() {
		return AccountInfo{}, fmt.Errorf("cpanel: account home %s is not a directory", home)
	}

	databases, err := r.databases(ctx, user)
	if err != nil {
		return AccountInfo{}, err
	}
	hasPostgreSQL, postgresRecorded := r.recordedPostgreSQL(user)
	if !postgresRecorded {
		// Split mode cannot dump PostgreSQL itself, so an account that
		// has it needs cPanel's complete archive. When the map cannot
		// say, ask the server instead of assuming the worst: assuming it
		// costs a full copy of the account every night, for ever, and
		// nothing anywhere says why.
		hasPostgreSQL = r.postgresInstalled()
	}
	size, err := directorySize(home)
	if err != nil {
		return AccountInfo{}, err
	}
	return AccountInfo{
		User: user, HomeDir: home, Databases: databases,
		HasPostgreSQL: hasPostgreSQL, SizeBytes: size,
	}, nil
}

// databases lists the account's MySQL databases.
//
// cPanel keeps its own record of which databases belong to which account,
// and that record is what this reads. The naming convention — everything
// starting with "<account>_" — is only a fallback, because it is wrong in
// both directions: a server with database prefixing turned off has
// databases that carry no account name at all, and an account called
// "rozin" would otherwise claim "rozingroup_data" from the account called
// "rozingroup".
//
// A failure here is returned rather than swallowed. Backing an account up
// while quietly omitting its databases would produce a backup that looks
// fine and restores a site with no content.
func (r *Real) databases(ctx context.Context, user string) ([]string, error) {
	if recorded, _, found := r.recordedDatabases(user); found {
		return recorded, nil
	}

	cmd := exec.CommandContext(ctx, r.mysql(), "--batch", "--skip-column-names",
		"-e", "SHOW DATABASES")
	output, err := cmd.Output()
	if err != nil {
		detail := ""
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			detail = ": " + lastLine(exitErr.Stderr)
		}
		return nil, fmt.Errorf(
			"cpanel: list databases for %s%s (root's MySQL credentials come from "+
				"~/.my.cnf, so the service needs HOME set): %w", user, detail, err)
	}
	var databases []string
	for _, line := range strings.Split(string(output), "\n") {
		name := strings.TrimSpace(line)
		if name != "" && strings.HasPrefix(name, user+"_") {
			databases = append(databases, name)
		}
	}
	// The prefix caught them; the server-wide owner map says which of
	// them are actually this account's.
	return r.ownedDatabases(user, databases), nil
}

// databaseUsersByName finds an account's database users by the cPanel
// naming convention. It is the fallback for a server that has no record.
func (r *Real) databaseUsersByName(ctx context.Context, account string) ([]string, error) {
	// account is checked by plainAccountName before this is reached, so
	// it cannot carry a quote; there is no parameter binding in the
	// mysql client and this is the whole reason that check exists.
	list := exec.CommandContext(ctx, r.mysql(), "--batch", "--raw", "--skip-column-names", "--execute",
		fmt.Sprintf("SELECT DISTINCT user FROM mysql.user "+
			"WHERE user = '%s' OR user LIKE '%s\\_%%'", account, account))
	output, err := list.Output()
	if err != nil {
		return nil, fmt.Errorf("cpanel: list database users for %s: %w", account, err)
	}
	var users []string
	for _, line := range strings.Split(string(output), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			users = append(users, name)
		}
	}
	return users, nil
}

// qualifyDatabaseUsers turns bare user names into the quoted user@host
// forms SHOW CREATE USER and SHOW GRANTS need. One name can have several
// hosts, and a grant on the wrong one restores an application that cannot
// connect.
func (r *Real) qualifyDatabaseUsers(ctx context.Context, names []string) ([]string, error) {
	var qualified []string
	for _, name := range names {
		if !plainDatabaseUser(name) {
			// Not a name this program will interpolate into a statement.
			continue
		}
		out, err := exec.CommandContext(ctx, r.mysql(), "--batch", "--skip-column-names",
			"--execute", fmt.Sprintf(
				"SELECT CONCAT(QUOTE(user), '@', QUOTE(host)) FROM mysql.user WHERE user = '%s'",
				name)).Output()
		if err != nil {
			return nil, fmt.Errorf("cpanel: look up database user %s: %w", name, err)
		}
		for _, row := range strings.Split(string(out), "\n") {
			if row = strings.TrimSpace(row); row != "" {
				qualified = append(qualified, row)
			}
		}
	}
	return qualified, nil
}

// Stage runs pkgacct and, in split mode, dumps each database separately.
func (r *Real) Stage(ctx context.Context, req StageRequest) (pkgacct.Payload, error) {
	caps, err := r.Capabilities(ctx)
	if err != nil {
		return pkgacct.Payload{}, err
	}
	mode, fallbackReason, err := safeDatabaseMode(req)
	if err != nil {
		return pkgacct.Payload{}, err
	}
	r.debug("staging an account",
		"account", req.Account.User, "dir", req.StagingDir, "mode", mode,
		"databases", len(req.Account.Databases),
		"skip_homedir", req.SkipHomedir, "skip_databases", req.SkipDatabases,
		"skip_email", req.SkipEmail)
	payload, err := pkgacct.Plan(pkgacct.PlanRequest{
		Account:       req.Account.User,
		HomeDir:       req.Account.HomeDir,
		Databases:     req.Account.Databases,
		StagingDir:    req.StagingDir,
		Mode:          mode,
		Caps:          caps,
		SkipHomedir:   req.SkipHomedir,
		SkipDatabases: req.SkipDatabases,
		SkipEmail:     req.SkipEmail,
	})
	if err != nil {
		return pkgacct.Payload{}, err
	}
	if fallbackReason != "" {
		payload.Degraded = true
		if payload.Reason != "" {
			payload.Reason += "; "
		}
		payload.Reason += fallbackReason
	}

	// pkgacct writes its archive into a directory it is given and does not
	// create it. Without this it wrote nothing, exited zero, and the
	// backup silently lost the account's configuration.
	for _, part := range payload.Parts {
		if part.Kind == pkgacct.PartMetadata {
			if err := os.MkdirAll(part.Path, 0o700); err != nil {
				return pkgacct.Payload{}, fmt.Errorf("cpanel: create %s: %w", part.Path, err)
			}
		}
	}

	// cPanel keeps one file per account, and that file is what makes a
	// name an account here. A name can have one and no unix user behind
	// it -- cPanel's own support tooling leaves such a user file behind,
	// and so does a removal that got half way -- and pkgacct cannot
	// package a name it cannot resolve to a uid. Said here, because what
	// pkgacct does about it is print its own usage and exit 2.
	if _, err := osuser.Lookup(req.Account.User); err != nil {
		var unknown osuser.UnknownUserError
		if errors.As(err, &unknown) {
			return pkgacct.Payload{}, fmt.Errorf(
				"cpanel: %s has a cPanel user file but no user on this server, "+
					"so cPanel cannot package it", req.Account.User)
		}
		return pkgacct.Payload{}, fmt.Errorf("cpanel: look up %s on this server: %w",
			req.Account.User, err)
	}

	args := pkgacct.CommandArgs(req.Account.User, req.StagingDir, mode, caps, req.SkipEmail)
	cmd := exec.CommandContext(ctx, r.pkgacct(), args...)
	started := time.Now()
	output, err := cmd.CombinedOutput()
	r.ranCommand("packaged an account", req.Account.User, cmd, started, err)
	if err != nil {
		return pkgacct.Payload{}, fmt.Errorf("cpanel: pkgacct failed: %w: %s",
			err, pkgacctFailure(output))
	}

	if mode == pkgacct.ModeSplit {
		missing, err := r.dumpDatabases(ctx, req, payload)
		if err != nil {
			return pkgacct.Payload{}, err
		}
		payload.Missing = append(payload.Missing, missing...)
	}

	// pkgacct can exit zero having produced nothing useful, so what it
	// left behind is checked rather than assumed.
	if err := payload.Verify(); err != nil {
		return pkgacct.Payload{}, fmt.Errorf("%w (pkgacct said: %s)", err, lastLine(output))
	}
	return payload, nil
}

// safeDatabaseMode prevents split mode's MySQL-only dump path from dropping
// PostgreSQL databases. A complete account can fall back to pkgacct's own
// archive. A request that deliberately omitted the home directory cannot:
// monolithic pkgacct would violate that request, so it fails explicitly.
func safeDatabaseMode(req StageRequest) (pkgacct.Mode, string, error) {
	if req.Mode != pkgacct.ModeSplit || req.SkipDatabases || !req.Account.HasPostgreSQL {
		return req.Mode, "", nil
	}
	if req.SkipHomedir {
		return "", "", fmt.Errorf(
			"cpanel: cannot create a databases-only split backup for %s: "+
				"the account has PostgreSQL databases and split mode only dumps MySQL",
			req.Account.User)
	}
	return pkgacct.ModeMonolithic,
		"account has PostgreSQL databases; using cPanel's complete archive instead of MySQL-only split mode", nil
}

// dumpDatabaseUsers writes the account's database users and their grants.
//
// pkgacct puts them in the archive it builds — but only when it is also
// dumping the databases, which in split mode it is not. Without this a
// restore brings back every table and nothing that can read them.
func (r *Real) dumpDatabaseUsers(ctx context.Context, req StageRequest, dir string) error {
	account := req.Account.User
	if !plainAccountName(account) {
		return fmt.Errorf("cpanel: %q is not a cPanel account name", account)
	}

	// Which database users are this account's is a question cPanel has
	// already answered, in the same record that says which databases are.
	// The name convention is the fallback, and it is wrong in the same
	// two directions: it misses a user that carries no prefix, and it
	// claims one belonging to an account whose name starts with this one.
	named, recorded := r.recordedDatabaseUsers(account)
	if !recorded {
		var err error
		if named, err = r.databaseUsersByName(ctx, account); err != nil {
			return err
		}
	}

	// Two files, for two readers.
	//
	// The first is written the way cPanel writes its own, because
	// restorepkg is what reads it, and a restore proved that it reads
	// nothing else: a file of modern CREATE USER statements is valid
	// SQL, sits in exactly the right place, and every line of it was
	// ignored -- the account came back with its tables and without the
	// user that reads them.
	//
	// The second is for a person. cPanel's format uses IDENTIFIED BY
	// PASSWORD, which MySQL 8 removed, so the file the restore needs is
	// one nobody can run. An operator or a customer who pulls their
	// database users out of a backup gets one they can paste into a
	// client, with the same hashes.
	var script strings.Builder
	script.WriteString("-- cPanel mysql backup\n")
	script.WriteString("--\n-- This is the file cPanel's own restore reads. Its GRANT ... " +
		"IDENTIFIED BY\n-- PASSWORD syntax was removed in MySQL 8 and will not run there: to " +
		"recreate\n-- these users by hand, use " + granular.RunnableDatabaseUsersFile + " beside it.\n")

	var runnable strings.Builder
	runnable.WriteString("-- Database users and grants for " + account + "\n")
	runnable.WriteString("--\n-- The same users as " + granular.DatabaseUsersFile +
		", written so they can be run against\n-- a current MySQL. Passwords are carried as " +
		"their stored hashes, so the users\n-- come back with the passwords they had.\n")

	auth := map[string]map[string]databaseAuth{}

	for _, name := range named {
		if !plainDatabaseUser(name) {
			continue
		}
		accounts, err := r.databaseUserAccounts(ctx, name)
		if err != nil {
			return err
		}
		for _, who := range accounts {
			// GRANT USAGE carries the password, which is how cPanel's
			// own file carries it and the only line restorepkg reads it
			// from.
			script.WriteString(fmt.Sprintf(
				"GRANT USAGE ON *.* TO '%s'@'%s' IDENTIFIED BY PASSWORD '%s';\n",
				who.User, who.Host, who.Hash))

			grants, err := r.grantsFor(ctx, fmt.Sprintf("`%s`@`%s`", who.User, who.Host))
			if err != nil {
				return err
			}
			for _, grant := range grants {
				script.WriteString(grant + ";\n")
			}

			// The same thing in statements a current MySQL accepts.
			runnable.WriteString(fmt.Sprintf(
				"CREATE USER IF NOT EXISTS `%s`@`%s` IDENTIFIED WITH '%s' AS '%s';\n",
				who.User, who.Host, who.Plugin, who.Hash))
			for _, grant := range grants {
				// grantsFor rewrote the account into cPanel's single
				// quotes; MySQL takes either, and backticks are what it
				// prints itself.
				runnable.WriteString(grant + ";\n")
			}

			if auth[who.User] == nil {
				auth[who.User] = map[string]databaseAuth{}
			}
			auth[who.User][who.Host] = databaseAuth{
				PassHash: who.HexHash,
				Plugin:   who.Plugin,
			}
		}
	}

	path := filepath.Join(dir, granular.DatabaseUsersFile)
	if err := os.WriteFile(path, []byte(script.String()), 0o600); err != nil {
		return fmt.Errorf("cpanel: write database users: %w", err)
	}
	runnablePath := filepath.Join(dir, granular.RunnableDatabaseUsersFile)
	if err := os.WriteFile(runnablePath, []byte(runnable.String()), 0o600); err != nil {
		return fmt.Errorf("cpanel: write the runnable database users: %w", err)
	}
	// cPanel keeps the authentication plugin and the hash beside the
	// grants, because "IDENTIFIED BY PASSWORD" is not valid on MySQL 8
	// and its restore reads the real values from here.
	encoded, err := json.Marshal(auth)
	if err != nil {
		return fmt.Errorf("cpanel: encode database authentication: %w", err)
	}
	if err := os.WriteFile(path+"-auth.json", encoded, 0o600); err != nil {
		return fmt.Errorf("cpanel: write database authentication: %w", err)
	}
	return nil
}

// databaseAuth is one user's credentials as cPanel records them beside the
// grants: the hash as hex of its printable form, and the plugin that
// interprets it.
type databaseAuth struct {
	PassHash string `json:"pass_hash"`
	Plugin   string `json:"auth_plugin"`
}

// databaseUserAccount is one user@host and what authenticates it.
type databaseUserAccount struct {
	User string
	Host string
	// Hash is the stored password as MySQL prints it, which is what
	// cPanel's files carry. HexHash is the same bytes as hex, which is
	// how they are read and how a restore puts them back: a
	// caching_sha2_password string is binary, and reading it as text
	// through the mysql client loses it.
	Hash    string
	HexHash string
	Plugin  string
}

// databaseUserAccounts reads every host a database user exists on. One
// name can have several, and a grant restored against the wrong one is an
// application that cannot connect.
func (r *Real) databaseUserAccounts(ctx context.Context, name string) ([]databaseUserAccount, error) {
	// name is checked by plainDatabaseUser before this is reached; the
	// mysql client has no parameter binding, which is why that check
	// exists.
	out, err := exec.CommandContext(ctx, r.mysql(), "--batch", "--raw", "--skip-column-names",
		"--execute", fmt.Sprintf(
			"SELECT user, host, HEX(authentication_string), plugin "+
				"FROM mysql.user WHERE user = '%s'",
			name)).Output()
	if err != nil {
		return nil, fmt.Errorf("cpanel: look up database user %s: %w", name, err)
	}
	var accounts []databaseUserAccount
	for _, row := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(row) == "" {
			continue
		}
		fields := strings.Split(row, "\t")
		if len(fields) < 4 {
			return nil, fmt.Errorf("cpanel: unreadable row for database user %s", name)
		}
		// HEX() on the way out and decoded here: the column can hold
		// bytes that are not text, and a tab or a newline among them
		// would have split this row in the wrong place.
		stored, err := hex.DecodeString(fields[2])
		if err != nil {
			return nil, fmt.Errorf(
				"cpanel: unreadable stored password for database user %s: %w", name, err)
		}
		accounts = append(accounts, databaseUserAccount{
			User: fields[0], Host: fields[1],
			Hash: string(stored), HexHash: fields[2], Plugin: fields[3],
		})
	}
	return accounts, nil
}

// grantee matches the "TO `user`@`host`" clause of a grant.
var grantee = regexp.MustCompile("TO `((?:[^`]|``)*)`@`((?:[^`]|``)*)`")

// quoteGrantee rewrites a grant's TO clause the way cPanel writes it,
// leaving the rest of the statement alone.
func quoteGrantee(row string) string {
	return grantee.ReplaceAllString(row, "TO '$1'@'$2'")
}

// grantsFor reads a user's privileges, leaving out the USAGE line that
// carries no privilege of its own -- that one is written separately,
// because it is where the password goes.
func (r *Real) grantsFor(ctx context.Context, who string) ([]string, error) {
	// --raw matters: without it the client escapes the backslash in a
	// database name like `account\_shop`, and the restored grant then
	// names a database that does not exist.
	out, err := exec.CommandContext(ctx, r.mysql(), "--batch", "--raw", "--skip-column-names",
		"--execute", "SHOW GRANTS FOR "+who).Output()
	if err != nil {
		return nil, fmt.Errorf("cpanel: read grants for %s: %w", who, err)
	}
	var grants []string
	for _, row := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		row = strings.TrimSpace(row)
		if row == "" || strings.HasPrefix(row, "GRANT USAGE ON *.* TO") {
			continue
		}
		// cPanel's own file quotes the account with single quotes. Only
		// the TO clause is rewritten: the database name before it stays
		// backtick-quoted, and anything after it -- WITH GRANT OPTION,
		// which an operator can have granted by hand -- has to survive
		// intact. Three string swaps got this wrong: they turned
		// "TO `u`@`h` WITH GRANT OPTION" into a line with one quote
		// missing, which is a malformed statement in the middle of the
		// file restorepkg reads.
		grants = append(grants, strings.TrimSuffix(quoteGrantee(row), ";"))
	}
	return grants, nil
}

// plainAccountName keeps a name that reaches a SQL statement to what a
// cPanel account name can actually be.
func plainAccountName(name string) bool {
	if name == "" || len(name) > 32 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// dumpDatabaseCreate writes the statement that creates the database, in
// the separate file a cpmove archive keeps it in.
//
// The table dump beside it is "mysqldump <name>", which carries no
// CREATE DATABASE and no character set: restoring it into a server that
// does not already have the database puts every table somewhere else, or
// nowhere. cPanel's own archives carry this as <name>.create, and this
// writes the same thing under the same name.
func (r *Real) dumpDatabaseCreate(ctx context.Context, account, name, dumpPath string) error {
	path := strings.TrimSuffix(dumpPath, ".sql") + ".create"
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("cpanel: create %s: %w", path, err)
	}
	cmd := exec.CommandContext(ctx, r.mysqldump(),
		"--no-data", "--no-create-info", "--databases", name)
	cmd.Stdout = file
	var complaint bytes.Buffer
	cmd.Stderr = &complaint
	started := time.Now()
	err = cmd.Run()
	r.ranCommand("dumped a database definition", account, cmd, started, err)
	closeErr := file.Close()
	if err != nil {
		return fmt.Errorf("cpanel: dump the definition of %s: %w%s",
			name, err, saidOnStderr(complaint.String()))
	}
	if closeErr != nil {
		return fmt.Errorf("cpanel: close %s: %w", path, closeErr)
	}
	return nil
}

// plainDatabaseUser holds a database user name to the same shape. It is
// separate from plainAccountName because these come from a file cPanel
// wrote rather than from a directory of account names, and a name from a
// file is exactly the kind that has never been checked.
func plainDatabaseUser(name string) bool { return plainAccountName(name) }

// dumpDatabases writes one dump per database, and keeps going when one of
// them will not dump.
//
// It used to return on the first failure. A live account with forty-seven
// databases and one corrupt table in one of them therefore had no backup
// at all for a week: not the other forty-six, not its home directory. One
// table is not a reason to store nothing.
//
// What it must not do is leave a quiet hole. cPanel's own pkgacct was
// measured doing exactly that on this account: it survived the crash, lost
// every database that came after it, and exited 0 saying "pkgacct
// completed". So a dump that failed is deleted rather than left as an
// empty file that would restore as a database with no tables, and every
// database not taken is returned to the caller to be recorded against the
// run.
func (r *Real) dumpDatabases(ctx context.Context, req StageRequest, payload pkgacct.Payload) ([]pkgacct.Omission, error) {
	if len(payload.DumpPaths) == 0 {
		return nil, nil
	}
	dir := filepath.Join(req.StagingDir, "databases")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("cpanel: create database staging: %w", err)
	}
	if err := r.dumpDatabaseUsers(ctx, req, dir); err != nil {
		// A database restored without the user that owns it is a database
		// no site can open, so this is part of the backup, not a bonus.
		return nil, err
	}

	// In name order, so that two runs of the same account report their
	// troubles in the same order and a reader can compare them.
	names := make([]string, 0, len(payload.DumpPaths))
	for name := range payload.DumpPaths {
		names = append(names, name)
	}
	sort.Strings(names)

	var missing []pkgacct.Omission
	for _, name := range names {
		path := payload.DumpPaths[name]
		err := r.dumpOneDatabase(ctx, req, name, path)
		if err != nil && r.serverIsGone(ctx) {
			// Not this database's fault. Reading a corrupt table can take
			// the whole server down with it, and every dump attempted
			// while it restarts fails for a reason that has nothing to do
			// with the database being dumped. Wait for it, then try this
			// one again -- pkgacct does not, which is how it lost
			// thirty-one databases in one run.
			if waitErr := r.waitForServer(ctx); waitErr == nil {
				err = r.dumpOneDatabase(ctx, req, name, path)
			}
		}
		if err == nil {
			if err := r.dumpDatabaseCreate(ctx, req.Account.User, name, path); err != nil {
				return nil, err
			}
			continue
		}

		// An empty dump left on disk is worse than no dump: it restores
		// as a database with no tables, and nothing says so.
		_ = os.Remove(path)
		_ = os.Remove(strings.TrimSuffix(path, ".sql") + ".create")
		missing = append(missing, pkgacct.Omission{
			What: "database " + name,
			Why:  errorText(err),
		})
		r.debug("a database would not dump", "account", req.Account.User,
			"database", name, "error", errorText(err))
	}
	return missing, nil
}

// dumpOneDatabase writes one database's dump, and says what mysqldump said
// when it could not.
func (r *Real) dumpOneDatabase(ctx context.Context, req StageRequest, name, path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("cpanel: create dump %s: %w", path, err)
	}
	// Dumps are written uncompressed on purpose: restic deduplicates
	// plain SQL between nights, and cannot deduplicate gzip at all.
	cmd := exec.CommandContext(ctx, r.mysqldump(),
		"--single-transaction", "--quick", "--routines", "--events", name)
	cmd.Stdout = file
	// mysqldump says why it stopped on stderr, and this used to throw
	// that away: a backup failed with "exit status 2" and finding out
	// what that meant took running the same command by hand on the
	// server. It had already said "Lost connection to MySQL server
	// during query" the first time.
	var complaint bytes.Buffer
	cmd.Stderr = &complaint
	started := time.Now()
	err = cmd.Run()
	r.ranCommand("dumped a database", req.Account.User, cmd, started, err)
	closeErr := file.Close()
	if err != nil {
		return fmt.Errorf("cpanel: mysqldump %s: %w%s",
			name, err, saidOnStderr(complaint.String()))
	}
	if closeErr != nil {
		return fmt.Errorf("cpanel: close dump %s: %w", path, closeErr)
	}
	return nil
}

// errorText is an error as one line, and empty when there is none.
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// serverIsGone reports whether the database server has stopped answering.
func (r *Real) serverIsGone(ctx context.Context) bool {
	ping, cancel := context.WithTimeout(ctx, serverPingTimeout)
	defer cancel()
	return exec.CommandContext(ping, r.mysql(), "--batch", "--skip-column-names",
		"--execute", "SELECT 1").Run() != nil
}

// waitForServer waits for the database server to answer again, for as long
// as a crash and its InnoDB recovery plausibly take and no longer.
func (r *Real) waitForServer(ctx context.Context) error {
	deadline := time.Now().Add(serverWait)
	for {
		if !r.serverIsGone(ctx) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("cpanel: the database server did not come back within %s", serverWait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(serverPollEvery):
		}
	}
}

const (
	serverPingTimeout = 5 * time.Second
	serverPollEvery   = 2 * time.Second
	serverWait        = 90 * time.Second
)

// restorepkg returns the script that applies an account archive.
func (r *Real) restorepkg() string {
	if r.RestorepkgPath != "" {
		return r.RestorepkgPath
	}
	return "/scripts/restorepkg"
}

func (r *Real) removeacct() string {
	if r.RemoveacctPath != "" {
		return r.RemoveacctPath
	}
	return "/usr/local/cpanel/scripts/removeacct"
}

// Apply hands a rebuilt archive to cPanel.
//
// This overwrites the live account, so the agent only reaches it when an
// operator set apply on the restore job.
//
// It asks for a restricted restore. cPanel's own default is the opposite,
// and the reason to differ is what is in the archive: an account's home
// directory, which that account's owner controls, being unpacked by root.
// An account that was compromised at any point since its last backup can
// have left something in there for this moment. Restricted mode is
// cPanel's answer to exactly that, and an operator who needs the archive
// restored whole can say so.
func (r *Real) Apply(ctx context.Context, archivePath string, options ApplyOptions) (string, error) {
	info, err := os.Stat(archivePath)
	if err != nil {
		return "", fmt.Errorf("cpanel: restore archive: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("cpanel: restore archive %s is a directory", archivePath)
	}
	if options.NewUser != "" {
		if err := validateUser(options.NewUser); err != nil {
			return "", err
		}
		if options.Overwrite {
			return "", fmt.Errorf("cpanel: a restore cannot overwrite and use a new username")
		}
	}

	// cPanel refuses --force with --restricted: "You may not force
	// Restricted Restore." Restoring into an account that is already
	// here is --skipaccount instead, which is what --force means once
	// the account exists -- cPanel's own help says --force implies it.
	var args []string
	switch {
	case options.Unrestricted:
		args = append(args, "--unrestricted")
		if options.Overwrite {
			args = append(args, "--force")
		}
	default:
		args = append(args, "--restricted")
		if options.Overwrite {
			args = append(args, "--skipaccount")
		}
	}
	if options.NewUser != "" {
		args = append(args, "--newuser="+options.NewUser)
	}
	if options.SkipDNS {
		args = append(args, "--update_dns_zone=0")
	}
	// The separator matters: everything after it is the archive, whatever
	// it is called.
	args = append(args, "--", archivePath)

	cmd := exec.CommandContext(ctx, r.restorepkg(), args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// restorepkg's last line is a column of module names, so the
		// last line alone says nothing. The reason is the first line
		// that reads like one.
		return string(output), fmt.Errorf(
			"cpanel: restorepkg failed: %w: %s", err, restoreFailure(output))
	}
	return string(output), nil
}

// Certify performs the destructive half of a restore drill on a dedicated
// cPanel test host: restore under a disposable username, confirm cPanel can
// enumerate it, and remove it again. DNS updates are disabled. Operators
// must not run this on a host where the archive's domains are live.
func (r *Real) Certify(ctx context.Context, archivePath, disposableUser string) error {
	if err := validateUser(disposableUser); err != nil {
		return err
	}
	if r.accountRegistered(disposableUser) {
		return fmt.Errorf("cpanel: certification account %s already exists", disposableUser)
	}

	_, applyErr := r.Apply(ctx, archivePath, ApplyOptions{
		NewUser: disposableUser, SkipDNS: true,
	})
	created := r.accountRegistered(disposableUser)
	certifyErr := applyErr
	if created && certifyErr == nil {
		accounts, listErr := r.Accounts(context.Background())
		if listErr != nil {
			certifyErr = fmt.Errorf("cpanel: list accounts after certification restore: %w", listErr)
		} else {
			found := false
			for _, account := range accounts {
				if account.User == disposableUser && !account.Missing {
					found = true
					break
				}
			}
			if !found {
				certifyErr = fmt.Errorf("cpanel: restored account %s has no usable home directory",
					disposableUser)
			}
		}
	}
	if created {
		// Cleanup must still run if the restore context was cancelled. A
		// disposable hosting account is not safe debris to leave behind.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(cleanupCtx, r.removeacct(), disposableUser, "--force")
		if output, err := cmd.CombinedOutput(); err != nil {
			cleanupErr := fmt.Errorf("remove disposable account: %w: %s", err, strings.TrimSpace(string(output)))
			if certifyErr != nil {
				return errors.Join(certifyErr, cleanupErr)
			}
			return cleanupErr
		}
		if r.accountRegistered(disposableUser) {
			return fmt.Errorf("cpanel: removeacct returned success but certification account %s still exists",
				disposableUser)
		}
	}
	if certifyErr != nil {
		return certifyErr
	}
	if !created {
		return fmt.Errorf("cpanel: restorepkg returned success but account %s was not created: %w",
			disposableUser, os.ErrNotExist)
	}
	return nil
}

func (r *Real) accountRegistered(user string) bool {
	info, err := os.Stat(filepath.Join(r.usersDir(), user))
	return err == nil && !info.IsDir()
}

// pkgacctFailure picks the line an operator needs out of pkgacct's output.
//
// pkgacct answers a refusal by printing its own message, then its whole
// usage block and every option it knows. The last line of that is
// "--stdout-archive  output the cpmove file to STDOUT and disable all
// other output", which is how "Unable to get user id for user
// "cptktjyvhbvisx9x"" reached the interface as a sentence about STDOUT.
func pkgacctFailure(output []byte) string {
	var reason string
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		// The usage block itself: the options, their descriptions, and
		// the headings above them.
		if line == "" || strings.HasPrefix(line, "-") || strings.HasPrefix(line, "Usage:") ||
			strings.HasPrefix(line, "Options:") || strings.HasPrefix(line, "/usr/local/cpanel") ||
			!strings.Contains(line, " ") {
			continue
		}
		lower := strings.ToLower(line)
		switch {
		case strings.Contains(lower, "unable to"), strings.Contains(lower, "unrecognized"),
			strings.Contains(lower, "is required"), strings.Contains(lower, "cannot"),
			strings.Contains(lower, "invalid"), strings.Contains(lower, "failed"),
			strings.Contains(lower, "error"), strings.Contains(lower, "no such"):
			if reason == "" {
				reason = line
			}
		}
	}
	if reason != "" {
		return reason
	}
	return lastLine(output)
}

// restoreFailure picks the line an operator needs out of restorepkg's
// transcript.
//
// It prints a usage block and a two-column list of every module it knows
// when it rejects its arguments, so the last line of a failed run is a
// module name -- which is how a real refusal ("You may not force
// Restricted Restore") reached the interface as the word "ManualMX".
func restoreFailure(output []byte) string {
	var reason string
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "-") || !strings.Contains(line, " ") {
			continue
		}
		lower := strings.ToLower(line)
		switch {
		case strings.Contains(lower, "requires the"), strings.Contains(lower, "may not"),
			strings.Contains(lower, "failed"), strings.Contains(lower, "error"),
			strings.Contains(lower, "cannot"), strings.Contains(lower, "invalid"):
			if reason == "" {
				reason = line
			}
		}
	}
	if reason != "" {
		return reason
	}
	return lastLine(output)
}

func validateUser(user string) error {
	if user == "" {
		return fmt.Errorf("cpanel: account user is empty")
	}
	for _, r := range user {
		isAllowed := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_'
		if !isAllowed {
			return fmt.Errorf("cpanel: account user %q contains an unsupported character", user)
		}
	}
	return nil
}

func directorySize(root string) (uint64, error) {
	var total uint64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			// A file that vanished mid-walk should not fail an estimate.
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		total += uint64(info.Size())
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("cpanel: measure %s: %w", root, err)
	}
	return total, nil
}

func lastLine(output []byte) string {
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) == 0 {
		return "(no output)"
	}
	return strings.TrimSpace(lines[len(lines)-1])
}

// saidOnStderr renders what a command complained about, for appending to
// the error that reports it. It keeps the last lines rather than the first:
// a tool that fails after a page of warnings fails on the last one. Empty
// stderr adds nothing, so an error that was already clear stays that way.
func saidOnStderr(complaint string) string {
	complaint = strings.TrimSpace(complaint)
	if complaint == "" {
		return ""
	}
	lines := strings.Split(complaint, "\n")
	if len(lines) > stderrLinesKept {
		lines = lines[len(lines)-stderrLinesKept:]
	}
	said := strings.Join(lines, "; ")
	if len(said) > stderrBytesKept {
		said = said[len(said)-stderrBytesKept:]
	}
	return ": " + said
}

// stderrLinesKept and stderrBytesKept hold the quoted complaint to a size
// that fits on a page beside the failure it explains.
const (
	stderrLinesKept = 3
	stderrBytesKept = 400
)
