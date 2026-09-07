package cpmove

import (
	"path"

	"github.com/shukiv/gniza/internal/panel"
)

// Members inside a cpmove archive, verified against cPanel 136.0.37. They
// are cPanel's names, not ours, and a snapshot from a different version may
// not carry all of them -- a plan asks for what it wants and the extraction
// reports what it actually found.
var (
	settingsMembers = []string{
		"cp/", "meta/", "quota", "shell", "shadow", "digestshadow",
		"userconfig/", "version", "packaged_in_version",
	}
	cronMembers = []string{"cron/"}
	ftpMembers  = []string{"proftpdpasswd"}
	// A mailbox is not only its maildir: forwarders, filters and the
	// domain's mail configuration live in the metadata archive.
	mailMembers = []string{"va/", "vad/", "vf/", "meta/mailserver"}
	// What a per-domain SSL restore carries whichever domain was chosen.
	// cPanel 136 keeps the certificate and its key in apache_tls, one
	// file per domain, and these are empty; a server that still uses them
	// would otherwise hand back a certificate with no key, so they travel
	// whole rather than being filtered by a name whose shape inside them
	// is not known here.
	sslAlways = []string{"ssl/", "sslcerts/", "sslkeys/", "has_sslstorage", "autossl.json"}
	// What a per-domain restore of the domains carries regardless: which
	// domain is the main one and which are addons is a property of the
	// account, not of any one domain, and a domain restored without it
	// has nowhere to be put back.
	domainAlways = []string{"userdata/main", "userdata/cache.json", "ips/", "addons"}
)

// SettingsMembers is cPanel's own record of the account.
func (Layout) SettingsMembers() []string { return settingsMembers }

// CronMembers is the account's scheduled jobs.
func (Layout) CronMembers() []string { return cronMembers }

// FTPMembers is its FTP logins.
func (Layout) FTPMembers() []string { return ftpMembers }

// MailMembers is the mail configuration that is not maildir.
func (Layout) MailMembers() []string { return mailMembers }

// DNSMembers is the account's zone files, or the zones of the domains
// named.
func (Layout) DNSMembers(names []string) ([]string, error) {
	if len(names) == 0 {
		return []string{"dnszones/"}, nil
	}
	members := make([]string, 0, len(names))
	for _, name := range names {
		if err := panel.UsableDomainName(name); err != nil {
			return nil, err
		}
		members = append(members, "dnszones/"+name+".db")
	}
	return members, nil
}

// SSLMembers is the account's certificates, or those of the domains named.
func (Layout) SSLMembers(names []string) ([]string, error) {
	if len(names) == 0 {
		return append([]string{"apache_tls/"}, sslAlways...), nil
	}
	members := make([]string, 0, len(names)+len(sslAlways))
	for _, name := range names {
		if err := panel.UsableDomainName(name); err != nil {
			return nil, err
		}
		members = append(members, "apache_tls/"+name)
	}
	return append(members, sslAlways...), nil
}

// DomainMembers is the web server configuration of the account's domains,
// or of the domains named.
func (Layout) DomainMembers(names []string) ([]string, error) {
	if len(names) == 0 {
		return []string{"userdata/", "dnszones/", "ips/", "addons"}, nil
	}
	members := make([]string, 0, 4*len(names)+len(domainAlways))
	for _, name := range names {
		if err := panel.UsableDomainName(name); err != nil {
			return nil, err
		}
		members = append(members,
			"userdata/"+name,
			"userdata/"+name+"_SSL",
			"userdata/"+name+".php-fpm.yaml",
			"dnszones/"+name+".db")
	}
	return append(members, domainAlways...), nil
}

// WebsitePaths is the document root, relative to the home directory.
func (Layout) WebsitePaths() []string { return []string{"public_html"} }

// MailboxPaths is where cPanel keeps the messages of a mailbox: under
// mail/ in the account's own home directory, one maildir per address.
func (Layout) MailboxPaths(names []string) []string {
	paths := make([]string, 0, len(names))
	for _, name := range names {
		paths = append(paths, path.Join("mail", name))
	}
	return paths
}

// Layout is a complete panel layout.
var _ panel.Layout = Layout{}
