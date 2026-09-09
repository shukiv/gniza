package webui

import (
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/nodestore"
)

func measured(at time.Time, snapshots int, size uint64) nodestore.RepositoryCensus {
	return nodestore.RepositoryCensus{Snapshots: snapshots, SizeBytes: size, MeasuredAt: &at}
}

func TestWhatEveryDestinationHoldsIsOneFigure(t *testing.T) {
	now := time.Now()
	held := holdingsOf([]destinationView{
		{Repository: nodestore.Repository{Census: measured(now.Add(-time.Hour), 294, 100<<30)}},
		{Repository: nodestore.Repository{Census: measured(now.Add(-3*time.Hour), 118, 40<<30)}},
	}, now)
	if held.Copies != 412 || held.Stored != 140<<30 {
		t.Fatalf("holdings = %+v", held)
	}
	if !held.Known() {
		t.Fatal("two measured repositories read as nothing measured")
	}
	// The oldest reading is the one the page has to date itself by: a
	// total is only as current as its stalest part.
	if !held.MeasuredAt.Equal(now.Add(-3 * time.Hour)) {
		t.Fatalf("dated %s, want the older of the two readings", held.MeasuredAt)
	}
	if held.Stale(now) {
		t.Fatal("a reading three hours old was called stale")
	}
}

// A total that leaves a repository out is not a total. Say how many were
// counted rather than presenting part of the answer as all of it.
func TestARepositoryNotYetMeasuredIsCountedAsMissing(t *testing.T) {
	now := time.Now()
	held := holdingsOf([]destinationView{
		{Repository: nodestore.Repository{Census: measured(now, 294, 100<<30)}},
		{Repository: nodestore.Repository{}},
	}, now)
	if held.Pending != 1 || held.Measured != 1 {
		t.Fatalf("holdings = %+v", held)
	}
	if !held.Partial() {
		t.Fatal("a total missing one of two repositories was presented as complete")
	}
}

func TestNothingMeasuredYetIsNotAServerHoldingNothing(t *testing.T) {
	now := time.Now()
	held := holdingsOf([]destinationView{{Repository: nodestore.Repository{}}}, now)
	if held.Known() {
		t.Fatalf("holdings = %+v, want nothing to show", held)
	}
}

// A reading old enough to have missed several censuses is shown with its
// date, so that a figure from before a destination went unreachable is
// not read as today's.
func TestAnOldReadingIsMarkedAsOld(t *testing.T) {
	now := time.Now()
	held := holdingsOf([]destinationView{
		{Repository: nodestore.Repository{Census: measured(now.Add(-2*24*time.Hour), 294, 100<<30)}},
	}, now)
	if !held.Stale(now) {
		t.Fatal("a reading two days old was presented as current")
	}
}

// Why the figure is old belongs beside it: the last attempt failed, and
// the operator needs the reason, not only the date.
func TestARepositoryThatCannotBeMeasuredIsCounted(t *testing.T) {
	now := time.Now()
	attempted := now.Add(-time.Hour)
	census := measured(now.Add(-2*24*time.Hour), 294, 100<<30)
	census.AttemptedAt, census.LastError = &attempted, "unable to open config file"
	held := holdingsOf([]destinationView{{Repository: nodestore.Repository{Census: census}}}, now)
	if held.Failing != 1 {
		t.Fatalf("holdings = %+v", held)
	}
	if held.Copies != 294 {
		t.Fatalf("the last good reading was dropped: %+v", held)
	}
}
