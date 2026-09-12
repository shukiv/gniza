package plain

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// Layout is where a plain server's backup keeps the parts of an account.
//
// There is no panel archive here. What a backup holds is the account's
// directory, read where it lies, and beside it a metadata part with the
// database dumps and one record saying what the account is. A rebuilt
// tree puts the directory under files/ and the dumps under databases/,
// which is what a restore of files or of a database takes them from.
type Layout struct{}

const (
	// PanelName is what this provider is called where an operator reads
	// it. A server with no panel is described by what it is.
	PanelName = "Plain server"
	// RecordDir is where, inside the metadata part, the account's record
	// lives, and RecordFile is the record.
	RecordDir  = "gniza"
	RecordFile = "account.json"
	// DumpDir is where the database dumps live inside the metadata part.
	DumpDir = "databases"
	// FilesDir is where the account's directory goes in a rebuilt tree.
	FilesDir = "files"
	// NoFolderNote is the one file in the files part of a source that
	// has no folder.
	NoFolderNote = "no-folder.txt"
)

func (Layout) Panel() string       { return PanelName }
func (Layout) HomedirDir() string  { return FilesDir }
func (Layout) DatabaseDir() string { return DumpDir }

// AccountRoot is the account's directory inside an extracted tree: one
// directory named after it, holding the files part.
func (Layout) AccountRoot(treeDir, account string) (string, error) {
	root := filepath.Join(treeDir, account)
	info, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("plain: the tree holds no account named %s: %w", account, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("plain: %s is not a directory", root)
	}
	return root, nil
}

// PlaceDatabaseUsers has nowhere to put grants: a plain server's restore
// loads dumps into databases the operator already has users for.
func (Layout) PlaceDatabaseUsers(string) error { return nil }

// ValidateArchive binds an archive to an account. A plain server makes
// no archive, so there is none to validate: a restore that reaches this
// is a restore of a shape this provider does not produce.
func (Layout) ValidateArchive(_ context.Context, filename, account string) error {
	return unverified(fmt.Sprintf("there is no native archive of %s to validate (%s)", account, filename))
}

// SettingsMembers is the account record. It is all the "panel
// configuration" a plain server has.
func (Layout) SettingsMembers() []string { return []string{RecordDir + "/"} }

// CronMembers, FTPMembers and MailMembers: a plain server's backup does
// not carry these as members of the account. Cron jobs live in
// /etc/cron.d and the users' crontabs, which the system backup takes.
func (Layout) CronMembers() []string { return nil }
func (Layout) FTPMembers() []string  { return nil }
func (Layout) MailMembers() []string { return nil }

func (Layout) DNSMembers([]string) ([]string, error) {
	return nil, fmt.Errorf("plain: a plain server's backup holds no DNS zones")
}
func (Layout) SSLMembers([]string) ([]string, error) {
	return nil, fmt.Errorf("plain: a plain server's backup holds no certificates of its own; they are in the system backup")
}
func (Layout) DomainMembers([]string) ([]string, error) {
	return nil, fmt.Errorf("plain: a plain server's backup holds no domains")
}

// WebsitePaths is the whole directory: on a plain server the account is
// the site.
func (Layout) WebsitePaths() []string { return []string{"."} }

// MailboxPaths: there are no mailboxes in an account directory.
func (Layout) MailboxPaths([]string) []string { return nil }
