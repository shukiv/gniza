package plain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/shukiv/gniza/internal/granular"
	"github.com/shukiv/gniza/internal/panel"
)

// The PostgreSQL roles of a server without a panel.
//
// A plain pg_dump names the role that owns each table and sequence
// ("ALTER TABLE public.orders OWNER TO app"), and psql loading it under
// ON_ERROR_STOP on a server without that role stops at the first such
// line. So a role the operator ticks is kept with every source that
// dumps a database it owns or has rights on -- its password verifier,
// its attributes and its rights on those databases -- and the restore
// makes it again before the dump is loaded. The MySQL users have the
// same shape (users.go); the files differ because the servers do.

// systemPostgresRoles are the server's own: shown, not offered. The
// predefined pg_* roles are not even shown.
var systemPostgresRoles = map[string]bool{"postgres": true}

// pgRole is one role as pg_authid describes it.
type pgRole struct {
	Name                                                       string
	Login, CreateDB, Super, CreateRole, Replication, BypassRLS bool
	// Verifier is what authenticates the role -- a SCRAM verifier or an
	// md5 hash, as pg_authid keeps it -- or empty for a role with no
	// password.
	Verifier string
}

// pgQuery runs one statement through psql against the postgres database
// and returns its rows, tab separated, one per line.
func (p *Provider) pgQuery(ctx context.Context, statement string) ([][]string, error) {
	if _, err := exec.LookPath(p.psql()); err != nil {
		return nil, fmt.Errorf("plain: no psql on this server")
	}
	cmd := p.pgCommand(ctx, p.psql(), "-At", "-F", "\t", "-d", "postgres", "-c", statement)
	var out, complaint bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &complaint
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("plain: psql: %w%s", err, saidOnStderr(complaint.String()))
	}
	var rows [][]string
	for _, line := range strings.Split(out.String(), "\n") {
		if line != "" {
			rows = append(rows, strings.Split(line, "\t"))
		}
	}
	return rows, nil
}

// postgresRoleList is every role the server has, the predefined pg_*
// ones left out, with what authenticates it. pg_authid is read by a
// superuser alone, which the postgres account is.
func (p *Provider) postgresRoleList(ctx context.Context) ([]pgRole, error) {
	rows, err := p.pgQuery(ctx, "SELECT rolname, rolcanlogin, rolcreatedb, rolsuper, rolcreaterole, rolreplication, rolbypassrls, "+
		"COALESCE(rolpassword, '') FROM pg_authid WHERE rolname NOT LIKE 'pg\\_%' ORDER BY rolname")
	if err != nil {
		return nil, err
	}
	var roles []pgRole
	for _, row := range rows {
		if len(row) < 7 || !usableName(row[0]) || strings.HasPrefix(row[0], "pg_") {
			continue
		}
		role := pgRole{Name: row[0], Login: row[1] == "t", CreateDB: row[2] == "t", Super: row[3] == "t",
			CreateRole: row[4] == "t", Replication: row[5] == "t", BypassRLS: row[6] == "t"}
		if len(row) > 7 {
			role.Verifier = row[7]
		}
		roles = append(roles, role)
	}
	return roles, nil
}

// verifierKind names what a verifier is, never the verifier itself.
func verifierKind(verifier string) string {
	switch {
	case verifier == "":
		return "no password"
	case strings.HasPrefix(verifier, "SCRAM-SHA-256$"):
		return "scram-sha-256"
	case strings.HasPrefix(verifier, "md5"):
		return "md5"
	}
	return "password"
}

// roleAttributes are the attributes worth a word on a page, and among
// them the ones a restore leaves out: what makes a role more than a
// login is not handed to a server by a backup.
func roleAttributes(role pgRole) (shown, leftOut []string) {
	if !role.Login {
		shown = append(shown, "NOLOGIN")
	}
	if role.CreateDB {
		shown = append(shown, "CREATEDB")
	}
	for _, attribute := range []struct {
		on   bool
		name string
	}{{role.Super, "SUPERUSER"}, {role.CreateRole, "CREATEROLE"}, {role.Replication, "REPLICATION"}, {role.BypassRLS, "BYPASSRLS"}} {
		if attribute.on {
			shown = append(shown, attribute.name)
			leftOut = append(leftOut, attribute.name)
		}
	}
	return shown, leftOut
}

// databasePrivileges are the privileges a role can hold on a database,
// as aclexplode names them; owning one is written OWNER.
var databasePrivileges = map[string]bool{"CONNECT": true, "CREATE": true, "TEMPORARY": true}

const ownerPrivilege = "OWNER"

// postgresRights lists, for each database, the roles with rights on it:
// the owner, then what the database's ACL grants to others. PUBLIC's
// grants are nobody's and are not listed.
func (p *Provider) postgresRights(ctx context.Context, owners map[string]string) (map[string][]panel.DatabaseRight, error) {
	rows, err := p.pgQuery(ctx, "SELECT d.datname, x.grantee::regrole::text, x.privilege_type FROM pg_database d, "+
		"aclexplode(d.datacl) x WHERE NOT d.datistemplate AND d.datname <> 'postgres' AND x.grantee <> 0 ORDER BY 1, 2, 3")
	if err != nil {
		return nil, err
	}
	rights := map[string][]panel.DatabaseRight{}
	for database, owner := range owners {
		if usableName(database) && usableName(owner) {
			rights[database] = append(rights[database], panel.DatabaseRight{User: owner, Database: database, Privileges: []string{ownerPrivilege}})
		}
	}
	at := map[string]int{}
	for _, row := range rows {
		if len(row) < 3 || !usableName(row[0]) || !usableName(row[1]) || !databasePrivileges[row[2]] || row[1] == owners[row[0]] {
			continue
		}
		key := row[0] + "\t" + row[1]
		index, seen := at[key]
		if !seen {
			index = len(rights[row[0]])
			at[key] = index
			rights[row[0]] = append(rights[row[0]], panel.DatabaseRight{User: row[1], Database: row[0]})
		}
		rights[row[0]][index].Privileges = append(rights[row[0]][index].Privileges, row[2])
	}
	return rights, nil
}

// postgresMemberships lists, for each role, the roles it is a member of,
// the predefined ones left out.
func (p *Provider) postgresMemberships(ctx context.Context) (map[string][]string, error) {
	rows, err := p.pgQuery(ctx, "SELECT member::regrole::text, roleid::regrole::text FROM pg_auth_members ORDER BY 1, 2")
	if err != nil {
		return nil, err
	}
	memberOf := map[string][]string{}
	for _, row := range rows {
		if len(row) < 2 || !usableName(row[0]) || !usableName(row[1]) || strings.HasPrefix(row[1], "pg_") {
			continue
		}
		memberOf[row[0]] = append(memberOf[row[0]], row[1])
	}
	return memberOf, nil
}

// postgresUserCandidates is every role with what it owns or can reach,
// and the sources each is kept with.
func (p *Provider) postgresUserCandidates(ctx context.Context, sources []panel.Source,
	rights map[string][]panel.DatabaseRight) ([]panel.DatabaseUserCandidate, error) {

	roles, err := p.postgresRoleList(ctx)
	if err != nil {
		return nil, err
	}
	byWho := map[string][]panel.DatabaseRight{}
	for _, holders := range rights {
		for _, right := range holders {
			byWho[right.User] = append(byWho[right.User], right)
		}
	}
	attached := map[string][]string{}
	for _, source := range sources {
		for _, name := range source.PostgreSQLUsers {
			attached[name] = append(attached[name], source.Name)
		}
	}
	var candidates []panel.DatabaseUserCandidate
	for _, role := range roles {
		shown, _ := roleAttributes(role)
		candidate := panel.DatabaseUserCandidate{User: role.Name, Plugin: verifierKind(role.Verifier),
			System: systemPostgresRoles[role.Name], Global: shown, Rights: byWho[role.Name],
			AttachedTo: strings.Join(attached[role.Name], ",")}
		sort.Slice(candidate.Rights, func(a, b int) bool { return candidate.Rights[a].Database < candidate.Rights[b].Database })
		candidates = append(candidates, candidate)
	}
	// The server's own last: the operator came for the others.
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].System != candidates[j].System {
			return !candidates[i].System
		}
		return candidates[i].User < candidates[j].User
	})
	return candidates, nil
}

// AttachPostgreSQLUser keeps a role with every source that dumps a
// database it owns or has rights on. See panel.Chooser.
func (p *Provider) AttachPostgreSQLUser(ctx context.Context, role string) ([]string, error) {
	if p.Catalog == nil {
		return nil, ErrNoCatalog
	}
	role = strings.TrimSpace(role)
	if !usableName(role) {
		return nil, fmt.Errorf("%q is not a role name this can use", role)
	}
	if systemPostgresRoles[role] {
		return nil, fmt.Errorf("%s is the server's own role, not one a backup carries", role)
	}
	roles, err := p.postgresRoleList(ctx)
	if err != nil {
		return nil, err
	}
	known := false
	for _, each := range roles {
		known = known || each.Name == role
	}
	if !known {
		return nil, fmt.Errorf("the server has no PostgreSQL role %s", role)
	}
	_, owners, err := p.postgresList(ctx)
	if err != nil {
		return nil, err
	}
	rights, err := p.postgresRights(ctx, owners)
	if err != nil {
		return nil, err
	}
	reaches := map[string]bool{}
	for database, holders := range rights {
		for _, right := range holders {
			if right.User == role {
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
		for _, database := range source.PostgreSQL {
			keeps = keeps || reaches[database]
		}
		if !keeps {
			continue
		}
		names = append(names, source.Name)
		if contains(source.PostgreSQLUsers, role) {
			continue
		}
		source.PostgreSQLUsers = sortedCopy(append(source.PostgreSQLUsers, role))
		if err := p.Catalog.SaveSource(source); err != nil {
			return nil, fmt.Errorf("plain: keep the choice: %w", err)
		}
		p.log().Info("a PostgreSQL role was kept with a source", "source", source.Name, "role", role)
	}
	if len(names) == 0 {
		var databases []string
		for database := range reaches {
			databases = append(databases, database)
		}
		sort.Strings(databases)
		if len(databases) == 0 {
			return nil, fmt.Errorf("%s owns no database and has rights on none; there is nothing to keep it with", role)
		}
		return nil, fmt.Errorf("%s has rights on no database that is backed up; tick %s with it", role, strings.Join(databases, ", "))
	}
	return names, nil
}

// --- keeping roles with the dumps ---

// keptRole is one role as the file beside the dumps carries it.
type keptRole struct {
	Name     string `json:"name"`
	Login    bool   `json:"login"`
	CreateDB bool   `json:"createdb,omitempty"`
	// Password is the verifier pg_authid keeps, SCRAM or md5, which
	// CREATE ROLE takes back as it is; empty for a role without one.
	Password string `json:"password,omitempty"`
	// LeftOut are the attributes a restore does not give back.
	LeftOut []string `json:"left_out,omitempty"`
	// Owns are the source's databases the role owns, Rights what it
	// holds on the others of them, and MemberOf the kept roles it is a
	// member of.
	Owns     []string    `json:"owns,omitempty"`
	Rights   []keptRight `json:"rights,omitempty"`
	MemberOf []string    `json:"member_of,omitempty"`
}

type keptRight struct {
	Database   string   `json:"database"`
	Privileges []string `json:"privileges"`
}

// usableVerifier says a password verifier is made of what SCRAM and md5
// verifiers are made of and nothing a statement could hide in, since it
// goes into a quoted literal inside a dollar-quoted block.
func usableVerifier(verifier string) bool {
	if len(verifier) > 512 || strings.Contains(verifier, "$gniza$") {
		return false
	}
	for _, r := range verifier {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '+' || r == '/' || r == '=' || r == '$' || r == ':' || r == '-':
		default:
			return false
		}
	}
	return true
}

// createRole is the statement that makes the role again unless it is
// there, with the verifier it had: a DO block, since CREATE ROLE has no
// IF NOT EXISTS. A role that exists is left as it is.
func createRole(role keptRole) (string, error) {
	if !usableName(role.Name) {
		return "", fmt.Errorf("%q is not a role name this can use", role.Name)
	}
	if !usableVerifier(role.Password) {
		return "", fmt.Errorf("%s: the stored password holds a character this cannot carry into a statement", role.Name)
	}
	with := "NOLOGIN"
	if role.Login {
		with = "LOGIN"
	}
	if role.CreateDB {
		with += " CREATEDB"
	}
	if role.Password != "" {
		with += " PASSWORD '" + role.Password + "'"
	}
	return fmt.Sprintf(`DO $gniza$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '%s') THEN CREATE ROLE "%s" WITH %s; END IF; END $gniza$`,
		role.Name, role.Name, with), nil
}

// roleStatements are the statements that put the roles back for one
// database, or for every database they were kept with when none is
// named: each role made unless it is there, then what it owns, what it
// is granted, and the memberships among the roles put back. Every name
// is checked here, since the file they come from was written by an
// earlier backup and read by a later restore.
func roleStatements(roles []keptRole, chosen []string, database string) ([]string, error) {
	var statements []string
	made := map[string]bool{}
	for _, role := range roles {
		var owns []string
		var rights []keptRight
		for _, name := range role.Owns {
			if database == "" || name == database {
				owns = append(owns, name)
			}
		}
		for _, right := range role.Rights {
			if database == "" || right.Database == database {
				rights = append(rights, right)
			}
		}
		if database != "" && len(owns)+len(rights) == 0 {
			continue
		}
		create, err := createRole(role)
		if err != nil {
			return nil, err
		}
		statements = append(statements, create)
		made[role.Name] = true
		for _, name := range owns {
			if !usableName(name) || !contains(chosen, name) {
				return nil, fmt.Errorf("%s was not chosen with this source; its owner is not put back through it", name)
			}
			statements = append(statements, fmt.Sprintf(`ALTER DATABASE "%s" OWNER TO "%s"`, name, role.Name))
		}
		for _, right := range rights {
			if !usableName(right.Database) || !contains(chosen, right.Database) {
				return nil, fmt.Errorf("%s was not chosen with this source; a grant on it is not put back through it", right.Database)
			}
			for _, privilege := range right.Privileges {
				if !databasePrivileges[privilege] {
					return nil, fmt.Errorf("%q is not a privilege on a database", privilege)
				}
			}
			if len(right.Privileges) == 0 {
				continue
			}
			statements = append(statements, fmt.Sprintf(`GRANT %s ON DATABASE "%s" TO "%s"`,
				strings.Join(right.Privileges, ", "), right.Database, role.Name))
		}
	}
	for _, role := range roles {
		if !made[role.Name] {
			continue
		}
		for _, parent := range role.MemberOf {
			if !usableName(parent) {
				return nil, fmt.Errorf("%q is not a role name this can use", parent)
			}
			if made[parent] {
				statements = append(statements, fmt.Sprintf(`GRANT "%s" TO "%s"`, parent, role.Name))
			}
		}
	}
	return statements, nil
}

// keepRoles writes the source's roles beside its dumps: as data, and as
// the statements a person or the restore runs. Nothing here is a dump,
// so the names start with an underscore, as the MySQL users' do.
func (p *Provider) keepRoles(ctx context.Context, source panel.Source, dumps string) ([]string, []string, error) {
	roles, err := p.postgresRoleList(ctx)
	if err != nil {
		return nil, nil, err
	}
	_, owners, err := p.postgresList(ctx)
	if err != nil {
		return nil, nil, err
	}
	rights, err := p.postgresRights(ctx, owners)
	if err != nil {
		return nil, nil, err
	}
	memberOf, err := p.postgresMemberships(ctx)
	if err != nil {
		return nil, nil, err
	}
	byName := map[string]pgRole{}
	for _, role := range roles {
		byName[role.Name] = role
	}
	var databases []string
	for database := range rights {
		databases = append(databases, database)
	}
	sort.Strings(databases)
	var kept []keptRole
	var names, warnings []string
	for _, name := range source.PostgreSQLUsers {
		role, known := byName[name]
		if !known {
			return nil, nil, fmt.Errorf("the server has no PostgreSQL role %s", name)
		}
		_, leftOut := roleAttributes(role)
		item := keptRole{Name: name, Login: role.Login, CreateDB: role.CreateDB, Password: role.Verifier, LeftOut: leftOut}
		var elsewhere []string
		for _, database := range databases {
			for _, right := range rights[database] {
				if right.User != name {
					continue
				}
				if !contains(source.PostgreSQL, database) {
					elsewhere = append(elsewhere, database)
					continue
				}
				if len(right.Privileges) == 1 && right.Privileges[0] == ownerPrivilege {
					item.Owns = append(item.Owns, database)
				} else {
					item.Rights = append(item.Rights, keptRight{Database: database, Privileges: right.Privileges})
				}
			}
		}
		for _, parent := range memberOf[name] {
			if contains(source.PostgreSQLUsers, parent) {
				item.MemberOf = append(item.MemberOf, parent)
			} else {
				elsewhere = append(elsewhere, "membership of "+parent)
			}
		}
		for _, attribute := range leftOut {
			warnings = append(warnings, name+": not put back by a restore: "+attribute)
		}
		if len(elsewhere) > 0 {
			warnings = append(warnings, name+": not put back by a restore: rights on "+strings.Join(elsewhere, ", "))
		}
		kept = append(kept, item)
		names = append(names, name)
	}
	statements, err := roleStatements(kept, source.PostgreSQL, "")
	if err != nil {
		return nil, nil, err
	}
	var runnable strings.Builder
	fmt.Fprintf(&runnable, "-- The PostgreSQL roles kept with %s, with the password verifiers they had.\n", source.Name)
	fmt.Fprintf(&runnable, "-- A restore runs these before it loads the dumps; so can a person, through psql -d postgres.\n\n")
	for _, statement := range statements {
		runnable.WriteString(statement + ";\n")
	}
	for _, warning := range warnings {
		fmt.Fprintf(&runnable, "-- %s\n", warning)
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		return nil, nil, err
	}
	for name, body := range map[string][]byte{
		granular.RunnablePostgresRolesFile: []byte(runnable.String()),
		granular.PostgresRolesFile:         encoded,
	} {
		if err := os.WriteFile(filepath.Join(dumps, name), body, 0o600); err != nil {
			return nil, nil, fmt.Errorf("plain: write %s: %w", name, err)
		}
	}
	return names, warnings, nil
}

// putRoles makes the roles kept beside a dump again before the dump is
// loaded into the database: the ones that own it or have rights on it,
// from the file the backup wrote. No file means no role was kept, and
// the dump is loaded as it is.
func (p *Provider) putRoles(ctx context.Context, source panel.Source, database, dumps string) error {
	raw, err := os.ReadFile(filepath.Join(dumps, granular.PostgresRolesFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("plain: read the roles kept with the dump: %w", err)
	}
	var roles []keptRole
	if err := json.Unmarshal(raw, &roles); err != nil {
		return fmt.Errorf("plain: the roles kept with the dump are not readable: %w", err)
	}
	statements, err := roleStatements(roles, source.PostgreSQL, database)
	if err != nil {
		return fmt.Errorf("plain: %w", err)
	}
	if len(statements) == 0 {
		return nil
	}
	cmd := p.pgCommand(ctx, p.psql(), "-v", "ON_ERROR_STOP=1", "-q", "-d", "postgres")
	cmd.Stdin = strings.NewReader(strings.Join(statements, ";\n") + ";\n")
	var complaint bytes.Buffer
	cmd.Stderr = &complaint
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("plain: recreate the roles kept with %s: %w%s", database, err, saidOnStderr(complaint.String()))
	}
	p.log().Warn("PostgreSQL roles recreated from a backup", "source", source.Name, "database", database, "statements", len(statements))
	return nil
}
