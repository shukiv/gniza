package cpanel

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/shukiv/gniza/internal/granular"
	"github.com/shukiv/gniza/internal/layout/cpmove"
	"github.com/shukiv/gniza/internal/panel"
	"github.com/shukiv/gniza/internal/pkgacct"
)

// Fake builds a synthetic account tree with the same shape the real
// provider produces.
//
// It exists so the agent, the controller and restic can be exercised end to
// end on a machine with no cPanel installation. It is not a test double
// living in a _test.go file because the E2E harness and the agent's
// development mode both need it.
type Fake struct {
	// Root is where synthetic home directories are created.
	Root string
	// Caps controls which pkgacct flags the fake host claims to support,
	// so degraded-mode behaviour can be exercised too.
	Caps pkgacct.Capabilities
	// Databases maps a user to the databases that account owns.
	Databases map[string][]string
	// FileCount and FileSize shape the generated home directory.
	FileCount int
	FileSize  int
	// Applied records the archives Apply was called with.
	Applied []string
	// AppliedWith records the options each of those was applied under.
	AppliedWith []panel.ApplyOptions
	// AppliedDirectory records, for each of those, whether what cPanel
	// was handed was a directory at the moment it was handed over. A test
	// cannot ask afterwards: staging is swept as soon as the restore
	// finishes, so by then the path is gone either way.
	AppliedDirectory []bool
	// Excludes is what NativeExcludes returns, for a test that needs the
	// account to have some.
	Excludes []string
	// PutBackHome and LoadedDatabases record what was written back into
	// live accounts, for a test that needs to know a restore did more than
	// leave a copy behind.
	PutBackHome      []PutBackHome
	CreatedDatabases []CreatedDatabase
	LoadedDatabases  []LoadedDatabase
	RestoredDBUsers  []RestoredDBUsers
	// PutBackCrontabs records the cron jobs written into live accounts.
	PutBackCrontabs []PutBackCrontab
	// DBUserOwners is which account each database user belongs to on this
	// synthetic server. A user nobody claims is one this account may
	// recreate, which is the case a restore of a deleted user is.
	DBUserOwners map[string]string
	// RefuseCreate makes CreateDatabase fail with this reason, the way
	// cPanel refuses one when the account's database quota is reached.
	RefuseCreate string
	// RefuseLoadInto names a database whose load fails, for the case a
	// restore has already overwritten the ones before it. cPanel gives no
	// way to undo those, so what the caller does about the failure is
	// what matters.
	RefuseLoadInto string
	// ApplyOutput is what restorepkg is made to print. cPanel's restore
	// exits zero with a module failed, so the transcript is the only
	// thing that says so.
	ApplyOutput string
	// Gone marks accounts this server does not have, for the case where
	// cPanel says it restored one and it is not there afterwards.
	Gone map[string]bool
}

// PutBackHome is one home-directory tree written back into an account.
type PutBackHome struct {
	User string
	From string
}

// CreatedDatabase is one database made for a restore to load into.
type CreatedDatabase struct {
	User     string
	Database string
}

// LoadedDatabase is one dump loaded into a live database.
type LoadedDatabase struct {
	User     string
	Database string
	DumpPath string
}

// PutBackCrontab is one account's cron jobs written back.
type PutBackCrontab struct {
	User string
	Body string
}

// RestoredDBUsers is one set of database users written back into an account.
type RestoredDBUsers struct {
	User  string
	Users []panel.DatabaseUser
}

var _ panel.Provider = (*Fake)(nil)

// DefaultFakeCaps is a modern cPanel: every flag we care about is present.
var DefaultFakeCaps = pkgacct.Capabilities{
	NoCompressFlag:     "--nocompress",
	SkipHomedirFlag:    "--skiphomedir",
	SkipDBFlag:         "--skipdb",
	SkipMailFlag:       "--skipmail",
	SkipMailConfigFlag: "--skipmailconfig",
}

func (f *Fake) Capabilities(context.Context) (pkgacct.Capabilities, error) {
	if f.Caps == (pkgacct.Capabilities{}) {
		return DefaultFakeCaps, nil
	}
	return f.Caps, nil
}

// Accounts lists the synthetic accounts, creating their home directories on
// first sight. Like the real provider it reports names and home directories
// only; sizes and databases cost too much to gather for a listing.
func (f *Fake) Accounts(_ context.Context) ([]panel.AccountInfo, error) {
	names := make([]string, 0, len(f.Databases))
	for user := range f.Databases {
		names = append(names, user)
	}
	sort.Strings(names)

	accounts := make([]panel.AccountInfo, 0, len(names))
	for _, user := range names {
		home := filepath.Join(f.Root, "home", user)
		if err := f.populateHome(home, user); err != nil {
			return nil, err
		}
		accounts = append(accounts, panel.AccountInfo{User: user, HomeDir: home})
	}
	return accounts, nil
}

// Account creates the account's home directory if it does not exist yet and
// reports its contents.
func (f *Fake) Account(_ context.Context, user string) (panel.AccountInfo, error) {
	if err := validateUser(user); err != nil {
		return panel.AccountInfo{}, err
	}
	if f.Gone[user] {
		return panel.AccountInfo{}, fmt.Errorf("cpanel: account home for %s: no such directory", user)
	}
	home := filepath.Join(f.Root, "home", user)
	if err := f.populateHome(home, user); err != nil {
		return panel.AccountInfo{}, err
	}
	size, err := directorySize(home)
	if err != nil {
		return panel.AccountInfo{}, err
	}
	return panel.AccountInfo{
		User:      user,
		HomeDir:   home,
		Databases: f.Databases[user],
		SizeBytes: size,
	}, nil
}

// Stage writes a metadata archive and per-database dumps, mirroring what
// pkgacct and mysqldump would leave behind.
func (f *Fake) Stage(ctx context.Context, req panel.StageRequest) (pkgacct.Payload, error) {
	caps, err := f.Capabilities(ctx)
	if err != nil {
		return pkgacct.Payload{}, err
	}
	payload, err := pkgacct.Plan(pkgacct.PlanRequest{
		Account:       req.Account.User,
		HomeDir:       req.Account.HomeDir,
		Databases:     req.Account.Databases,
		StagingDir:    req.StagingDir,
		Mode:          req.Mode,
		Caps:          caps,
		SkipHomedir:   req.SkipHomedir,
		SkipDatabases: req.SkipDatabases,
	})
	if err != nil {
		return pkgacct.Payload{}, err
	}

	for _, part := range payload.Parts {
		switch part.Kind {
		case pkgacct.PartHomedir:
			// Already on disk.
		case pkgacct.PartMetadata:
			// pkgacct writes an archive into the directory it is given, so
			// the fake does too: restore has to find and unpack it.
			if err := writeMetadataArchive(part.Path, req.Account.User); err != nil {
				return pkgacct.Payload{}, err
			}
		case pkgacct.PartDatabase:
			if err := os.MkdirAll(part.Path, 0o700); err != nil {
				return pkgacct.Payload{}, fmt.Errorf("cpanel: create %s: %w", part.Path, err)
			}
			for name, path := range payload.DumpPaths {
				if err := writeFile(path, sqlDumpFor(name)); err != nil {
					return pkgacct.Payload{}, err
				}
			}
			// The users that own those databases are staged beside them,
			// as the real provider does.
			users := filepath.Join(part.Path, granular.DatabaseUsersFile)
			if err := writeFile(users, []byte("-- database users for "+req.Account.User+"\n")); err != nil {
				return pkgacct.Payload{}, err
			}
		case pkgacct.PartArchive:
			if err := writeMonolithicArchive(part.Path, req.Account.User); err != nil {
				return pkgacct.Payload{}, err
			}
		}
	}
	return payload, nil
}

// Apply records that an archive would have been handed to cPanel.
//
// The fake cannot restore an account, so it verifies the archive exists and
// writes a marker the caller can assert on. A test that needs to know
// restorepkg was invoked checks Applied.
// NativeExcludes has none: a synthetic host has no cPanel configuration
// to read them from.
func (f *Fake) NativeExcludes(string) []string { return f.Excludes }

// Apply stands in for restorepkg, which takes an account archive or the
// extracted account directory -- its usage lists both, and it copies
// whichever it is given into a temporary directory of its own. A
// directory is checked for the cpuser file restorepkg reads to know whose
// account it is, because a directory without one is what restorepkg
// refuses.
func (f *Fake) Apply(_ context.Context, archivePath string, options panel.ApplyOptions) (string, error) {
	info, err := os.Stat(archivePath)
	if err != nil {
		return "", fmt.Errorf("cpanel: restore archive: %w", err)
	}
	if info.IsDir() {
		if _, err := os.Stat(filepath.Join(archivePath, "meta", "user")); err != nil {
			return "", fmt.Errorf(
				"cpanel: %s is not an extracted account: no cpanel user file in it", archivePath)
		}
	}
	f.AppliedWith = append(f.AppliedWith, options)
	f.Applied = append(f.Applied, archivePath)
	f.AppliedDirectory = append(f.AppliedDirectory, info.IsDir())
	return f.ApplyOutput, nil
}

// PutHomeDir records that a restored tree would have been copied into an
// account's home directory. The fake has no separate account to become, so
// it checks the tree is there and remembers the call.
func (f *Fake) PutHomeDir(_ context.Context, user, from string) error {
	info, err := os.Stat(from)
	if err != nil {
		return fmt.Errorf("cpanel: restored home directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("cpanel: %s is not a directory", from)
	}
	f.PutBackHome = append(f.PutBackHome, PutBackHome{User: user, From: from})
	return nil
}

// CreateDatabase records that a database would have been made, and adds it
// to what the account owns, so a load that follows finds it there.
func (f *Fake) CreateDatabase(_ context.Context, user, database string) error {
	if f.RefuseCreate != "" {
		return fmt.Errorf("cpanel: create the database %s: %s", database, f.RefuseCreate)
	}
	if f.owns(user, database) {
		return fmt.Errorf("cpanel: %s already has the database %s", user, database)
	}
	if f.Databases == nil {
		f.Databases = map[string][]string{}
	}
	f.Databases[user] = append(f.Databases[user], database)
	f.CreatedDatabases = append(f.CreatedDatabases, CreatedDatabase{
		User: user, Database: database,
	})
	return nil
}

// LoadDatabase records that a dump would have been loaded into a live
// database. It makes the same ownership check the real provider makes, so a
// caller that fails to confine a request to the account's own databases
// fails here too.
func (f *Fake) LoadDatabase(_ context.Context, user, database, dumpPath string) error {
	if f.RefuseLoadInto != "" && f.RefuseLoadInto == database {
		return fmt.Errorf("cpanel: load %s: the dump failed halfway through", database)
	}
	if !f.owns(user, database) {
		return fmt.Errorf("cpanel: %s does not own the database %s", user, database)
	}
	info, err := os.Stat(dumpPath)
	if err != nil {
		return fmt.Errorf("cpanel: database dump: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("cpanel: database dump %s is a directory", dumpPath)
	}
	f.LoadedDatabases = append(f.LoadedDatabases, LoadedDatabase{
		User: user, Database: database, DumpPath: dumpPath,
	})
	return nil
}

// PutCrontab records that an account's cron jobs would have been replaced,
// and keeps what the file said, so a test can check the whole crontab went
// rather than a line of it.
func (f *Fake) PutCrontab(_ context.Context, user, from string) error {
	body, err := os.ReadFile(from)
	if err != nil {
		return fmt.Errorf("cpanel: cron jobs from the backup: %w", err)
	}
	f.PutBackCrontabs = append(f.PutBackCrontabs, PutBackCrontab{
		User: user, Body: string(body),
	})
	return nil
}

// PutDatabaseUsers records that database users would have been recreated.
// It makes the same two checks the real provider makes -- that no other
// account holds the user, and that every database granted is this
// account's -- so a caller that fails to confine a request fails here too.
func (f *Fake) PutDatabaseUsers(_ context.Context, user string, users []panel.DatabaseUser) error {
	if len(users) == 0 {
		return errors.New("cpanel: no database user was named to restore")
	}
	for _, account := range users {
		if owner, known := f.DBUserOwners[account.Name]; known && owner != user {
			return fmt.Errorf("cpanel: the database user %s belongs to %s, not to %s",
				account.Name, owner, user)
		}
		for _, grant := range account.Grants {
			if !f.owns(user, grant.Database) {
				return fmt.Errorf("cpanel: %s does not own the database %s",
					user, grant.Database)
			}
		}
	}
	f.RestoredDBUsers = append(f.RestoredDBUsers, RestoredDBUsers{User: user, Users: users})
	return nil
}

func (f *Fake) owns(user, database string) bool {
	for _, owned := range f.Databases[user] {
		if owned == database {
			return true
		}
	}
	return false
}

func (f *Fake) Certify(ctx context.Context, archivePath, disposableUser string) error {
	_, err := f.Apply(ctx, archivePath, panel.ApplyOptions{NewUser: disposableUser, SkipDNS: true})
	return err
}

// cpmoveRoot is the top-level directory name inside a cPanel account
// archive. Reassembly discovers this name rather than assuming it; the
// fake picks cPanel's conventional one so the discovery is exercised.
func cpmoveRoot(account string) string { return "cpmove-" + account }

// writeMetadataArchive produces what pkgacct leaves in its output
// directory when the home directory and databases are excluded: one
// archive holding the account's configuration.
func writeMetadataArchive(dir, account string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("cpanel: create %s: %w", dir, err)
	}
	root := cpmoveRoot(account)
	return writeTar(filepath.Join(dir, root+".tar"), map[string][]byte{
		root + "/version":             []byte("6\n"),
		root + "/meta/user":           []byte(account + "\n"),
		root + "/cp/" + account:       []byte("DNS=" + account + ".example\nUSER=" + account + "\n"),
		root + "/userdata/main":       []byte("main_domain: " + account + ".example\n"),
		root + "/dnszones/" + account: []byte("; fake zone for " + account + "\n"),
	})
}

// writeMonolithicArchive produces the single-archive payload: the same
// configuration plus the home directory, the way pkgacct would.
func writeMonolithicArchive(path, account string) error {
	root := cpmoveRoot(account)
	return writeTar(path, map[string][]byte{
		root + "/version":                        []byte("6\n"),
		root + "/meta/user":                      []byte(account + "\n"),
		root + "/cp/" + account:                  []byte("USER=" + account + "\n"),
		root + "/homedir/public_html/index.html": []byte("<h1>" + account + "</h1>\n"),
	})
}

// writeTar writes an uncompressed archive with the given contents, creating
// the directory entries each path implies.
func writeTar(path string, files map[string][]byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("cpanel: create %s: %w", filepath.Dir(path), err)
	}
	out, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("cpanel: create %s: %w", path, err)
	}
	defer out.Close()

	writer := tar.NewWriter(out)
	for _, dir := range tarDirectories(files) {
		if err := writer.WriteHeader(&tar.Header{
			Name: dir + "/", Typeflag: tar.TypeDir, Mode: 0o755,
		}); err != nil {
			return fmt.Errorf("cpanel: write %s: %w", dir, err)
		}
	}

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		body := files[name]
		if err := writer.WriteHeader(&tar.Header{
			Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)),
		}); err != nil {
			return fmt.Errorf("cpanel: write %s: %w", name, err)
		}
		if _, err := writer.Write(body); err != nil {
			return fmt.Errorf("cpanel: write %s: %w", name, err)
		}
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("cpanel: finish %s: %w", path, err)
	}
	return nil
}

// tarDirectories returns every directory the file paths imply, parents
// first, so the archive is well-formed.
func tarDirectories(files map[string][]byte) []string {
	seen := map[string]bool{}
	for name := range files {
		parts := strings.Split(name, "/")
		for i := 1; i < len(parts); i++ {
			seen[strings.Join(parts[:i], "/")] = true
		}
	}
	dirs := make([]string, 0, len(seen))
	for dir := range seen {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs
}

// populateHome creates a deterministic-per-user tree the first time an
// account is seen, so repeated backups deduplicate the way real ones do.
func (f *Fake) populateHome(home, user string) error {
	if _, err := os.Stat(home); err == nil {
		return nil
	}
	count := f.FileCount
	if count <= 0 {
		count = 12
	}
	size := f.FileSize
	if size <= 0 {
		size = 4096
	}

	for _, dir := range []string{"public_html", "mail", ".cpanel"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o755); err != nil {
			return fmt.Errorf("cpanel: create fake home: %w", err)
		}
	}
	// Seeded by user name so two runs produce identical bytes.
	source := rand.New(rand.NewSource(int64(len(user)) * 7919))
	for i := range count {
		body := make([]byte, size)
		if _, err := source.Read(body); err != nil {
			return fmt.Errorf("cpanel: generate fake content: %w", err)
		}
		path := filepath.Join(home, "public_html", fmt.Sprintf("page-%02d.html", i))
		if err := writeFile(path, body); err != nil {
			return err
		}
	}
	return nil
}

func sqlDumpFor(name string) []byte {
	return []byte(fmt.Sprintf(
		"-- Gniza fake dump of %s\nCREATE TABLE posts (id int, body text);\n"+
			"INSERT INTO posts VALUES (1, 'hello');\n", name))
}

func writeFile(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("cpanel: create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return fmt.Errorf("cpanel: write %s: %w", path, err)
	}
	return nil
}

// StageSystem writes a small stand-in for the server's own configuration,
// so the pipeline that backs it up can be tested without a cPanel.
func (f *Fake) StageSystem(_ context.Context, stagingDir string) (pkgacct.Payload, error) {
	root := filepath.Join(stagingDir, "system")
	if err := os.MkdirAll(filepath.Join(root, "files", "etc"), 0o700); err != nil {
		return pkgacct.Payload{}, err
	}
	for name, body := range map[string]string{
		filepath.Join(root, "files", "etc", "wwwacct.conf"): "HOST fake.example.com\n",
		filepath.Join(root, "ea4-profile.json"):             `{"os":"fake","pkgs":["ea-apache24"]}` + "\n",
		filepath.Join(root, "manifest.txt"):                 "# Gniza system backup\npaths_copied\t1\n",
	} {
		if err := writeFile(name, []byte(body)); err != nil {
			return pkgacct.Payload{}, err
		}
	}
	payload := pkgacct.Payload{
		Mode:    pkgacct.ModeSystem,
		Account: SystemAccount,
		Parts:   []pkgacct.Part{{Kind: pkgacct.PartSystem, Path: root}},
	}
	return payload, payload.Verify()
}

// Layout is the same cpmove tree the real provider produces: the fake
// exists to exercise the machinery, not to invent a second format.
func (f *Fake) Name() string { return "cPanel" }

func (f *Fake) Layout() panel.Layout { return cpmove.Layout{} }
