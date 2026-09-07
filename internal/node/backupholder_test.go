package node

import (
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/nodestore"
)

// TestABackupRunBelongsToWhoeverHeldTheNameThen.
//
// The account-facing logs page lists the backups of one account, and a
// backup names the databases it could not store. A recycled username
// would otherwise show the new customer when the old one's site was
// backed up and what their databases were called. Restores have been
// judged against the identity boundary since it existed; runs are the
// same question about the same names.
func TestABackupRunBelongsToWhoeverHeldTheNameThen(t *testing.T) {
	engine, store := lifecycleEngine(t,
		map[string][]string{"webshop": nil}, map[string]int{"webshop": 1042})

	handedOver := time.Now().Add(-24 * time.Hour).UTC()
	theirs := nodestore.Job{
		Account: "webshop", QueuedAt: handedOver.Add(-30 * 24 * time.Hour),
		Missing: []string{"database webshop_old: whatever the server saw"},
	}
	ours := nodestore.Job{Account: "webshop", QueuedAt: handedOver.Add(time.Hour)}

	// Before the name changes hands, every run of it is the account's own.
	if !engine.BackupBelongsToCurrentHolder(theirs) {
		t.Fatal("the account's own backup was hidden from it before anything changed")
	}

	if _, err := store.PutIdentity(nodestore.AccountIdentity{
		Account: "webshop", UID: 1042, SinceAt: handedOver, Recycled: true,
	}); err != nil {
		t.Fatal(err)
	}

	if engine.BackupBelongsToCurrentHolder(theirs) {
		t.Error("the previous customer's backup is shown to the new holder of the name")
	}
	if !engine.BackupBelongsToCurrentHolder(ours) {
		t.Error("the new holder cannot see their own backup")
	}
}
