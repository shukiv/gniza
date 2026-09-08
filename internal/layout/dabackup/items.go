package dabackup

import (
	"fmt"
	"path"
	"strings"

	"github.com/shukiv/gniza/internal/panel"
)

// Members inside a DirectAdmin account backup.
//
// Every path here is one this package has seen in a real archive, and
// testdata holds the listing that proves it. What DirectAdmin's
// documentation had suggested was wrong in almost every particular, which
// is the reason the listing is checked in beside the tables rather than
// summarised in a comment.
//
// The shape it settles is that DirectAdmin keeps an account's per-domain
// records with the domain: backup/<domain>/ holds that domain's mail
// configuration, its FTP logins, its zone file and its own settings.
// Only what belongs to the account as a whole -- who it is, its cron, its
// database dumps -- sits directly in backup/.
//
// A plan asks for what it wants and the extraction reports what it
// actually found, so a member named here that an archive does not carry
// costs an operator a restore that comes back short. What is worse than
// short is broad: backup/ contains the account's password hash and every
// database dump, so a selection that cannot be expressed exactly is
// refused rather than widened to its nearest containing directory.
var (
	// The account itself: who it is, what it may use, what it has used,
	// and the password hash that lets it log in.
	settingsMembers = []string{
		path.Join(BackupDir, UserConf),
		path.Join(BackupDir, "user.usage"),
		path.Join(BackupDir, "user.db"),
		path.Join(BackupDir, ".shadow"),
		path.Join(BackupDir, "ticket.conf"),
		path.Join(BackupDir, "backup_options.list"),
	}
	cronMembers = []string{path.Join(BackupDir, CrontabConf)}
)

// SettingsMembers is DirectAdmin's own record of the account.
func (Layout) SettingsMembers() []string { return settingsMembers }

// CronMembers is the account's scheduled jobs.
func (Layout) CronMembers() []string { return cronMembers }

// FTPMembers is its FTP logins, and returns none.
//
// DirectAdmin keeps them per domain, at backup/<domain>/ftp.conf and
// backup/<domain>/ftp.passwd, and this method is not told which domains
// the account has. The containing directory holds that domain's mail and
// zone as well, so returning it would hand over far more than FTP.
//
// Returning nothing makes the restore refuse -- the caller reports that
// the backup held nothing for the FTP accounts -- which is the outcome
// this should have until the layout interface can be asked for the FTP of
// a named domain. ADR 0019 question 9 says so.
func (Layout) FTPMembers() []string { return nil }

// MailMembers is the mail configuration that is not the messages, and
// returns none, for the same reason FTPMembers does: it lives at
// backup/<domain>/email/ and this method is not told the domains.
//
// The consequence is narrower than it sounds. Restoring a mailbox still
// brings back its messages, which is what a mailbox restore is for; what
// it does not bring back is that domain's forwarders and autoresponders.
func (Layout) MailMembers() []string { return nil }

// DNSMembers is the zone of each domain named. DirectAdmin writes it
// beside the domain's other records, as backup/<domain>/<domain>.db.
func (Layout) DNSMembers(names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf(
			"dabackup: DirectAdmin keeps each domain's zone with the domain, " +
				"so name the domains whose DNS you want")
	}
	members := make([]string, 0, len(names))
	for _, name := range names {
		if err := panel.UsableDomainName(name); err != nil {
			return nil, err
		}
		members = append(members, path.Join(BackupDir, name, name+".db"))
	}
	return members, nil
}

// SSLMembers is the account's certificates, and refuses.
//
// The archive this layout was checked against belongs to an account with
// no certificate on it, so where DirectAdmin puts one is still unknown.
// Guessing would produce a restore that reports success and hands back
// nothing, which for a certificate is a site that is still down after the
// restore somebody ran to bring it back.
func (Layout) SSLMembers(names []string) ([]string, error) {
	return nil, fmt.Errorf(
		"dabackup: where DirectAdmin keeps a certificate has not been " +
			"established: see ADR 0019")
}

// DomainMembers is the configuration of each domain named: what it is,
// which address it answers on, and the subdomains under it.
func (Layout) DomainMembers(names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf(
			"dabackup: DirectAdmin keeps each domain's settings with the " +
				"domain, so name the domains you want")
	}
	members := make([]string, 0, 4*len(names))
	for _, name := range names {
		if err := panel.UsableDomainName(name); err != nil {
			return nil, err
		}
		members = append(members,
			path.Join(BackupDir, name, "domain.conf"),
			path.Join(BackupDir, name, "domain.ip_list"),
			path.Join(BackupDir, name, "subdomain.list"),
			path.Join(BackupDir, name, "domain.mime.types"))
	}
	return members, nil
}

// WebsitePaths is the document root, relative to the home directory.
//
// DirectAdmin gives each domain its own, under domains/<domain>, so an
// account with two domains has two and this cannot be a constant path to
// one of them. The caller asks for a domain by name when it means one of
// them; with no name it means the lot.
func (Layout) WebsitePaths() []string { return []string{DomainsDir} }

// MailboxPaths is where DirectAdmin keeps the messages of a mailbox:
// under imap/<domain>/<mailbox> in the account's own home directory.
//
// A name reaches this in whichever shape the interface offered it --
// sales@example.com, example.com/sales, or example.com for a whole
// domain's mail -- and all three name the same place here. The caller has
// already refused anything that is not one of them.
func (Layout) MailboxPaths(names []string) []string {
	paths := make([]string, 0, len(names))
	for _, name := range names {
		domain, mailbox := splitMailbox(name)
		if mailbox == "" {
			paths = append(paths, path.Join("imap", domain))
			continue
		}
		paths = append(paths, path.Join("imap", domain, mailbox))
	}
	return paths
}

// splitMailbox reads a mailbox name into the domain it belongs to and the
// mailbox inside it, which is empty when the name is a whole domain.
func splitMailbox(name string) (domain, mailbox string) {
	name = strings.TrimSpace(name)
	if mailbox, domain, found := strings.Cut(name, "@"); found {
		return domain, mailbox
	}
	if domain, mailbox, found := strings.Cut(name, "/"); found {
		return domain, mailbox
	}
	return name, ""
}

// Layout is a complete panel layout, and one whose archive a rehearsal
// can read inside.
var (
	_ panel.Layout       = Layout{}
	_ panel.ArchiveDrill = Layout{}
)
