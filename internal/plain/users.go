package plain

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/shukiv/gniza/internal/granular"
	"github.com/shukiv/gniza/internal/panel"
)

// The MySQL accounts of a server without a panel.
//
// A panel knows which database users belong to an account and carries
// them in its archive. Here the operator says: an account attached to a
// source is kept beside that source's dumps -- its password hash and the
// grants it holds on the source's databases, in the files a panel's
// backup uses for the same thing -- so a restore brings back the login
// that opens the database, not only the tables.

// systemMySQLUsers are the server's own accounts and the operator's.
// They are shown so the picture is whole, and not offered: nobody means
// root when they say they want a database backed up.
var systemMySQLUsers = map[string]bool{
	"root": true, "mysql": true, "mariadb.sys": true, "mysql.sys": true,
	"mysql.session": true, "mysql.infoschema": true, "debian-sys-maint": true,
	"healthchecker": true,
}

// usableAccountPart says a user or host name is made of what MySQL
// account names are made of and nothing a statement could hide in. It
// is handed back to the server inside SHOW GRANTS and CREATE USER, and
// the client has no parameter binding, which is why this exists.
func usableAccountPart(part string) bool {
	if part == "" || len(part) > 255 {
		return false
	}
	for _, r := range part {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.' || r == '%' || r == ':':
		default:
			return false
		}
	}
	return true
}

// splitWho reads an account written as user@host, the way the pages
// post it and the record keeps it.
func splitWho(who string) (user, host string, err error) {
	at := strings.LastIndex(who, "@")
	if at < 0 {
		return "", "", fmt.Errorf("%q is not an account: one is written user@host", who)
	}
	user, host = who[:at], who[at+1:]
	if !usableAccountPart(user) || !usableAccountPart(host) {
		return "", "", fmt.Errorf("%q is not an account name this can use", who)
	}
	return user, host, nil
}

// unquoteGrantee reads 'user'@'host' as information_schema writes it.
func unquoteGrantee(grantee string) (user, host string, ok bool) {
	if !usableGrantee(grantee) {
		return "", "", false
	}
	user, host, _ = strings.Cut(grantee, "'@'")
	user = strings.TrimPrefix(user, "'")
	host = strings.TrimSuffix(host, "'")
	return user, host, usableAccountPart(user) && usableAccountPart(host)
}

// query runs one statement through the mysql client and returns its
// rows, tab separated, one per line.
func (p *Provider) query(ctx context.Context, statement string) ([][]string, error) {
	cmd := exec.CommandContext(ctx, p.mysql(), "-N", "-B", "--raw", "-e", statement)
	var out, complaint bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &complaint
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("plain: mysql: %w%s", err, saidOnStderr(complaint.String()))
	}
	var rows [][]string
	scanner := bufio.NewScanner(&out)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		if line := scanner.Text(); line != "" {
			rows = append(rows, strings.Split(line, "\t"))
		}
	}
	return rows, nil
}

// mysqlAccounts lists every account the server has, with what
// authenticates it. A MariaDB role has no host and is not an account.
func (p *Provider) mysqlAccounts(ctx context.Context) ([]panel.DatabaseUserCandidate, error) {
	rows, err := p.query(ctx, "SELECT user, host, plugin FROM mysql.user ORDER BY user, host")
	if err != nil {
		return nil, err
	}
	var accounts []panel.DatabaseUserCandidate
	for _, row := range rows {
		if len(row) < 2 || !usableAccountPart(row[0]) || !usableAccountPart(row[1]) {
			continue
		}
		account := panel.DatabaseUserCandidate{User: row[0], Host: row[1], System: systemMySQLUsers[row[0]]}
		if len(row) > 2 {
			account.Plugin = row[2]
		}
		accounts = append(accounts, account)
	}
	// The server's own accounts last: the operator came for the others.
	sort.SliceStable(accounts, func(i, j int) bool {
		if accounts[i].System != accounts[j].System {
			return !accounts[i].System
		}
		if accounts[i].User != accounts[j].User {
			return accounts[i].User < accounts[j].User
		}
		return accounts[i].Host < accounts[j].Host
	})
	return accounts, nil
}

// mysqlGlobal lists the privileges each account holds on every
// database, USAGE left out since it is the right to connect and nothing
// more.
func (p *Provider) mysqlGlobal(ctx context.Context) (map[string][]string, error) {
	rows, err := p.query(ctx, "SELECT grantee, GROUP_CONCAT(privilege_type ORDER BY privilege_type SEPARATOR ',') "+
		"FROM information_schema.user_privileges WHERE privilege_type <> 'USAGE' GROUP BY grantee ORDER BY grantee")
	if err != nil {
		return nil, err
	}
	global := map[string][]string{}
	for _, row := range rows {
		if len(row) < 2 {
			continue
		}
		user, host, ok := unquoteGrantee(strings.TrimSpace(row[0]))
		if !ok {
			continue
		}
		global[user+"@"+host] = strings.Split(row[1], ",")
	}
	return global, nil
}

// mysqlRights lists, for each database, the accounts with rights on it
// at the schema level and what those rights are.
func (p *Provider) mysqlRights(ctx context.Context) (map[string][]panel.DatabaseRight, error) {
	rows, err := p.query(ctx, "SELECT table_schema, grantee, GROUP_CONCAT(privilege_type ORDER BY privilege_type SEPARATOR ',') "+
		"FROM information_schema.schema_privileges GROUP BY table_schema, grantee ORDER BY 1, 2")
	if err != nil {
		return nil, err
	}
	rights := map[string][]panel.DatabaseRight{}
	for _, row := range rows {
		if len(row) < 2 || row[0] == "" {
			continue
		}
		user, host, ok := unquoteGrantee(strings.TrimSpace(row[1]))
		if !ok {
			continue
		}
		right := panel.DatabaseRight{User: user, Host: host, Database: row[0]}
		if len(row) > 2 && row[2] != "" {
			right.Privileges = strings.Split(row[2], ",")
		}
		rights[row[0]] = append(rights[row[0]], right)
	}
	return rights, nil
}

// mysqlUsers lists, for each database, the accounts granted rights on
// it at the schema level, as 'user'@'host'. An account granted
// everything on every database is not listed against any of them; root
// is the operator, not a customer.
func (p *Provider) mysqlUsers(ctx context.Context) (map[string][]string, error) {
	rights, err := p.mysqlRights(ctx)
	if err != nil {
		return nil, err
	}
	users := map[string][]string{}
	for database, holders := range rights {
		for _, right := range holders {
			users[database] = append(users[database], "'"+right.User+"'@'"+right.Host+"'")
		}
	}
	return users, nil
}

// mysqlUserCandidates is every account with what it can reach, and the
// sources each is kept with.
func (p *Provider) mysqlUserCandidates(ctx context.Context, sources []panel.Source) ([]panel.DatabaseUserCandidate, error) {
	accounts, err := p.mysqlAccounts(ctx)
	if err != nil {
		return nil, err
	}
	global, err := p.mysqlGlobal(ctx)
	if err != nil {
		return nil, err
	}
	rights, err := p.mysqlRights(ctx)
	if err != nil {
		return nil, err
	}
	byWho := map[string][]panel.DatabaseRight{}
	for _, holders := range rights {
		for _, right := range holders {
			byWho[right.Who()] = append(byWho[right.Who()], right)
		}
	}
	attached := map[string][]string{}
	for _, source := range sources {
		for _, who := range source.MySQLUsers {
			attached[who] = append(attached[who], source.Name)
		}
	}
	for i := range accounts {
		who := accounts[i].Who()
		accounts[i].Global = global[who]
		accounts[i].Rights = byWho[who]
		sort.Slice(accounts[i].Rights, func(a, b int) bool { return accounts[i].Rights[a].Database < accounts[i].Rights[b].Database })
		accounts[i].AttachedTo = strings.Join(attached[who], ",")
	}
	return accounts, nil
}

// AttachMySQLUser keeps an account with every source that dumps a
// database it has rights on. See panel.Chooser.
func (p *Provider) AttachMySQLUser(ctx context.Context, who string) ([]string, error) {
	if p.Catalog == nil {
		return nil, ErrNoCatalog
	}
	user, host, err := splitWho(strings.TrimSpace(who))
	if err != nil {
		return nil, err
	}
	who = user + "@" + host
	if systemMySQLUsers[user] {
		return nil, fmt.Errorf("%s is the server's own account, not one a backup carries", who)
	}
	accounts, err := p.mysqlAccounts(ctx)
	if err != nil {
		return nil, err
	}
	known := false
	for _, account := range accounts {
		if account.User == user && account.Host == host {
			known = true
		}
	}
	if !known {
		return nil, fmt.Errorf("the server has no account %s", who)
	}
	rights, err := p.mysqlRights(ctx)
	if err != nil {
		return nil, err
	}
	reaches := map[string]bool{}
	for database, holders := range rights {
		for _, right := range holders {
			if right.User == user && right.Host == host {
				reaches[database] = true
			}
		}
	}
	sources, err := p.Sources(ctx)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, source := range sources {
		keeps := false
		for _, database := range source.MySQL {
			keeps = keeps || reaches[database]
		}
		if !keeps {
			continue
		}
		names = append(names, source.Name)
		if contains(source.MySQLUsers, who) {
			continue
		}
		source.MySQLUsers = sortedCopy(append(source.MySQLUsers, who))
		if err := p.Catalog.SaveSource(source); err != nil {
			return nil, fmt.Errorf("plain: keep the choice: %w", err)
		}
		p.log().Info("an account was kept with a source", "source", source.Name, "user", who)
	}
	if len(names) == 0 {
		var databases []string
		for database := range reaches {
			databases = append(databases, database)
		}
		sort.Strings(databases)
		if len(databases) == 0 {
			return nil, fmt.Errorf("%s has rights on no database; there is nothing to keep it with", who)
		}
		return nil, fmt.Errorf("%s has rights on no database that is backed up; tick %s with it", who, strings.Join(databases, ", "))
	}
	return names, nil
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// --- keeping accounts with the dumps ---

// mysqlIsMariaDB says whether the server is MariaDB, whose CREATE USER
// takes a hash as VIA plugin USING 'hash', where MySQL takes WITH
// plugin AS 0xhash.
func (p *Provider) mysqlIsMariaDB(ctx context.Context) (bool, error) {
	rows, err := p.query(ctx, "SELECT VERSION()")
	if err != nil {
		return false, err
	}
	return len(rows) > 0 && len(rows[0]) > 0 && strings.Contains(strings.ToLower(rows[0][0]), "mariadb"), nil
}

// keptAccount is one account as the files beside the dumps carry it.
type keptAccount struct {
	User, Host, Plugin, HexHash string
	// Grants are the GRANT statements on the source's databases, with
	// the account written 'user'@'host', as the restore reads them.
	Grants []string
	// LeftOut are the grants not carried: on other databases, or ones
	// the restore could not put back as they were.
	LeftOut []string
}

// granteeClause matches the TO clause as SHOW GRANTS prints it.
var granteeClause = regexp.MustCompile("TO `((?:[^`]|``)*)`@`((?:[^`]|``)*)`")

// singleDatabaseGrant matches a grant on one database, which is the
// only kind the restore puts back.
var singleDatabaseGrant = regexp.MustCompile("^GRANT (.+) ON `([^`]+)`\\.\\* TO '[^']+'@'[^']+'( WITH GRANT OPTION)?$")

// readAccount reads one account's hash, plugin and grants on the
// source's databases.
func (p *Provider) readAccount(ctx context.Context, who string, databases []string) (keptAccount, error) {
	user, host, err := splitWho(who)
	if err != nil {
		return keptAccount{}, err
	}
	kept := keptAccount{User: user, Host: host}
	// user and host passed usableAccountPart: letters, digits and a few
	// marks, nothing that closes a quote.
	rows, err := p.query(ctx, fmt.Sprintf("SELECT plugin, HEX(authentication_string) FROM mysql.user WHERE user = '%s' AND host = '%s'", user, host))
	if err != nil {
		return keptAccount{}, err
	}
	if len(rows) == 0 {
		return keptAccount{}, fmt.Errorf("the server has no account %s", who)
	}
	kept.Plugin = rows[0][0]
	if len(rows[0]) > 1 {
		kept.HexHash = rows[0][1]
	}
	rows, err = p.query(ctx, fmt.Sprintf("SHOW GRANTS FOR '%s'@'%s'", user, host))
	if err != nil {
		return keptAccount{}, err
	}
	for _, row := range rows {
		line := strings.TrimSuffix(strings.TrimSpace(strings.Join(row, "\t")), ";")
		if strings.HasPrefix(line, "GRANT USAGE ON *.* TO") {
			continue
		}
		line = granteeClause.ReplaceAllString(line, "TO '$1'@'$2'")
		// MariaDB carries the password on the USAGE line; a grant that
		// still names one is not a grant the restore reads.
		if at := strings.Index(line, " IDENTIFIED BY"); at > 0 {
			line = line[:at]
		}
		parts := singleDatabaseGrant.FindStringSubmatch(line)
		if parts == nil || !contains(databases, parts[2]) {
			kept.LeftOut = append(kept.LeftOut, line)
			continue
		}
		if parts[3] != "" {
			// The grant option is not put back: a restored account
			// gets what it needs to open the database, not to hand it
			// on. The line is kept beside, so the operator knows.
			kept.LeftOut = append(kept.LeftOut, line)
			line = strings.TrimSuffix(line, parts[3])
		}
		kept.Grants = append(kept.Grants, line)
	}
	return kept, nil
}

// createUser is the statement that makes the account again with the
// hash it had, in the dialect of the server it is written for.
func createUser(kept keptAccount, mariadb bool) (string, error) {
	if !usableAccountPart(kept.Plugin) {
		return "", fmt.Errorf("%s@%s authenticates with a plugin this cannot name: %q", kept.User, kept.Host, kept.Plugin)
	}
	if _, err := hex.DecodeString(kept.HexHash); err != nil {
		return "", fmt.Errorf("%s@%s: the stored password is not readable: %w", kept.User, kept.Host, err)
	}
	if kept.HexHash == "" {
		return fmt.Sprintf("CREATE USER IF NOT EXISTS '%s'@'%s' IDENTIFIED WITH %s", kept.User, kept.Host, kept.Plugin), nil
	}
	if !mariadb {
		return fmt.Sprintf("CREATE USER IF NOT EXISTS '%s'@'%s' IDENTIFIED WITH %s AS 0x%s", kept.User, kept.Host, kept.Plugin, kept.HexHash), nil
	}
	hash, _ := hex.DecodeString(kept.HexHash)
	for _, r := range string(hash) {
		if r < 0x21 || r > 0x7e || r == '\'' || r == '\\' {
			return "", fmt.Errorf("%s@%s: the stored password holds a character this cannot carry into a statement", kept.User, kept.Host)
		}
	}
	return fmt.Sprintf("CREATE USER IF NOT EXISTS '%s'@'%s' IDENTIFIED VIA %s USING '%s'", kept.User, kept.Host, kept.Plugin, hash), nil
}

// keepAccounts writes the source's accounts beside its dumps, in the
// files a panel's backup uses: the hashes as data, and the statements a
// person or the restore can run. Nothing here is a dump, so the names
// start with an underscore, as they do there.
func (p *Provider) keepAccounts(ctx context.Context, source panel.Source, dumps string) ([]string, []string, error) {
	mariadb, err := p.mysqlIsMariaDB(ctx)
	if err != nil {
		return nil, nil, err
	}
	type auth struct {
		Hash   string `json:"pass_hash"`
		Plugin string `json:"auth_plugin"`
	}
	hashes := map[string]map[string]auth{}
	var runnable strings.Builder
	fmt.Fprintf(&runnable, "-- The accounts kept with %s, with the password hashes they had.\n", source.Name)
	fmt.Fprintf(&runnable, "-- A restore runs these; so can a person, against a %s.\n", map[bool]string{true: "MariaDB", false: "MySQL"}[mariadb])
	var kept, warnings []string
	for _, who := range source.MySQLUsers {
		account, err := p.readAccount(ctx, who, source.MySQL)
		if err != nil {
			return nil, nil, err
		}
		statement, err := createUser(account, mariadb)
		if err != nil {
			return nil, nil, err
		}
		fmt.Fprintf(&runnable, "\n-- %s\n%s;\n", who, statement)
		for _, grant := range account.Grants {
			runnable.WriteString(grant + ";\n")
		}
		for _, left := range account.LeftOut {
			fmt.Fprintf(&runnable, "-- not put back by a restore: %s;\n", left)
			warnings = append(warnings, who+": not put back by a restore: "+left)
		}
		if hashes[account.User] == nil {
			hashes[account.User] = map[string]auth{}
		}
		hashes[account.User][account.Host] = auth{Hash: account.HexHash, Plugin: account.Plugin}
		kept = append(kept, who)
	}
	encoded, err := json.Marshal(hashes)
	if err != nil {
		return nil, nil, err
	}
	for name, body := range map[string][]byte{
		granular.RunnableDatabaseUsersFile: []byte(runnable.String()),
		granular.DatabaseUsersAuthFile:     encoded,
	} {
		if err := os.WriteFile(filepath.Join(dumps, name), body, 0o600); err != nil {
			return nil, nil, fmt.Errorf("plain: write %s: %w", name, err)
		}
	}
	return kept, warnings, nil
}

// PutDatabaseUsers makes the accounts again as the backup carried them,
// with the hashes they had and their grants on the source's databases.
// An account that exists is left as it is; the grants are added to it.
func (p *Provider) PutDatabaseUsers(ctx context.Context, account string, users []panel.DatabaseUser) error {
	source, err := p.source(account)
	if err != nil {
		return err
	}
	mariadb, err := p.mysqlIsMariaDB(ctx)
	if err != nil {
		return err
	}
	var statements []string
	for _, user := range users {
		if !usableAccountPart(user.Name) || !usableAccountPart(user.Host) {
			return fmt.Errorf("plain: %q@%q is not an account name this can use", user.Name, user.Host)
		}
		statement, err := createUser(keptAccount{User: user.Name, Host: user.Host, Plugin: user.Plugin, HexHash: user.Hash}, mariadb)
		if err != nil {
			return fmt.Errorf("plain: %w", err)
		}
		statements = append(statements, statement)
		for _, grant := range user.Grants {
			if !usableName(grant.Database) {
				return fmt.Errorf("plain: %q is not a database name", grant.Database)
			}
			if !contains(source.MySQL, grant.Database) {
				return fmt.Errorf("plain: %s was not chosen with %s; a grant on it is not put back through this source", grant.Database, source.Name)
			}
			for _, privilege := range grant.Privileges {
				for _, r := range privilege {
					if !(r >= 'A' && r <= 'Z') && !(r >= 'a' && r <= 'z') && r != ' ' && r != '_' {
						return fmt.Errorf("plain: %q is not a privilege", privilege)
					}
				}
			}
			statements = append(statements, fmt.Sprintf("GRANT %s ON `%s`.* TO '%s'@'%s'",
				strings.Join(grant.Privileges, ", "), grant.Database, user.Name, user.Host))
		}
	}
	if len(statements) == 0 {
		return nil
	}
	cmd := exec.CommandContext(ctx, p.mysql())
	cmd.Stdin = strings.NewReader(strings.Join(statements, ";\n") + ";\n")
	var complaint bytes.Buffer
	cmd.Stderr = &complaint
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("plain: recreate the accounts of %s: %w%s", source.Name, err, saidOnStderr(complaint.String()))
	}
	p.log().Warn("database accounts recreated from a backup", "source", source.Name, "accounts", len(users))
	return nil
}
