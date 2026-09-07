package cpanel

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	osuser "os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/shukiv/gniza/internal/granular"
	"github.com/shukiv/gniza/internal/panel"
)

// PutHomeDir copies a restored tree into an account's home directory.
//
// The tree is in staging, which is root's; the home directory is the
// account's. The copy is therefore split in two: root reads, and the
// account writes. Nothing here runs as root inside the home directory, so
// the files that land there are owned by the account without a chown pass,
// and a link planted in the home directory before the restore can lead
// nowhere the account could not already write.
func (r *Real) PutHomeDir(ctx context.Context, user, from string) error {
	account, err := osuser.Lookup(user)
	if err != nil {
		return fmt.Errorf("cpanel: look up %s: %w", user, err)
	}
	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil {
		return fmt.Errorf("cpanel: %s has no numeric uid: %w", user, err)
	}
	gid, err := strconv.ParseUint(account.Gid, 10, 32)
	if err != nil {
		return fmt.Errorf("cpanel: %s has no numeric gid: %w", user, err)
	}
	// Not a guard against a hostile caller -- one that reached here has
	// root already -- but against a mistake that would write a customer's
	// backup over a system account's files.
	if uid == 0 {
		return fmt.Errorf("cpanel: %s is not a cPanel account", user)
	}
	home := filepath.Clean(account.HomeDir)
	if home == "" || home == "." || home == "/" || filepath.Dir(home) == home {
		return fmt.Errorf("cpanel: %s has no home directory of its own", user)
	}
	info, err := os.Stat(home)
	if err != nil {
		return fmt.Errorf("cpanel: home directory of %s: %w", user, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("cpanel: home directory of %s is not a directory", user)
	}

	if err := dropSetIDBits(from); err != nil {
		return err
	}

	// GNU tar on both ends: the reader keeps the tree's shape and modes,
	// and the writer is the account, so ownership follows from who it is
	// rather than from what the archive claims.
	read := exec.CommandContext(ctx, "tar", "-C", from, "--numeric-owner", "-cf", "-", ".")
	write := exec.CommandContext(ctx, "tar", "-C", home, "-xpf", "-", "--no-same-owner")
	write.SysProcAttr = &syscall.SysProcAttr{
		// Groups is left empty on purpose: without it the child would
		// keep root's supplementary groups while carrying the account's
		// uid, which is more than the account has.
		Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)},
	}

	pipeRead, pipeWrite, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("cpanel: pipe: %w", err)
	}
	read.Stdout = pipeWrite
	write.Stdin = pipeRead
	var readErr, writeErr bytes.Buffer
	read.Stderr = &readErr
	write.Stderr = &writeErr

	if err := read.Start(); err != nil {
		pipeRead.Close()
		pipeWrite.Close()
		return fmt.Errorf("cpanel: read the restored tree: %w", err)
	}
	if err := write.Start(); err != nil {
		pipeRead.Close()
		pipeWrite.Close()
		_ = read.Process.Kill()
		_, _ = read.Process.Wait()
		return fmt.Errorf("cpanel: write into %s: %w", home, err)
	}
	// The parent's ends have been handed to the children. Holding them
	// open would keep the writer waiting for an end of file that the
	// finished reader has already stopped producing.
	pipeWrite.Close()
	pipeRead.Close()

	readWait := read.Wait()
	writeWait := write.Wait()
	// The writer's failure is reported first on purpose. A full disk or a
	// quota stops the writer, the reader then finds a closed pipe, and
	// "broken pipe" is not what went wrong.
	if writeWait != nil {
		return fmt.Errorf("cpanel: write into %s as %s: %s: %w",
			home, user, lastLine(writeErr.Bytes()), writeWait)
	}
	if readWait != nil {
		return fmt.Errorf("cpanel: read the restored tree: %s: %w",
			lastLine(readErr.Bytes()), readWait)
	}
	return nil
}

// grantPattern writes a database name as the pattern a GRANT takes.
//
// The database of a GRANT is matched like a LIKE pattern, so the underscore
// every cPanel database name carries means "any character" unless it is
// escaped. Backticks do not do it -- they quote the identifier and leave
// the wildcard meaning intact -- so a grant written without this covers
// every database whose name differs by one character.
func grantPattern(database string) string {
	return strings.ReplaceAll(database, "_", `\_`)
}

// dropSetIDBits clears set-user-ID and set-group-ID from a staged tree
// before it is written into an account.
//
// The account could set those bits itself on its own files, so this is not
// what stops it. It stops a bit that was set when the backup was taken from
// coming back after the reason for removing it -- a compromise, a policy
// change -- has been dealt with.
func dropSetIDBits(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&(os.ModeSetuid|os.ModeSetgid) == 0 {
			return nil
		}
		if !info.Mode().IsRegular() && !info.IsDir() {
			return nil
		}
		// Perm() is the nine permission bits and nothing else, so writing
		// it back is what drops the two.
		return os.Chmod(path, info.Mode().Perm())
	})
}

// CreateDatabase makes a database for the account so a dump has somewhere
// to go.
//
// Run as the account rather than as root: cPanel then applies the account's
// database quota and its name prefix, refuses a name another account holds,
// and records the new database as this account's. Made any other way it
// would be a database MySQL has and the panel does not, which the customer
// can neither see nor delete.
func (r *Real) CreateDatabase(ctx context.Context, user, database string) error {
	if !plainAccountName(user) {
		return fmt.Errorf("cpanel: %q is not a cPanel account name", user)
	}
	if err := granular.UsableDatabaseName(database); err != nil {
		return err
	}
	create := exec.CommandContext(ctx, r.uapi(),
		"--user="+user, "--output=jsonpretty", "Mysql", "create_database",
		"name="+database)
	var stderr bytes.Buffer
	create.Stderr = &stderr
	output, err := create.Output()
	if err != nil {
		return fmt.Errorf("cpanel: create the database %s: %s: %w",
			database, lastLine(stderr.Bytes()), err)
	}
	// uapi reports a refusal in its answer and still exits zero, so the
	// exit status is not the check. A quota reached and a name somebody
	// else holds both arrive this way.
	if err := uapiRefusal(output); err != nil {
		return fmt.Errorf("cpanel: create the database %s: %w", database, err)
	}
	return nil
}

// LoadDatabase replaces the contents of one of the account's databases with
// a dump taken from a backup.
func (r *Real) LoadDatabase(ctx context.Context, user, database, dumpPath string) error {
	if err := granular.UsableDatabaseName(database); err != nil {
		return err
	}
	// Whose database this is, asked of the server rather than taken from
	// the request. The name in the backup is the name it had when the
	// backup was taken, and a name can have changed hands since.
	owned, err := r.databases(ctx, user)
	if err != nil {
		return err
	}
	found := false
	for _, name := range owned {
		if name == database {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf(
			"cpanel: the database %s does not belong to %s", database, user)
	}

	dump, err := os.Open(dumpPath)
	if err != nil {
		return fmt.Errorf("cpanel: database dump: %w", err)
	}
	defer dump.Close()
	info, err := dump.Stat()
	if err != nil {
		return fmt.Errorf("cpanel: database dump: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("cpanel: database dump %s is not a file", dumpPath)
	}

	// A dump is not a script of SQL and nothing else. The mysql client
	// reads some lines itself rather than sending them to the server, and
	// one of those is "\!", which runs a shell command -- here, as root,
	// on the machine an account is being recovered onto. The archive this
	// dump came out of is another server's backup, and this program's
	// whole premise is that the other server may be the one that was
	// compromised.
	//
	// --binary-mode turns the client's own commands off for input that is
	// not being typed by a person: everything but charset and delimiter,
	// and dumps need delimiter for triggers and routines. --local-infile=0
	// refuses LOAD DATA LOCAL INFILE, which is the other line in a dump
	// that reaches the local filesystem rather than the server.
	//
	// Those flags only protect the client. Server-side isolation comes
	// from a separate login with grants on precisely this schema.
	return r.loadIsolatedDatabase(ctx, database, dump)
}

// maxCrontab is as large as a crontab is allowed to be. A cPanel account's
// is a few lines; anything approaching this is not one.
const maxCrontab = 1 << 20

// PutCrontab replaces an account's cron jobs with the ones in a backup.
//
// Through crontab(1) rather than by writing /var/spool/cron directly: it
// checks the syntax, writes the file with the ownership and mode cron
// insists on, and tells the daemon. A file copied into place with a line
// cron cannot read is a crontab cron ignores in full, which is an account
// whose jobs all stopped without anything failing.
//
// The whole crontab is replaced, because that is what the backup holds and
// what cron reads. A job added since the backup was taken is gone -- so the
// crontab as it was is written beside the restored one first, and the
// staging directory a restore keeps is where it stays.
func (r *Real) PutCrontab(ctx context.Context, user, from string) error {
	if !plainAccountName(user) {
		return fmt.Errorf("cpanel: %q is not a cPanel account name", user)
	}
	restored, err := os.ReadFile(from)
	if err != nil {
		return fmt.Errorf("cpanel: cron jobs from the backup: %w", err)
	}
	if len(restored) > maxCrontab {
		return fmt.Errorf(
			"cpanel: the cron jobs in this backup are %d bytes, which is not a crontab",
			len(restored))
	}
	if bytes.IndexByte(restored, 0) >= 0 {
		return fmt.Errorf("cpanel: the cron jobs in this backup are not text")
	}

	// What is running now, kept where it outlives the restore. The
	// staging directory an applied restore used is removed when it
	// finishes -- the files went into the account -- so a copy left there
	// would be a copy nobody could find.
	//
	// An account with no crontab at all makes this command fail, and
	// there is nothing to keep. Not a reason to refuse the restore.
	previous := exec.CommandContext(ctx, r.crontab(), "-u", user, "-l")
	if current, err := previous.Output(); err == nil && len(current) > 0 {
		if err := os.MkdirAll(r.replacedDir(), 0o700); err != nil {
			return fmt.Errorf("cpanel: keep the crontab being replaced: %w", err)
		}
		kept := filepath.Join(r.replacedDir(), fmt.Sprintf("crontab-%s-%s",
			user, time.Now().UTC().Format("20060102-150405")))
		if err := os.WriteFile(kept, current, 0o600); err != nil {
			return fmt.Errorf("cpanel: keep the crontab being replaced: %w", err)
		}
	}

	install := exec.CommandContext(ctx, r.crontab(), "-u", user, from)
	var stderr bytes.Buffer
	install.Stderr = &stderr
	if err := install.Run(); err != nil {
		return fmt.Errorf("cpanel: put back %s's cron jobs: %s: %w",
			user, lastLine(stderr.Bytes()), err)
	}
	return nil
}

// replacedDir is where a copy of what a restore wrote over is kept.
func (r *Real) replacedDir() string {
	if r.ReplacedDir != "" {
		return r.ReplacedDir
	}
	return "/var/lib/gniza/replaced"
}

func (r *Real) crontab() string {
	if r.Crontab != "" {
		return r.Crontab
	}
	return "crontab"
}

// PutDatabaseUsers recreates an account's database users from a backup and
// grants them what they had.
//
// It is three steps because no single interface does all of it. MySQL is
// the only thing that can be given a password as a stored hash, which is
// all a backup has -- cPanel's own API needs the password in the clear, and
// nobody has it. cPanel is the only thing that knows an account owns a
// user, and a user MySQL has but the panel does not is one the customer
// cannot see or change. So: root creates the logins, dbmaptool says whose
// they are, and the grants go through cPanel as the account, which writes
// them into the panel's record as well as into MySQL.
//
// Everything is checked before anything runs. A user another account owns,
// or a grant on a database this account does not have, fails the whole
// request rather than being skipped: a restore that quietly left out the
// grant an application connects with is a restore that reports success and
// leaves the site down.
func (r *Real) PutDatabaseUsers(ctx context.Context, user string, users []panel.DatabaseUser) error {
	if !plainAccountName(user) {
		return fmt.Errorf("cpanel: %q is not a cPanel account name", user)
	}
	if len(users) == 0 {
		return errors.New("cpanel: no database user was named to restore")
	}

	// What this account has now, asked of the server. The backup says
	// what it had when it was taken.
	ownedList, err := r.databases(ctx, user)
	if err != nil {
		return err
	}
	owned := make(map[string]bool, len(ownedList))
	for _, name := range ownedList {
		owned[name] = true
	}
	claimed, err := r.databaseUserOwners()
	if err != nil {
		return err
	}
	if claimed[user] != user {
		return fmt.Errorf("cpanel: cannot establish database ownership for %s", user)
	}
	existing, err := r.serverDatabaseUsers(ctx)
	if err != nil {
		return err
	}

	for _, account := range users {
		if !plainDatabaseUser(account.Name) {
			return fmt.Errorf("cpanel: %q is not a database user name", account.Name)
		}
		if reservedDatabaseUser(account.Name) || (existing[account.Name] && claimed[account.Name] != user) {
			return fmt.Errorf("cpanel: database login %s is a server login not owned by %s", account.Name, user)
		}
		// A deleted user is the reason this exists, so its absence from
		// this account's record proves nothing. Another account holding
		// it is what has to stop the restore.
		if owner, known := claimed[account.Name]; known && owner != user {
			return fmt.Errorf(
				"cpanel: the database user %s belongs to %s, not to %s",
				account.Name, owner, user)
		}
		if !usableDatabaseHost(account.Host) {
			return fmt.Errorf("cpanel: %q is not a host a database user can exist on",
				account.Host)
		}
		if !usableAuthPlugin(account.Plugin) {
			return fmt.Errorf("cpanel: %q is not an authentication plugin", account.Plugin)
		}
		if !usableAuthHash(account.Hash) {
			return fmt.Errorf("cpanel: the stored password of %s is not readable in this backup",
				account.Name)
		}
		for _, grant := range account.Grants {
			if err := granular.UsableDatabaseName(grant.Database); err != nil {
				return err
			}
			if !owned[grant.Database] {
				return fmt.Errorf(
					"cpanel: the database %s does not belong to %s", grant.Database, user)
			}
			for _, privilege := range grant.Privileges {
				if !usableDatabasePrivilege(privilege) {
					return fmt.Errorf("cpanel: %q is not a database privilege", privilege)
				}
			}
		}
	}

	// The account's own MySQL user is not one of its database users: it
	// is the owner of the record they are kept in. cPanel refuses both
	// halves of the normal path for it -- dbmaptool answers "A DB map
	// Owner cannot own itself", and set_privileges_on_database fails --
	// so it is created and granted directly instead. Every account on a
	// cPanel server has one, so this is the common case rather than an
	// edge.
	var owner, mapped, fresh, known []panel.DatabaseUser
	for _, account := range users {
		if account.Name == user {
			owner = append(owner, account)
			continue
		}
		mapped = append(mapped, account)
		if existing[account.Name] {
			known = append(known, account)
		} else {
			fresh = append(fresh, account)
		}
	}

	if err := r.createDatabaseUsers(ctx, owner, false); err != nil {
		return err
	}
	// A name absent at preflight must still be absent when created. Do not
	// turn a CREATE collision into ALTER of a login created by somebody else.
	if err := r.writeDatabaseUsers(ctx, fresh, false, false); err != nil {
		return err
	}
	if err := r.createDatabaseUsers(ctx, known, true); err != nil {
		return err
	}
	if len(mapped) > 0 {
		if err := r.mapDatabaseUsers(ctx, user, mapped); err != nil {
			return err
		}
		if err := r.grantDatabaseUsers(ctx, user, mapped); err != nil {
			return err
		}
	}
	return r.grantOwnerDatabases(ctx, owner)
}

// grantOwnerDatabases gives the account's own MySQL user back what it had,
// as MySQL rather than through cPanel.
//
// cPanel has nowhere to record this one: it is the owner of the record, not
// an entry in it, and its access to the account's own databases is not
// something the panel tracks per database. The grant is still confined to
// databases PutDatabaseUsers has already checked are this account's.
func (r *Real) grantOwnerDatabases(ctx context.Context, owner []panel.DatabaseUser) error {
	var script strings.Builder
	for _, account := range owner {
		for _, grant := range account.Grants {
			privileges := "ALL PRIVILEGES"
			if len(grant.Privileges) > 0 {
				privileges = strings.Join(grant.Privileges, ", ")
			}
			// The database of a GRANT is a pattern, so the underscore
			// every cPanel database name carries has to be escaped or
			// the grant covers every name it matches. Backticks do not
			// do it: they quote the identifier and leave the wildcard
			// meaning intact.
			fmt.Fprintf(&script, "GRANT %s ON `%s`.* TO `%s`@`%s`;\n",
				privileges, grantPattern(grant.Database),
				account.Name, account.Host)
		}
	}
	if script.Len() == 0 {
		return nil
	}
	grant := exec.CommandContext(ctx, r.mysql())
	grant.Stdin = strings.NewReader(script.String())
	var stderr bytes.Buffer
	grant.Stderr = &stderr
	if err := grant.Run(); err != nil {
		return fmt.Errorf("cpanel: grant the account's own database user: %s: %w",
			lastLine(stderr.Bytes()), err)
	}
	return nil
}

// createDatabaseUsers makes the logins exist with the passwords they had.
//
// setPassword says whether a user who is already there is set back to the
// password in the backup. For the account's own database users it is what
// was asked for: CREATE USER IF NOT EXISTS does nothing at all to an
// existing user, so without the ALTER a restore of a user whose password
// was changed would silently not restore the password.
//
// For the account's own MySQL user it is false, and that is not a
// shortcut. cPanel keeps that login's password in step with the cPanel
// account password, so putting the backup's password back would set the
// two out of step -- and would undo a password change made because the old
// one leaked, which is exactly what a restore must not quietly do. That
// user is created if it is somehow missing and otherwise left as it is.
func (r *Real) createDatabaseUsers(ctx context.Context, users []panel.DatabaseUser, setPassword bool) error {
	return r.writeDatabaseUsers(ctx, users, setPassword, true)
}

func (r *Real) writeDatabaseUsers(ctx context.Context, users []panel.DatabaseUser, setPassword, allowExisting bool) error {
	if len(users) == 0 {
		return nil
	}
	var script strings.Builder
	for _, account := range users {
		identified := fmt.Sprintf("IDENTIFIED WITH '%s'", account.Plugin)
		if account.Hash != "" {
			identified += " AS 0x" + account.Hash
		}
		if allowExisting {
			fmt.Fprintf(&script, "CREATE USER IF NOT EXISTS `%s`@`%s` %s;\n", account.Name, account.Host, identified)
		} else {
			fmt.Fprintf(&script, "CREATE USER `%s`@`%s` %s;\n", account.Name, account.Host, identified)
		}
		if setPassword {
			fmt.Fprintf(&script, "ALTER USER `%s`@`%s` %s;\n",
				account.Name, account.Host, identified)
		}
	}
	create := exec.CommandContext(ctx, r.mysql())
	create.Stdin = strings.NewReader(script.String())
	var stderr bytes.Buffer
	create.Stderr = &stderr
	if err := create.Run(); err != nil {
		return fmt.Errorf("cpanel: create the database users: %s: %w",
			lastLine(stderr.Bytes()), err)
	}
	return nil
}

// mapDatabaseUsers tells cPanel that these users are the account's.
//
// Without it MySQL has users the panel has never heard of: they do not
// appear under MySQL Databases, the customer cannot change their password
// or delete them, and the next thing that rebuilds the account leaves them
// behind.
func (r *Real) mapDatabaseUsers(ctx context.Context, user string, users []panel.DatabaseUser) error {
	names := make([]string, 0, len(users))
	for _, account := range users {
		names = append(names, account.Name)
	}
	sort.Strings(names)
	names = unique(names)

	mapUser := exec.CommandContext(ctx, r.dbMapTool(), user,
		"--type", "mysql", "--dbusers", strings.Join(names, ","))
	// Both streams, whole. dbmaptool reports a name it would not map on
	// stdout and still exits zero, and when it does fail it fails with a
	// Perl stack trace whose last line is the trace rather than the
	// reason.
	output, err := mapUser.CombinedOutput()
	if err != nil {
		return fmt.Errorf("cpanel: record the database users of %s: %s: %w",
			user, strings.TrimSpace(string(output)), err)
	}
	if report := strings.TrimSpace(string(output)); report != "" {
		return fmt.Errorf("cpanel: record the database users of %s: %s", user, report)
	}
	return nil
}

// grantDatabaseUsers gives each user back what it could do, through
// cPanel and as the account.
//
// The grant could be written straight into MySQL, and this deliberately
// does not: cPanel keeps its own record of which user may read which
// database, the panel's interface is drawn from that record rather than
// from MySQL, and running as the account means cPanel refuses a database
// that is not the account's whatever this program believed.
func (r *Real) grantDatabaseUsers(ctx context.Context, user string, users []panel.DatabaseUser) error {
	for _, account := range users {
		for _, grant := range account.Grants {
			privileges := "ALL PRIVILEGES"
			if len(grant.Privileges) > 0 {
				privileges = strings.Join(grant.Privileges, ",")
			}
			set := exec.CommandContext(ctx, r.uapi(),
				"--user="+user, "--output=jsonpretty", "Mysql",
				"set_privileges_on_database",
				"user="+account.Name,
				"database="+grant.Database,
				"privileges="+privileges)
			var stderr bytes.Buffer
			set.Stderr = &stderr
			output, err := set.Output()
			if err != nil {
				return fmt.Errorf("cpanel: grant %s on %s: %s: %w",
					account.Name, grant.Database, lastLine(stderr.Bytes()), err)
			}
			// uapi reports a refusal in its answer and still exits
			// zero, so the exit status is not the check.
			if err := uapiRefusal(output); err != nil {
				return fmt.Errorf("cpanel: grant %s on %s: %w",
					account.Name, grant.Database, err)
			}
		}
	}
	return nil
}

// uapiRefusal reads whether cPanel actually did what it was asked.
func uapiRefusal(output []byte) error {
	var answer struct {
		Result struct {
			Status int      `json:"status"`
			Errors []string `json:"errors"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &answer); err != nil {
		return fmt.Errorf("cpanel answered something this could not read: %w", err)
	}
	if answer.Result.Status == 1 {
		return nil
	}
	if len(answer.Result.Errors) > 0 {
		return errors.New(strings.Join(answer.Result.Errors, "; "))
	}
	return errors.New("cpanel refused it without saying why")
}

func (r *Real) dbMapTool() string {
	if r.DBMapTool != "" {
		return r.DBMapTool
	}
	return "/usr/local/cpanel/bin/dbmaptool"
}

// databaseUserOwners is every database user cPanel attributes to an
// account, and which account that is.
//
// There is no server-wide index of these the way /etc/dbowners indexes
// databases, so the per-account records are read instead. A record that
// cannot be read makes the operation fail: a partial index cannot authorize
// changing a login's password.
func (r *Real) databaseUserOwners() (map[string]string, error) {
	entries, err := os.ReadDir(r.databasesDir())
	if err != nil {
		return nil, fmt.Errorf("cpanel: read database ownership: %w", err)
	}
	owners := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		account := strings.TrimSuffix(name, ".json")
		if !plainAccountName(account) {
			// dbindex.db.json and anything else that is not one
			// account's record.
			continue
		}
		_, users, recorded := r.recordedDatabases(account)
		if !recorded {
			return nil, fmt.Errorf("cpanel: cannot read database ownership for %s", account)
		}
		for _, user := range append(users, account) {
			if previous, exists := owners[user]; exists && previous != account {
				return nil, fmt.Errorf("cpanel: conflicting owners for database user %s", user)
			}
			owners[user] = account
		}
	}
	return owners, nil
}

func reservedDatabaseUser(name string) bool {
	switch strings.ToLower(name) {
	case "root", "mysql.sys", "mysql.session", "mysql.infoschema", "mariadb.sys", "mysql", "cpanel", "cpses":
		return true
	}
	return strings.HasPrefix(strings.ToLower(name), "cpses_")
}

// A cPanel map says who owns a panel login, not that every other MySQL login
// is free. Read the server too so unmapped administrative/service logins
// cannot be claimed through a backup. HEX avoids interpreting MySQL escapes.
func (r *Real) serverDatabaseUsers(ctx context.Context) (map[string]bool, error) {
	cmd := exec.CommandContext(ctx, r.mysql(), "--batch", "--skip-column-names", "--execute=SELECT DISTINCT HEX(User) FROM mysql.user")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("cpanel: read server database logins: %w", err)
	}
	users := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		name, err := hex.DecodeString(strings.TrimSpace(line))
		if err != nil {
			return nil, fmt.Errorf("cpanel: unreadable server database login list")
		}
		users[string(name)] = true
	}
	if len(users) == 0 {
		return nil, fmt.Errorf("cpanel: server returned no database logins; ownership cannot be verified")
	}
	return users, nil
}

// usableDatabaseHost holds the host half of a MySQL account to what one
// can be. It reaches a statement the mysql client cannot bind parameters
// for, which is why it is checked rather than escaped.
func usableDatabaseHost(host string) bool {
	if host == "" || len(host) > 255 {
		return false
	}
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_', r == ':', r == '%':
		default:
			return false
		}
	}
	return true
}

// usableAuthPlugin holds the plugin name to the shape MySQL's own are.
func usableAuthPlugin(plugin string) bool {
	if plugin == "" || len(plugin) > 64 {
		return false
	}
	for _, r := range plugin {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
	}
	return true
}

// usableAuthHash holds the stored password to hex, which is how it goes
// into the statement: caching_sha2_password's is binary, and carrying it
// as text is what loses it.
func usableAuthHash(hash string) bool {
	if len(hash) > 4096 || len(hash)%2 != 0 {
		return false
	}
	for _, r := range hash {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// usableDatabasePrivilege holds a privilege to a MySQL privilege name:
// capitals and single spaces, as SHOW GRANTS prints them.
func usableDatabasePrivilege(privilege string) bool {
	if privilege == "" || len(privilege) > 64 {
		return false
	}
	if strings.HasPrefix(privilege, " ") || strings.HasSuffix(privilege, " ") {
		return false
	}
	for _, r := range privilege {
		switch {
		case r >= 'A' && r <= 'Z', r == ' ':
		default:
			return false
		}
	}
	return true
}
