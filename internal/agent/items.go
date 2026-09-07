package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/shukiv/gniza/internal/granular"
	"github.com/shukiv/gniza/internal/inventory"
	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/panel"
	"github.com/shukiv/gniza/internal/protocol"
	"github.com/shukiv/gniza/internal/reassemble"
	"github.com/shukiv/gniza/internal/resticrun"
	"github.com/shukiv/gniza/internal/staging"
)

// restoreItems takes one part of an account out of a snapshot: a mailbox,
// a database, the DNS records, the certificates.
//
// The result is a directory and an archive of it, left on this server to
// collect. When the request asks for it, and only for the parts that can be
// written back -- files, the website, mail, a database, the database users
// -- what came out is then written into the live account. The rest stay a
// copy, not because putting them back is impossible but because each needs
// a change the control panel has to make -- a zone edit, an installed
// certificate, an FTP login -- and none of those is built yet.
func (a *Agent) restoreItems(ctx context.Context, log *slog.Logger,
	assignment protocol.RestoreAssignment, repo resticrun.Repository,
	dir *staging.Dir, report protocol.RestoreReport,
	watch *restoreWatch) (protocol.RestoreReport, bool) {

	snapshot, err := reassemble.FindSnapshot(ctx, a.runner, reassemble.Request{
		Account:    assignment.CPanelUser,
		SnapshotID: assignment.SnapshotID,
		WorkDir:    dir.Path,
		Repo:       repo,
	})
	if err != nil {
		report.Error = err.Error()
		return report, false
	}
	parts, err := reassemble.Classify(snapshot.Paths)
	if err != nil {
		report.Error = err.Error()
		return report, false
	}
	plan, err := granular.BuildAll(parts, itemRequests(assignment))
	if err != nil {
		report.Error = err.Error()
		return report, false
	}

	raw := filepath.Join(dir.Path, "raw")
	watch.Stage("reading " + plan.Description + " out of the backup")
	restored, err := a.runner.Restore(ctx, repo, resticrun.RestoreSpec{
		SnapshotID: snapshot.ID,
		Target:     raw,
		Include:    plan.Include,
		OnProgress: watch.ProgressFunc(),
	})
	if err != nil {
		log.Error("restore items", "error", err, "include", plan.Include)
		report.Error = err.Error()
		return report, false
	}
	if restored.FilesRestored == 0 {
		report.Error = fmt.Sprintf(
			"agent: nothing in this backup matched %s", plan.Description)
		return report, false
	}

	out := filepath.Join(dir.Path, "items")
	if err := os.MkdirAll(out, 0o700); err != nil {
		report.Error = fmt.Sprintf("agent: create the restore tree: %v", err)
		return report, false
	}

	// What came out of the home directory and the database dumps keeps the
	// shape it had, under names that say where it belongs.
	if parts.Homedir != "" {
		if err := adopt(filepath.Join(raw, parts.Homedir), filepath.Join(out, "homedir")); err != nil {
			report.Error = err.Error()
			return report, false
		}
	}
	if parts.Databases != "" {
		if err := adopt(filepath.Join(raw, parts.Databases), filepath.Join(out, "databases")); err != nil {
			report.Error = err.Error()
			return report, false
		}
	}
	if parts.System != "" {
		if err := adopt(filepath.Join(raw, parts.System), filepath.Join(out, "system")); err != nil {
			report.Error = err.Error()
			return report, false
		}
	}

	// Configuration lives inside the account's metadata archive, so only
	// the members that were asked for are taken out of it.
	if len(plan.Members) > 0 {
		archive, err := soleArchive(filepath.Join(raw, plan.Metadata))
		if err != nil {
			report.Error = err.Error()
			return report, false
		}
		written, err := reassemble.ExtractMembers(archive, filepath.Join(out, "metadata"), plan.Members)
		if err != nil {
			report.Error = err.Error()
			return report, false
		}
		log.Info("took configuration out of the account archive",
			"files", written, "members", plan.Members)
	}

	files, bytes, err := measure(out)
	if err != nil {
		report.Error = err.Error()
		return report, false
	}
	if files == 0 {
		// restic restored something, but none of it was what the operator
		// asked for. Reporting success here would hand over an empty
		// directory as though it were their data.
		report.Error = fmt.Sprintf(
			"agent: this backup holds nothing for %s", plan.Description)
		return report, false
	}

	if assignment.Apply {
		// Written into the live account rather than handed over. The raw
		// restic tree is not wanted either way.
		if err := os.RemoveAll(raw); err != nil {
			log.Warn("remove the raw restore tree", "error", err)
		}
		watch.Stage("putting " + plan.Description + " back into the account")
		written, hint, err := a.applyItems(ctx, log, assignment, out)
		if err != nil {
			report.Error = err.Error()
			report.Hint = hint
			// A restore has no transaction: some of it may already be in
			// the live account. Saying only that it failed leaves the
			// operator to find out from the customer, so what was written
			// is reported alongside the failure and the job is marked as
			// having changed the account.
			report.Detail = written
			report.Applied = written != ""
			if written != "" {
				log.Warn("a granular restore failed after changing the account",
					"account", assignment.CPanelUser, "wrote", written, "error", err)
			}
			return report, false
		}
		report.Status = string(job.StatusSuccess)
		report.BytesRestored = bytes
		report.Applied = true
		report.Detail = written
		log.Warn("granular restore written into the live account",
			"account", assignment.CPanelUser, "parts", itemsLabel(assignment),
			"files", files, "wrote", written)
		return report, false
	}

	archivePath := filepath.Join(dir.Path, fmt.Sprintf("items-%s-%s.tar",
		assignment.CPanelUser, itemsLabel(assignment)))
	watch.Stage("packing up " + plan.Description)
	if err := reassemble.PackDir(out, archivePath); err != nil {
		report.Error = err.Error()
		return report, false
	}

	// The raw restic tree has served its purpose and would otherwise
	// double the space this restore holds.
	if err := os.RemoveAll(raw); err != nil {
		log.Warn("remove the raw restore tree", "error", err)
	}

	retained, err := a.staging.Retain(dir)
	if err != nil {
		log.Error("retain the restored items", "error", err)
		report.Error = err.Error()
		return report, false
	}

	report.Status = string(job.StatusSuccess)
	report.BytesRestored = bytes
	report.ArchivePath = filepath.Join(retained.Path, filepath.Base(archivePath))
	report.RestoredTo = filepath.Join(retained.Path, "items")
	report.Detail = fmt.Sprintf("%d files of %s", files, plan.Description)
	log.Info("granular restore ready to collect",
		"parts", itemsLabel(assignment), "files", files, "archive", report.ArchivePath)
	return report, true
}

// adopt moves a restored subtree to where the operator will look for it.
// A missing source is not an error: a plan may ask for parts a particular
// request does not use.
func adopt(from, to string) error {
	if _, err := os.Stat(from); err != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		return fmt.Errorf("agent: create %s: %w", filepath.Dir(to), err)
	}
	if err := os.Rename(from, to); err == nil {
		return nil
	}
	// Staging and the restore tree are the same volume in practice, but a
	// rename across devices fails and a copy still has to work.
	return copyTree(from, to)
}

func copyTree(from, to string) error {
	return filepath.Walk(from, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		switch {
		case info.IsDir():
			return os.MkdirAll(target, 0o700)
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case !info.Mode().IsRegular():
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		source, err := os.Open(path)
		if err != nil {
			return err
		}
		defer source.Close()
		sink, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(sink, source)
		closeErr := sink.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}

// soleArchive finds the account archive inside a restored metadata part.
// pkgacct names it after the account, so it is discovered rather than
// assumed.
func soleArchive(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("agent: read the restored metadata: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".tar") {
			return filepath.Join(dir, entry.Name()), nil
		}
	}
	return "", fmt.Errorf("agent: the metadata in this backup holds no account archive")
}

// measure counts what a restore actually produced.
func measure(dir string) (files int, bytes uint64, err error) {
	err = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			files++
			bytes += uint64(info.Size())
		}
		return nil
	})
	if err != nil {
		return 0, 0, fmt.Errorf("agent: measure the restored files: %w", err)
	}
	return files, bytes, nil
}

// itemRequests is what this restore asks for, as granular reads it.
func itemRequests(assignment protocol.RestoreAssignment) []granular.Request {
	selections := assignment.Selections()
	requests := make([]granular.Request, 0, len(selections))
	for _, selection := range selections {
		requests = append(requests, granular.Request{
			Kind:    granular.Kind(selection.Kind),
			Account: assignment.CPanelUser,
			Names:   selection.Names,
		})
	}
	return requests
}

// itemsLabel names this restore in a log line and in the archive left to
// collect. One part of an account is named; several are counted, because
// the parts of a basket spelt out in a filename would run past what a
// filename can hold.
func itemsLabel(assignment protocol.RestoreAssignment) string {
	selections := assignment.Selections()
	switch len(selections) {
	case 0:
		return "nothing"
	case 1:
		return selections[0].Kind
	}
	return fmt.Sprintf("%dparts", len(selections))
}

// applyItems writes what came out of the backup into the live account, and
// says what it wrote.
//
// Only the kinds granular says can be applied reach here; the node refuses
// the others before a job exists. The check is made again because this runs
// as root on a live account, and a second reading of the same rule costs
// nothing next to what getting it wrong costs.
//
// Everything is checked before anything is written. A basket holding a
// database and its users, where the users would be refused, must not leave
// the database replaced and its users missing -- the account would come out
// of the restore worse off than it went in.
func (a *Agent) applyItems(ctx context.Context, log *slog.Logger,
	assignment protocol.RestoreAssignment, out string) (written, hint string, err error) {

	selections := assignment.Selections()
	if len(selections) == 0 {
		return "", "", errors.New(
			"agent: this restore names no part of the account to write back")
	}

	var (
		databaseNames, userNames                    []string
		wantDumps, wantUsers, wantHomedir, wantCron bool
	)
	for _, selection := range selections {
		kind := granular.Kind(selection.Kind)
		if !kind.CanApply() {
			return "", "", fmt.Errorf(
				"agent: a %s restore cannot be written into the live account", kind)
		}
		switch kind {
		case granular.KindDatabase:
			wantDumps = true
			databaseNames = append(databaseNames, selection.Names...)
		case granular.KindDBUsers:
			wantUsers = true
			userNames = append(userNames, selection.Names...)
		case granular.KindCron:
			// Never by name. The account's jobs are lines in one file,
			// and the page says so: what comes back is the file.
			wantCron = true
		default:
			// Files, the website and mail are all the home directory,
			// restored where they were. A basket asking for two of them
			// asks for one tree, and writes it once.
			wantHomedir = true
		}
	}

	databases := filepath.Join(out, "databases")
	homedir := filepath.Join(out, "homedir")

	// Which databases the account has now. A dump has to load into a
	// database that is there, and a grant has to be given on one, so both
	// checks read the same list.
	var present map[string]bool
	if wantDumps || wantUsers {
		account, err := a.provider.Account(ctx, assignment.CPanelUser)
		if err != nil {
			return "", "", err
		}
		present = make(map[string]bool, len(account.Databases))
		for _, name := range account.Databases {
			present[name] = true
		}
	}

	var dumps, create []string
	if wantDumps {
		if dumps, create, hint, err = checkDatabaseDumps(
			databaseNames, present, databases); err != nil {
			return "", hint, err
		}
		// A database this restore is about to make counts as the
		// account's for what follows: a user granted access to it is
		// asking for the database beside it in the same basket, not for
		// one nobody is restoring.
		for _, name := range create {
			present[name] = true
		}
	}
	var users []panel.DatabaseUser
	if wantUsers {
		if users, hint, err = checkDatabaseUsers(
			assignment.CPanelUser, userNames, present, databases); err != nil {
			return "", hint, err
		}
	}
	if wantHomedir {
		if _, err := os.Stat(homedir); err != nil {
			return "", "This backup does not contain the account's files.", errors.New(
				"agent: this backup holds none of the account's files")
		}
	}
	crontab := ""
	if wantCron {
		if crontab, err = stagedCrontab(out, assignment.CPanelUser); err != nil {
			// An account with no cron jobs has no file in its archive.
			// Writing an empty crontab here would read as a restore and
			// be a deletion: whatever is running now would stop.
			return "", "This backup holds no cron jobs for the account. Nothing " +
				"has been changed.", err
		}
	}

	// Written in the order the account needs: a database exists before a
	// dump goes into it, and a grant is given on a database that is
	// already there.
	//
	// There is no transaction across these. cPanel has no way to undo a
	// loaded database, and a home directory written over cannot be put
	// back. So what has already happened is carried out of every failure
	// path from here on: a restore that overwrote one database and failed
	// on the next must not report only "failed" -- the account has been
	// changed, and whoever reads that report is the person who has to
	// decide what to do about it.
	//
	// The hint carries it too. The error and the detail are read by the
	// operator; the hint is the only line the customer is shown, and the
	// customer is the person whose databases were overwritten. Telling
	// them nothing but "failed" is the same non-disclosure one audience
	// over. What is listed here is their own account's names, so there is
	// nothing in it they are not entitled to read.
	var wrote []string
	partly := func(hint string, err error) (string, string, error) {
		if len(wrote) == 0 {
			return "", hint, err
		}
		changed := strings.Join(wrote, "; ")
		if hint != "" {
			hint += " "
		}
		hint += "Part of this was put back before it failed: " + changed +
			". Check the account before asking for it again."
		return changed, hint, fmt.Errorf(
			"%w -- the account has already been changed: %s", err, changed)
	}
	for _, name := range create {
		if err := a.provider.CreateDatabase(ctx, assignment.CPanelUser, name); err != nil {
			return partly(createFailureHint(name, err), err)
		}
		log.Warn("database created for a restore",
			"account", assignment.CPanelUser, "database", name)
		wrote = append(wrote, "created "+name)
	}
	for i, name := range databaseNames {
		if err := a.provider.LoadDatabase(ctx, assignment.CPanelUser, name, dumps[i]); err != nil {
			return partly("", fmt.Errorf("agent: load %s: %w", name, err))
		}
		log.Warn("database loaded from a backup",
			"account", assignment.CPanelUser, "database", name)
		wrote = append(wrote, "loaded into "+name)
	}
	if wantUsers {
		if err := a.provider.PutDatabaseUsers(ctx, assignment.CPanelUser, users); err != nil {
			return partly("", err)
		}
		names := distinctUserNames(users)
		log.Warn("database users recreated from a backup",
			"account", assignment.CPanelUser, "users", strings.Join(names, ","))
		wrote = append(wrote, "recreated "+strings.Join(names, ", "))
	}
	if wantHomedir {
		if err := a.provider.PutHomeDir(ctx, assignment.CPanelUser, homedir); err != nil {
			return partly("", err)
		}
		wrote = append(wrote, "written into the home directory of "+assignment.CPanelUser)
	}
	if wantCron {
		if err := a.provider.PutCrontab(ctx, assignment.CPanelUser, crontab); err != nil {
			return partly("", err)
		}
		log.Warn("cron jobs put back from a backup", "account", assignment.CPanelUser)
		wrote = append(wrote, "cron jobs of "+assignment.CPanelUser)
	}
	return strings.Join(wrote, "; "), "", nil
}

// stagedCrontab finds the account's cron jobs in a restored tree.
//
// They come out of the account's own archive, which keeps its own top-level
// directory when members are taken out of it -- cpmove-<account>, usually,
// but that is the archive's name for itself and not something to depend on.
func stagedCrontab(out, user string) (string, error) {
	root := filepath.Join(out, "metadata")
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", fmt.Errorf("agent: this backup holds no cron jobs for %s", user)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(root, entry.Name(), "cron", user)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		return path, nil
	}
	return "", fmt.Errorf("agent: this backup holds no cron jobs for %s", user)
}

// createFailureHint says why a database could not be made, in cPanel's own
// words where they can be repeated.
//
// cPanel refuses for reasons the customer can act on -- the plan allows no
// more databases, the name is one somebody else holds -- and repeating what
// it said beats "ask your host". What comes back from a command that failed
// rather than refused is a program's diagnostic, so anything carrying a
// path is left out: a customer is not shown where this server keeps things.
func createFailureHint(name string, err error) string {
	reason := ""
	if err != nil {
		if at := strings.LastIndex(err.Error(), ": "); at >= 0 {
			reason = strings.TrimSpace(err.Error()[at+2:])
		}
	}
	if reason == "" || strings.Contains(reason, "/") {
		return fmt.Sprintf(
			"The database %s is not on the account any more, and it could not be "+
				"made again. Your host can say why.", name)
	}
	return fmt.Sprintf(
		"The database %s is not on the account any more, and it could not be "+
			"made again: %s", name, reason)
}

// checkDatabaseDumps finds the dump for each named database, and says which
// of them the account does not have.
//
// A database somebody dropped is the case this feature exists for, so a
// name the account no longer has is not a failure: it is one to make before
// the dump goes into it. Whether the account may have it is cPanel's
// answer, given when the database is created as the account -- its quota
// and its name prefix, applied by the panel that owns those rules.
func checkDatabaseDumps(names []string, present map[string]bool,
	databases string) (dumps, create []string, hint string, err error) {

	if len(names) == 0 {
		return nil, nil, "", errors.New("agent: no database was named to restore")
	}
	for _, name := range names {
		dump := filepath.Join(databases, name+".sql")
		if _, err := os.Stat(dump); err != nil {
			return nil, nil, fmt.Sprintf(
					"This backup holds no copy of the database %s. Try an earlier "+
						"restore point.", name),
				fmt.Errorf("agent: this backup holds no dump of the database %s", name)
		}
		dumps = append(dumps, dump)
		if !present[name] {
			create = append(create, name)
		}
	}
	return dumps, create, "", nil
}

// checkDatabaseUsers reads the account's database users out of the backup
// and makes sure every database they were granted access to is still there.
//
// The users are read out of the staged files here rather than in the
// privileged provider: what runs there runs as root against the server's
// MySQL, and it takes checked values rather than a file whose contents
// nobody has looked at.
func checkDatabaseUsers(account string, wanted []string, present map[string]bool,
	databases string) (users []panel.DatabaseUser, hint string, err error) {

	users, err = readStagedDatabaseUsers(databases)
	if err != nil {
		// A more recent restore point, not an earlier one. What a backup
		// can be missing here is the file that carries the stored
		// passwords, which Gniza has not always written -- so going
		// further back is going further from having it.
		return nil, "This backup does not hold the account's database users. Try a " +
			"more recent restore point, or download this one and ask your host.", err
	}
	if len(users) == 0 {
		return nil, "This backup holds no database users for the account.", errors.New(
			"agent: this backup holds no database users")
	}

	// Named users, when some were chosen. A name is one login on several
	// hosts, and all of its hosts come back: a grant put back against
	// some of them is an application that connects and is refused.
	if len(wanted) > 0 {
		var chosen []panel.DatabaseUser
		for _, user := range users {
			if contains(wanted, user.Name) {
				chosen = append(chosen, user)
			}
		}
		var absent []string
		for _, name := range wanted {
			if !contains(distinctUserNames(chosen), name) {
				absent = append(absent, name)
			}
		}
		if len(absent) > 0 {
			sort.Strings(absent)
			called := "This backup holds no database user called %s. Choose one " +
				"of the users it does hold, or try another restore point."
			if len(absent) > 1 {
				called = "This backup holds no database users called %s. Choose " +
					"from the users it does hold, or try another restore point."
			}
			return nil, fmt.Sprintf(called, granular.JoinAnd(absent)), fmt.Errorf(
				"agent: this backup holds no database user(s) %s",
				strings.Join(absent, ", "))
		}
		users = chosen
	}

	// A grant on a database that is neither on the account nor in this
	// restore. The database beside it in a basket is made before this
	// runs, so what is left here is a grant on something nobody is
	// restoring -- and an empty database made to hold a grant would be a
	// database the customer did not ask for.
	var missing []string
	for _, user := range users {
		for _, grant := range user.Grants {
			if !present[grant.Database] && !contains(missing, grant.Database) {
				missing = append(missing, grant.Database)
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		had := "These users had access to %s, which the account does not have " +
			"any more. Add that database to this restore, or create it first: a " +
			"grant cannot be given on a database that is not there."
		if len(missing) > 1 {
			had = "These users had access to %s, which the account does not have " +
				"any more. Add those databases to this restore, or create them " +
				"first: a grant cannot be given on a database that is not there."
		}
		return nil, fmt.Sprintf(had, granular.JoinAnd(missing)), fmt.Errorf(
			"agent: %s no longer has the database(s) %s",
			account, strings.Join(missing, ", "))
	}
	return users, "", nil
}

// distinctUserNames names each user once, however many hosts it exists on.
// Naming it once per host is what the operator's log and the customer's
// page would otherwise both say.
func distinctUserNames(users []panel.DatabaseUser) []string {
	var names []string
	seen := map[string]bool{}
	for _, user := range users {
		if !seen[user.Name] {
			seen[user.Name] = true
			names = append(names, user.Name)
		}
	}
	sort.Strings(names)
	return names
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// readStagedDatabaseUsers reads the users, their stored passwords and their
// grants out of a restored databases directory.
func readStagedDatabaseUsers(databases string) ([]panel.DatabaseUser, error) {
	raw, err := os.ReadFile(filepath.Join(databases, granular.DatabaseUsersAuthFile))
	if err != nil {
		return nil, fmt.Errorf("agent: read the database users in this backup: %w", err)
	}
	auth, err := inventory.ParseAuth(raw)
	if err != nil {
		return nil, err
	}
	raw, err = os.ReadFile(filepath.Join(databases, granular.RunnableDatabaseUsersFile))
	if err != nil {
		return nil, fmt.Errorf("agent: read the database grants in this backup: %w", err)
	}
	grants, err := inventory.ParseGrants(raw)
	if err != nil {
		return nil, err
	}

	var users []panel.DatabaseUser
	for name, hosts := range auth {
		for host, credentials := range hosts {
			user := panel.DatabaseUser{
				Name:   name,
				Host:   host,
				Plugin: credentials.Plugin,
				Hash:   credentials.Hash,
			}
			for _, grant := range grants[name+"@"+host] {
				user.Grants = append(user.Grants, panel.DatabaseGrant{
					Database:   grant.Database,
					Privileges: grant.Privileges,
				})
			}
			users = append(users, user)
		}
	}
	sort.Slice(users, func(i, j int) bool {
		if users[i].Name != users[j].Name {
			return users[i].Name < users[j].Name
		}
		return users[i].Host < users[j].Host
	})
	return users, nil
}
