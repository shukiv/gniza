package node

import (
	"context"
	"errors"
	"fmt"
	"os/user"
	"strconv"
	"time"

	"github.com/shukiv/gniza/internal/hookspool"
	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/resticrun"
)

// A cPanel username is a label. The unix account behind it is the
// identity, and the two come apart exactly once: when an account is
// deleted and another is created with the same name. A host does that
// whenever a customer leaves and the next one asks for a name they liked.
//
// Everything in this program that goes by name would then hand the second
// customer the first one's backups. Their own self-service page would
// list them, and let them restore and download them. Nothing would look
// wrong.

// BackfillIdentities records every account that is on the server now
// against the unix account it means.
//
// It runs once, and what it establishes is the thing every later check
// leans on: these accounts are here today, so the backups already stored
// under their names are theirs. Without it, the first branch of
// noteIdentity would be reasoning from an absent record -- and an absent
// record is not evidence that a name has always meant the same customer.
// A name recycled before this program ever recorded it would have looked
// exactly the same.
func (e *Engine) BackfillIdentities(ctx context.Context) error {
	settings := e.Settings()
	if settings.IdentitiesBackfilledAt != nil {
		return nil
	}
	accounts, err := e.provider.Accounts(ctx)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	for _, account := range accounts {
		if _, err := e.store.Identity(account.User); err == nil {
			continue
		}
		uid, err := e.accountUID(account.User)
		if err != nil {
			// An account cPanel lists that the system does not know is a
			// problem for the backup to report, not for this to guess at.
			e.log.Warn("no unix account behind a cPanel name",
				"account", account.User, "error", err)
			continue
		}
		if _, err := e.store.PutIdentity(nodestore.AccountIdentity{
			Account: account.User, UID: uid, SinceAt: now,
		}); err != nil {
			return err
		}
	}

	settings.IdentitiesBackfilledAt = &now
	if err := e.store.SaveSettings(settings); err != nil {
		return err
	}
	e.settings = settings
	e.log.Info("recorded which unix account each cPanel name means",
		"accounts", len(accounts))
	return nil
}

// noteIdentity records which unix account a name means, and reports
// whether the name has just changed hands.
func (e *Engine) noteIdentity(account string) (nodestore.AccountIdentity, error) {
	uid, err := e.accountUID(account)
	if err != nil {
		return nodestore.AccountIdentity{}, err
	}

	now := time.Now().UTC()
	stored, err := e.store.Identity(account)
	switch {
	case err != nil:
		// No record. What that means depends on whether every account
		// then present has already been recorded: before the backfill it
		// means nobody has looked yet, and after it, it means this name
		// was not on the server when we looked -- so it is either new or
		// has changed hands, and anything stored under it from before now
		// is not this account's to read.
		fresh := nodestore.AccountIdentity{Account: account, UID: uid, SinceAt: now}
		fresh.Recycled = e.Settings().IdentitiesBackfilledAt != nil
		if fresh.Recycled {
			e.log.Warn("a cPanel name appeared that was not here when accounts were recorded",
				"account", account, "uid", uid,
				"older_backups_hidden_from_the_account", true)
		}
		return e.store.PutIdentity(fresh)
	case stored.RetiredAt != nil:
		return e.store.PutIdentity(nodestore.AccountIdentity{
			Account: account, UID: uid, SinceAt: now, Recycled: true,
			CreatedAt: stored.CreatedAt,
		})
	case stored.UID == uid:
		stored.SinceAt = orZero(stored.SinceAt, now)
		return e.store.PutIdentity(stored)
	}

	// The name means a different unix account than it did. Whoever holds
	// it now is not who took the older backups.
	e.log.Warn("a cPanel name has changed hands",
		"account", account, "was_uid", stored.UID, "now_uid", uid,
		"backups_before", stored.SinceAt)
	return e.store.PutIdentity(nodestore.AccountIdentity{
		Account: account, UID: uid, SinceAt: now, Recycled: true,
		CreatedAt: stored.CreatedAt,
	})
}

// visibleSince is the earliest backup of an account that the person
// holding that name today may see.
//
// Zero means all of them: a name that has never changed hands has one
// owner, and the date this program first noticed it is not a boundary
// between anybody.
// AccountSince is when the account holding a name now began, which is
// what a record has to carry to be told apart from the last holder's.
func (e *Engine) AccountSince(account string) time.Time {
	stored, err := e.store.Identity(account)
	if err != nil {
		return time.Time{}
	}
	return stored.SinceAt
}

// BelongsToCurrentHolder says whether a restore is the present account's
// to see and to collect.
//
// A name that has never changed hands answers yes to everything: its
// SinceAt is only when this program first noticed the account, not a
// boundary between two customers. Once a name has been recycled, what the
// last customer recovered stays on this server -- an operator may still
// need it -- and stops being theirs.
//
// A record written before restores carried an incarnation is judged by
// when it was asked for, which is the best that can be said about it.
func (e *Engine) BelongsToCurrentHolder(restore nodestore.Restore) bool {
	stored, err := e.store.Identity(restore.Account)
	if err != nil || !stored.Recycled {
		return true
	}
	if restore.AccountSince.IsZero() {
		return restore.QueuedAt.After(stored.SinceAt)
	}
	return !restore.AccountSince.Before(stored.SinceAt)
}

// BackupBelongsToCurrentHolder says whether a backup run is the present
// account's to see.
//
// The same boundary as BelongsToCurrentHolder, asked of a run rather than
// a restore. A run carries no incarnation of its own, so it is judged by
// when it was queued -- which is what a restore written before restores
// carried one is judged by too.
//
// It matters because a run names what it could not store, and those names
// are the last customer's database names.
func (e *Engine) BackupBelongsToCurrentHolder(run nodestore.Job) bool {
	stored, err := e.store.Identity(run.Account)
	if err != nil || !stored.Recycled {
		return true
	}
	return run.QueuedAt.After(stored.SinceAt)
}

func (e *Engine) visibleSince(account string) time.Time {
	stored, err := e.store.Identity(account)
	if err != nil || !stored.Recycled {
		return time.Time{}
	}
	return stored.SinceAt
}

// OwnedByCaller reports whether the unix account on the other end of the
// socket is the one this name currently means, and from when its backups
// are theirs.
//
// It is checked against the running system rather than against what was
// recorded, because the record is what the last backup saw and the socket
// is who is asking now.
func (e *Engine) OwnedByCaller(account string, callerUID uint32) (since time.Time, err error) {
	uid, err := e.accountUID(account)
	if err != nil {
		return time.Time{}, err
	}
	if uid != int(callerUID) {
		return time.Time{}, fmt.Errorf(
			"node: %s is uid %d on this server and the request came from uid %d",
			account, uid, callerUID)
	}
	return e.visibleSince(account), nil
}

// ReconcileAccounts applies cPanel account lifecycle changes to the local
// identity and policy records. All-account policies need no edit: they are
// deliberately resolved against cPanel at run time. Named policies do need
// a rename carried across, otherwise a customer can silently fall out of
// protection when WHM changes their username.
func (e *Engine) ReconcileAccounts(ctx context.Context) error {
	return e.reconcileAccounts(ctx, false)
}

// ReconcileAccountRenames is used only after cPanel reports an account
// modification. A missing old name with the same uid can then safely be
// treated as a rename. Ordinary polling and create hooks must not make that
// inference because Linux may reuse a deleted account's uid.
func (e *Engine) ReconcileAccountRenames(ctx context.Context) error {
	return e.reconcileAccounts(ctx, true)
}

// AccountCreated records a hard ownership boundary reported by cPanel.
// Unlike polling, the create event is proof that this is a new account even
// when Linux reused both the former username and its uid.
//
// The time is when cPanel made the account, not when this was called: an
// event replayed from the hook spool has to record the boundary where it
// really falls, or the new owner's own first backups end up on the wrong
// side of it. Recording the same event twice is harmless for the same
// reason -- a boundary already at or after this one is left alone.
func (e *Engine) AccountCreated(account string, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	at = at.UTC()
	uid, err := e.accountUID(account)
	if err != nil {
		return err
	}
	identity := nodestore.AccountIdentity{
		Account: account, UID: uid, SinceAt: at, Recycled: true,
	}
	if previous, lookupErr := e.store.Identity(account); lookupErr == nil {
		identity.CreatedAt = previous.CreatedAt
		if previous.Recycled && !previous.SinceAt.Before(at) {
			// Already recorded, by the live hook or by an earlier
			// replay. Writing it again would move the boundary forward
			// and hide the present owner's own backups from them.
			return nil
		}
	}
	_, err = e.store.PutIdentity(identity)
	return err
}

// ReplayHookSpool records the lifecycle events that reached the hook
// while this service was not running.
//
// It is the other half of the hook's fail-open behaviour. A create or a
// remove that nobody heard cannot be recovered from the account list: a
// username deleted and recreated onto the same uid looks exactly like the
// account that was there before, and treating it as the same account
// hands the new customer the last one's backups. So the hook writes the
// event down, and this puts it back before anything reads the account
// list and infers ownership from it.
func (e *Engine) ReplayHookSpool() error {
	if e.hookSpool == "" {
		return nil
	}
	pending, problems := hookspool.Pending(e.hookSpool)
	for _, problem := range problems {
		// Left where it is rather than deleted: an event that cannot be
		// read is still evidence that something happened.
		e.log.Error("read a cPanel event left by a hook", "error", problem)
	}
	for _, entry := range pending {
		var err error
		switch entry.Event.Event {
		case "create":
			err = e.AccountCreated(entry.Event.Account, entry.Event.At)
		case "remove":
			err = e.AccountRemoved(entry.Event.Account)
		}
		if errors.Is(err, ErrNoSuchAccount) {
			// The account was made and removed again while this service
			// was down. There is nobody holding the name to protect, and
			// a name created later brings its own hook, so this event
			// has nothing left to record. Kept, it would fail on every
			// reconciliation from now on.
			e.log.Warn("a cPanel account event names an account that is gone; discarded",
				"event", entry.Event.Event, "account", entry.Event.Account)
			if err := hookspool.Done(entry); err != nil {
				e.log.Error("clear a cPanel event for an account that is gone", "error", err)
			}
			continue
		}
		if err != nil {
			// Keep the file. The store may be busy, or the account list
			// may not be readable right now; either way the boundary is
			// not recorded, so it must not be forgotten.
			e.log.Error("replay a cPanel event left by a hook",
				"event", entry.Event.Event, "account", entry.Event.Account, "error", err)
			continue
		}
		if err := hookspool.Done(entry); err != nil {
			e.log.Error("clear a replayed cPanel event", "error", err)
			continue
		}
		e.log.Warn("recorded a cPanel account event that happened while this service was down",
			"event", entry.Event.Event, "account", entry.Event.Account, "at", entry.Event.At)
		if entry.Event.Event != "create" {
			continue
		}
		// The live hook gives a new account a baseline immediately rather
		// than leaving it with nothing until the next nightly run. An
		// account made while this service was down is the case that needs
		// it most: nothing ran for it at all, and the replay was recording
		// the boundary and stopping there.
		//
		// The event is already cleared. A failure here is not a reason to
		// bring it back: the boundary is recorded, which is the part that
		// cannot be reconstructed later, and the ordinary schedule still
		// covers the account.
		queued, err := e.QueueInitialBackup(entry.Event.Account)
		if err != nil {
			e.log.Error("queue the initial backup for an account created while this service was down",
				"account", entry.Event.Account, "error", err)
			continue
		}
		if queued {
			e.log.Warn("queued the initial backup for an account created while this service was down",
				"account", entry.Event.Account)
		}
	}
	return nil
}

// QueueInitialBackup gives a newly-created account a baseline immediately
// instead of leaving it exposed until the next nightly schedule. Only an
// all-account policy is eligible: an explicitly named policy did not opt in
// to future customers. When several apply, the one writing the most copies
// is the safest single job; the ordinary scheduler will run the others.
func (e *Engine) QueueInitialBackup(account string) (bool, error) {
	running, err := e.store.RunningJobFor(account)
	if err != nil || running {
		return false, err
	}
	policies, err := e.store.Policies()
	if err != nil {
		return false, err
	}
	var selected *nodestore.Policy
	selectedFull := false
	for i := range policies {
		policy := &policies[i]
		if !policy.Enabled || !policy.AllAccounts() || len(policy.RepositoryIDs) == 0 {
			continue
		}
		full := !policy.SkipHomedir && !policy.SkipDatabases && !policy.SkipEmail
		if selected == nil || full && !selectedFull ||
			full == selectedFull && len(policy.RepositoryIDs) > len(selected.RepositoryIDs) {
			selected = policy
			selectedFull = full
		}
	}
	if selected == nil {
		return false, nil
	}
	if _, err := e.QueueBackup(selected.ID, account); err != nil {
		return false, err
	}
	e.log.Info("queued initial backup for new cPanel account",
		"account", account, "policy", selected.Name)
	return true, nil
}

// SuspensionBackup describes what the post-suspension lifecycle hook did.
type SuspensionBackup struct {
	Enabled  bool
	Queued   bool
	Busy     bool
	Policies []nodestore.Policy
}

// QueueSuspensionBackup preserves an account at the point cPanel suspends it.
// It queues the smallest set of full-account policies that reaches every
// destination those policies promise. The hook itself stays quick: it only
// writes pending jobs and never waits for pkgacct or remote storage.
func (e *Engine) QueueSuspensionBackup(account string) (SuspensionBackup, error) {
	settings, err := e.store.Settings()
	if err != nil {
		return SuspensionBackup{}, err
	}
	result := SuspensionBackup{Enabled: settings.BackupOnSuspension}
	if !result.Enabled {
		return result, nil
	}
	policies, err := e.store.Policies()
	if err != nil {
		return result, err
	}
	var candidates []nodestore.Policy
	wanted := map[string]bool{}
	for _, policy := range policies {
		if !fullAccountPolicy(policy, account) {
			continue
		}
		candidates = append(candidates, policy)
		for _, repositoryID := range policy.RepositoryIDs {
			wanted[repositoryID] = true
		}
	}
	missing := make([]string, 0, len(wanted))
	for repositoryID := range wanted {
		missing = append(missing, repositoryID)
	}
	result.Policies = minimumPolicyCover(missing, candidates)
	if len(result.Policies) == 0 {
		return result, nil
	}

	e.workMu.Lock()
	defer e.workMu.Unlock()
	busy, err := e.store.RunningJobFor(account)
	if err != nil {
		return result, err
	}
	if busy {
		result.Busy = true
		return result, nil
	}
	jobs := make([]nodestore.Job, 0, len(result.Policies))
	for _, policy := range result.Policies {
		jobs = append(jobs, nodestore.Job{
			PolicyID: policy.ID, Account: account, Status: job.StatusPending,
		})
	}
	if _, err := e.store.PutJobs(jobs); err != nil {
		return result, err
	}
	result.Queued = true
	return result, nil
}

func (e *Engine) reconcileAccounts(ctx context.Context, allowRenames bool) error {
	// Anything a hook could not deliver is recorded first. Polling sees
	// only the account list as it is now, so a name that changed hands
	// while this service was down would otherwise be reconciled as the
	// same customer -- and the boundary the hook did record would be
	// replayed too late to matter.
	if err := e.ReplayHookSpool(); err != nil {
		return err
	}
	accounts, err := e.provider.Accounts(ctx)
	if err != nil {
		return err
	}
	identities, err := e.store.Identities()
	if err != nil {
		return err
	}

	current := make(map[string]int, len(accounts))
	for _, account := range accounts {
		uid, lookupErr := e.accountUID(account.User)
		if lookupErr != nil {
			e.log.Warn("cannot reconcile cPanel account identity",
				"account", account.User, "error", lookupErr)
			continue
		}
		current[account.User] = uid
	}

	byName := make(map[string]nodestore.AccountIdentity, len(identities))
	for _, identity := range identities {
		byName[identity.Account] = identity
	}
	for name, uid := range current {
		if identity, exists := byName[name]; exists {
			if identity.UID == uid && identity.RetiredAt == nil {
				_, err = e.store.PutIdentity(identity)
			} else {
				_, err = e.noteIdentity(name)
			}
			if err != nil {
				return err
			}
			continue
		}

		// A cPanel rename keeps the Unix uid. Only match an identity whose
		// old name is no longer present; an extant account with the same uid
		// would indicate corrupt system state, not a rename.
		var renamedFrom *nodestore.AccountIdentity
		if allowRenames {
			for i := range identities {
				candidate := &identities[i]
				if candidate.UID != uid || candidate.RetiredAt != nil {
					// A retired identity was removed by cPanel, and a
					// removal is not a rename. Linux reuses a deleted
					// account's uid, so without this a new account that
					// reached the server without a create hook -- one
					// restored by restorepkg, or created while this
					// service was stopped -- would inherit the departed
					// customer's visibility window and be shown their
					// backups.
					continue
				}
				if _, stillPresent := current[candidate.Account]; !stillPresent {
					renamedFrom = candidate
					break
				}
			}
		}
		if renamedFrom == nil {
			if _, err := e.noteIdentity(name); err != nil {
				return err
			}
			continue
		}
		if err := e.renamePolicyAccount(renamedFrom.Account, name); err != nil {
			return err
		}
		if _, err := e.store.PutIdentity(nodestore.AccountIdentity{
			Account: name, UID: uid, SinceAt: renamedFrom.SinceAt,
			Recycled: renamedFrom.Recycled, CreatedAt: renamedFrom.CreatedAt,
		}); err != nil {
			return err
		}
		retiredAt := time.Now().UTC()
		renamedFrom.RetiredAt = &retiredAt
		if _, err := e.store.PutIdentity(*renamedFrom); err != nil {
			return err
		}
		e.log.Info("reconciled a cPanel account rename",
			"old_account", renamedFrom.Account, "new_account", name, "uid", uid)
	}
	return nil
}

func (e *Engine) renamePolicyAccount(oldName, newName string) error {
	policies, err := e.store.Policies()
	if err != nil {
		return err
	}
	for _, policy := range policies {
		changed := false
		for i, account := range policy.Accounts {
			if account == oldName {
				policy.Accounts[i] = newName
				changed = true
			}
		}
		if changed {
			if _, err := e.store.PutPolicy(policy); err != nil {
				return err
			}
		}
	}
	return nil
}

// AccountRemoved removes a terminated username from explicitly named
// policies. Its snapshots and identity record remain: retention decides
// when backups expire, while the identity record prevents a future owner of
// the same username from seeing the former customer's data.
func (e *Engine) AccountRemoved(account string) error {
	// An unfinished basket is the one thing here that is not evidence: it
	// is a half-made choice, and a future owner of the same username must
	// not be handed the last customer's.
	if err := e.store.ForgetBaskets(account); err != nil {
		return err
	}
	if identity, err := e.store.Identity(account); err == nil && identity.RetiredAt == nil {
		now := time.Now().UTC()
		identity.RetiredAt = &now
		if _, err := e.store.PutIdentity(identity); err != nil {
			return err
		}
	}
	policies, err := e.store.Policies()
	if err != nil {
		return err
	}
	for _, policy := range policies {
		if policy.AllAccounts() {
			continue
		}
		kept := policy.Accounts[:0]
		for _, name := range policy.Accounts {
			if name != account {
				kept = append(kept, name)
			}
		}
		if len(kept) == len(policy.Accounts) {
			continue
		}
		policy.Accounts = append([]string(nil), kept...)
		// Empty means all accounts in the stored model. Disabling a policy
		// that lost its only named account avoids accidentally widening it.
		if len(policy.Accounts) == 0 {
			policy.Enabled = false
		}
		if _, err := e.store.PutPolicy(policy); err != nil {
			return err
		}
	}
	return nil
}

// ErrNoSuchAccount says the name is not an account on this server. It is
// worth telling apart from a lookup that failed for some other reason: a
// name that is gone cannot be given an ownership boundary, and a name the
// lookup could not read might still get one on the next try.
var ErrNoSuchAccount = errors.New("not an account on this server")

// accountUID is the unix account a cPanel name means right now.
func accountUID(account string) (int, error) {
	found, err := user.Lookup(account)
	if err != nil {
		var unknown user.UnknownUserError
		if errors.As(err, &unknown) {
			return 0, fmt.Errorf("node: %s is %w", account, ErrNoSuchAccount)
		}
		return 0, fmt.Errorf("node: cannot look up %s on this server: %w", account, err)
	}
	uid, err := strconv.Atoi(found.Uid)
	if err != nil {
		return 0, fmt.Errorf("node: %s has an unreadable uid %q", account, found.Uid)
	}
	return uid, nil
}

func orZero(value, fallback time.Time) time.Time {
	if value.IsZero() {
		return fallback
	}
	return value
}

// UserSnapshots is what the person holding an account's name today may
// see of it.
//
// It is the account's snapshots, minus anything taken before the name
// changed hands. A name that has never changed hands loses nothing: the
// filter exists for the one case where "this account's backups" and
// "this customer's backups" are different sets.
func (e *Engine) UserSnapshots(ctx context.Context, repositoryID, account string) ([]resticrun.Snapshot, error) {
	// Checked here and not only when a backup runs. Between an account
	// being deleted and the new one's first backup there can be days, and
	// this is the interface the new customer is looking at during them.
	if _, err := e.noteIdentity(account); err != nil {
		return nil, err
	}
	snapshots, err := e.Snapshots(ctx, repositoryID, account)
	if err != nil {
		return nil, err
	}
	since := e.visibleSince(account)
	if since.IsZero() {
		return snapshots, nil
	}
	theirs := make([]resticrun.Snapshot, 0, len(snapshots))
	for _, snapshot := range snapshots {
		if snapshot.Time.Before(since) {
			continue
		}
		theirs = append(theirs, snapshot)
	}
	return theirs, nil
}

// OwnsSnapshot reports whether a given backup is one this account's
// current owner may act on. Restores go through it as well as listings:
// hiding a snapshot from a page while still restoring it on request
// would be no protection at all.
func (e *Engine) OwnsSnapshot(ctx context.Context, repositoryID, account, snapshotID string) error {
	snapshots, err := e.UserSnapshots(ctx, repositoryID, account)
	if err != nil {
		return err
	}
	for _, snapshot := range snapshots {
		if snapshot.ID == snapshotID || snapshot.ShortID == snapshotID {
			return nil
		}
	}
	return fmt.Errorf("node: that backup is not one of this account's")
}
