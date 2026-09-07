package node

import (
	"path/filepath"
	"testing"

	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/panel"
)

// TestSkipEmailLeavesOutTheMailboxPasswordsToo covers what "leave email
// out" has to mean.
//
// Excluding ~/mail leaves the messages behind and nothing else. cPanel
// keeps the mail accounts themselves under ~/etc, one directory per
// domain: passwd names every mailbox, and shadow holds its password as a
// crypt hash. A schedule set to skip email was shipping all of them, so a
// backup an operator believed held no email held the credentials to read
// all of it -- on a destination chosen because it does not hold email.
func TestSkipEmailLeavesOutTheMailboxPasswordsToo(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "home", "customer1")
	excludes := excludesFor(nodestore.Policy{SkipEmail: true}, panel.AccountInfo{
		HomeDir: home,
	}, nil)

	wantMail := filepath.Join(home, "mail")
	wantEtc := filepath.Join(home, "etc")
	var mailExcluded, etcExcluded bool
	for _, exclude := range excludes {
		mailExcluded = mailExcluded || exclude == wantMail
		etcExcluded = etcExcluded || exclude == wantEtc
	}
	if !mailExcluded {
		t.Errorf("mailbox directory was not excluded: %v", excludes)
	}
	if !etcExcluded {
		t.Errorf("the mail account names and password hashes under %s are still "+
			"in the backup: %v", wantEtc, excludes)
	}

	// And a schedule that keeps email keeps all of it: ~/etc is only
	// dropped because the operator asked for a backup without email.
	full := excludesFor(nodestore.Policy{}, panel.AccountInfo{HomeDir: home}, nil)
	for _, exclude := range full {
		if exclude == wantMail || exclude == wantEtc {
			t.Errorf("a schedule that keeps email excluded %s", exclude)
		}
	}
}
