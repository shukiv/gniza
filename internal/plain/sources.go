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

// Candidates is what could be chosen now: the directories under the
// roots that are not a source yet, the databases each client can see,
// and the containers each engine knows about.
func (p *Provider) Candidates(ctx context.Context) (panel.Candidates, error) {
	sources, err := p.Sources(ctx)
	if err != nil {
		return panel.Candidates{}, err
	}
	chosenPath := map[string]bool{}
	chosenContainer := map[string]bool{}
	for _, source := range sources {
		if source.Path != "" {
			chosenPath[filepath.Clean(source.Path)] = true
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
			if !chosenPath[path] {
				found.Folders = append(found.Folders, path)
			}
		}
	}
	if sizes, err := p.databaseSizes(ctx); err != nil {
		found.MySQLError = err.Error()
	} else {
		for name := range sizes {
			found.MySQL = append(found.MySQL, name)
		}
		sort.Strings(found.MySQL)
	}
	if sizes, err := p.postgresSizes(ctx); err != nil {
		found.PostgreSQLError = err.Error()
	} else {
		for name := range sizes {
			found.PostgreSQL = append(found.PostgreSQL, name)
		}
		sort.Strings(found.PostgreSQL)
	}
	for _, engine := range []string{"docker", "podman"} {
		containers, err := p.containers(ctx, engine)
		if err != nil {
			p.log().Debug("no containers listed", "engine", engine, "error", err)
			continue
		}
		for i := range containers {
			containers[i].Chosen = chosenContainer[engine+"/"+containers[i].Name]
		}
		found.Containers = append(found.Containers, containers...)
	}
	return found, nil
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
// first database.
func suggestName(source panel.Source) string {
	switch {
	case source.Path != "":
		return sanitizeName(filepath.Base(source.Path))
	case len(source.MySQL) > 0:
		return sanitizeName(source.MySQL[0])
	case len(source.PostgreSQL) > 0:
		return sanitizeName(source.PostgreSQL[0])
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

// postgresSizes lists every PostgreSQL database with the bytes it holds.
// The templates are not anybody's.
func (p *Provider) postgresSizes(ctx context.Context) (map[string]uint64, error) {
	if _, err := exec.LookPath(p.psql()); err != nil {
		return nil, fmt.Errorf("plain: no psql on this server")
	}
	cmd := p.pgCommand(ctx, p.psql(), "-At", "-F", "\t", "-d", "postgres", "-c",
		"SELECT datname, pg_database_size(datname) FROM pg_database WHERE NOT datistemplate")
	var out, complaint bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &complaint
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("plain: psql: %w%s", err, saidOnStderr(complaint.String()))
	}
	sizes := map[string]uint64{}
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
	}
	return sizes, nil
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

// containers lists what one engine knows about, running or not.
func (p *Provider) containers(ctx context.Context, engine string) ([]panel.ContainerCandidate, error) {
	tool, err := p.engine(engine)
	if err != nil {
		return nil, err
	}
	if _, err := exec.LookPath(tool); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, tool, "ps", "-a", "--format", "{{.Names}}\t{{.Image}}\t{{.Status}}")
	var out, complaint bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &complaint
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("plain: %s ps: %w%s", engine, err, saidOnStderr(complaint.String()))
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
	return found, nil
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
