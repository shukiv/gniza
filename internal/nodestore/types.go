package nodestore

import (
	"time"

	"github.com/shukiv/gniza/internal/job"
)

// Destination is a storage endpoint. Config holds non-secret settings;
// credentials live sealed under CredentialsSecretID.
//
// Field names match internal/store.Destination on purpose.
type Destination struct {
	ID                  string            `json:"id"`
	Name                string            `json:"name"`
	Type                string            `json:"type"`
	Config              map[string]string `json:"config"`
	CredentialsSecretID string            `json:"credentials_secret_id,omitempty"`
	AppendOnly          bool              `json:"append_only"`
	CreatedAt           time.Time         `json:"created_at"`
	LastCheckedAt       *time.Time        `json:"last_checked_at,omitempty"`
	LastCheckError      string            `json:"last_check_error,omitempty"`
	// Space is what the storage behind this destination last reported.
	// It is stored rather than asked for on demand because the page that
	// shows it must render while a backup server is unreachable, and
	// "when this was last true" is more useful than a spinner.
	Space DestinationSpace `json:"space,omitempty"`
}

// DestinationSpace is the room on the storage behind a destination.
//
// Zero TotalBytes with no error means the kind of destination cannot say:
// an S3 bucket has no size, and a restic REST server does not report the
// disk underneath it. That is different from a disk that is full, and the
// interface says so in different words.
type DestinationSpace struct {
	TotalBytes uint64     `json:"total_bytes,omitempty"`
	FreeBytes  uint64     `json:"free_bytes,omitempty"`
	MeasuredAt *time.Time `json:"measured_at,omitempty"`
	Error      string     `json:"error,omitempty"`
	// Unsupported records that this destination type has no such number,
	// so the page can say that instead of showing nothing measured yet.
	Unsupported bool `json:"unsupported,omitempty"`
}

// UsedBytes is everything on that storage, ours and everybody else's.
func (s DestinationSpace) UsedBytes() uint64 {
	if s.TotalBytes < s.FreeBytes {
		return 0
	}
	return s.TotalBytes - s.FreeBytes
}

// UsedPercent is how full the storage is, 0 when it cannot say.
func (s DestinationSpace) UsedPercent() int {
	if s.TotalBytes == 0 {
		return 0
	}
	return int(float64(s.UsedBytes()) / float64(s.TotalBytes) * 100)
}

// Known reports whether there is a measurement worth showing.
func (s DestinationSpace) Known() bool { return s.TotalBytes > 0 && s.MeasuredAt != nil }

// Repository is a restic repository inside a destination.
type Repository struct {
	ID               string `json:"id"`
	DestinationID    string `json:"destination_id"`
	Path             string `json:"path"`
	PasswordSecretID string `json:"password_secret_id"`
	// ChunkerSourceRepoID names the repository whose chunker parameters
	// this one must copy. Those parameters are fixed at creation and can
	// never change, so every repository after the first must have one.
	// See docs/DESIGN.md §7.
	ChunkerSourceRepoID string     `json:"chunker_source_repo_id,omitempty"`
	InitialisedAt       *time.Time `json:"initialised_at,omitempty"`
	// RecoveryNotedAt is when an operator confirmed they had written the
	// repository password down somewhere off this server. Until then the
	// interface keeps saying so: the disaster these backups exist for is
	// also the one that destroys the only copy of the key.
	RecoveryNotedAt *time.Time `json:"recovery_noted_at,omitempty"`
	// RetentionApprovedAt is when an operator looked at what retention
	// would delete from this repository and said go. Nothing is ever
	// deleted before that: a keep policy is easy to write and hard to
	// picture, and this is the one thing the program does that cannot be
	// undone.
	RetentionApprovedAt *time.Time `json:"retention_approved_at,omitempty"`
	// RetentionApprovedKeeps is the keep policy that approval was for.
	// Approving is approving something in particular: a repository whose
	// schedules have since been edited to keep a tenth as much is not
	// what the operator read, and nothing may be deleted under it until
	// they have read the new one. Zero means an approval recorded before
	// this was stored, which counts as unapproved.
	RetentionApprovedKeeps Retention `json:"retention_approved_keeps,omitempty"`
	// Retention records what the last plan said and what the last run
	// did.
	Retention RetentionState `json:"retention,omitempty"`
	// Census is what this repository was last found to hold. Asking
	// restic costs a walk of the repository index over the network, so it
	// is taken on a slow cadence and stored: a page that ran it while
	// drawing would take minutes to render and would do it again on every
	// refresh.
	Census    RepositoryCensus `json:"census,omitempty"`
	CreatedAt time.Time        `json:"created_at"`
}

// RepositoryCensus is how many copies a repository holds and what they
// cost the storage under it.
//
// The last good reading is kept apart from the last attempt on purpose. A
// destination that is unreachable tonight still has a size, and "94 GiB,
// as of yesterday" with the reason underneath is worth more to an
// operator than an empty column.
type RepositoryCensus struct {
	Snapshots  int        `json:"snapshots,omitempty"`
	SizeBytes  uint64     `json:"size_bytes,omitempty"`
	MeasuredAt *time.Time `json:"measured_at,omitempty"`
	// AttemptedAt and LastError are the last measurement that did not
	// finish, which leaves the numbers above standing.
	AttemptedAt *time.Time `json:"attempted_at,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
}

// Known reports whether anything has ever been measured here.
func (c RepositoryCensus) Known() bool { return c.MeasuredAt != nil }

// StaleAfter is how old a reading has to be before the page says when it
// was taken rather than presenting it as the truth. It is three of the
// intervals the census runs on, so an ordinary missed measurement does
// not mark every figure on the page as doubtful.
const StaleAfter = 18 * time.Hour

// Stale reports whether this reading is old enough to need its date shown
// beside it.
func (c RepositoryCensus) Stale(now time.Time) bool {
	return c.Known() && now.Sub(*c.MeasuredAt) > StaleAfter
}

// RetentionState is what happened the last time retention looked at a
// repository. It is stored rather than held in memory so that restarting
// the service does not start the whole cycle again, and so the page can
// say what is about to happen without running restic to find out.
type RetentionState struct {
	// PlannedAt is when the last dry run was taken, and Plan is what it
	// said. A plan is a record, not a promise: a backup landing after it
	// changes what the next real run removes.
	PlannedAt *time.Time       `json:"planned_at,omitempty"`
	Plan      []RetentionGroup `json:"plan,omitempty"`
	// PlannedKeeps is the keep policy the plan was taken under, which is
	// what an operator approves when they approve the plan.
	PlannedKeeps Retention `json:"planned_keeps,omitempty"`
	WouldKeep    int       `json:"would_keep,omitempty"`
	WouldDrop    int       `json:"would_drop,omitempty"`
	// AppliedAt and Dropped are the last run that actually removed
	// something.
	AppliedAt *time.Time `json:"applied_at,omitempty"`
	Dropped   int        `json:"dropped,omitempty"`
	// AttemptedAt is when retention last tried and did not finish. It is
	// separate from PlannedAt and AppliedAt so that a repository whose
	// lock is held by something stale is left alone for as long as a
	// successful one, rather than retried on every scheduler tick.
	AttemptedAt *time.Time `json:"attempted_at,omitempty"`
	// LastError is why that attempt did not finish. A stale lock is the
	// usual one, and it is worth saying rather than retrying in silence
	// forever.
	LastError string `json:"last_error,omitempty"`
}

// RetentionGroup is one account's share of a plan.
type RetentionGroup struct {
	Protected bool       `json:"protected,omitempty"`
	Account   string     `json:"account"`
	Host      string     `json:"host,omitempty"`
	Keep      int        `json:"keep"`
	Drop      int        `json:"drop"`
	Oldest    *time.Time `json:"oldest,omitempty"`
	Newest    *time.Time `json:"newest,omitempty"`
}

// AccountIdentity is which unix account a cPanel name currently means.
//
// A cPanel username is a label, not an identity. Delete an account and
// create another with the same name -- which a host does when a customer
// leaves and the next one asks for the same name -- and the second
// customer is, to every part of this program that goes by name, the
// first one. Their self-service page would list the previous customer's
// backups and let them download them.
//
// The unix uid is the thing that actually changes. This records which uid
// a name meant when its backups were taken, so that when the name is
// recycled the new owner sees only what was taken since.
type AccountIdentity struct {
	Account string `json:"account"`
	// UID is the unix account the name meant when last seen.
	UID int `json:"uid"`
	// SinceAt is when this uid was first seen for this name. After a
	// recycle it is the moment the new account appeared, and nothing
	// older than it belongs to whoever holds the name now.
	SinceAt time.Time `json:"since_at"`
	// Recycled records that the name has changed hands at least once. An
	// identity that has never changed hands does not filter anything:
	// its SinceAt is only when this program first noticed the account,
	// not a boundary between two owners.
	Recycled bool `json:"recycled,omitempty"`
	// RetiredAt records cPanel's remove event. It makes a later create a
	// new identity even if Linux reuses both the username and uid.
	RetiredAt *time.Time `json:"retired_at,omitempty"`
	LastSeen  time.Time  `json:"last_seen"`
	CreatedAt time.Time  `json:"created_at"`
}

// LifecycleEvent is one cPanel Standardized Hook handled by the standalone
// service. Raw hook payloads are deliberately not retained: they are large,
// version-specific, and may contain account metadata the dashboard does not
// need.
type LifecycleEvent struct {
	ID      string    `json:"id"`
	Event   string    `json:"event"`
	Account string    `json:"account,omitempty"`
	OK      bool      `json:"ok"`
	Detail  string    `json:"detail,omitempty"`
	At      time.Time `json:"at"`
}

// When adapts the stored value to the pointer-shaped time helper used by
// the WHM templates.
func (e LifecycleEvent) When() *time.Time { return &e.At }

// Title and Outcome keep stored machine event names out of the WHM UI and
// ensure color is never the only description of lifecycle state.
func (e LifecycleEvent) Title() string {
	switch e.Event {
	case "create":
		return "Created"
	case "modify":
		return "Modified"
	case "suspend":
		return "Suspended"
	case "unsuspend":
		return "Unsuspended"
	case "remove-pre":
		return "Removal check"
	case "remove":
		return "Removed"
	default:
		return e.Event
	}
}

func (e LifecycleEvent) Outcome() string {
	if e.Event == "remove-pre" {
		if e.OK {
			return "Allowed"
		}
		return "Blocked"
	}
	if e.OK {
		return "Handled"
	}
	return "Failed"
}

// Retention is the keep policy handed to "restic forget".
type Retention struct {
	KeepLast    int `json:"keep_last,omitempty"`
	KeepDaily   int `json:"keep_daily,omitempty"`
	KeepWeekly  int `json:"keep_weekly,omitempty"`
	KeepMonthly int `json:"keep_monthly,omitempty"`
	KeepYearly  int `json:"keep_yearly,omitempty"`
}

// Policy is a schedule with its retention and payload settings.
type Policy struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	ScheduleCron   string    `json:"schedule_cron"`
	PayloadMode    string    `json:"payload_mode"`
	Compression    string    `json:"compression"`
	LimitUploadKiB int       `json:"limit_upload_kib"`
	Retention      Retention `json:"retention"`
	// RepositoryIDs are the copies this policy keeps. One unreachable
	// destination does not invalidate the others.
	RepositoryIDs []string `json:"repository_ids"`
	// Accounts are the cPanel users on this schedule. Empty means every
	// account the server has, resolved at run time so new accounts are
	// picked up without an edit.
	Accounts []string `json:"accounts"`
	// Excludes are restic exclude patterns: what is not worth storing.
	// A cache directory backed up nightly costs storage every night and
	// is of no use in a restore.
	Excludes []string `json:"excludes,omitempty"`
	// The parts of an account this schedule leaves out. They are Skip
	// rather than Include so that a policy stored before they existed
	// still means "everything", which is what it meant when it was
	// written.
	SkipHomedir   bool `json:"skip_homedir,omitempty"`
	SkipDatabases bool `json:"skip_databases,omitempty"`
	SkipEmail     bool `json:"skip_email,omitempty"`
	// IncludeSystem adds the server's own configuration to what this
	// schedule backs up: EasyApache, the tweak settings, the packages and
	// the service configuration a replacement machine would need before
	// the accounts restored onto it mean anything.
	IncludeSystem bool `json:"include_system,omitempty"`
	// RetryFailed gives a destination that failed one more attempt before
	// the job is called failed. The payload is still staged, so a retry
	// costs an upload rather than another pkgacct.
	RetryFailed bool `json:"retry_failed,omitempty"`
	// AlertNoBackupDays raises a warning when an account this schedule
	// covers has gone that long without a successful backup. Zero is the
	// schedule's own interval, doubled; negative is never.
	AlertNoBackupDays int `json:"alert_no_backup_days,omitempty"`
	// AlertRunHours warns when a single run has been going longer than
	// this. Zero means six hours.
	AlertRunHours int        `json:"alert_run_hours,omitempty"`
	Enabled       bool       `json:"enabled"`
	LastRunAt     *time.Time `json:"last_run_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

// AllAccounts reports whether the policy covers every account on the server.
func (p Policy) AllAccounts() bool { return len(p.Accounts) == 0 }

// Job is one backup run of one account.
type Job struct {
	ID       string `json:"id"`
	PolicyID string `json:"policy_id"`
	Account  string `json:"account"`
	// CompleteAccount records what this particular run staged. Looking at
	// today's policy is insufficient because payload exclusions may have
	// changed since an older snapshot was made. Jobs from before this field
	// existed remain false and cannot authorize destructive account removal.
	CompleteAccount bool        `json:"complete_account,omitempty"`
	Status          job.Status  `json:"status"`
	Targets         []JobTarget `json:"targets"`
	StagingErr      string      `json:"staging_error,omitempty"`
	// Missing is what this run could not put in the backup, one line
	// each: a database that would not dump, most often. The run still
	// stored everything else, which is why it is not a failure -- but it
	// is not a complete account either, so CompleteAccount is false and
	// termination safety will not accept it.
	Missing []string `json:"missing,omitempty"`
	// Progress is what restic last reported about a running job. It is
	// cleared when the job finishes: a percentage on a job that is over
	// says nothing, and "100%" beside a failure would be a lie.
	Progress   *JobProgress `json:"progress,omitempty"`
	QueuedAt   time.Time    `json:"queued_at"`
	StartedAt  *time.Time   `json:"started_at,omitempty"`
	FinishedAt *time.Time   `json:"finished_at,omitempty"`
}

// JobProgress is how far a running backup has got.
// RestoreProgress is how far a running restore has got.
//
// A restore is several things in a row -- reading each part of the account
// out of the repository, unpacking it, building the archive, handing that
// to cPanel -- and restic can count only the reading. So the stage is what
// this record is really for, and the percentage is what it can add when
// there is one to add.
type RestoreProgress struct {
	// Stage is what is happening now, in words: "reading the home
	// directory", "handing the archive to cPanel's restore".
	Stage string `json:"stage"`
	// Percent is 0-100 within the stage, and Known says whether there is
	// one: a stage restic cannot count would otherwise read as stuck at
	// nothing.
	Percent       float64   `json:"percent"`
	Known         bool      `json:"known"`
	BytesRestored uint64    `json:"bytes_restored,omitempty"`
	TotalBytes    uint64    `json:"total_bytes,omitempty"`
	At            time.Time `json:"at"`
}

type JobProgress struct {
	// Percent is 0-100, as restic reports it.
	Percent    float64 `json:"percent"`
	BytesDone  uint64  `json:"bytes_done"`
	TotalBytes uint64  `json:"total_bytes"`
	FilesDone  uint64  `json:"files_done"`
	TotalFiles uint64  `json:"total_files"`
	// Repository names which copy is being written, since a job uploads
	// the same payload to every destination in turn.
	Repository string    `json:"repository,omitempty"`
	At         time.Time `json:"at"`
}

// JobTarget is one repository's outcome within a job.
type JobTarget struct {
	RepositoryID   string           `json:"repository_id"`
	Status         job.TargetStatus `json:"status"`
	SnapshotID     string           `json:"snapshot_id,omitempty"`
	BytesAdded     uint64           `json:"bytes_added"`
	BytesProcessed uint64           `json:"bytes_processed"`
	DurationSecs   float64          `json:"duration_seconds"`
	Incomplete     bool             `json:"incomplete"`
	Error          string           `json:"error,omitempty"`
	// Detail is restic's own account of the run: the files it could not
	// read, and any warnings. Kept so "some files unreadable" can be
	// looked into rather than merely noticed.
	Detail string `json:"detail,omitempty"`
}

// RestoreSelection is one part of an account inside a restore, with the
// mailboxes, databases or paths asked for of it.
type RestoreSelection struct {
	Kind  string   `json:"kind"`
	Names []string `json:"names,omitempty"`
}

// Restore is one restore run.
type Restore struct {
	ID           string   `json:"id"`
	Account      string   `json:"account"`
	RepositoryID string   `json:"repository_id"`
	SnapshotID   string   `json:"snapshot_id"`
	Kind         string   `json:"kind"`
	IncludePaths []string `json:"include_paths,omitempty"`
	TargetDir    string   `json:"target_dir,omitempty"`
	// ItemKind and ItemNames record a granular restore of one part of the
	// account; Items records one of several parts at once. Read them
	// through Selections rather than directly, so a record written before
	// baskets existed is read the same way as one written after.
	ItemKind  string             `json:"item_kind,omitempty"`
	ItemNames []string           `json:"item_names,omitempty"`
	Items     []RestoreSelection `json:"items,omitempty"`
	// Apply writes the restore into the live account rather than leaving a
	// copy to collect. A whole account goes to restorepkg; a part of one
	// is written back where it belongs. Off unless explicitly asked for.
	// Unrestricted asks cPanel to skip its Restricted Restore checks
	// when applying the archive. The default is restricted.
	Unrestricted  bool       `json:"unrestricted,omitempty"`
	Apply         bool       `json:"apply"`
	Status        job.Status `json:"status"`
	BytesRestored uint64     `json:"bytes_restored"`
	// Progress is how far a running restore has got. Nil until it starts
	// and after it finishes: a percentage against a restore that is over
	// says nothing.
	Progress    *RestoreProgress `json:"progress,omitempty"`
	ArchivePath string           `json:"archive_path,omitempty"`
	RestoredTo  string           `json:"restored_to,omitempty"`
	// Detail records what a rehearsal actually checked, so a passing
	// drill says more than "success".
	Detail string `json:"detail,omitempty"`
	// PartialSource says the snapshot this was made from was taken of
	// less than the whole account, because the schedule that wrote it
	// leaves the databases, the home directory or the mail out. A
	// rehearsal of one proves what it holds and nothing about the rest,
	// so it must not read as the whole account having been verified.
	PartialSource bool `json:"partial_source,omitempty"`
	// SkippedParts names what that snapshot was taken without.
	SkippedParts []string `json:"skipped_parts,omitempty"`
	// Hint is what the person who asked for this restore can do about a
	// failure. Error is written for whoever runs the server and names
	// repositories, paths and commands; this is what a customer is shown
	// in its place, so nothing that carries either may go in here.
	Hint    string `json:"hint,omitempty"`
	Applied bool   `json:"applied"`
	Error   string `json:"error,omitempty"`
	// AccountSince is when the account this belongs to began, as the
	// identity record has it. A username can be given to somebody else,
	// and what the last customer recovered is not the next one's to
	// collect; the name alone cannot tell those two apart, and this can.
	// Zero on records written before this existed.
	AccountSince time.Time  `json:"account_since,omitempty"`
	QueuedAt     time.Time  `json:"queued_at"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
}

// Selections is what this restore asks for, whether it was recorded as one
// part of an account or as several.
func (r Restore) Selections() []RestoreSelection {
	if len(r.Items) > 0 {
		return r.Items
	}
	if r.ItemKind == "" {
		return nil
	}
	return []RestoreSelection{{Kind: r.ItemKind, Names: r.ItemNames}}
}

// Channel is somewhere this server tells someone what happened.
type Channel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Kind is smtp, ntfy, telegram or webhook.
	Kind string `json:"kind"`
	// Config is the channel's settings. Nothing secret is in here: the
	// credentials are sealed like a destination's and referenced by id.
	Config map[string]string `json:"config"`
	// SecretsID names the sealed credentials, empty for a channel that
	// needs none.
	SecretsID string `json:"secrets_id,omitempty"`
	// Events it asked for. Empty means the ones that report a problem.
	Events    []string   `json:"events,omitempty"`
	Enabled   bool       `json:"enabled"`
	LastSent  *time.Time `json:"last_sent,omitempty"`
	LastError string     `json:"last_error,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// Secret is a sealed credential. The plaintext never reaches this file.
type Secret struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Ciphertext []byte    `json:"ciphertext"`
	KeyID      string    `json:"key_id"`
	CreatedAt  time.Time `json:"created_at"`
}

// Settings are the node's own configuration.
type Settings struct {
	// StagingRoot is where pkgacct writes. Snapshot paths embed it and
	// restic groups retention by path, so changing it after the first
	// backup orphans every existing retention group. Treat it as fixed.
	StagingRoot   string `json:"staging_root"`
	MaxConcurrent int    `json:"max_concurrent"`
	// IdentitiesBackfilledAt is when every account then on the server was
	// recorded against the unix account it meant. After it, an account
	// with no record is one that appeared later -- which may be a name
	// that has changed hands, and is treated as one.
	IdentitiesBackfilledAt *time.Time `json:"identities_backfilled_at,omitempty"`
	SafetyMargin           float64    `json:"safety_margin"`
	ResticBinary           string     `json:"restic_binary"`
	ResticCache            string     `json:"restic_cache"`
	ResticCACert           string     `json:"restic_cacert,omitempty"`
	// ConfigDir holds the SSH keys and known_hosts files Gniza generates
	// for SFTP destinations.
	ConfigDir string `json:"config_dir"`
	Hostname  string `json:"hostname"`
	// PkgacctFlags records what the installed pkgacct actually supports,
	// probed rather than assumed.
	PkgacctFlags map[string]string `json:"pkgacct_flags,omitempty"`
	// KeepOutputDays is how long a finished restore's files stay in the
	// work directory before they are swept. They are there to be
	// collected; nothing collected them on a server where every account
	// had been restored once, and the disk filled. Zero means the
	// default; a negative number keeps them for ever.
	KeepOutputDays int `json:"keep_output_days,omitempty"`
	// ProtectAccountRemoval makes the blocking cPanel pre-remove hook
	// refuse termination until every destination promised by an enabled,
	// full-account policy has a recent complete copy. It is opt-in so an
	// upgrade cannot unexpectedly change cPanel's account-removal behavior.
	ProtectAccountRemoval bool `json:"protect_account_removal,omitempty"`
	// BackupOnSuspension queues a fresh full-account copy when cPanel
	// suspends an account. It is opt-in because billing systems may suspend
	// large numbers of accounts automatically.
	BackupOnSuspension bool `json:"backup_on_suspension,omitempty"`
	// DeletedAccountDays is how long a deleted account's backups are kept
	// before they are forgotten: the snapshots removed from every
	// destination and the account's history with them.
	//
	// Nothing else ever removes them. Retention keeps a series thinned but
	// never empties it, so without this a server accumulates the backups
	// of every customer who has ever left, forever. Zero means the
	// default; a negative number keeps them until somebody says otherwise.
	DeletedAccountDays int `json:"deleted_account_days,omitempty"`
	// BugEmail is retained for settings-file compatibility only. Reports now
	// go to the fixed bug tracker intake, never to the old email destination.
	BugEmail string `json:"bug_email,omitempty"`
	// LogLevel is how much the service writes to its own log: one of
	// LogLevels. It is read while the service is running rather than only
	// at startup, because the reason to turn debug on is usually something
	// that is happening now and would not survive a restart.
	LogLevel string `json:"log_level,omitempty"`
	// SendmailPath is retained for compatibility; bug reporting no longer
	// invokes sendmail. Notification channel settings are unaffected.
	SendmailPath string `json:"sendmail_path,omitempty"`
	// NoUpdateCheck stops this server asking GitHub once a day whether a
	// newer release has been published. It is opt-out rather than opt-in
	// because the check only reads a version number, and a backup program
	// nobody knows is out of date is the worse failure.
	NoUpdateCheck bool `json:"no_update_check,omitempty"`
	// UpdateChannel is where updates are read from: "releases" (the
	// default) or "dist", the branch the latest build is published on. A
	// server following the work rather than the releases takes the
	// second; what is published there is signed with the same key.
	UpdateChannel string `json:"update_channel,omitempty"`
}

// UpdateState is what the last check for a newer release found. It is kept
// so that every page can say so without asking GitHub again.
// UpgradeState is an installation of a newer release: what was asked for,
// how far it got, and what the installer said.
//
// It is stored rather than held in memory because installing replaces this
// program and restarts it. Whatever is remembered about an upgrade has to
// survive the upgrade, or the page an operator is watching comes back
// saying nothing ever happened.
type UpgradeState struct {
	// Version is the release being installed, and From is what was
	// running when it was asked for.
	Version string `json:"version,omitempty"`
	From    string `json:"from,omitempty"`
	// Stage is what is happening now: "downloading" while the release is
	// being fetched and checked, "installing" once the installer has been
	// handed it. Empty when nothing is in flight.
	Stage     string    `json:"stage,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
	// FinishedAt is when it stopped, either way. A zero time with a
	// StartedAt is an upgrade still going.
	FinishedAt time.Time `json:"finished_at,omitempty"`
	Failed     bool      `json:"failed,omitempty"`
	Error      string    `json:"error,omitempty"`
	// Log is the tail of what the installer printed, which is what says
	// why an upgrade that failed did.
	Log string `json:"log,omitempty"`
	// Dir is where the release was unpacked, so the run can be picked up
	// again by the process that replaced the one that started it.
	Dir string `json:"dir,omitempty"`
}

type UpdateState struct {
	CheckedAt time.Time `json:"checked_at"`
	// Channel is where this was read from: published releases, or the
	// dist branch. Stored so a page cannot show a branch build under the
	// heading of a release after the setting is changed.
	Channel string `json:"channel,omitempty"`
	// BuiltAt is the commit the published build was made from. It is
	// what orders two builds of a branch, which have no version numbers
	// to compare.
	BuiltAt time.Time `json:"built_at,omitempty"`
	// Version, URL and Notes describe the newest published release,
	// whether or not it is newer than what is running here.
	Version string `json:"version,omitempty"`
	URL     string `json:"url,omitempty"`
	Notes   string `json:"notes,omitempty"`
	// Error is why the last check failed, kept so a server that has been
	// unable to ask for a month can say that rather than saying nothing.
	Error string `json:"error,omitempty"`
}

// DefaultKeepOutputDays is a week: long enough that a restore taken on a
// Friday is still there on Monday.
const DefaultKeepOutputDays = 7

// DefaultDeletedAccountDays is ninety: long enough that a customer who
// left in anger and came back in March still has their site, and short
// enough that a destination is not paid for forever to hold the backups of
// people who are gone.
const DefaultDeletedAccountDays = 90

// KeepDeletedAccountsFor is how long a deleted account's backups survive.
// Zero means they are kept until somebody removes them by hand.
func (s Settings) KeepDeletedAccountsFor() time.Duration {
	switch {
	case s.DeletedAccountDays < 0:
		return 0
	case s.DeletedAccountDays == 0:
		return DefaultDeletedAccountDays * 24 * time.Hour
	default:
		return time.Duration(s.DeletedAccountDays) * 24 * time.Hour
	}
}

// KeepOutputFor is how long finished restore output survives.
func (s Settings) KeepOutputFor() time.Duration {
	switch {
	case s.KeepOutputDays < 0:
		return 0
	case s.KeepOutputDays == 0:
		return DefaultKeepOutputDays * 24 * time.Hour
	default:
		return time.Duration(s.KeepOutputDays) * 24 * time.Hour
	}
}
