// Package nodestore is the state a standalone cPanel server keeps for
// itself: its destinations, repositories, schedules and job history.
//
// Fleet mode keeps the same things in PostgreSQL on a controller. The types
// here deliberately mirror internal/store's field names and semantics, so
// that moving a standalone server into a fleet is a data copy rather than a
// translation. See docs/adr/0007-standalone-mode.md.
package nodestore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

// ErrNotFound means no record matched.
var ErrNotFound = errors.New("nodestore: not found")

// Bucket names. Each holds JSON documents keyed by id.
var (
	bucketDestinations = []byte("destinations")
	bucketRepositories = []byte("repositories")
	bucketPolicies     = []byte("policies")
	bucketAccounts     = []byte("accounts")
	bucketJobs         = []byte("jobs")
	bucketRestores     = []byte("restores")
	bucketSecrets      = []byte("secrets")
	bucketSettings     = []byte("settings")
	bucketChannels     = []byte("channels")
	bucketIdentities   = []byte("account_identities")
	bucketLifecycle    = []byte("lifecycle_events")
	bucketBaskets      = []byte("baskets")
	// bucketSources holds what an operator chose to back up on a server
	// without a panel (panel.Source), keyed by name.
	bucketSources = []byte("sources")
)

var allBuckets = [][]byte{
	bucketDestinations, bucketRepositories, bucketPolicies, bucketAccounts,
	bucketJobs, bucketRestores, bucketSecrets, bucketSettings,
	bucketChannels,
	bucketIdentities,
	bucketLifecycle,
	bucketBaskets,
	bucketSources,
}

// Store is the on-disk state file.
type Store struct {
	db *bolt.DB
}

// Open creates or opens the state file, which holds sealed credentials and
// so is owner-only.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("nodestore: create %s: %w", filepath.Dir(path), err)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("nodestore: open %s: %w", path, err)
	}

	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range allBuckets {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return fmt.Errorf("nodestore: create bucket %s: %w", name, err)
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close flushes and releases the state file.
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// put stores a JSON document.
func (s *Store) put(bucket []byte, id string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("nodestore: encode %s: %w", bucket, err)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucket).Put([]byte(id), encoded)
	})
}

// change applies one change to one record inside a single transaction.
//
// A read in one transaction followed by a write in a later one is a lost
// update: two writers each read the record, each alter their own field,
// and whichever writes second writes the other's change away. The
// scheduler, the queue and the pages run as concurrent goroutines over
// one store, so two writers at one record is ordinary rather than rare.
// A schedule disabled from the page came back on when the scheduler
// recorded a firing it had read before the edit.
func change[T any](s *Store, bucket []byte, id string, apply func(*T)) (T, error) {
	var record T
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		raw := b.Get([]byte(id))
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &record); err != nil {
			return fmt.Errorf("nodestore: decode %s/%s: %w", bucket, id, err)
		}
		apply(&record)
		encoded, err := json.Marshal(record)
		if err != nil {
			return fmt.Errorf("nodestore: encode %s: %w", bucket, err)
		}
		return b.Put([]byte(id), encoded)
	})
	if err != nil {
		var nothing T
		return nothing, err
	}
	return record, nil
}

// get reads one JSON document.
func (s *Store) get(bucket []byte, id string, target any) error {
	return s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucket).Get([]byte(id))
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, target); err != nil {
			return fmt.Errorf("nodestore: decode %s/%s: %w", bucket, id, err)
		}
		return nil
	})
}

// delete removes one document, reporting ErrNotFound when it was not there.
func (s *Store) delete(bucket []byte, id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b.Get([]byte(id)) == nil {
			return ErrNotFound
		}
		return b.Delete([]byte(id))
	})
}

// forEach walks a bucket in key order, decoding each document.
func (s *Store) forEach(bucket []byte, decode func(id string, raw []byte) error) error {
	return s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucket).ForEach(func(key, raw []byte) error {
			return decode(string(key), raw)
		})
	})
}
