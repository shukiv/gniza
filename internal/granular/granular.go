// Package granular works out what has to come out of a snapshot to restore
// one thing rather than a whole account.
//
// A restore of a single mailbox, one database or an account's DNS records
// is the common case in practice: something was deleted this morning and
// everything else on the account is fine. Rebuilding and reapplying the
// whole account to fix it would replace far more than was lost.
//
// Nothing here runs restic or touches a disk. It turns a request into the
// paths a snapshot holds, so the mapping can be tested against known
// snapshot layouts rather than against a live cPanel.
package granular

import (
	"fmt"
	"path"
	"strings"

	"github.com/shukiv/gniza/internal/panel"
	"github.com/shukiv/gniza/internal/reassemble"
)

// Kind names a thing an operator restores on its own.
type Kind string

const (
	// KindFiles restores named paths from the home directory.
	KindFiles Kind = "files"
	// KindWebsite restores the document root.
	KindWebsite Kind = "website"
	// KindMailbox restores one mailbox, or a whole domain's mail.
	KindMailbox Kind = "mailbox"
	// KindDatabase restores one database dump.
	KindDatabase Kind = "database"
	// KindDNS restores the account's zone files.
	KindDNS Kind = "dns"
	// KindSSL restores its certificates and keys.
	KindSSL Kind = "ssl"
	// KindSettings restores the cPanel configuration of the account.
	KindSettings Kind = "settings"
	// KindCron restores the account's cron jobs.
	KindCron Kind = "cron"
	// KindDomains restores its domains and their web server configuration.
	KindDomains Kind = "domains"
	// KindFTP restores its FTP accounts.
	KindFTP Kind = "ftp"
	// KindDBUsers restores the database users and their grants. A database
	// without the user that owns it is a database no site can open.
	KindDBUsers Kind = "dbusers"
	// KindSystem restores the server's own configuration from a system
	// backup: EasyApache, the tweak settings, packages, service
	// configuration.
	KindSystem Kind = "system"
)

// Kinds is every kind, in the order the interface offers them.
var Kinds = []Kind{
	KindFiles, KindWebsite, KindMailbox, KindDatabase, KindDBUsers,
	KindDNS, KindDomains, KindSSL, KindCron, KindFTP, KindSettings,
}

// Title is the kind as the interface names it.
func (k Kind) Title() string {
	switch k {
	case KindFiles:
		return "Files or folders"
	case KindWebsite:
		return "Website files"
	case KindMailbox:
		return "A mailbox"
	case KindDatabase:
		return "A database"
	case KindDNS:
		return "DNS records"
	case KindSSL:
		return "SSL certificates"
	case KindSettings:
		return "Panel configuration"
	case KindCron:
		return "Cron jobs"
	case KindDomains:
		return "Domains"
	case KindFTP:
		return "FTP accounts"
	case KindDBUsers:
		return "Database users"
	case KindSystem:
		return "Server settings"
	}
	return string(k)
}

// NeedsNames reports whether a kind is meaningless without the operator
// naming what they want.
// CanApply reports whether a restore of this kind can be written back into
// a live account, rather than only handed over as a copy.
//
// The ones that can are where the backup holds everything needed to make
// the account whole again: files land in the home directory, a dump loads
// into the database it came from, a database user is recreated from the
// hash the backup carries, an account's cron jobs are the file cron reads.
// The rest are not refused because putting them back is impossible -- it is
// that each needs the control panel to make a change of its own, and that
// is not built yet. A DNS zone, an installed certificate, an FTP login and
// the account's own configuration are all a copy their host puts back
// until it is.
func (k Kind) CanApply() bool {
	switch k {
	case KindFiles, KindWebsite, KindMailbox, KindDatabase, KindDBUsers, KindCron:
		return true
	}
	return false
}

func (k Kind) NeedsNames() bool {
	switch k {
	case KindFiles, KindMailbox, KindDatabase:
		return true
	}
	return false
}

// Request is one granular restore.
type Request struct {
	Kind    Kind
	Account string
	// Names are the paths, mailboxes or databases asked for. Which of
	// those it means depends on Kind, and some kinds need none.
	Names []string
}

// Plan is everything that has to come out of a snapshot to satisfy a
// request.
type Plan struct {
	// Include are snapshot paths handed to restic.
	Include []string
	// Members are prefixes inside the account's metadata archive to keep.
	// Empty when the request needs nothing from it.
	Members []string
	// Metadata is the snapshot path of the metadata part, set only when
	// Members is non-empty.
	Metadata string
	// Describes the restore in the words the operator chose it by.
	Description string
}

// Build turns a request into the paths that satisfy it.
//
// It fails rather than returning an empty plan: a granular restore that
// quietly asks for nothing would report success having produced nothing,
// which is the failure this program most has to avoid.
func Build(layout panel.ItemLayout, parts reassemble.Parts, req Request) (Plan, error) {
	if layout == nil {
		return Plan{}, fmt.Errorf("granular: the panel's layout is required")
	}
	if req.Account == "" {
		return Plan{}, fmt.Errorf("granular: account is required")
	}
	if req.Kind.NeedsNames() && len(req.Names) == 0 {
		return Plan{}, fmt.Errorf("granular: %s needs at least one name", req.Kind)
	}

	switch req.Kind {
	case KindFiles:
		return buildFiles(parts, req)
	case KindWebsite:
		return buildHomedir(parts, req, layout.WebsitePaths(), "the website files")
	case KindMailbox:
		return buildMailbox(layout, parts, req)
	case KindDatabase:
		return buildDatabase(parts, req)
	case KindDNS:
		return buildChosen(parts, req, layout.DNSMembers, "the DNS records")
	case KindSSL:
		return buildChosen(parts, req, layout.SSLMembers, "the SSL certificates and keys")
	case KindSettings:
		return buildMetadata(parts, layout.SettingsMembers(), "the panel configuration")
	case KindCron:
		return buildMetadata(parts, layout.CronMembers(), "the cron jobs")
	case KindDomains:
		return buildChosen(parts, req, layout.DomainMembers, "the domains")
	case KindFTP:
		return buildMetadata(parts, layout.FTPMembers(), "the FTP accounts")
	case KindDBUsers:
		return buildDatabaseUsers(parts, req.Names)
	case KindSystem:
		if parts.System == "" {
			return Plan{}, fmt.Errorf(
				"granular: this is a backup of an account, not of the server's own settings")
		}
		return Plan{
			Include:     []string{parts.System},
			Description: "the server's settings",
		}, nil
	default:
		return Plan{}, fmt.Errorf("granular: unknown restore kind %q", req.Kind)
	}
}

// BuildAll turns several requests into the one set of paths that satisfies
// all of them.
//
// The parts of an account depend on each other: a database nothing can
// open, or a database user with no database to open, is what two separate
// restores produce when the second one fails after the first has been
// written. Restoring them together makes that impossible. Every request is
// built before any of it is used, so a basket holding one thing this cannot
// do fails whole rather than half way through.
func BuildAll(layout panel.ItemLayout, parts reassemble.Parts, reqs []Request) (Plan, error) {
	if len(reqs) == 0 {
		return Plan{}, fmt.Errorf("granular: nothing was asked for")
	}
	if len(reqs) == 1 {
		return Build(layout, parts, reqs[0])
	}

	var (
		merged       Plan
		descriptions []string
	)
	for _, req := range reqs {
		plan, err := Build(layout, parts, req)
		if err != nil {
			return Plan{}, err
		}
		merged.Include = addNew(merged.Include, plan.Include)
		merged.Members = addNew(merged.Members, plan.Members)
		if plan.Metadata != "" {
			merged.Metadata = plan.Metadata
		}
		descriptions = append(descriptions, plan.Description)
	}
	merged.Description = JoinAnd(descriptions)
	return merged, nil
}

// addNew appends the values not already there, keeping the order they were
// first asked for. restic and the archive extraction both take the same
// path twice without complaint, but a plan that says a thing twice is read
// by a person too.
func addNew(have, more []string) []string {
	for _, value := range more {
		found := false
		for _, existing := range have {
			if existing == value {
				found = true
				break
			}
		}
		if !found {
			have = append(have, value)
		}
	}
	return have
}

// JoinAnd lists things the way somebody reading the result would say them.
func JoinAnd(values []string) string {
	switch len(values) {
	case 0:
		return ""
	case 1:
		return values[0]
	case 2:
		return values[0] + " and " + values[1]
	}
	return strings.Join(values[:len(values)-1], ", ") + " and " + values[len(values)-1]
}

func buildFiles(parts reassemble.Parts, req Request) (Plan, error) {
	if parts.Homedir == "" {
		return Plan{}, fmt.Errorf("granular: this snapshot has no home directory in it")
	}
	plan := Plan{Description: "the named files"}
	for _, name := range req.Names {
		full, err := underHome(parts.Homedir, name)
		if err != nil {
			return Plan{}, err
		}
		plan.Include = append(plan.Include, full)
	}
	return plan, nil
}

func buildHomedir(parts reassemble.Parts, req Request, relative []string, description string) (Plan, error) {
	if parts.Homedir == "" {
		return Plan{}, fmt.Errorf("granular: this snapshot has no home directory in it")
	}
	plan := Plan{Description: description}
	for _, name := range relative {
		full, err := underHome(parts.Homedir, name)
		if err != nil {
			return Plan{}, err
		}
		plan.Include = append(plan.Include, full)
	}
	return plan, nil
}

func buildMailbox(layout panel.ItemLayout, parts reassemble.Parts, req Request) (Plan, error) {
	for _, name := range req.Names {
		if err := usableMailboxName(name); err != nil {
			return Plan{}, err
		}
	}
	plan, err := buildHomedir(parts, req, layout.MailboxPaths(req.Names),
		"the mail for "+strings.Join(req.Names, ", "))
	if err != nil {
		return Plan{}, err
	}
	// Forwarders and filters are configuration, not maildir, so they only
	// come back if the metadata part is in the snapshot too -- and only
	// if this panel's layout can name where they are inside it. A panel
	// that keeps them somewhere the layout cannot ask for leaves nothing
	// to take out, and restoring the account's whole configuration to
	// take nothing out of it is time and scratch space a customer waiting
	// for one mailbox pays for.
	if members := layout.MailMembers(); parts.Metadata != "" && len(members) > 0 {
		plan.Metadata = parts.Metadata
		plan.Members = members
		plan.Include = append(plan.Include, parts.Metadata)
	}
	return plan, nil
}

func buildDatabase(parts reassemble.Parts, req Request) (Plan, error) {
	if parts.Databases == "" {
		return Plan{}, fmt.Errorf(
			"granular: this snapshot holds no separate database dumps, " +
				"so a single database cannot be taken out of it")
	}
	plan := Plan{Description: "the database " + strings.Join(req.Names, ", ")}
	for _, name := range req.Names {
		if err := plainName(name); err != nil {
			return Plan{}, err
		}
		plan.Include = append(plan.Include, path.Join(parts.Databases, name+".sql"))
	}
	return plan, nil
}

// DatabaseUsersFile is where the account's database users and their
// grants are staged, inside the same directory as the dumps so a
// snapshot's paths do not change when an account gains or loses one.
const DatabaseUsersFile = "_users.sql"

// RunnableDatabaseUsersFile holds the same users written so a person can
// run them.
//
// DatabaseUsersFile is cPanel's format because cPanel's restore is what
// reads it, and that format uses GRANT ... IDENTIFIED BY PASSWORD, which
// MySQL 8 removed. Handing somebody who asked for their database users a
// file their database will not accept is not giving them their database
// users.
const RunnableDatabaseUsersFile = "_users-runnable.sql"

// DatabaseUsersAuthFile is where the hash and the authentication plugin of
// each user are staged, as cPanel stages them: beside the grants, named
// after DatabaseUsersFile.
//
// It exists because the grants cannot carry the password on a current
// MySQL. Restoring a user from the SQL alone would recreate the login and
// not what authenticates it, which is a user nothing can connect as.
const DatabaseUsersAuthFile = DatabaseUsersFile + "-auth.json"

func buildDatabaseUsers(parts reassemble.Parts, names []string) (Plan, error) {
	if parts.Databases == "" {
		return Plan{}, fmt.Errorf(
			"granular: this backup holds no databases, so it holds no database users either")
	}
	for _, name := range names {
		if err := UsableDatabaseUserName(name); err != nil {
			return Plan{}, err
		}
	}
	return Plan{
		Include: []string{
			path.Join(parts.Databases, DatabaseUsersFile),
			path.Join(parts.Databases, RunnableDatabaseUsersFile),
			path.Join(parts.Databases, DatabaseUsersAuthFile),
		},
		Description: "the database users and their grants" + forNames(names),
	}, nil
}

// buildChosen is a metadata restore narrowed to the items chosen, or the
// whole of that part of the account when none were.
func buildChosen(parts reassemble.Parts, req Request,
	members func([]string) ([]string, error), description string) (Plan, error) {

	chosen, err := members(req.Names)
	if err != nil {
		return Plan{}, err
	}
	return buildMetadata(parts, chosen, description+forNames(req.Names))
}

// forNames says which items a description covers, when it covers some of
// them rather than all.
func forNames(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return " for " + JoinAnd(names)
}

func buildMetadata(parts reassemble.Parts, members []string, description string) (Plan, error) {
	if parts.Metadata == "" {
		return Plan{}, fmt.Errorf(
			"granular: this snapshot has no account metadata in it, "+
				"so %s cannot be taken out of it", description)
	}
	return Plan{
		Include:     []string{parts.Metadata},
		Members:     members,
		Metadata:    parts.Metadata,
		Description: description,
	}, nil
}

// underHome resolves a name inside the account's home directory.
//
// An absolute path is accepted only if it is already inside that home
// directory: every account on a cPanel server is a different customer, and
// a restore that could be steered at /home/someone-else would hand one
// customer's files to another.
func underHome(home, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("granular: empty path")
	}
	full := name
	if !strings.HasPrefix(name, "/") {
		full = path.Join(home, name)
	}
	full = path.Clean(full)
	if full != home && !strings.HasPrefix(full, home+"/") {
		return "", fmt.Errorf("granular: %s is not inside %s", name, home)
	}
	return full, nil
}

// usableMailboxName rejects anything that is not a mailbox or a domain.
//
// The shape a mailbox is named in is the panel's: cPanel keeps them as
// <domain>/<mailbox> under mail/, DirectAdmin as <mailbox>@<domain>, and
// a whole domain's mail is named by the domain alone. What all of them
// need is that the name is a name -- one domain, and at most one mailbox
// inside it.
//
// "../" is the case that made this necessary. Joined under mail/ it
// resolves to the home directory itself, which is inside the account and
// so passed every check there was -- and a request to restore one mailbox
// put back every file the account owns.
func usableMailboxName(name string) error {
	refuse := fmt.Errorf("granular: %q is not a mailbox", name)
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return fmt.Errorf("granular: empty mailbox name")
	}
	if strings.ContainsAny(trimmed, "\\\x00") || strings.Contains(trimmed, "..") {
		return refuse
	}
	if strings.Count(trimmed, "@") > 1 || strings.Count(trimmed, "/") > 1 {
		return refuse
	}
	// An "@" with nothing on one side of it is not an address. On
	// DirectAdmin it splits into an empty domain, and the path built from
	// it is imap/ itself.
	if before, after, found := strings.Cut(trimmed, "@"); found &&
		(strings.TrimSpace(before) == "" || strings.TrimSpace(after) == "") {
		return refuse
	}
	for _, element := range strings.Split(trimmed, "/") {
		element = strings.TrimSpace(element)
		// "." names the directory the mailboxes are in rather than a
		// mailbox in it: mail/. is mail/, and imap/. is imap/. It is the
		// same failure as "../", one level shallower -- a request to
		// restore one mailbox that puts back every mailbox the account
		// has, and with Apply set overwrites all of them.
		if element == "" || element == "." {
			return refuse
		}
	}
	return nil
}

// plainName rejects anything that is not simply a name, so a database
// cannot be spelt as a path.
func plainName(name string) error {
	if name == "" {
		return fmt.Errorf("granular: empty name")
	}
	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return fmt.Errorf("granular: %q is not a database name", name)
	}
	return nil
}

// UsableDatabaseName refuses a name that would be read as something other
// than a database.
//
// Nothing that takes one of these goes through a shell, so the hazard is
// not a metacharacter: it is a name beginning with a dash, which a command
// line tool reads as an option, or one carrying a path separator, which
// would take a dump from somewhere other than where the restore put it.
// Names come from a form and from a backup, so both are checked.
func UsableDatabaseName(database string) error {
	if database == "" {
		return fmt.Errorf("granular: no database named")
	}
	if strings.HasPrefix(database, "-") || len(database) > 64 {
		return fmt.Errorf("granular: %q is not a usable database name", database)
	}
	for _, char := range database {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z',
			char >= '0' && char <= '9', char == '_', char == '$':
		default:
			return fmt.Errorf("granular: %q is not a usable database name", database)
		}
	}
	return nil
}

// UsableDomainName refuses anything that is not a domain name. The check
// itself belongs with the panel layouts that build paths out of one, and
// this is the name the rest of the program already calls.
func UsableDomainName(domain string) error {
	if err := panel.UsableDomainName(domain); err != nil {
		return fmt.Errorf("granular: %w", err)
	}
	return nil
}

// UsableDatabaseUserName refuses anything that is not one.
//
// A MySQL user name is not a database name -- it is capped at 32 characters
// rather than 64 -- but what may be in one is the same, and for the same
// reason: the name reaches a statement this program writes out itself.
func UsableDatabaseUserName(user string) error {
	if user == "" || len(user) > 32 {
		return fmt.Errorf("granular: %q is not a usable database user name", user)
	}
	if err := UsableDatabaseName(user); err != nil {
		return fmt.Errorf("granular: %q is not a usable database user name", user)
	}
	return nil
}

// ListsItems reports whether a backup can be asked what this part of the
// account holds, one item at a time.
//
// Files, mail and the databases are listed by restic itself: they are files
// and directories in the snapshot. The rest are inside a single archive or
// a single SQL file, and listing them means reading that container --
// which is worth doing, because a page that says only "your DNS records"
// cannot tell somebody whether the zone they lost is in this backup.
func (k Kind) ListsItems() bool {
	switch k {
	case KindDBUsers, KindDNS, KindSSL, KindDomains, KindCron, KindFTP:
		return true
	}
	return k.NeedsNames()
}

// PicksItems reports whether a restore of this kind can be narrowed to the
// items chosen rather than only showing them.
//
// Cron jobs and FTP logins are lines inside one file. Taking some lines out
// of it would hand back a file that is not the one the backup holds, so
// they are listed to be read and restored together.
func (k Kind) PicksItems() bool {
	switch k {
	case KindCron, KindFTP:
		return false
	}
	return k.ListsItems()
}
