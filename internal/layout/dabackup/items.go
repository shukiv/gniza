package dabackup

import (
	"path"
	"strings"

	"github.com/shukiv/gniza/internal/panel"
)

// Members inside a DirectAdmin account backup.
//
// Everything here is assumed unless it names a file DirectAdmin's own
// documentation names. What is documented is the per-account
// configuration directory and its contents -- user.conf, domains.list,
// crontab.conf, user_ip.list, httpd.conf, nginx.conf and a domains
// subdirectory -- and a backup archive carries that directory under
// backup/. Which of those a single-item restore should take, and what
// else lives beside them, is what a real host has to say.
//
// A plan asks for what it wants and the extraction reports what it
// actually found, so a member named here that a real archive does not
// carry costs an operator a restore that comes back short rather than one
// that fails -- which is the reason each of these is written down rather
// than guessed at the point of use.
var (
	settingsMembers = []string{
		path.Join(BackupDir, UserConf),
		path.Join(BackupDir, "user_ip.list"),
		path.Join(BackupDir, "packages.list"),
	}
	cronMembers = []string{path.Join(BackupDir, CrontabConf)}
	ftpMembers  = []string{path.Join(BackupDir, "ftp.conf"), path.Join(BackupDir, "proftpd.passwd")}
	// Mail that is not the messages themselves: the addresses, the
	// forwarders, the filters and the autoresponders.
	mailMembers = []string{
		path.Join(BackupDir, "email"),
		path.Join(BackupDir, "forwarders"),
		path.Join(BackupDir, "filter"),
		path.Join(BackupDir, "vacation.conf"),
	}
)

// SettingsMembers is DirectAdmin's own record of the account.
func (Layout) SettingsMembers() []string { return settingsMembers }

// CronMembers is the account's scheduled jobs.
func (Layout) CronMembers() []string { return cronMembers }

// FTPMembers is its FTP logins.
func (Layout) FTPMembers() []string { return ftpMembers }

// MailMembers is the mail configuration that is not the messages.
func (Layout) MailMembers() []string { return mailMembers }

// DNSMembers is the account's zone files, or the zones of the domains
// named.
func (Layout) DNSMembers(names []string) ([]string, error) {
	if len(names) == 0 {
		return []string{path.Join(BackupDir, "dns")}, nil
	}
	members := make([]string, 0, len(names))
	for _, name := range names {
		if err := panel.UsableDomainName(name); err != nil {
			return nil, err
		}
		members = append(members, path.Join(BackupDir, "dns", name+".db"))
	}
	return members, nil
}

// SSLMembers is the account's certificates, or those of the domains
// named. DirectAdmin keeps a domain's certificate with the domain, so a
// per-domain choice reaches into the domains directory rather than into a
// directory of its own.
func (Layout) SSLMembers(names []string) ([]string, error) {
	if len(names) == 0 {
		return []string{path.Join(BackupDir, "domains")}, nil
	}
	members := make([]string, 0, len(names))
	for _, name := range names {
		if err := panel.UsableDomainName(name); err != nil {
			return nil, err
		}
		members = append(members,
			path.Join(BackupDir, "domains", name+".cert"),
			path.Join(BackupDir, "domains", name+".key"),
			path.Join(BackupDir, "domains", name+".cacert"))
	}
	return members, nil
}

// DomainMembers is the configuration of the account's domains, or of the
// domains named.
func (Layout) DomainMembers(names []string) ([]string, error) {
	if len(names) == 0 {
		return []string{path.Join(BackupDir, "domains"), path.Join(BackupDir, DomainsList)}, nil
	}
	members := make([]string, 0, 2*len(names)+1)
	for _, name := range names {
		if err := panel.UsableDomainName(name); err != nil {
			return nil, err
		}
		members = append(members,
			path.Join(BackupDir, "domains", name+".conf"),
			path.Join(BackupDir, "domains", name+".subdomains"))
	}
	// Which domain is the main one is a property of the account rather
	// than of any one domain, and a domain restored without it has
	// nowhere to be put back.
	return append(members, path.Join(BackupDir, DomainsList)), nil
}

// WebsitePaths is the document root, relative to the home directory.
//
// DirectAdmin gives each domain its own, so an account with two domains
// has two, and this is the one path here that cannot be a constant. The
// caller asks for a domain by name when it means one of them; with no
// name it means the lot.
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

// Layout is a complete panel layout.
var _ panel.Layout = Layout{}
