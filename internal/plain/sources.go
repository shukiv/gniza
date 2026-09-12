package plain

import (
	"bufio"
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
	"strconv"
	"strings"
	"time"

	"github.com/shukiv/gniza/internal/panel"
)

// The operator's choices: what panel.Chooser asks of a plain server.

// ErrNoCatalog says the provider has nowhere to keep a choice.
var ErrNoCatalog = errors.New("plain: nothing can be chosen here; the provider has no catalog")

// Sources is what has been chosen, by name.
func (p *Provider) Sources(context.Context) ([]panel.Source, error) {
	if p.Catalog == nil {
		return nil, nil
	}
	sources, err := p.Catalog.Sources()
	if err != nil {
		return nil, fmt.Errorf("plain: read the chosen sources: %w", err)
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Name < sources[j].Name })
	return sources, nil
}

// pathAs names the source that backs each chosen folder up, by path.
func (p *Provider) pathAs(ctx context.Context) (map[string]string, error) {
	sources, err := p.Sources(ctx)
	if err != nil {
		return nil, err
	}
	pathAs := map[string]string{}
	for _, source := range sources {
		if source.Path != "" && source.Container == nil {
			pathAs[filepath.Clean(source.Path)] = source.Name
		}
	}
	return pathAs, nil
}

// pseudoFilesystems are the directories under / that hold no files of
// anyone's: the kernel's views and the boot-time mounts. The browser
// leaves them out of the root.
var pseudoFilesystems = map[string]bool{"/proc": true, "/sys": true, "/dev": true, "/run": true}

// Browse lists the directories under one, for the folder browser, with
// the ones chosen already marked. Symbolic links are not followed: a
// link is not a folder to back up, its target is.
func (p *Provider) Browse(ctx context.Context, dir string) (panel.Listing, error) {
	if !filepath.IsAbs(dir) {
		return panel.Listing{}, fmt.Errorf("%s is not an absolute path; a folder is named from /", dir)
	}
	dir = filepath.Clean(dir)
	stat, err := os.Stat(dir)
	switch {
	case err != nil:
		return panel.Listing{}, fmt.Errorf("%s is not there: %v", dir, err)
	case !stat.IsDir():
		return panel.Listing{}, fmt.Errorf("%s is a file; choose the folder that holds it", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return panel.Listing{}, fmt.Errorf("plain: list %s: %w", dir, err)
	}
	pathAs, err := p.pathAs(ctx)
	if err != nil {
		return panel.Listing{}, err
	}
	listing := panel.Listing{Dir: dir}
	if dir != "/" {
		listing.Parent = filepath.Dir(dir)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if dir == "/" && pseudoFilesystems[path] {
			continue
		}
		listing.Entries = append(listing.Entries, panel.FolderEntry{Name: entry.Name(), Path: path, ChosenAs: pathAs[path]})
	}
	return listing, nil
}

// source finds one choice by name.
func (p *Provider) source(name string) (panel.Source, error) {
	sources, err := p.Sources(context.Background())
	if err != nil {
		return panel.Source{}, err
	}
	for _, source := range sources {
		if source.Name == name {
			return source, nil
		}
	}
	return panel.Source{}, fmt.Errorf("plain: %s is %w: nothing of that name has been chosen to back up", name, panel.ErrNoSuchAccount)
}

// Candidates is what could be chosen now, by kind: the directories
// under the roots, the databases each client can see with the users
// that hold rights on them, and the containers each engine knows about
// grouped into the stacks their compose files make. What is chosen
// already is marked, not left out, so the pages can say so.
func (p *Provider) Candidates(ctx context.Context) (panel.Candidates, error) {
	sources, err := p.Sources(ctx)
	if err != nil {
		return panel.Candidates{}, err
	}
	pathAs := map[string]string{}
	mysqlAs := map[string]string{}
	postgresAs := map[string]string{}
	chosenContainer := map[string]bool{}
	for _, source := range sources {
		if source.Path != "" && source.Container == nil {
			pathAs[filepath.Clean(source.Path)] = source.Name
		}
		for _, name := range source.MySQL {
			mysqlAs[name] = source.Name
		}
		for _, name := range source.PostgreSQL {
			postgresAs[name] = source.Name
		}
		if source.Container != nil {
			chosenContainer[source.Container.Engine+"/"+source.Container.Name] = true
		}
	}
	found := panel.Candidates{Roots: p.Roots}
	for _, root := range p.Roots {
		entries, err := os.ReadDir(root)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return panel.Candidates{}, fmt.Errorf("plain: list %s: %w", root, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if !entry.IsDir() || strings.HasPrefix(name, ".") {
				continue
			}
			path := filepath.Join(root, name)
			found.Folders = append(found.Folders, panel.FolderCandidate{Path: path, ChosenAs: pathAs[path]})
		}
	}
	if sizes, err := p.databaseSizes(ctx); err != nil {
		found.MySQLError = err.Error()
	} else {
		users, err := p.mysqlUsers(ctx)
		if err != nil {
			p.log().Debug("the MySQL users could not be listed", "error", err)
		}
		for name, size := range sizes {
			found.MySQL = append(found.MySQL, panel.DatabaseCandidate{
				Name: name, Size: size, Users: users[name], ChosenAs: mysqlAs[name]})
		}
		sort.Slice(found.MySQL, func(i, j int) bool { return found.MySQL[i].Name < found.MySQL[j].Name })
	}
	if sizes, owners, err := p.postgresList(ctx); err != nil {
		found.PostgreSQLError = err.Error()
	} else {
		for name, size := range sizes {
			candidate := panel.DatabaseCandidate{Name: name, Size: size, ChosenAs: postgresAs[name]}
			if owner := owners[name]; owner != "" {
				candidate.Users = []string{owner}
			}
			found.PostgreSQL = append(found.PostgreSQL, candidate)
		}
		sort.Slice(found.PostgreSQL, func(i, j int) bool { return found.PostgreSQL[i].Name < found.PostgreSQL[j].Name })
	}
	for _, engine := range []string{"docker", "podman"} {
		looked := panel.EngineCandidate{Name: engine}
		if dir := engineConfigDir(engine); dir != "" {
			looked.ConfigDir = dir
			looked.ChosenAs = pathAs[dir]
		}
		containers, stacks, err := p.containers(ctx, engine)
		switch {
		case err == nil:
			looked.Present = true
		case errors.Is(err, exec.ErrNotFound), errors.Is(err, fs.ErrNotExist):
			// Not installed: nothing to say beyond that.
		default:
			looked.Present = true
			looked.Error = err.Error()
			p.log().Debug("no containers listed", "engine", engine, "error", err)
		}
		found.Engines = append(found.Engines, looked)
		for i := range containers {
			containers[i].Chosen = chosenContainer[engine+"/"+containers[i].Name]
		}
		found.Containers = append(found.Containers, containers...)
		for _, stack := range stacks {
			stack.ChosenAs = pathAs[stack.Dir]
			found.Stacks = append(found.Stacks, stack)
		}
	}
	sort.Slice(found.Stacks, func(i, j int) bool {
		if found.Stacks[i].Engine != found.Stacks[j].Engine {
			return found.Stacks[i].Engine < found.Stacks[j].Engine
		}
		return found.Stacks[i].Name < found.Stacks[j].Name
	})
	return found, nil
}

// engineConfigDir is where an engine keeps its own configuration, when
// that directory is there: the daemon's settings, registries, and on
// podman the quadlets under systemd.
func engineConfigDir(engine string) string {
	dir := map[string]string{"docker": "/etc/docker", "podman": "/etc/containers"}[engine]
	if stat, err := os.Stat(dir); err != nil || !stat.IsDir() {
		return ""
	}
	return dir
}

// AddSource records a choice, after checking it is one this server can
// honour.
func (p *Provider) AddSource(ctx context.Context, source panel.Source) error {
	if p.Catalog == nil {
		return ErrNoCatalog
	}
	source.Name = strings.TrimSpace(source.Name)
	source.Path = strings.TrimSpace(source.Path)
	source.MySQL = sortedCopy(source.MySQL)
	source.PostgreSQL = sortedCopy(source.PostgreSQL)
	if source.Path == "" && len(source.MySQL)+len(source.PostgreSQL) == 0 && source.Container == nil {
		return fmt.Errorf("choose a folder, a database, or both; there is nothing to back up otherwise")
	}
	if source.Path != "" {
		if !filepath.IsAbs(source.Path) {
			return fmt.Errorf("%s is not an absolute path; a folder is named from /", source.Path)
		}
		source.Path = filepath.Clean(source.Path)
		stat, err := os.Stat(source.Path)
		switch {
		case err != nil:
			return fmt.Errorf("%s is not there: %v", source.Path, err)
		case !stat.IsDir():
			return fmt.Errorf("%s is a file; choose the folder that holds it", source.Path)
		case source.Path == "/":
			return fmt.Errorf("the whole filesystem is not a source; choose the folders that hold what matters, and the system backup takes the configuration")
		}
	}
	if source.Name == "" {
		source.Name = suggestName(source)
	}
	if !usableName(source.Name) {
		return fmt.Errorf("%q cannot be a name: letters, digits, dot, dash and underscore, up to 64", source.Name)
	}
	for _, name := range append(append([]string{}, source.MySQL...), source.PostgreSQL...) {
		if !usableName(name) {
			return fmt.Errorf("%q is not a database name", name)
		}
	}
	existing, err := p.Sources(ctx)
	if err != nil {
		return err
	}
	for _, other := range existing {
		if other.Name == source.Name {
			return fmt.Errorf("%s is already chosen; remove it first to change what it holds", source.Name)
		}
		if source.Path != "" && other.Path == source.Path {
			return fmt.Errorf("%s is already backed up as %s", source.Path, other.Name)
		}
	}
	if source.AddedAt.IsZero() {
		source.AddedAt = time.Now().UTC()
	}
	if err := p.Catalog.SaveSource(source); err != nil {
		return fmt.Errorf("plain: keep the choice: %w", err)
	}
	p.log().Info("a source was chosen", "source", source.Name, "path", source.Path,
		"mysql", len(source.MySQL), "postgresql", len(source.PostgreSQL))
	return nil
}

// suggestName names a source nobody named: after its folder, or its
// first database with the engine in front, so a database and a folder
// called the same do not fight over one name.
func suggestName(source panel.Source) string {
	switch {
	case source.Path != "":
		return sanitizeName(filepath.Base(source.Path))
	case len(source.MySQL) > 0:
		return sanitizeName("mysql-" + source.MySQL[0])
	case len(source.PostgreSQL) > 0:
		return sanitizeName("pg-" + source.PostgreSQL[0])
	}
	return ""
}

// sanitizeName makes a usable name out of whatever a container or a
// mount was called.
func sanitizeName(raw string) string {
	var b strings.Builder
	for _, r := range strings.TrimLeft(raw, "/") {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-.")
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

// RemoveSource forgets a choice. The backups taken of it stay, and so
// does its history: a source removed by mistake is chosen again under
// the same name and carries on.
func (p *Provider) RemoveSource(ctx context.Context, name string) error {
	if p.Catalog == nil {
		return ErrNoCatalog
	}
	if _, err := p.source(name); err != nil {
		return err
	}
	if err := p.Catalog.DeleteSource(name); err != nil {
		return fmt.Errorf("plain: forget %s: %w", name, err)
	}
	p.log().Info("a source was removed", "source", name)
	return nil
}

// --- PostgreSQL ---

func (p *Provider) psql() string {
	if p.PsqlPath != "" {
		return p.PsqlPath
	}
	return "psql"
}

func (p *Provider) pgdump() string {
	if p.PgDumpPath != "" {
		return p.PgDumpPath
	}
	return "pg_dump"
}

// pgCommand is a PostgreSQL client command, run as the postgres unix
// account when this process is root: a fresh PostgreSQL trusts that
// account over its socket and nobody else, and a service that ran psql
// as root would be asked for a password it does not have.
func (p *Provider) pgCommand(ctx context.Context, tool string, args ...string) *exec.Cmd {
	user := p.PostgresUser
	if user == "" {
		user = "postgres"
	}
	if user == "-" || os.Geteuid() != 0 {
		return exec.CommandContext(ctx, tool, args...)
	}
	return exec.CommandContext(ctx, "runuser", append([]string{"-u", user, "--", tool}, args...)...)
}

// pgdumpall is pg_dumpall: named, or beside pg_dump when that was, or
// the one on PATH.
func (p *Provider) pgdumpall() string {
	switch {
	case p.PgDumpallPath != "":
		return p.PgDumpallPath
	case p.PgDumpPath != "":
		return filepath.Join(filepath.Dir(p.PgDumpPath), "pg_dumpall")
	}
	return "pg_dumpall"
}

// postgresSizes lists every PostgreSQL database with the bytes it holds.
func (p *Provider) postgresSizes(ctx context.Context) (map[string]uint64, error) {
	sizes, _, err := p.postgresList(ctx)
	return sizes, err
}

// postgresList lists every PostgreSQL database with the bytes it holds
// and the role that owns it. The templates are not anybody's, and
// neither is postgres itself.
func (p *Provider) postgresList(ctx context.Context) (sizes map[string]uint64, owners map[string]string, err error) {
	if _, err := exec.LookPath(p.psql()); err != nil {
		return nil, nil, fmt.Errorf("plain: no psql on this server")
	}
	cmd := p.pgCommand(ctx, p.psql(), "-At", "-F", "\t", "-d", "postgres", "-c",
		"SELECT datname, pg_database_size(datname), pg_get_userbyid(datdba) FROM pg_database WHERE NOT datistemplate")
	var out, complaint bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &complaint
	if err := cmd.Run(); err != nil {
		return nil, nil, fmt.Errorf("plain: psql: %w%s", err, saidOnStderr(complaint.String()))
	}
	sizes, owners = map[string]uint64{}, map[string]string{}
	scanner := bufio.NewScanner(&out)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "\t")
		if len(fields) == 0 || fields[0] == "" || fields[0] == "postgres" {
			continue
		}
		var size uint64
		if len(fields) > 1 {
			size, _ = strconv.ParseUint(strings.TrimSpace(fields[1]), 10, 64)
		}
		sizes[fields[0]] = size
		if len(fields) > 2 && strings.TrimSpace(fields[2]) != "" {
			owners[fields[0]] = strings.TrimSpace(fields[2])
		}
	}
	return sizes, owners, nil
}

// postgresRoles is every role on the server with its attributes and
// memberships, as pg_dumpall writes them, so a restore elsewhere can
// create the owner before the dump is loaded. It is kept beside the
// dump and not run on restore.
func (p *Provider) postgresRoles(ctx context.Context) ([]byte, error) {
	if _, err := exec.LookPath(p.pgdumpall()); err != nil {
		return nil, fmt.Errorf("plain: no pg_dumpall on this server")
	}
	cmd := p.pgCommand(ctx, p.pgdumpall(), "--roles-only")
	var out, complaint bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &complaint
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("plain: pg_dumpall --roles-only: %w%s", err, saidOnStderr(complaint.String()))
	}
	return out.Bytes(), nil
}

// --- MySQL users ---

// mysqlUsers lists, for each database, the accounts granted rights on
// it at the schema level. A user granted everything on every database
// is not listed against any of them; root is the operator, not a
// customer.
func (p *Provider) mysqlUsers(ctx context.Context) (map[string][]string, error) {
	cmd := exec.CommandContext(ctx, p.mysql(), "-N", "-B", "-e",
		"SELECT table_schema, grantee FROM information_schema.schema_privileges GROUP BY table_schema, grantee ORDER BY 1, 2")
	var out, complaint bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &complaint
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("plain: mysql: %w%s", err, saidOnStderr(complaint.String()))
	}
	users := map[string][]string{}
	scanner := bufio.NewScanner(&out)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "\t")
		if len(fields) < 2 || fields[0] == "" {
			continue
		}
		grantee := strings.TrimSpace(fields[1])
		if grantee == "" || !usableGrantee(grantee) {
			continue
		}
		users[fields[0]] = append(users[fields[0]], grantee)
	}
	return users, nil
}

// usableGrantee says a grantee reads as 'user'@'host', as the server
// writes it, and nothing else: it is handed back to the server inside
// SHOW GRANTS, so it is not allowed to be anything a query could hide
// in.
func usableGrantee(grantee string) bool {
	if len(grantee) > 200 {
		return false
	}
	for _, r := range grantee {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '\'' || r == '@' || r == '_' || r == '-' || r == '.' || r == '%' || r == '`':
		default:
			return false
		}
	}
	return strings.Count(grantee, "'") == 4 && strings.Contains(grantee, "'@'")
}

// mysqlGrants is the grants of every account with rights on the
// database, as SHOW GRANTS writes them, one account after another. They
// are kept beside the dump so a restore elsewhere knows who used the
// database; they are not run on restore, and on MySQL 8 they do not
// carry the password anyway.
func (p *Provider) mysqlGrants(ctx context.Context, database string) ([]byte, error) {
	users, err := p.mysqlUsers(ctx)
	if err != nil {
		return nil, err
	}
	var text bytes.Buffer
	fmt.Fprintf(&text, "-- Accounts with rights on %s, as SHOW GRANTS listed them.\n", database)
	fmt.Fprintf(&text, "-- Kept beside the dump for reference; not run on restore.\n")
	for _, grantee := range users[database] {
		cmd := exec.CommandContext(ctx, p.mysql(), "-N", "-B", "-e", "SHOW GRANTS FOR "+grantee)
		var out, complaint bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &complaint
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("plain: show grants for %s: %w%s", grantee, err, saidOnStderr(complaint.String()))
		}
		fmt.Fprintf(&text, "\n-- %s\n", grantee)
		scanner := bufio.NewScanner(&out)
		for scanner.Scan() {
			text.WriteString(scanner.Text())
			text.WriteString(";\n")
		}
	}
	return text.Bytes(), nil
}

// dumpPostgres writes one PostgreSQL database's dump as plain SQL,
// uncompressed so restic can deduplicate it between nights.
func (p *Provider) dumpPostgres(ctx context.Context, name, path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("plain: create dump %s: %w", path, err)
	}
	cmd := p.pgCommand(ctx, p.pgdump(), "--no-password", name)
	cmd.Stdout = file
	var complaint bytes.Buffer
	cmd.Stderr = &complaint
	err = cmd.Run()
	closeErr := file.Close()
	p.log().Debug("dumped a PostgreSQL database", "database", name, "error", err)
	if err != nil {
		return fmt.Errorf("plain: pg_dump %s: %w%s", name, err, saidOnStderr(complaint.String()))
	}
	return closeErr
}

// createPostgres makes a database that is gone. One that is there is
// left as it is; PostgreSQL has no IF NOT EXISTS for a database.
func (p *Provider) createPostgres(ctx context.Context, name string) error {
	sizes, err := p.postgresSizes(ctx)
	if err != nil {
		return err
	}
	if _, there := sizes[name]; there {
		return nil
	}
	cmd := p.pgCommand(ctx, p.psql(), "-v", "ON_ERROR_STOP=1", "-q", "-d", "postgres", "-c",
		`CREATE DATABASE "`+name+`"`)
	var complaint bytes.Buffer
	cmd.Stderr = &complaint
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("plain: create database %s: %w%s", name, err, saidOnStderr(complaint.String()))
	}
	return nil
}

// --- containers ---

func (p *Provider) docker() string {
	if p.DockerPath != "" {
		return p.DockerPath
	}
	return "docker"
}

func (p *Provider) podman() string {
	if p.PodmanPath != "" {
		return p.PodmanPath
	}
	return "podman"
}

func (p *Provider) engine(name string) (string, error) {
	switch name {
	case "docker":
		return p.docker(), nil
	case "podman":
		return p.podman(), nil
	}
	return "", fmt.Errorf("plain: %q is not a container engine; docker or podman", name)
}

// containers lists what one engine knows about, running or not, with
// the stack each belongs to and how many mounts it has. One ps for the
// names and one inspect for the rest: the labels and mounts come from
// the same description a backup keeps.
func (p *Provider) containers(ctx context.Context, engine string) ([]panel.ContainerCandidate, []panel.StackCandidate, error) {
	tool, err := p.engine(engine)
	if err != nil {
		return nil, nil, err
	}
	if _, err := exec.LookPath(tool); err != nil {
		return nil, nil, err
	}
	cmd := exec.CommandContext(ctx, tool, "ps", "-a", "--format", "{{.Names}}\t{{.Image}}\t{{.Status}}")
	var out, complaint bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &complaint
	if err := cmd.Run(); err != nil {
		return nil, nil, fmt.Errorf("plain: %s ps: %w%s", engine, err, saidOnStderr(complaint.String()))
	}
	var found []panel.ContainerCandidate
	scanner := bufio.NewScanner(&out)
	for scanner.Scan() {
		fields := strings.SplitN(scanner.Text(), "\t", 3)
		if len(fields) == 0 || fields[0] == "" {
			continue
		}
		candidate := panel.ContainerCandidate{Engine: engine, Name: fields[0]}
		if len(fields) > 1 {
			candidate.Image = fields[1]
		}
		if len(fields) > 2 {
			candidate.Status = fields[2]
		}
		found = append(found, candidate)
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Name < found[j].Name })
	if len(found) == 0 {
		return nil, nil, nil
	}
	names := make([]string, len(found))
	for i, c := range found {
		names[i] = c.Name
	}
	described, err := p.inspectAll(ctx, engine, names)
	if err != nil {
		// The list is still worth showing without the stacks and the
		// mount counts.
		p.log().Debug("containers listed but not inspected", "engine", engine, "error", err)
		return found, nil, nil
	}
	byName := map[string]inspection{}
	for _, d := range described {
		byName[strings.TrimPrefix(d.Name, "/")] = d
	}
	stacks := map[string]*panel.StackCandidate{}
	for i := range found {
		d, known := byName[found[i].Name]
		if !known {
			continue
		}
		for _, mount := range d.Mounts {
			if mount.Source != "" {
				found[i].Mounts++
			}
		}
		project, dir := d.stack()
		if project == "" {
			continue
		}
		found[i].Stack = project
		stack, seen := stacks[project]
		if !seen {
			stack = &panel.StackCandidate{Engine: engine, Name: project, Dir: dir}
			stacks[project] = stack
		}
		stack.Containers = append(stack.Containers, found[i].Name)
	}
	var made []panel.StackCandidate
	for _, stack := range stacks {
		made = append(made, *stack)
	}
	return found, made, nil
}

// inspectAll describes every container named in one call.
func (p *Provider) inspectAll(ctx context.Context, engine string, names []string) ([]inspection, error) {
	tool, err := p.engine(engine)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, tool, append([]string{"inspect", "--type", "container"}, names...)...)
	var out, complaint bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &complaint
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("plain: %s inspect: %w%s", engine, err, saidOnStderr(complaint.String()))
	}
	var described []inspection
	if err := json.Unmarshal(out.Bytes(), &described); err != nil {
		return nil, fmt.Errorf("plain: %s inspect: %w", engine, err)
	}
	return described, nil
}

// inspection is what a container says about itself, as much as a backup
// needs: where its data is, whether it is running, and which compose
// files made it.
type inspection struct {
	Name   string `json:"Name"`
	State  struct{ Running bool }
	Config struct {
		Image  string
		Labels map[string]string
	}
	Mounts []struct {
		Type        string
		Name        string
		Source      string
		Destination string
	}
	raw []byte
}

func (p *Provider) inspect(ctx context.Context, engine, name string) (inspection, error) {
	tool, err := p.engine(engine)
	if err != nil {
		return inspection{}, err
	}
	cmd := exec.CommandContext(ctx, tool, "inspect", "--type", "container", name)
	var out, complaint bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &complaint
	if err := cmd.Run(); err != nil {
		return inspection{}, fmt.Errorf("plain: %s inspect %s: %w%s", engine, name, err, saidOnStderr(complaint.String()))
	}
	var described []inspection
	if err := json.Unmarshal(out.Bytes(), &described); err != nil {
		return inspection{}, fmt.Errorf("plain: %s inspect %s: %w", engine, name, err)
	}
	if len(described) == 0 {
		return inspection{}, fmt.Errorf("plain: %s knows no container called %s", engine, name)
	}
	described[0].raw = out.Bytes()
	return described[0], nil
}

// stack is the compose project the container belongs to and the
// directory its compose file lives in, from the labels docker compose
// and podman-compose both write. Empty for a container run by hand.
func (i inspection) stack() (project, dir string) {
	for key, value := range i.Config.Labels {
		if strings.HasSuffix(key, "compose.project") && value != "" {
			project = value
			dir = i.Config.Labels[key+".working_dir"]
		}
	}
	if dir != "" {
		if stat, err := os.Stat(dir); err != nil || !stat.IsDir() {
			dir = ""
		}
	}
	return project, dir
}

// composeFiles are the compose files a container's labels name, that
// are there to be read. docker compose and podman-compose both write
// the com.docker.compose labels; podman's own quadlets write none.
func (i inspection) composeFiles() []string {
	var files []string
	for key, value := range i.Config.Labels {
		if !strings.HasSuffix(key, "compose.project.config_files") {
			continue
		}
		for _, file := range strings.Split(value, ",") {
			file = strings.TrimSpace(file)
			if file == "" {
				continue
			}
			if !filepath.IsAbs(file) {
				if dir, ok := i.Config.Labels[strings.TrimSuffix(key, "config_files")+"working_dir"]; ok {
					file = filepath.Join(dir, file)
				}
			}
			if stat, err := os.Stat(file); err == nil && stat.Mode().IsRegular() {
				files = append(files, file)
			}
		}
	}
	sort.Strings(files)
	return files
}

// AddContainer makes a source of each of the container's mounts -- its
// named volumes and its bind mounts, read where they lie on the host --
// and one of its description alone when it has none.
func (p *Provider) AddContainer(ctx context.Context, engine, name string) ([]panel.Source, error) {
	if p.Catalog == nil {
		return nil, ErrNoCatalog
	}
	described, err := p.inspect(ctx, engine, name)
	if err != nil {
		return nil, err
	}
	container := strings.TrimPrefix(described.Name, "/")
	if container == "" {
		container = name
	}
	existing, err := p.Sources(ctx)
	if err != nil {
		return nil, err
	}
	for _, other := range existing {
		if other.Container != nil && other.Container.Engine == engine && other.Container.Name == container {
			return nil, fmt.Errorf("%s is already chosen, as %s", container, other.Name)
		}
	}
	taken := map[string]bool{}
	for _, other := range existing {
		taken[other.Name] = true
	}
	var made []panel.Source
	now := time.Now().UTC()
	for _, mount := range described.Mounts {
		if mount.Source == "" {
			continue
		}
		if stat, err := os.Stat(mount.Source); err != nil || !stat.IsDir() {
			p.log().Warn("a mount of the container is not a directory on this host; it is left out",
				"container", container, "mount", mount.Destination, "source", mount.Source)
			continue
		}
		base := sanitizeName(container + "-" + filepath.Base(mount.Destination))
		sourceName := base
		for n := 2; taken[sourceName]; n++ {
			sourceName = fmt.Sprintf("%s-%d", base, n)
		}
		taken[sourceName] = true
		made = append(made, panel.Source{
			Name: sourceName, Path: filepath.Clean(mount.Source),
			Container: &panel.ContainerRef{Engine: engine, Name: container, Mount: mount.Destination},
			AddedAt:   now,
		})
	}
	if len(made) == 0 {
		sourceName := sanitizeName(container)
		for n := 2; taken[sourceName]; n++ {
			sourceName = fmt.Sprintf("%s-%d", sanitizeName(container), n)
		}
		made = append(made, panel.Source{
			Name:      sourceName,
			Container: &panel.ContainerRef{Engine: engine, Name: container},
			AddedAt:   now,
		})
	}
	for _, source := range made {
		if err := p.Catalog.SaveSource(source); err != nil {
			return made, fmt.Errorf("plain: keep the choice: %w", err)
		}
	}
	p.log().Info("a container was chosen", "engine", engine, "container", container, "sources", len(made))
	return made, nil
}

// described is what describeContainer found out.
type described struct {
	composeFiles []string
	running      bool
}

// describeContainer writes the container's own description and its
// compose files beside the record, so a machine that has to run it
// again knows how it was run.
func (p *Provider) describeContainer(ctx context.Context, source panel.Source, recordDir string) (described, error) {
	found, err := p.inspect(ctx, source.Container.Engine, source.Container.Name)
	if err != nil {
		return described{}, err
	}
	if err := os.WriteFile(filepath.Join(recordDir, "container.json"), found.raw, 0o600); err != nil {
		return described{}, fmt.Errorf("plain: write the container's description: %w", err)
	}
	result := described{running: found.State.Running}
	for _, file := range found.composeFiles() {
		into := filepath.Join(recordDir, "compose", filepath.Base(file))
		if err := os.MkdirAll(filepath.Dir(into), 0o700); err != nil {
			return described{}, err
		}
		if err := copyFile(file, into, 0o600); err != nil {
			return described{}, fmt.Errorf("plain: copy %s: %w", file, err)
		}
		result.composeFiles = append(result.composeFiles, file)
	}
	return result, nil
}
