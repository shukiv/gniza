package nodestore_test

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/nodestore"
)

func newStore(t *testing.T) *nodestore.Store {
	t.Helper()
	store, err := nodestore.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestDefaultSettingsMatchFleetMode(t *testing.T) {
	// Snapshot paths embed the staging root and restic groups retention by
	// path, so a standalone server that later joins a fleet must have been
	// using the fleet default all along.
	settings := nodestore.DefaultSettings()
	if settings.StagingRoot != "/var/lib/gniza/staging" {
		t.Errorf("staging root = %q, does not match fleet mode", settings.StagingRoot)
	}
	if settings.MaxConcurrent != 1 {
		t.Errorf("max concurrent = %d", settings.MaxConcurrent)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	store := newStore(t)

	settings, err := store.Settings()
	if err != nil {
		t.Fatalf("Settings on a fresh store: %v", err)
	}
	if settings.StagingRoot == "" {
		t.Fatal("a fresh store should return defaults, not zero values")
	}

	settings.Hostname = "cp01.example.com"
	settings.MaxConcurrent = 3
	if err := store.SaveSettings(settings); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	reloaded, err := store.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Hostname != "cp01.example.com" || reloaded.MaxConcurrent != 3 {
		t.Errorf("reloaded = %+v", reloaded)
	}
}

func TestRepositoriesCopyTheFirstChunkerSource(t *testing.T) {
	store := newStore(t)

	add := func(path string) nodestore.Repository {
		t.Helper()
		repo, err := store.PutRepository(nodestore.Repository{
			DestinationID: "dest-" + path, Path: path, PasswordSecretID: "secret",
		})
		if err != nil {
			t.Fatalf("PutRepository: %v", err)
		}
		// bbolt keys are ordered, but chunker selection is by creation
		// time; keep them distinct.
		time.Sleep(time.Millisecond)
		return repo
	}

	first := add("a")
	second := add("b")
	third := add("c")

	if first.ChunkerSourceRepoID != "" {
		t.Errorf("the first repository has a chunker source: %q", first.ChunkerSourceRepoID)
	}
	// Chunker parameters are fixed at creation and cannot be changed, so
	// every later repository copies the first one's — and the third
	// follows the chain to its root rather than pointing at the second.
	if second.ChunkerSourceRepoID != first.ID {
		t.Errorf("second source = %q, want %q", second.ChunkerSourceRepoID, first.ID)
	}
	if third.ChunkerSourceRepoID != first.ID {
		t.Errorf("third source = %q, want %q", third.ChunkerSourceRepoID, first.ID)
	}
}

func TestDeleteDestinationTakesItsRepositoryWithIt(t *testing.T) {
	store := newStore(t)

	dest, err := store.PutDestination(nodestore.Destination{Name: "Backup disk", Type: "local"})
	if err != nil {
		t.Fatalf("PutDestination: %v", err)
	}
	repo, err := store.PutRepository(nodestore.Repository{
		DestinationID: dest.ID, Path: "cp01", PasswordSecretID: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}

	// A destination typed in wrongly has to be removable, or the operator
	// is stuck with it.
	if err := store.DeleteDestination(dest.ID); err != nil {
		t.Fatalf("DeleteDestination: %v", err)
	}
	if _, err := store.Repository(repo.ID); !errors.Is(err, nodestore.ErrNotFound) {
		t.Error("the repository record outlived its destination")
	}
}

func TestDeleteDestinationRefusesWhileAScheduleUsesIt(t *testing.T) {
	store := newStore(t)

	dest, err := store.PutDestination(nodestore.Destination{Name: "Backup disk", Type: "local"})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := store.PutRepository(nodestore.Repository{
		DestinationID: dest.ID, Path: "cp01", PasswordSecretID: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutPolicy(nodestore.Policy{
		Name: "Nightly", ScheduleCron: "0 2 * * *", RepositoryIDs: []string{repo.ID},
	}); err != nil {
		t.Fatal(err)
	}

	// Otherwise the schedule would quietly stop making one of the copies
	// it promises.
	err = store.DeleteDestination(dest.ID)
	if err == nil {
		t.Fatal("a destination a schedule still uses should not be removable")
	}
	if !strings.Contains(err.Error(), "Nightly") {
		t.Errorf("err = %v, want it to name the schedule", err)
	}
}

func TestRunningJobForCoversBackupsAndRestores(t *testing.T) {
	store := newStore(t)

	running, err := store.RunningJobFor("customer1")
	if err != nil || running {
		t.Fatalf("clean store reported running=%v (%v)", running, err)
	}

	stored, err := store.PutJob(nodestore.Job{Account: "customer1", Status: job.StatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	if running, _ := store.RunningJobFor("customer1"); !running {
		t.Error("a running backup was not reported")
	}
	if running, _ := store.RunningJobFor("customer2"); running {
		t.Error("another account was reported as busy")
	}

	stored.Status = job.StatusSuccess
	if _, err := store.PutJob(stored); err != nil {
		t.Fatal(err)
	}

	// A restore blocks a backup of the same account too: both stage in the
	// same place.
	if _, err := store.PutRestore(nodestore.Restore{
		Account: "customer1", Status: job.StatusRunning,
	}); err != nil {
		t.Fatal(err)
	}
	if running, _ := store.RunningJobFor("customer1"); !running {
		t.Error("a running restore was not reported")
	}
}

func TestPendingWorkPrefersRestores(t *testing.T) {
	store := newStore(t)

	if _, err := store.PutJob(nodestore.Job{Account: "c1", Status: job.StatusPending}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutRestore(nodestore.Restore{
		Account: "c2", SnapshotID: "abc", Status: job.StatusPending,
	}); err != nil {
		t.Fatal(err)
	}

	// Someone is usually waiting for a restore; a backup can start a
	// minute later without anyone noticing.
	restore, backup, err := store.PendingWork()
	if err != nil {
		t.Fatalf("PendingWork: %v", err)
	}
	if restore == nil || backup != nil {
		t.Fatalf("got restore=%v backup=%v, want the restore first", restore, backup)
	}
}

func TestMissingRecordsReportNotFound(t *testing.T) {
	store := newStore(t)
	for name, err := range map[string]error{
		"destination": errOf(func() error { _, err := store.Destination("nope"); return err }),
		"repository":  errOf(func() error { _, err := store.Repository("nope"); return err }),
		"policy":      errOf(func() error { _, err := store.Policy("nope"); return err }),
		"job":         errOf(func() error { _, err := store.Job("nope"); return err }),
		"restore":     errOf(func() error { _, err := store.Restore("nope"); return err }),
		"secret":      errOf(func() error { _, err := store.Secret("nope"); return err }),
	} {
		if !errors.Is(err, nodestore.ErrNotFound) {
			t.Errorf("missing %s gave %v, want ErrNotFound", name, err)
		}
	}
}

func errOf(fn func() error) error { return fn() }

// PutJobs spaces a batch a nanosecond apart, so a nightly run's rows already
// have a fixed order. This covers the other way jobs arrive: a caller that
// supplies its own timestamp, where sorting on the timestamp alone would
// leave the order to bbolt's iteration and let the page move under whoever
// is watching it.
func TestJobsQueuedTogetherKeepAFixedOrder(t *testing.T) {
	store, err := nodestore.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	queued := time.Now().UTC().Truncate(time.Second)
	for _, account := range []string{"studio", "arkady", "rtflow", "cloud"} {
		if _, err := store.PutJob(nodestore.Job{
			Account: account, Status: job.StatusSuccess, QueuedAt: queued,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Something newer, which must still come first.
	if _, err := store.PutJob(nodestore.Job{
		Account: "later", Status: job.StatusSuccess, QueuedAt: queued.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	var first []string
	for run := 0; run < 5; run++ {
		jobs, err := store.Jobs(0)
		if err != nil {
			t.Fatal(err)
		}
		order := make([]string, 0, len(jobs))
		for _, one := range jobs {
			order = append(order, one.Account)
		}
		if run == 0 {
			first = order
			if order[0] != "later" {
				t.Errorf("newest is not first: %v", order)
			}
			continue
		}
		if !slices.Equal(order, first) {
			t.Errorf("the order moved between reads: %v then %v", first, order)
		}
	}
}

func TestJobsAndRestoresSurviveAReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	store, err := nodestore.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	stored, err := store.PutJob(nodestore.Job{Account: "customer1", Status: job.StatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// A job left running is exactly what a crash leaves behind; the engine
	// has to be able to find it again to close it out.
	reopened, err := nodestore.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	found, err := reopened.Job(stored.ID)
	if err != nil {
		t.Fatalf("Job after reopen: %v", err)
	}
	if found.Status != job.StatusRunning || found.Account != "customer1" {
		t.Errorf("job = %+v", found)
	}
}

func TestLifecycleHistoryIsNewestFirstAndBounded(t *testing.T) {
	store := newStore(t)
	start := time.Now().Add(-time.Hour).UTC()
	for i := 0; i < 105; i++ {
		if _, err := store.PutLifecycleEvent(nodestore.LifecycleEvent{
			Event: "create", Account: "customer1", OK: true,
			At: start.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	events, err := store.LifecycleEvents(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 100 {
		t.Fatalf("retained %d lifecycle events, want 100", len(events))
	}
	if !events[0].At.Equal(start.Add(104*time.Second)) ||
		!events[len(events)-1].At.Equal(start.Add(5*time.Second)) {
		t.Fatalf("lifecycle ordering/retention is wrong: first=%v last=%v",
			events[0].At, events[len(events)-1].At)
	}
}

// Choosing runs across several pages and the pages remember nothing
// between them, so what has been chosen so far is kept here.
func TestABasketRemembersWhatWasChosenAcrossPages(t *testing.T) {
	store, err := nodestore.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if basket, err := store.Basket(nodestore.BasketOfAccount, "c1", "repo1", "snap1"); err != nil || !basket.Empty() {
		t.Fatalf("a basket nobody started = %+v, %v", basket, err)
	}

	if _, err := store.PutInBasket(nodestore.BasketOfAccount, "c1", "repo1", "snap1", nodestore.RestoreSelection{
		Kind: "database", Names: []string{"c1_shop"},
	}); err != nil {
		t.Fatal(err)
	}
	basket, err := store.PutInBasket(nodestore.BasketOfAccount, "c1", "repo1", "snap1", nodestore.RestoreSelection{
		Kind: "dbusers", Names: []string{"c1_shop"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(basket.Items) != 2 || basket.Count() != 2 {
		t.Fatalf("basket = %+v", basket)
	}

	// Choosing a category again replaces what was chosen for it: the page
	// it was chosen on shows what is ticked now, and a basket that
	// disagreed with its own tick boxes would restore what nobody saw.
	basket, err = store.PutInBasket(nodestore.BasketOfAccount, "c1", "repo1", "snap1", nodestore.RestoreSelection{
		Kind: "database", Names: []string{"c1_wp", "c1_blog"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(basket.Items) != 2 || basket.Count() != 3 {
		t.Fatalf("basket = %+v", basket)
	}

	// A basket assembled out of one restore point is not a basket out of
	// another: the same name means different data in each.
	if other, err := store.Basket(nodestore.BasketOfAccount, "c1", "repo1", "snap2"); err != nil || !other.Empty() {
		t.Errorf("another restore point = %+v, %v", other, err)
	}
	// Nor does it belong to another account, whatever the form said.
	if other, err := store.Basket(nodestore.BasketOfAccount, "c2", "repo1", "snap1"); err != nil || !other.Empty() {
		t.Errorf("another account = %+v, %v", other, err)
	}

	if _, err := store.TakeFromBasket(nodestore.BasketOfAccount, "c1", "repo1", "snap1", "database"); err != nil {
		t.Fatal(err)
	}
	read, err := store.Basket(nodestore.BasketOfAccount, "c1", "repo1", "snap1")
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Items) != 1 || read.Items[0].Kind != "dbusers" {
		t.Fatalf("after removing the databases = %+v", read)
	}

	// The last thing taken out leaves nothing behind.
	if _, err := store.TakeFromBasket(nodestore.BasketOfAccount, "c1", "repo1", "snap1", "dbusers"); err != nil {
		t.Fatal(err)
	}
	if read, err := store.Basket(nodestore.BasketOfAccount, "c1", "repo1", "snap1"); err != nil || !read.Empty() {
		t.Errorf("emptied basket = %+v, %v", read, err)
	}
}

// A half-made choice is not evidence of anything, and a future owner of a
// recycled username must not be handed the last customer's.
func TestABasketDoesNotOutliveTheAccountThatMadeIt(t *testing.T) {
	store, err := nodestore.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	for _, snapshot := range []string{"snap1", "snap2"} {
		if _, err := store.PutInBasket(nodestore.BasketOfAccount, "c1", "repo1", snapshot, nodestore.RestoreSelection{
			Kind: "dns",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.PutInBasket(nodestore.BasketOfAccount, "c2", "repo1", "snap1", nodestore.RestoreSelection{
		Kind: "dns",
	}); err != nil {
		t.Fatal(err)
	}

	if err := store.ForgetBaskets("c1"); err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range []string{"snap1", "snap2"} {
		if basket, err := store.Basket(nodestore.BasketOfAccount, "c1", "repo1", snapshot); err != nil || !basket.Empty() {
			t.Errorf("%s survived: %+v, %v", snapshot, basket, err)
		}
	}
	// Somebody else's basket is not theirs to forget.
	if basket, err := store.Basket(nodestore.BasketOfAccount, "c2", "repo1", "snap1"); err != nil || basket.Empty() {
		t.Errorf("another account's basket went with it: %+v, %v", basket, err)
	}
}

// WHM offers parts of an account the recovery centre does not, so a basket
// filled there is not the customer's basket: theirs would name a choice
// their own page can neither show nor start.
func TestAnOperatorsBasketIsNotTheCustomersBasket(t *testing.T) {
	store, err := nodestore.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.PutInBasket(nodestore.BasketOfOperator, "c1", "repo1", "snap1",
		nodestore.RestoreSelection{Kind: "settings"}); err != nil {
		t.Fatal(err)
	}
	if basket, err := store.Basket(nodestore.BasketOfAccount, "c1", "repo1", "snap1"); err != nil ||
		!basket.Empty() {
		t.Errorf("the customer sees the operator's basket: %+v, %v", basket, err)
	}
	if _, err := store.PutInBasket(nodestore.BasketOfAccount, "c1", "repo1", "snap1",
		nodestore.RestoreSelection{Kind: "database", Names: []string{"c1_shop"}}); err != nil {
		t.Fatal(err)
	}
	operator, err := store.Basket(nodestore.BasketOfOperator, "c1", "repo1", "snap1")
	if err != nil || len(operator.Items) != 1 || operator.Items[0].Kind != "settings" {
		t.Errorf("the customer wrote over the operator's basket: %+v, %v", operator, err)
	}

	// Both are still one account's, and go when the account does.
	if err := store.ForgetBaskets("c1"); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []nodestore.BasketOwner{
		nodestore.BasketOfAccount, nodestore.BasketOfOperator,
	} {
		if basket, err := store.Basket(owner, "c1", "repo1", "snap1"); err != nil || !basket.Empty() {
			t.Errorf("%s basket survived: %+v, %v", owner, basket, err)
		}
	}
}

// A button on one row of a picker chooses one thing and says nothing about
// the rows above it, so it adds to what is there. A form of tick boxes
// shows what is chosen now, so it replaces.
func TestAddingOneThingAtATimeKeepsTheRest(t *testing.T) {
	store, err := nodestore.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	for _, name := range []string{"studio.co.il/sales", "studio.co.il/info"} {
		if _, err := store.AddToBasket(nodestore.BasketOfAccount, "c1", "repo1", "snap1", nodestore.RestoreSelection{
			Kind: "mailbox", Names: []string{name},
		}); err != nil {
			t.Fatal(err)
		}
	}
	basket, err := store.Basket(nodestore.BasketOfAccount, "c1", "repo1", "snap1")
	if err != nil {
		t.Fatal(err)
	}
	if len(basket.Items) != 1 || len(basket.Items[0].Names) != 2 {
		t.Fatalf("basket = %+v", basket)
	}
	// The same one twice is still one.
	if _, err := store.AddToBasket(nodestore.BasketOfAccount, "c1", "repo1", "snap1", nodestore.RestoreSelection{
		Kind: "mailbox", Names: []string{"studio.co.il/sales"},
	}); err != nil {
		t.Fatal(err)
	}
	if basket, _ = store.Basket(nodestore.BasketOfAccount, "c1", "repo1", "snap1"); len(basket.Items[0].Names) != 2 {
		t.Errorf("basket = %+v", basket)
	}

	// A part chosen whole stays whole: a list of names beside "all of
	// them" says less than "all of them" does.
	if _, err := store.AddToBasket(nodestore.BasketOfAccount, "c1", "repo1", "snap1", nodestore.RestoreSelection{
		Kind: "dns",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddToBasket(nodestore.BasketOfAccount, "c1", "repo1", "snap1", nodestore.RestoreSelection{
		Kind: "dns", Names: []string{"studio.co.il"},
	}); err != nil {
		t.Fatal(err)
	}
	basket, _ = store.Basket(nodestore.BasketOfAccount, "c1", "repo1", "snap1")
	for _, item := range basket.Items {
		if item.Kind == "dns" && len(item.Names) != 0 {
			t.Errorf("dns = %+v", item)
		}
	}

	// Ticking a form replaces, because the form shows what is ticked.
	if _, err := store.PutInBasket(nodestore.BasketOfAccount, "c1", "repo1", "snap1", nodestore.RestoreSelection{
		Kind: "mailbox", Names: []string{"studio.co.il/info"},
	}); err != nil {
		t.Fatal(err)
	}
	basket, _ = store.Basket(nodestore.BasketOfAccount, "c1", "repo1", "snap1")
	for _, item := range basket.Items {
		if item.Kind == "mailbox" && len(item.Names) != 1 {
			t.Errorf("mailbox = %+v", item)
		}
	}
}

// A customer has gone and asked for their backups to go with them. What is
// left behind after that has to be nothing: a name in a picker, a job in
// the history and a half-made basket are all records of somebody who is
// entitled to have none.
func TestForgettingAnAccountLeavesNothingOfIt(t *testing.T) {
	store, err := nodestore.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	for _, account := range []string{"c1", "c2"} {
		gone := time.Now().UTC()
		if _, err := store.PutIdentity(nodestore.AccountIdentity{
			Account: account, RetiredAt: &gone,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.PutJob(nodestore.Job{
			Account: account, Status: job.StatusSuccess, QueuedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.PutRestore(nodestore.Restore{
			Account: account, RepositoryID: "repo1", SnapshotID: "snap1",
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.PutInBasket(nodestore.BasketOfOperator, account, "repo1", "snap1",
			nodestore.RestoreSelection{Kind: "dns"}); err != nil {
			t.Fatal(err)
		}
	}

	if err := store.ForgetAccount("c1"); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Identity("c1"); !errors.Is(err, nodestore.ErrNotFound) {
		t.Errorf("the account is still known: %v", err)
	}
	jobs, err := store.Jobs(0)
	if err != nil {
		t.Fatal(err)
	}
	restores, err := store.Restores(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range jobs {
		if run.Account == "c1" {
			t.Errorf("a backup of c1 is still recorded")
		}
	}
	for _, run := range restores {
		if run.Account == "c1" {
			t.Errorf("a restore of c1 is still recorded")
		}
	}
	if basket, err := store.Basket(nodestore.BasketOfOperator, "c1", "repo1", "snap1"); err != nil ||
		!basket.Empty() {
		t.Errorf("c1's basket survived: %+v, %v", basket, err)
	}

	// Nobody else's records go with it.
	if _, err := store.Identity("c2"); err != nil {
		t.Errorf("another account was forgotten too: %v", err)
	}
	if len(jobs) != 1 || len(restores) != 1 {
		t.Errorf("kept %d jobs and %d restores", len(jobs), len(restores))
	}
}

// Two writers to one record is normal here: the scheduler records a
// firing on its tick while the operator saves an edit from the page.
// Both read the policy, change their own field and write the whole
// record back, so whichever writes second writes the other's change
// away -- a disable that vanishes while the page says the schedule was
// updated, or a LastRunAt that reverts and queues every account on the
// policy for a second backup tonight.
func TestARecordedFiringDoesNotUndoAnEdit(t *testing.T) {
	store := newStore(t)
	policy, err := store.PutPolicy(nodestore.Policy{Name: "nightly", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	stop, firing, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var firingErr error
	go func() {
		defer close(done)
		first := true
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := store.SetPolicyLastRun(policy.ID, time.Now()); err != nil {
				firingErr = err
				return
			}
			if first {
				close(firing)
				first = false
			}
		}
	}()

	<-firing
	time.Sleep(5 * time.Millisecond)
	edited := policy
	edited.Enabled = false
	if _, err := store.PutPolicy(edited); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	close(stop)
	<-done
	if firingErr != nil {
		t.Fatal(firingErr)
	}

	after, err := store.Policy(policy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Enabled {
		t.Error("the schedule was disabled, and a recorded firing turned it back on")
	}
	if after.LastRunAt == nil {
		t.Error("the firing was not recorded")
	}
}

// A destination is usually removed because the key it holds has leaked.
// The page said it was gone and the sealed key pair stayed in state.db,
// openable with the master key that sits on the same host -- so the one
// action an operator takes to revoke an access key revoked nothing.
func TestRemovingADestinationRevokesWhatOnlyItHeld(t *testing.T) {
	store := newStore(t)
	credentials, err := store.PutSecret("s3", []byte("sealed key pair"), "master")
	if err != nil {
		t.Fatal(err)
	}
	password, err := store.PutSecret("restic", []byte("sealed password"), "master")
	if err != nil {
		t.Fatal(err)
	}
	shared, err := store.PutSecret("s3", []byte("sealed and used twice"), "master")
	if err != nil {
		t.Fatal(err)
	}

	going, err := store.PutDestination(nodestore.Destination{
		Name: "offsite", CredentialsSecretID: credentials,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutRepository(nodestore.Repository{
		Path: "offsite/repo", DestinationID: going.ID, PasswordSecretID: password,
	}); err != nil {
		t.Fatal(err)
	}
	// A second repository here is sealed under the same password as one
	// somewhere else, which is not this destination's to revoke.
	if _, err := store.PutRepository(nodestore.Repository{
		Path: "offsite/other", DestinationID: going.ID, PasswordSecretID: shared,
	}); err != nil {
		t.Fatal(err)
	}
	staying, err := store.PutDestination(nodestore.Destination{Name: "second"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutRepository(nodestore.Repository{
		Path: "second/repo", DestinationID: staying.ID, PasswordSecretID: shared,
	}); err != nil {
		t.Fatal(err)
	}

	if err := store.DeleteDestination(going.ID); err != nil {
		t.Fatal(err)
	}
	for what, id := range map[string]string{
		"the destination's credentials": credentials,
		"the repository's password":     password,
	} {
		if _, err := store.Secret(id); !errors.Is(err, nodestore.ErrNotFound) {
			t.Errorf("%s is still in the state file: %v", what, err)
		}
	}
	if _, err := store.Secret(shared); err != nil {
		t.Errorf("a password another repository is still sealed under was revoked: %v", err)
	}
}

// The same for somewhere notifications are sent: a webhook URL with a
// token in it is a credential, and removing the channel is how it is
// taken back.
func TestRemovingAChannelRevokesItsCredentials(t *testing.T) {
	store := newStore(t)
	sealed, err := store.PutSecret("webhook", []byte("sealed url"), "master")
	if err != nil {
		t.Fatal(err)
	}
	channel, err := store.PutChannel(nodestore.Channel{Name: "ops", SecretsID: sealed})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteChannel(channel.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Secret(sealed); !errors.Is(err, nodestore.ErrNotFound) {
		t.Errorf("the channel's credentials are still in the state file: %v", err)
	}
}
