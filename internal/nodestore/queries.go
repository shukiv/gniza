package nodestore

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/shukiv/gniza/internal/job"
	bolt "go.etcd.io/bbolt"
)

// NewID returns a random identifier for a stored record.
func NewID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		// crypto/rand failing means the system is in no state to be
		// taking backups either.
		panic("nodestore: no randomness available: " + err.Error())
	}
	return hex.EncodeToString(raw)
}

// --- settings ---

const settingsKey = "node"

// Settings reads the node's configuration, returning defaults when it has
// not been configured yet.
func (s *Store) Settings() (Settings, error) {
	var settings Settings
	err := s.get(bucketSettings, settingsKey, &settings)
	if errors.Is(err, ErrNotFound) {
		return DefaultSettings(), nil
	}
	if err != nil {
		return Settings{}, err
	}
	return settings, nil
}

// DefaultSettings matches fleet mode's defaults exactly. Snapshot paths
// embed the staging root, so a standalone server that later joins a fleet
// must have been using the same one all along.
func DefaultSettings() Settings {
	return Settings{
		StagingRoot:   "/var/lib/gniza/staging",
		MaxConcurrent: 1,
		SafetyMargin:  0.2,
		ResticBinary:  "restic",
		ResticCache:   "/var/cache/gniza/restic",
		ConfigDir:     "/etc/gniza",
		LogLevel:      DefaultLogLevel,
	}
}

// LogLevels are what the service log can be turned down or up to, quietest
// first. They are slog's own names, so the setting, the -log-level flag and
// the level written into every line all spell a level the same way.
var LogLevels = []string{"error", "warn", "info", "debug"}

// DefaultLogLevel is what a server logs at until somebody changes it.
const DefaultLogLevel = "info"

// SaveSettings writes the node's configuration.
func (s *Store) SaveSettings(settings Settings) error {
	return s.put(bucketSettings, settingsKey, settings)
}

// updateKey holds the last update check beside the settings, which is
// where the rest of this server's own state about itself lives.
const updateKey = "update"

// UpdateState reads what the last check for a newer release found. A
// server that has never checked reports a zero time, not an error.
func (s *Store) UpdateState() (UpdateState, error) {
	var state UpdateState
	err := s.get(bucketSettings, updateKey, &state)
	if errors.Is(err, ErrNotFound) {
		return UpdateState{}, nil
	}
	if err != nil {
		return UpdateState{}, err
	}
	return state, nil
}

// SaveUpdateState records what a check found, including that it failed.
func (s *Store) SaveUpdateState(state UpdateState) error {
	return s.put(bucketSettings, updateKey, state)
}

// upgradeKey holds the upgrade that is running, or the last one that ran.
const upgradeKey = "upgrade"

// UpgradeState reads the upgrade in flight, or the last one. A server that
// has never upgraded reports a zero value, not an error.
func (s *Store) UpgradeState() (UpgradeState, error) {
	var state UpgradeState
	err := s.get(bucketSettings, upgradeKey, &state)
	if errors.Is(err, ErrNotFound) {
		return UpgradeState{}, nil
	}
	if err != nil {
		return UpgradeState{}, err
	}
	return state, nil
}

// SaveUpgradeState records how far an upgrade got.
func (s *Store) SaveUpgradeState(state UpgradeState) error {
	return s.put(bucketSettings, upgradeKey, state)
}

// --- secrets ---

// PutSecret stores a sealed credential and returns its id.
func (s *Store) PutSecret(kind string, ciphertext []byte, keyID string) (string, error) {
	secret := Secret{
		ID: NewID(), Kind: kind, Ciphertext: ciphertext,
		KeyID: keyID, CreatedAt: time.Now().UTC(),
	}
	if err := s.put(bucketSecrets, secret.ID, secret); err != nil {
		return "", err
	}
	return secret.ID, nil
}

// Secret reads a sealed credential.
func (s *Store) Secret(id string) (Secret, error) {
	var secret Secret
	if err := s.get(bucketSecrets, id, &secret); err != nil {
		return Secret{}, err
	}
	return secret, nil
}

// --- destinations ---

// PutDestination stores a destination, assigning an id when it has none.
func (s *Store) PutDestination(dest Destination) (Destination, error) {
	if dest.ID == "" {
		dest.ID = NewID()
	}
	if dest.CreatedAt.IsZero() {
		dest.CreatedAt = time.Now().UTC()
	}
	if dest.Config == nil {
		dest.Config = map[string]string{}
	}
	return dest, s.put(bucketDestinations, dest.ID, dest)
}

// Destination reads one destination.
func (s *Store) Destination(id string) (Destination, error) {
	var dest Destination
	if err := s.get(bucketDestinations, id, &dest); err != nil {
		return Destination{}, err
	}
	return dest, nil
}

// Destinations lists every destination, by name.
func (s *Store) Destinations() ([]Destination, error) {
	var destinations []Destination
	err := s.forEach(bucketDestinations, func(_ string, raw []byte) error {
		var dest Destination
		if err := json.Unmarshal(raw, &dest); err != nil {
			return err
		}
		destinations = append(destinations, dest)
		return nil
	})
	sort.Slice(destinations, func(i, j int) bool {
		return destinations[i].Name < destinations[j].Name
	})
	return destinations, err
}

// DeleteDestination removes a destination and the repository record that
// belongs to it.
//
// The backups already stored there are not touched — this only forgets how
// to reach them. It is refused while a schedule still points at the
// repository, because that schedule would then silently stop making one of
// the copies it promises.
// All of it in one transaction: a failure partway through a record at a
// time leaves a destination whose repository records are already gone,
// and with them the only pointers to their restic passwords.
func (s *Store) DeleteDestination(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		destinations := tx.Bucket(bucketDestinations)
		raw := destinations.Get([]byte(id))
		if raw == nil {
			return ErrNotFound
		}
		var dest Destination
		if err := json.Unmarshal(raw, &dest); err != nil {
			return fmt.Errorf("nodestore: decode destination %s: %w", id, err)
		}

		owned := map[string]bool{}
		revoke := []string{dest.CredentialsSecretID}
		repositories := tx.Bucket(bucketRepositories)
		if err := repositories.ForEach(func(key, raw []byte) error {
			var repo Repository
			if err := json.Unmarshal(raw, &repo); err != nil {
				return fmt.Errorf("nodestore: decode repository %s: %w", key, err)
			}
			if repo.DestinationID != id {
				return nil
			}
			owned[repo.ID] = true
			revoke = append(revoke, repo.PasswordSecretID)
			return nil
		}); err != nil {
			return err
		}

		if err := tx.Bucket(bucketPolicies).ForEach(func(key, raw []byte) error {
			var policy Policy
			if err := json.Unmarshal(raw, &policy); err != nil {
				return fmt.Errorf("nodestore: decode policy %s: %w", key, err)
			}
			for _, target := range policy.RepositoryIDs {
				if owned[target] {
					return fmt.Errorf(
						"nodestore: the schedule %q still sends backups here; "+
							"remove it from that schedule first", policy.Name)
				}
			}
			return nil
		}); err != nil {
			return err
		}

		for repoID := range owned {
			if err := repositories.Delete([]byte(repoID)); err != nil {
				return err
			}
		}
		if err := destinations.Delete([]byte(id)); err != nil {
			return err
		}
		return revokeSecrets(tx, revoke)
	})
}

// revokeSecrets removes sealed credentials nothing points at any more.
//
// A destination is usually removed because the key it holds has leaked,
// so the sealed copy goes with the record that named it: leaving it
// behind leaves an access key openable with the master key on the same
// host, after the one action an operator takes to revoke it. A secret
// two records share is not this one's to revoke, so anything still
// pointed at is left where it is.
func revokeSecrets(tx *bolt.Tx, ids []string) error {
	wanted := map[string]bool{}
	for _, id := range ids {
		if id != "" {
			wanted[id] = true
		}
	}
	if len(wanted) == 0 {
		return nil
	}

	held := map[string]bool{}
	scan := func(bucket []byte, of func([]byte) (string, error)) error {
		return tx.Bucket(bucket).ForEach(func(key, raw []byte) error {
			id, err := of(raw)
			if err != nil {
				return fmt.Errorf("nodestore: decode %s/%s: %w", bucket, key, err)
			}
			if id != "" {
				held[id] = true
			}
			return nil
		})
	}
	if err := scan(bucketDestinations, func(raw []byte) (string, error) {
		var dest Destination
		err := json.Unmarshal(raw, &dest)
		return dest.CredentialsSecretID, err
	}); err != nil {
		return err
	}
	if err := scan(bucketRepositories, func(raw []byte) (string, error) {
		var repo Repository
		err := json.Unmarshal(raw, &repo)
		return repo.PasswordSecretID, err
	}); err != nil {
		return err
	}
	if err := scan(bucketChannels, func(raw []byte) (string, error) {
		var channel Channel
		err := json.Unmarshal(raw, &channel)
		return channel.SecretsID, err
	}); err != nil {
		return err
	}

	secrets := tx.Bucket(bucketSecrets)
	for id := range wanted {
		if held[id] {
			continue
		}
		if err := secrets.Delete([]byte(id)); err != nil {
			return err
		}
	}
	return nil
}

// --- repositories ---

// PutRepository stores a repository, filling in the chunker source.
//
// Chunker parameters are fixed when a repository is created and can never
// change, so every repository after the first must copy them from the
// first. Fleet mode enforces this with a database trigger; standalone mode
// has to do it here. See docs/DESIGN.md §7.
func (s *Store) PutRepository(repo Repository) (Repository, error) {
	if repo.ID != "" {
		return repo, s.put(bucketRepositories, repo.ID, repo)
	}

	existing, err := s.Repositories()
	if err != nil {
		return Repository{}, err
	}
	repo.ID = NewID()
	repo.CreatedAt = time.Now().UTC()
	if repo.ChunkerSourceRepoID == "" && len(existing) > 0 {
		repo.ChunkerSourceRepoID = chunkerSource(existing)
	}
	return repo, s.put(bucketRepositories, repo.ID, repo)
}

// chunkerSource picks the server's first repository, which is the one every
// later repository copies its chunker parameters from.
func chunkerSource(existing []Repository) string {
	oldest := existing[0]
	for _, repo := range existing[1:] {
		if repo.CreatedAt.Before(oldest.CreatedAt) {
			oldest = repo
		}
	}
	// Follow the chain to its root, so a third repository points at the
	// same source as the second rather than at the second itself.
	byID := map[string]Repository{}
	for _, repo := range existing {
		byID[repo.ID] = repo
	}
	for seen := 0; oldest.ChunkerSourceRepoID != "" && seen < len(existing); seen++ {
		parent, present := byID[oldest.ChunkerSourceRepoID]
		if !present {
			break
		}
		oldest = parent
	}
	return oldest.ID
}

// Repository reads one repository.
func (s *Store) Repository(id string) (Repository, error) {
	var repo Repository
	if err := s.get(bucketRepositories, id, &repo); err != nil {
		return Repository{}, err
	}
	return repo, nil
}

// Repositories lists every repository, oldest first.
func (s *Store) Repositories() ([]Repository, error) {
	var repos []Repository
	err := s.forEach(bucketRepositories, func(_ string, raw []byte) error {
		var repo Repository
		if err := json.Unmarshal(raw, &repo); err != nil {
			return err
		}
		repos = append(repos, repo)
		return nil
	})
	sort.Slice(repos, func(i, j int) bool { return repos[i].CreatedAt.Before(repos[j].CreatedAt) })
	return repos, err
}

// MarkRepositoryInitialised records that "restic init" succeeded.
func (s *Store) MarkRepositoryInitialised(id string) error {
	_, err := s.ChangeRepository(id, func(repo *Repository) {
		now := time.Now().UTC()
		repo.InitialisedAt = &now
	})
	return err
}

// ChangeRepository alters one repository in place, reading and writing it
// inside one transaction so a concurrent writer's change is not lost.
func (s *Store) ChangeRepository(id string, apply func(*Repository)) (Repository, error) {
	return change(s, bucketRepositories, id, apply)
}

// --- policies ---

// PutPolicy stores a policy.
func (s *Store) PutPolicy(policy Policy) (Policy, error) {
	if policy.ID == "" {
		policy.ID = NewID()
		policy.CreatedAt = time.Now().UTC()
	}
	if policy.PayloadMode == "" {
		policy.PayloadMode = "split"
	}
	if policy.Compression == "" {
		policy.Compression = "auto"
	}
	return policy, s.put(bucketPolicies, policy.ID, policy)
}

// Policy reads one policy.
func (s *Store) Policy(id string) (Policy, error) {
	var policy Policy
	if err := s.get(bucketPolicies, id, &policy); err != nil {
		return Policy{}, err
	}
	return policy, nil
}

// Policies lists every policy, by name.
func (s *Store) Policies() ([]Policy, error) {
	var policies []Policy
	err := s.forEach(bucketPolicies, func(_ string, raw []byte) error {
		var policy Policy
		if err := json.Unmarshal(raw, &policy); err != nil {
			return err
		}
		policies = append(policies, policy)
		return nil
	})
	sort.Slice(policies, func(i, j int) bool { return policies[i].Name < policies[j].Name })
	return policies, err
}

// DeletePolicy removes a policy. Its job history is kept.
func (s *Store) DeletePolicy(id string) error { return s.delete(bucketPolicies, id) }

// SetPolicyLastRun records a firing, so a restart neither skips a window
// nor replays past ones.
func (s *Store) SetPolicyLastRun(id string, at time.Time) error {
	at = at.UTC()
	_, err := change(s, bucketPolicies, id, func(policy *Policy) {
		policy.LastRunAt = &at
	})
	return err
}

// --- notification channels ---

// PutChannel stores somewhere to send notifications.
func (s *Store) PutChannel(channel Channel) (Channel, error) {
	if channel.ID == "" {
		channel.ID = NewID()
		channel.CreatedAt = time.Now().UTC()
	}
	return channel, s.put(bucketChannels, channel.ID, channel)
}

// Channel reads one back.
func (s *Store) Channel(id string) (Channel, error) {
	var channel Channel
	return channel, s.get(bucketChannels, id, &channel)
}

// Channels lists them, oldest first, so the order does not move about.
func (s *Store) Channels() ([]Channel, error) {
	var channels []Channel
	err := s.forEach(bucketChannels, func(_ string, raw []byte) error {
		var channel Channel
		if err := json.Unmarshal(raw, &channel); err != nil {
			return err
		}
		channels = append(channels, channel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(channels, func(i, j int) bool {
		return channels[i].CreatedAt.Before(channels[j].CreatedAt)
	})
	return channels, nil
}

// DeleteChannel removes one.
func (s *Store) DeleteChannel(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		channels := tx.Bucket(bucketChannels)
		raw := channels.Get([]byte(id))
		if raw == nil {
			return ErrNotFound
		}
		var channel Channel
		if err := json.Unmarshal(raw, &channel); err != nil {
			return fmt.Errorf("nodestore: decode channel %s: %w", id, err)
		}
		if err := channels.Delete([]byte(id)); err != nil {
			return err
		}
		return revokeSecrets(tx, []string{channel.SecretsID})
	})
}

// --- jobs ---

// PutJob stores a job.
func (s *Store) PutJob(j Job) (Job, error) {
	if j.ID == "" {
		j.ID = NewID()
		j.QueuedAt = time.Now().UTC()
	}
	return j, s.put(bucketJobs, j.ID, j)
}

// PutJobs stores a sequence of queued jobs atomically. Their timestamps are
// ordered a nanosecond apart so PendingWork processes the policy plan in the
// order supplied, even though bbolt keys are random IDs.
func (s *Store) PutJobs(jobs []Job) ([]Job, error) {
	now := time.Now().UTC()
	encoded := make([][]byte, len(jobs))
	for i := range jobs {
		if jobs[i].ID == "" {
			jobs[i].ID = NewID()
		}
		if jobs[i].QueuedAt.IsZero() {
			jobs[i].QueuedAt = now.Add(time.Duration(i) * time.Nanosecond)
		}
		if jobs[i].Status == "" {
			jobs[i].Status = job.StatusPending
		}
		var err error
		encoded[i], err = json.Marshal(jobs[i])
		if err != nil {
			return nil, fmt.Errorf("nodestore: encode jobs: %w", err)
		}
	}
	err := s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketJobs)
		for i := range jobs {
			if err := bucket.Put([]byte(jobs[i].ID), encoded[i]); err != nil {
				return err
			}
		}
		return nil
	})
	return jobs, err
}

// Job reads one job.
// SetJobProgress records how far a running job has got.
//
// It is a read-modify-write of one record rather than a field update,
// which bbolt has no notion of. Progress arrives about once a second per
// job and the caller throttles it further, so this stays cheap. A job that
// has already finished is left alone: a late status line must not reopen
// a closed record.
// SetRestoreProgress records how far a running restore has got.
//
// A restore that has finished keeps whatever it finished as: progress
// arriving late must not reopen it, and a percentage beside a finished
// restore would be read as one still running.
func (s *Store) SetRestoreProgress(id string, progress RestoreProgress) error {
	_, err := change(s, bucketRestores, id, func(stored *Restore) {
		if stored.Status.Terminal() {
			return
		}
		stored.Progress = &progress
	})
	return err
}

func (s *Store) SetJobProgress(id string, progress JobProgress) error {
	_, err := change(s, bucketJobs, id, func(stored *Job) {
		if stored.Status.Terminal() {
			return
		}
		stored.Progress = &progress
	})
	return err
}

func (s *Store) Job(id string) (Job, error) {
	var j Job
	if err := s.get(bucketJobs, id, &j); err != nil {
		return Job{}, err
	}
	return j, nil
}

// Jobs lists job history, newest first, capped at limit (zero means all).
func (s *Store) Jobs(limit int) ([]Job, error) {
	var jobs []Job
	err := s.forEach(bucketJobs, func(_ string, raw []byte) error {
		var j Job
		if err := json.Unmarshal(raw, &j); err != nil {
			return err
		}
		jobs = append(jobs, j)
		return nil
	})
	// Newest first, and a fixed order within one minute: a nightly run
	// queues nineteen accounts on the same timestamp, and an arbitrary
	// order among them means the rows move under the operator on every
	// refresh.
	sort.Slice(jobs, func(i, j int) bool {
		if !jobs[i].QueuedAt.Equal(jobs[j].QueuedAt) {
			return jobs[i].QueuedAt.After(jobs[j].QueuedAt)
		}
		if jobs[i].Account != jobs[j].Account {
			return jobs[i].Account < jobs[j].Account
		}
		return jobs[i].ID < jobs[j].ID
	})
	if limit > 0 && len(jobs) > limit {
		jobs = jobs[:limit]
	}
	return jobs, err
}

// RunningJobFor reports whether an account already has work in flight.
// Backups and restores of one account stage in the same place, so they
// cannot overlap.
func (s *Store) RunningJobFor(account string) (bool, error) {
	jobs, err := s.Jobs(0)
	if err != nil {
		return false, err
	}
	for _, j := range jobs {
		if j.Account == account && !j.Status.Terminal() {
			return true, nil
		}
	}
	restores, err := s.Restores(0)
	if err != nil {
		return false, err
	}
	for _, restore := range restores {
		if restore.Account == account && !restore.Status.Terminal() {
			return true, nil
		}
	}
	return false, nil
}

// --- restores ---

// PutRestore stores a restore.
func (s *Store) PutRestore(restore Restore) (Restore, error) {
	if restore.ID == "" {
		restore.ID = NewID()
		restore.QueuedAt = time.Now().UTC()
	}
	if restore.Status == "" {
		restore.Status = job.StatusPending
	}
	return restore, s.put(bucketRestores, restore.ID, restore)
}

// Restore reads one restore.
func (s *Store) Restore(id string) (Restore, error) {
	var restore Restore
	if err := s.get(bucketRestores, id, &restore); err != nil {
		return Restore{}, err
	}
	return restore, nil
}

// Restores lists restore history, newest first, capped at limit.
func (s *Store) Restores(limit int) ([]Restore, error) {
	var restores []Restore
	err := s.forEach(bucketRestores, func(_ string, raw []byte) error {
		var restore Restore
		if err := json.Unmarshal(raw, &restore); err != nil {
			return err
		}
		restores = append(restores, restore)
		return nil
	})
	sort.Slice(restores, func(i, j int) bool {
		if !restores[i].QueuedAt.Equal(restores[j].QueuedAt) {
			return restores[i].QueuedAt.After(restores[j].QueuedAt)
		}
		if restores[i].Account != restores[j].Account {
			return restores[i].Account < restores[j].Account
		}
		return restores[i].ID < restores[j].ID
	})
	if limit > 0 && len(restores) > limit {
		restores = restores[:limit]
	}
	return restores, err
}

// PendingWork returns the queued backup and restore, if any, oldest first.
// Restores come back first: someone is usually waiting for one.
func (s *Store) PendingWork() (*Restore, *Job, error) {
	restores, err := s.Restores(0)
	if err != nil {
		return nil, nil, err
	}
	for i := len(restores) - 1; i >= 0; i-- {
		if restores[i].Status == job.StatusPending {
			return &restores[i], nil, nil
		}
	}

	jobs, err := s.Jobs(0)
	if err != nil {
		return nil, nil, err
	}
	for i := len(jobs) - 1; i >= 0; i-- {
		if jobs[i].Status == job.StatusPending {
			return nil, &jobs[i], nil
		}
	}
	return nil, nil, nil
}

// --- account identities ---

// PutIdentity records which unix account a cPanel name means.
func (s *Store) PutIdentity(identity AccountIdentity) (AccountIdentity, error) {
	now := time.Now().UTC()
	if identity.CreatedAt.IsZero() {
		identity.CreatedAt = now
	}
	identity.LastSeen = now
	return identity, s.put(bucketIdentities, identity.Account, identity)
}

// Identity reads one back.
func (s *Store) Identity(account string) (AccountIdentity, error) {
	var identity AccountIdentity
	return identity, s.get(bucketIdentities, account, &identity)
}

// Identities lists them all.
func (s *Store) Identities() ([]AccountIdentity, error) {
	var identities []AccountIdentity
	err := s.forEach(bucketIdentities, func(_ string, raw []byte) error {
		var identity AccountIdentity
		if err := json.Unmarshal(raw, &identity); err != nil {
			return err
		}
		identities = append(identities, identity)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(identities, func(i, j int) bool {
		return identities[i].Account < identities[j].Account
	})
	return identities, nil
}

// PutLifecycleEvent records a hook outcome and retains only the most recent
// hundred. Lifecycle history is operational evidence, not an audit archive.
func (s *Store) PutLifecycleEvent(event LifecycleEvent) (LifecycleEvent, error) {
	event.Event = limitText(event.Event, 32)
	// 32 is what this program accepts as a cPanel account name elsewhere.
	// Truncating at 16 recorded the wrong name for a longer account, and
	// could record two different accounts under one name.
	event.Account = limitText(event.Account, 32)
	event.Detail = limitText(event.Detail, 1024)
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	if event.ID == "" {
		event.ID = event.At.Format("20060102T150405.000000000") + "-" + NewID()
	}
	if err := s.put(bucketLifecycle, event.ID, event); err != nil {
		return event, err
	}
	events, err := s.LifecycleEvents(0)
	if err != nil {
		return event, err
	}
	for len(events) > 100 {
		oldest := events[len(events)-1]
		if err := s.delete(bucketLifecycle, oldest.ID); err != nil {
			return event, err
		}
		events = events[:len(events)-1]
	}
	return event, nil
}

func limitText(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

// LifecycleEvents lists newest first. Limit zero means all retained events.
func (s *Store) LifecycleEvents(limit int) ([]LifecycleEvent, error) {
	var events []LifecycleEvent
	err := s.forEach(bucketLifecycle, func(_ string, raw []byte) error {
		var event LifecycleEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			return err
		}
		events = append(events, event)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(events, func(i, j int) bool { return events[i].At.After(events[j].At) })
	if limit > 0 && len(events) > limit {
		events = events[:limit]
	}
	return events, nil
}
