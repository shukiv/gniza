package dabackup

import (
	"os"
	"path"
	"strings"
	"testing"
)

// The member tables in this package were written from DirectAdmin's
// documentation, before anybody had seen an archive. testdata holds the
// listing of a real one -- every member of a whole-account backup taken
// from DirectAdmin 1.709, and every member of the second archive nested
// inside it -- so the tables can be held to what the panel actually
// writes instead of to what was expected of it.
//
// A path here that no archive carries is not a failure an operator sees
// as a wrong path. It is a restore that comes back empty, or one that
// comes back with somebody's password hashes because the only path broad
// enough to contain what was asked for contained everything else too.
const fixtureDomain = "gzv0908a.gniza-test.invalid"

func fixture(t *testing.T, name string) []string {
	t.Helper()
	body, err := os.ReadFile(path.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var members []string
	for _, line := range strings.Split(string(body), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			members = append(members, strings.TrimSuffix(line, "/"))
		}
	}
	if len(members) == 0 {
		t.Fatalf("%s lists no members", name)
	}
	return members
}

// carries says whether the archive has this member, or anything under it.
// A member table names directories as well as files, and asking for a
// directory means asking for what is in it.
func carries(members []string, want string) bool {
	for _, member := range members {
		if member == want || strings.HasPrefix(member, want+"/") {
			return true
		}
	}
	return false
}

// TestEveryMemberNamedIsInARealArchive holds the whole item layout to the
// fixture. A table that names something DirectAdmin does not write is a
// restore that comes back short, and the operator is told the backup held
// nothing for them rather than that this program looked in the wrong
// place.
func TestEveryMemberNamedIsInARealArchive(t *testing.T) {
	members := fixture(t, "account.tar.list")
	layout := Layout{}

	named := map[string][]string{
		"the account's own settings": layout.SettingsMembers(),
		"the cron jobs":              layout.CronMembers(),
		"the FTP logins":             layout.FTPMembers(),
		"the mail configuration":     layout.MailMembers(),
	}
	for _, each := range []struct {
		what string
		of   func([]string) ([]string, error)
	}{
		{"the DNS zones", layout.DNSMembers},
		{"the domain configuration", layout.DomainMembers},
	} {
		paths, err := each.of([]string{fixtureDomain})
		if err != nil {
			t.Errorf("%s of a domain the archive has: %v", each.what, err)
			continue
		}
		named[each.what] = paths
	}

	for what, paths := range named {
		for _, member := range paths {
			if !carries(members, member) {
				t.Errorf("%s names %q, which no real archive carries", what, member)
			}
		}
	}

	// The website and the messages are in the home directory rather than
	// in the metadata, so they are checked against the same listing but
	// with the home's own shape.
	for _, website := range layout.WebsitePaths() {
		if !carries(members, path.Join(website, fixtureDomain, "public_html")) {
			t.Errorf("the website path %q does not reach a document root", website)
		}
	}
	for _, mailbox := range layout.MailboxPaths([]string{"sales@" + fixtureDomain}) {
		if !carries(members, path.Join(mailbox, "Maildir")) {
			t.Errorf("the mailbox path %q does not reach a maildir", mailbox)
		}
	}
}

// TestTheDatabaseDumpsAreWhereTheLayoutSaysTheyAre is what a finished
// restore is held to, so it is the one table that is already load-bearing
// in production rather than waiting behind a refusal.
func TestTheDatabaseDumpsAreWhereTheLayoutSaysTheyAre(t *testing.T) {
	members := fixture(t, "account.tar.list")
	found := 0
	for _, member := range members {
		if database, ok := databaseIn(member, "gzv0908a", tarRegular); ok {
			found++
			if database != "gzv0908a_shop" {
				t.Errorf("read %q as a database of the account", database)
			}
		}
	}
	if found != 1 {
		t.Fatalf("found %d database dumps in a real archive, want 1", found)
	}
	if !carries(members, path.Join(DatabaseDirName, "gzv0908a_shop.sql")) {
		t.Fatal("the fixture no longer holds the dump this checks against")
	}
}

// TestTheHomeDirectoryIsInThreePlaces records the shape of a DirectAdmin
// account, which is the reason split mode is still refused.
//
// A cpmove tree has the account's files in one directory. This archive
// has the websites under domains/, the messages under imap/, and
// everything else in the home -- the dotfiles, and anything a customer
// keeps outside a domain -- inside a second compressed archive at
// backup/home.tar.zst. Reassembling that into one tree, and back, is what
// ADR 0019 still has open, and HomedirDir naming only the first of the
// three is why it says of itself that it is not for split reassembly.
func TestTheHomeDirectoryIsInThreePlaces(t *testing.T) {
	members := fixture(t, "account.tar.list")
	for _, place := range []string{"domains", "imap", "backup/home.tar.zst"} {
		if !carries(members, place) {
			t.Errorf("a real archive no longer keeps account files in %q", place)
		}
	}
	if (Layout{}).HomedirDir() != "domains" {
		t.Fatal("HomedirDir changed without the split-mode question being settled")
	}
	// The nested archive is the part nothing here can reach yet. What is
	// in it is listed beside the outer one so that the day somebody
	// implements split mode, they are not guessing either.
	home := fixture(t, "home.tar.list")
	if !carries(home, ".bashrc") {
		t.Error("the nested home archive no longer holds the account's dotfiles")
	}
}
