package granular

import (
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/layout/cpmove"
	"github.com/shukiv/gniza/internal/layout/dabackup"
	"github.com/shukiv/gniza/internal/panel"
	"github.com/shukiv/gniza/internal/reassemble"
)

// A split snapshot of one account, as the agent records it.
var split = reassemble.Parts{
	Metadata:  "/var/lib/gniza/staging/stage-backup-studio/metadata",
	Homedir:   "/home/studio",
	Databases: "/var/lib/gniza/staging/stage-backup-studio/databases",
}

func TestEachKindAsksForTheRightPartOfTheSnapshot(t *testing.T) {
	for _, tc := range []struct {
		kind        Kind
		names       []string
		wantInclude []string
		wantMembers bool
	}{
		{KindWebsite, nil, []string{"/home/studio/public_html"}, false},
		{KindFiles, []string{"public_html/index.php", "/home/studio/.htaccess"},
			[]string{"/home/studio/public_html/index.php", "/home/studio/.htaccess"}, false},
		{KindMailbox, []string{"studio.co.il/sales"},
			[]string{"/home/studio/mail/studio.co.il/sales", split.Metadata}, true},
		{KindDatabase, []string{"studio_kpeh1"},
			[]string{"/var/lib/gniza/staging/stage-backup-studio/databases/studio_kpeh1.sql"}, false},
		{KindDNS, nil, []string{split.Metadata}, true},
		{KindSSL, nil, []string{split.Metadata}, true},
		{KindSettings, nil, []string{split.Metadata}, true},
	} {
		plan, err := Build(cpmove.Layout{}, split, Request{Kind: tc.kind, Account: "studio", Names: tc.names})
		if err != nil {
			t.Errorf("%s: %v", tc.kind, err)
			continue
		}
		if strings.Join(plan.Include, "|") != strings.Join(tc.wantInclude, "|") {
			t.Errorf("%s include = %v, want %v", tc.kind, plan.Include, tc.wantInclude)
		}
		if got := len(plan.Members) > 0; got != tc.wantMembers {
			t.Errorf("%s members = %v, want any: %v", tc.kind, plan.Members, tc.wantMembers)
		}
		if plan.Description == "" {
			t.Errorf("%s: a plan has to say what it restores", tc.kind)
		}
	}
}

// Every account on a cPanel server belongs to a different customer, so a
// path that leaves this account's home directory is refused rather than
// quietly restored.
func TestAPathOutsideTheAccountIsRefused(t *testing.T) {
	for _, name := range []string{
		"/home/someone-else/public_html",
		"../someone-else/.ssh",
		"/etc/shadow",
		"public_html/../../someone-else",
		"",
	} {
		if _, err := Build(cpmove.Layout{}, split, Request{Kind: KindFiles, Account: "studio", Names: []string{name}}); err == nil {
			t.Errorf("a files restore of %q was accepted", name)
		}
	}
}

func TestADatabaseIsANameNotAPath(t *testing.T) {
	for _, name := range []string{"../../etc/passwd", "studio/../..", "a/b"} {
		if _, err := Build(cpmove.Layout{}, split, Request{Kind: KindDatabase, Account: "studio", Names: []string{name}}); err == nil {
			t.Errorf("a database restore of %q was accepted", name)
		}
	}
}

// A plan that asks for nothing would report success having restored
// nothing, so an empty or impossible request has to fail here.
func TestAnImpossibleRequestFailsRatherThanAskingForNothing(t *testing.T) {
	monolithic := reassemble.Parts{Archive: "/var/lib/gniza/staging/stage-backup-studio/cpmove-studio.tar"}

	for _, tc := range []struct {
		what  string
		parts reassemble.Parts
		req   Request
	}{
		{"a database from a snapshot with no dumps", monolithic,
			Request{Kind: KindDatabase, Account: "studio", Names: []string{"studio_kpeh1"}}},
		{"DNS from a snapshot with no metadata", monolithic,
			Request{Kind: KindDNS, Account: "studio"}},
		{"files from a snapshot with no home directory", monolithic,
			Request{Kind: KindFiles, Account: "studio", Names: []string{"public_html"}}},
		{"a mailbox with no mailbox named", split,
			Request{Kind: KindMailbox, Account: "studio"}},
		{"a database with none named", split,
			Request{Kind: KindDatabase, Account: "studio"}},
		{"an unknown kind", split, Request{Kind: "everything", Account: "studio"}},
		{"no account", split, Request{Kind: KindDNS}},
	} {
		if _, err := Build(cpmove.Layout{}, tc.parts, tc.req); err == nil {
			t.Errorf("%s was accepted", tc.what)
		}
	}
}

// The parts of an account an operator asks for by name, and where each
// one comes from. These are cPanel's own names inside a cpmove archive,
// read off a live 136.0.37 rather than assumed.
func TestEveryBackupItemHasSomewhereToComeFrom(t *testing.T) {
	for _, kind := range Kinds {
		req := Request{Kind: kind, Account: "studio"}
		switch kind {
		case KindFiles:
			req.Names = []string{"public_html/index.php"}
		case KindMailbox:
			req.Names = []string{"studio.co.il/sales"}
		case KindDatabase:
			req.Names = []string{"studio_kpeh1"}
		}
		plan, err := Build(cpmove.Layout{}, split, req)
		if err != nil {
			t.Errorf("%s: %v", kind, err)
			continue
		}
		if len(plan.Include) == 0 {
			t.Errorf("%s asks for nothing", kind)
		}
		if kind.Title() == string(kind) {
			t.Errorf("%s has no name an operator would recognise", kind)
		}
	}
}

// Database users are staged beside the dumps, so a snapshot's paths do not
// change when an account gains or loses a database.
func TestDatabaseUsersComeFromBesideTheDumps(t *testing.T) {
	plan, err := Build(cpmove.Layout{}, split, Request{Kind: KindDBUsers, Account: "studio"})
	if err != nil {
		t.Fatal(err)
	}
	// Both files travel. The first is the one cPanel's restore reads;
	// the second is the same users written so a person can run them,
	// because cPanel's format uses syntax MySQL 8 removed and a customer
	// who asked for their database users should not be handed a file
	// their database refuses.
	for _, want := range []string{
		split.Databases + "/" + DatabaseUsersFile,
		split.Databases + "/" + RunnableDatabaseUsersFile,
	} {
		found := false
		for _, included := range plan.Include {
			if included == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("include = %v, want it to carry %q", plan.Include, want)
		}
	}

	// A backup with no databases has no users to restore either, and says
	// so rather than producing an empty file.
	if _, err := Build(cpmove.Layout{}, reassemble.Parts{Metadata: "/stage/metadata", Homedir: "/home/studio"},
		Request{Kind: KindDBUsers, Account: "studio"}); err == nil {
		t.Error("database users were promised from a backup that holds none")
	}
}

// TestTheStagedUsersFileNameMatchesReassemble keeps the two spellings of
// one filename together. reassemble cannot import this package — this
// package imports it — so the name is written out there, and a rename
// here would otherwise leave the grants file behind in the rebuilt
// archive with nothing failing.
func TestTheStagedUsersFileNameMatchesReassemble(t *testing.T) {
	if DatabaseUsersFile != cpmove.StagedGrantsFile {
		t.Fatalf("granular calls it %q, reassemble looks for %q",
			DatabaseUsersFile, cpmove.StagedGrantsFile)
	}
	if RunnableDatabaseUsersFile != cpmove.StagedRunnableFile {
		t.Fatalf("granular calls it %q, reassemble takes out %q",
			RunnableDatabaseUsersFile, cpmove.StagedRunnableFile)
	}
}

// TestOnlyWhatCanBeWrittenBackSaysSo pins the list of kinds a restore may
// put into a live account. Every other kind still needs a change the
// control panel makes -- a zone edit, an installed certificate, a login --
// and that is not built, so copying the backed-up file over the live one is
// not offered as if it were the same thing.
func TestOnlyWhatCanBeWrittenBackSaysSo(t *testing.T) {
	canApply := map[Kind]bool{
		KindFiles:    true,
		KindWebsite:  true,
		KindMailbox:  true,
		KindDatabase: true,
		KindDBUsers:  true,
		KindCron:     true,
	}
	all := append([]Kind{KindSystem}, Kinds...)
	for _, kind := range all {
		if got := kind.CanApply(); got != canApply[kind] {
			t.Errorf("%s.CanApply() = %v, want %v", kind, got, canApply[kind])
		}
	}
}

func TestADatabaseNameHasToBeOne(t *testing.T) {
	for _, bad := range []string{
		"", "-h127.0.0.1", "../../etc/passwd", "shop; DROP", "a/b", "shop.sql",
		strings.Repeat("a", 65),
	} {
		if err := UsableDatabaseName(bad); err == nil {
			t.Errorf("%q was accepted as a database name", bad)
		}
	}
	for _, good := range []string{"studio_wp", "rtflow_shop", "a$b", "DB1"} {
		if err := UsableDatabaseName(good); err != nil {
			t.Errorf("%q was refused: %v", good, err)
		}
	}
}

// A restore of a database and the users that open it is one restore. The
// paths of both come out of the snapshot together, so the account cannot
// end up with one and not the other.
func TestABasketAsksForEveryPartOnce(t *testing.T) {
	plan, err := BuildAll(cpmove.Layout{}, split, []Request{
		{Kind: KindDatabase, Account: "studio", Names: []string{"studio_kpeh1"}},
		{Kind: KindDBUsers, Account: "studio"},
		{Kind: KindDNS, Account: "studio"},
		{Kind: KindSSL, Account: "studio"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// DNS and SSL both come out of the metadata archive, and asking restic
	// for it twice is asking twice for the same 20 megabytes.
	seen := map[string]int{}
	for _, include := range plan.Include {
		seen[include]++
	}
	for path, count := range seen {
		if count > 1 {
			t.Errorf("%s asked for %d times", path, count)
		}
	}
	if seen[split.Metadata] != 1 {
		t.Errorf("include = %v, want the metadata part once", plan.Include)
	}
	if seen[split.Databases+"/studio_kpeh1.sql"] != 1 {
		t.Errorf("include = %v, want the database dump", plan.Include)
	}
	if plan.Metadata != split.Metadata {
		t.Errorf("metadata = %q", plan.Metadata)
	}
	// Both kinds' members travel, or the archive gives up only half of
	// what was asked for.
	for _, want := range []string{"dnszones/", "apache_tls/"} {
		found := false
		for _, member := range plan.Members {
			if member == want {
				found = true
			}
		}
		if !found {
			t.Errorf("members = %v, want %q", plan.Members, want)
		}
	}
	for _, want := range []string{"database", "users", "DNS", "SSL"} {
		if !strings.Contains(plan.Description, want) {
			t.Errorf("description %q does not mention %s", plan.Description, want)
		}
	}
}

// A basket is restored so that its parts arrive together. One part this
// cannot do has to fail the request, not quietly restore the rest: an
// account left with a database and no user for it is the failure the
// basket exists to prevent.
func TestABasketWithOneImpossiblePartFailsWhole(t *testing.T) {
	if _, err := BuildAll(cpmove.Layout{}, split, []Request{
		{Kind: KindDatabase, Account: "studio", Names: []string{"studio_kpeh1"}},
		{Kind: KindFiles, Account: "studio", Names: []string{"/etc/shadow"}},
	}); err == nil {
		t.Error("a basket carrying a path outside the account was accepted")
	}
	if _, err := BuildAll(cpmove.Layout{}, split, nil); err == nil {
		t.Error("an empty basket was accepted")
	}
}

// TestAMailboxNameCannotReachOutsideTheAccount.
//
// The name is typed by an operator and becomes a path inside a snapshot.
// Each panel spells a mailbox differently -- mail/<address> on cPanel,
// imap/<domain>/<mailbox> on DirectAdmin -- and neither layout validates
// the name, because the clamp belongs here, where the home directory is
// known.
func TestAMailboxNameCannotReachOutsideTheAccount(t *testing.T) {
	parts := reassemble.Parts{
		Metadata:  "/stage/metadata",
		Homedir:   "/home/customer1",
		Databases: "/stage/databases",
	}
	for _, layout := range []panel.ItemLayout{cpmove.Layout{}, dabackup.Layout{}} {
		for _, name := range []string{"../../etc/shadow", "/etc/shadow", "../"} {
			plan, err := Build(layout, parts, Request{
				Kind: KindMailbox, Account: "customer1", Names: []string{name},
			})
			if err == nil {
				t.Errorf("%s: %q was accepted as a mailbox: %v",
					layout.(interface{ Panel() string }).Panel(), name, plan.Include)
			}
		}
	}
}

// TestAMailboxRestoreDoesNotFetchMetadataItCannotUse covers a panel whose
// mail configuration this layout cannot name.
//
// Restoring one mailbox pulls the metadata part out of the snapshot so
// that the domain's forwarders and autoresponders come back with the
// messages. Where the layout has no member to take out of it, that
// download is the whole account's configuration -- fetched, paid for in
// time and in scratch space, and then not used for anything.
//
// On a large account the metadata part is not small, and a customer
// waiting for one mailbox waits for it.
func TestAMailboxRestoreDoesNotFetchMetadataItCannotUse(t *testing.T) {
	parts := reassemble.Parts{Homedir: "/home", Metadata: "/meta"}
	req := Request{Kind: KindMailbox, Account: "studio", Names: []string{"sales@studio.co.il"}}

	plan, err := Build(namelessMailLayout{}, parts, req)
	if err != nil {
		t.Fatal(err)
	}
	for _, include := range plan.Include {
		if include == parts.Metadata {
			t.Error("the account's metadata was restored with nothing to take out of it")
		}
	}
	if plan.Metadata != "" {
		t.Errorf("the plan claims metadata it named no members of: %q", plan.Metadata)
	}

	// A layout that does name mail configuration still gets it.
	plan, err = Build(cpmove.Layout{}, parts, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Members) == 0 || plan.Metadata != parts.Metadata {
		t.Error("a layout that names its mail configuration did not get the metadata")
	}
}

// namelessMailLayout keeps a panel's mail configuration somewhere this
// interface cannot ask for, which is DirectAdmin's shape.
type namelessMailLayout struct{ panel.ItemLayout }

func (namelessMailLayout) MailMembers() []string { return nil }
func (l namelessMailLayout) MailboxPaths(names []string) []string {
	return cpmove.Layout{}.MailboxPaths(names)
}
