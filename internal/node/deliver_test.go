package node

import (
	"regexp"
	"testing"

	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/protocol"
)

// A copy delivered into a home directory is named from the account, what
// was asked for, and the restore: safe characters only, the archive's own
// suffix, and different for two copies of the same thing.
func TestADeliveredCopyHasASafeDistinctName(t *testing.T) {
	safe := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	cases := []struct {
		restore nodestore.Restore
		want    string
	}{
		{nodestore.Restore{ID: "0123456789abcdef", Account: "pager", Kind: protocol.RestoreAccount,
			ArchivePath: "/var/lib/gniza/work/x/user.admin.pager.tar.gz"}, "pager-account-01234567.tar.gz"},
		{nodestore.Restore{ID: "fedcba9876543210", Account: "pager", Kind: protocol.RestoreItems,
			ItemKind: "database", ArchivePath: "/var/lib/gniza/work/y/pager.tar"}, "pager-database-fedcba98.tar"},
		{nodestore.Restore{ID: "abc", Account: "a_b-c", Kind: protocol.RestoreItems,
			Items:       []nodestore.RestoreSelection{{Kind: "files"}, {Kind: "mailbox"}},
			ArchivePath: "/var/lib/gniza/work/z/thing.tar.zst"}, "a_b-c-basket-abc.tar.zst"},
		{nodestore.Restore{ID: "abc", Account: "x", Kind: protocol.RestoreAccount,
			ArchivePath: "/var/lib/gniza/work/z/odd:name+here"}, "x-account-abc.tar"},
	}
	for _, c := range cases {
		got := deliveredName(c.restore)
		if got != c.want {
			t.Errorf("deliveredName = %q, want %q", got, c.want)
		}
		if !safe.MatchString(got) {
			t.Errorf("%q is not a safe file name", got)
		}
	}
	first := deliveredName(nodestore.Restore{ID: "1111111111111111", Account: "p", Kind: protocol.RestoreAccount, ArchivePath: "a.tar.gz"})
	second := deliveredName(nodestore.Restore{ID: "2222222222222222", Account: "p", Kind: protocol.RestoreAccount, ArchivePath: "a.tar.gz"})
	if first == second {
		t.Errorf("two copies of the same account share a name: %q", first)
	}
}
