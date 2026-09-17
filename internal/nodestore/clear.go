package nodestore

import (
	"encoding/json"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Clearing the history.
//
// The history is a log and also a record: the overview, termination
// protection and the restore page read it to say whether an account has a
// copy, how fresh, and whether a snapshot has a hole in it. So nothing
// here decides what goes. The caller says what to keep, and the store
// removes the rest in one transaction, deciding each row as it stands at
// that moment -- a run that started after the caller looked is decided
// on what it is now, not on what it was.

// ClearJobs removes every backup run keep does not hold on to, and
// returns what it removed. A run that made a snapshot restic could not
// read every file for leaves that mark behind (IncompleteSnapshotMarks).
// A document that cannot be read is left alone.
func (s *Store) ClearJobs(keep func(Job) bool) ([]Job, error) {
	var removed []Job
	err := s.db.Update(func(tx *bolt.Tx) error {
		removed = nil
		jobs, marks := tx.Bucket(bucketJobs), tx.Bucket(bucketIncomplete)
		var gone [][]byte
		err := jobs.ForEach(func(key, raw []byte) error {
			var stored Job
			if err := json.Unmarshal(raw, &stored); err != nil {
				return nil
			}
			if !stored.Status.Terminal() || keep(stored) {
				return nil
			}
			gone = append(gone, append([]byte(nil), key...))
			removed = append(removed, stored)
			return nil
		})
		if err != nil {
			return err
		}
		noted, _ := json.Marshal(struct {
			At time.Time `json:"at"`
		}{time.Now().UTC()})
		for _, stored := range removed {
			for _, target := range stored.Targets {
				if !target.Incomplete || target.SnapshotID == "" || target.RepositoryID == "" {
					continue
				}
				if err := marks.Put([]byte(target.RepositoryID+"/"+target.SnapshotID), noted); err != nil {
					return err
				}
			}
		}
		for _, key := range gone {
			if err := jobs.Delete(key); err != nil {
				return err
			}
		}
		return nil
	})
	return removed, err
}

// IncompleteSnapshotMarks are the snapshots of one repository marked as
// having files restic could not read, by runs since cleared away.
func (s *Store) IncompleteSnapshotMarks(repositoryID string) (map[string]bool, error) {
	marked := map[string]bool{}
	prefix := repositoryID + "/"
	err := s.db.View(func(tx *bolt.Tx) error {
		cursor := tx.Bucket(bucketIncomplete).Cursor()
		for key, _ := cursor.Seek([]byte(prefix)); key != nil && strings.HasPrefix(string(key), prefix); key, _ = cursor.Next() {
			marked[strings.TrimPrefix(string(key), prefix)] = true
		}
		return nil
	})
	return marked, err
}

// ClearRestores removes every finished restore keep does not hold on to,
// and returns how many went.
func (s *Store) ClearRestores(keep func(Restore) bool) (int, error) {
	removed := 0
	err := s.db.Update(func(tx *bolt.Tx) error {
		removed = 0
		bucket := tx.Bucket(bucketRestores)
		var gone [][]byte
		err := bucket.ForEach(func(key, raw []byte) error {
			var stored Restore
			if err := json.Unmarshal(raw, &stored); err != nil {
				return nil
			}
			if !stored.Status.Terminal() || keep(stored) {
				return nil
			}
			gone = append(gone, append([]byte(nil), key...))
			return nil
		})
		if err != nil {
			return err
		}
		for _, key := range gone {
			if err := bucket.Delete(key); err != nil {
				return err
			}
		}
		removed = len(gone)
		return nil
	})
	return removed, err
}

// ClearLifecycle removes every account lifecycle event. Nothing reads
// them but the pages that show them.
func (s *Store) ClearLifecycle() (int, error) {
	removed := 0
	err := s.db.Update(func(tx *bolt.Tx) error {
		removed = 0
		bucket := tx.Bucket(bucketLifecycle)
		var gone [][]byte
		err := bucket.ForEach(func(key, _ []byte) error {
			gone = append(gone, append([]byte(nil), key...))
			return nil
		})
		if err != nil {
			return err
		}
		for _, key := range gone {
			if err := bucket.Delete(key); err != nil {
				return err
			}
		}
		removed = len(gone)
		return nil
	})
	return removed, err
}
