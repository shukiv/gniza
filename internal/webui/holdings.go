package webui

import (
	"time"

	"github.com/shukiv/gniza/internal/nodestore"
)

// holdings is what this server's repositories hold, added up from the
// last census of each.
//
// Nothing here runs restic. The numbers were measured on a slow cadence
// and stored, so the page draws whether or not the destinations can be
// reached, and says when each figure was true rather than pretending it
// is a live reading.
type holdings struct {
	// Copies is every snapshot in every repository, and Stored is what
	// they cost the storage under them after deduplication.
	Copies int
	Stored uint64
	// MeasuredAt is the oldest reading in the total: a sum is only as
	// current as its stalest part.
	MeasuredAt time.Time
	// Measured, Pending and Failing count repositories, not snapshots:
	// how many the total is made of, how many have never been measured,
	// and how many could not be measured last time it was tried.
	Measured int
	Pending  int
	Failing  int
}

// Known reports whether anything at all has been measured.
func (h holdings) Known() bool { return h.Measured > 0 }

// Partial says the total leaves a repository out, which the page has to
// say rather than presenting part of the answer as all of it.
func (h holdings) Partial() bool { return h.Pending > 0 }

// Stale says the oldest reading in the total is old enough to show its
// date beside it.
func (h holdings) Stale(now time.Time) bool {
	return h.Known() && now.Sub(h.MeasuredAt) > nodestore.StaleAfter
}

// holdingsOf adds up the census of every destination's repository.
func holdingsOf(destinations []destinationView, now time.Time) holdings {
	var held holdings
	for _, dest := range destinations {
		census := dest.Repository.Census
		if census.LastError != "" {
			held.Failing++
		}
		if !census.Known() {
			// A repository nothing has ever been written to holds
			// nothing to measure, and the census leaves it alone. It is
			// not a measurement anybody is waiting for.
			if dest.Repository.InitialisedAt != nil {
				held.Pending++
			}
			continue
		}
		held.Measured++
		held.Copies += census.Snapshots
		held.Stored += census.SizeBytes
		if held.MeasuredAt.IsZero() || census.MeasuredAt.Before(held.MeasuredAt) {
			held.MeasuredAt = *census.MeasuredAt
		}
	}
	return held
}
