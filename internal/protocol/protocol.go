// Package protocol defines the wire types the agent and controller
// exchange. The agent always initiates: cPanel servers sit behind NAT and
// provider firewalls, so the controller never dials them.
// See docs/DESIGN.md §3.
package protocol

import (
	"time"

	"github.com/shukiv/gniza/internal/destination"
)

// Paths on the controller's agent API.
//
// Backups and restores share one poll endpoint so an agent holds a single
// long-poll connection and one lease mechanism covers both.
const (
	PathEnrol         = "/v1/enrol"
	PathNextJob       = "/v1/jobs/next"
	PathReport        = "/v1/jobs/report"
	PathRestoreReport = "/v1/restores/report"
	// PathRenewLease is where an agent says it is still working. A lease
	// is a fixed span, and the work is not: a restore of a large account
	// can outlast it, and being taken back mid-write is how a destructive
	// restore ends up running twice.
	PathRenewLease = "/v1/leases/renew"
	PathHealthz    = "/healthz"
	DefaultPoll    = 60 * time.Second
	MaxBodyBytes   = 1 << 20
)

// Work kinds an assignment can carry.
const (
	KindBackup  = "backup"
	KindRestore = "restore"
)

// Assignment is one unit of work for an agent. Exactly one of Backup or
// Restore is set, matching Kind.
type Assignment struct {
	Kind    string             `json:"kind"`
	Backup  *JobAssignment     `json:"backup,omitempty"`
	Restore *RestoreAssignment `json:"restore,omitempty"`
}

// Restore kinds.
const (
	// RestoreAccount rebuilds the whole account archive.
	RestoreAccount = "account"
	// RestoreFiles pulls named paths out of a snapshot.
	RestoreFiles = "files"
	// RestoreItems takes one part of an account out of a snapshot — a
	// mailbox, a database, the DNS records — without rebuilding the rest.
	RestoreItems = "items"
)

// RestoreSelection is one part of an account inside a restore, together
// with the mailboxes, databases or paths asked for of it. A restore can
// carry several: a database is of little use without the users that open
// it, and asking for them separately means two jobs and two chances for
// one to fail after the other has already been written.
type RestoreSelection struct {
	Kind  string   `json:"kind"`
	Names []string `json:"names,omitempty"`
}

// RestoreAssignment is one account, or part of one, to bring back.
type RestoreAssignment struct {
	JobID string `json:"job_id"`
	// ClaimToken identifies this lease attempt. It comes back in the
	// report, which is how the controller tells this attempt's result
	// from that of an attempt whose lease it has already reclaimed.
	ClaimToken string `json:"claim_token"`
	AccountID  string `json:"account_id"`
	CPanelUser string `json:"cpanel_user"`

	SnapshotID string `json:"snapshot_id"`
	Kind       string `json:"kind"`
	// IncludePaths selects individual files for a RestoreFiles job. They
	// keep their original paths under TargetDir.
	IncludePaths []string `json:"include_paths,omitempty"`
	TargetDir    string   `json:"target_dir,omitempty"`
	// ItemKind and ItemNames describe a RestoreItems job asking for one
	// part of the account. Items describes one asking for several at
	// once. Read them through Selections rather than directly: a record
	// written before baskets existed carries only the first pair.
	ItemKind  string             `json:"item_kind,omitempty"`
	ItemNames []string           `json:"item_names,omitempty"`
	Items     []RestoreSelection `json:"items,omitempty"`
	// Apply writes the restore into the live account instead of leaving a
	// copy to collect: a whole account goes to cPanel's restorepkg, and
	// the parts of one that can be written back are written back. Off
	// unless it was explicitly asked for.
	Apply bool `json:"apply"`
	// Unrestricted asks cPanel to restore the archive without its
	// Restricted Restore protections. The default is restricted, because
	// the archive holds a customer's own home directory and restorepkg
	// runs as root.
	Unrestricted bool `json:"unrestricted,omitempty"`

	// Source is the repository to read from. The controller picks it; the
	// agent never chooses where a restore comes from.
	Source Target `json:"source"`
	// SizeEstimate drives the staging space preflight, taken from the
	// snapshot's recorded size.
	SizeEstimate   uint64    `json:"size_estimate"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}

// Selections is what this restore asks for, whether it was recorded as one
// part of an account or as several.
func (a RestoreAssignment) Selections() []RestoreSelection {
	if len(a.Items) > 0 {
		return a.Items
	}
	if a.ItemKind == "" {
		return nil
	}
	return []RestoreSelection{{Kind: a.ItemKind, Names: a.ItemNames}}
}

// RestoreReport closes out a restore.
type RestoreReport struct {
	JobID string `json:"job_id"`
	// ClaimToken is the token from the assignment this report answers.
	ClaimToken    string  `json:"claim_token"`
	Status        string  `json:"status"`
	BytesRestored uint64  `json:"bytes_restored"`
	DurationSecs  float64 `json:"duration_seconds"`
	// ArchivePath is where the rebuilt account archive was left, for a
	// restore that did not apply it.
	ArchivePath string `json:"archive_path,omitempty"`
	// RestoredTo is where a files restore left what it recovered.
	RestoredTo string `json:"restored_to,omitempty"`
	// Detail says what a granular restore actually produced, so "success"
	// is not the whole of what an operator is told.
	Detail string `json:"detail,omitempty"`
	// Hint is what the person who asked for the restore can do about a
	// failure, in their own terms. Error is written for whoever runs the
	// server -- it names repositories, paths and commands -- so it is not
	// shown to a customer; this is, which is why nothing that goes in here
	// may carry either.
	Hint    string `json:"hint,omitempty"`
	Applied bool   `json:"applied"`
	Error   string `json:"error,omitempty"`
}

// LeaseRenewal says an agent is still working on a job it claimed, and
// asks for more time before the controller gives the work to somebody
// else.
//
// The claim token is what makes it safe. Only the attempt holding the job
// may extend it, so an agent that lost the lease and does not know it
// cannot take it back from the attempt that has it now.
type LeaseRenewal struct {
	JobID      string `json:"job_id"`
	ClaimToken string `json:"claim_token"`
	// Restore says which queue the job is in. A backup and a restore can
	// share an id in neither table, but they are different rows.
	Restore bool `json:"restore,omitempty"`
}

// LeaseRenewed is what the controller answers with.
type LeaseRenewed struct {
	// LeaseExpiresAt is the new expiry. Zero means the lease was not
	// renewed, and the agent must stop: something else may hold the job.
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}

// EnrolRequest is sent once at startup so the controller learns what this
// server can do. Flag support differs between cPanel versions and is
// probed, not assumed.
type EnrolRequest struct {
	Hostname     string            `json:"hostname"`
	AgentVersion string            `json:"agent_version"`
	ResticVer    string            `json:"restic_version"`
	PkgacctFlags map[string]string `json:"pkgacct_flags"`
	StagingRoot  string            `json:"staging_root"`
}

// EnrolResponse confirms which server record the certificate maps to.
type EnrolResponse struct {
	ServerID     string        `json:"server_id"`
	PollInterval time.Duration `json:"poll_interval"`
}

// JobAssignment is one account to back up, with everything needed to do it.
// It is generated per job and carries resolved secrets, so it is never
// logged and never written to the agent's disk.
type JobAssignment struct {
	JobID string `json:"job_id"`
	// ClaimToken identifies this lease attempt; see RestoreAssignment.
	ClaimToken string `json:"claim_token"`
	AccountID  string `json:"account_id"`

	CPanelUser string   `json:"cpanel_user"`
	HomeDir    string   `json:"home_dir"`
	Databases  []string `json:"databases"`
	// SizeEstimate drives the staging space preflight. A job without one
	// is refused rather than allowed to fill the volume.
	SizeEstimate uint64 `json:"size_estimate"`

	PayloadMode string `json:"payload_mode"`
	// Excludes are restic exclude patterns for this job.
	Excludes []string `json:"excludes,omitempty"`
	// The parts of the account this job leaves out.
	SkipHomedir   bool `json:"skip_homedir,omitempty"`
	SkipDatabases bool `json:"skip_databases,omitempty"`
	// SkipEmail reaches pkgacct as well as restic: an exclude keeps the
	// messages out of the file backup, but the mail configuration is
	// inside pkgacct's archive where no exclude can reach it.
	SkipEmail bool `json:"skip_email,omitempty"`
	// RetryFailed gives a failed destination one more attempt while the
	// payload is still staged.
	RetryFailed    bool   `json:"retry_failed,omitempty"`
	Compression    string `json:"compression"`
	LimitUploadKiB int    `json:"limit_upload_kib"`

	Targets        []Target  `json:"targets"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}

// Target is one repository this job must write to.
type Target struct {
	RepositoryID string            `json:"repository_id"`
	Spec         destination.Spec  `json:"spec"`
	RepoPath     string            `json:"repo_path"`
	RepoPassword string            `json:"repo_password"`
	Tags         map[string]string `json:"tags,omitempty"`
}

// TargetReport is the outcome for one repository.
type TargetReport struct {
	RepositoryID   string  `json:"repository_id"`
	Status         string  `json:"status"`
	SnapshotID     string  `json:"snapshot_id,omitempty"`
	BytesAdded     uint64  `json:"bytes_added"`
	BytesProcessed uint64  `json:"bytes_processed"`
	DurationSecs   float64 `json:"duration_seconds"`
	Attempt        int     `json:"attempt"`
	Incomplete     bool    `json:"incomplete"`
	Error          string  `json:"error,omitempty"`
	// Detail is what restic reported on its error stream: which files it
	// could not read, and any warning it raised. Without it "some files
	// unreadable" is a dead end for whoever has to decide whether it
	// matters.
	Detail string `json:"detail,omitempty"`
}

// JobReport closes out a job. The controller derives the job's status from
// the target reports rather than trusting an agent-supplied rollup.
type JobReport struct {
	JobID string `json:"job_id"`
	// ClaimToken is the token from the assignment this report answers.
	ClaimToken string         `json:"claim_token"`
	Targets    []TargetReport `json:"targets"`
	// StagingError describes a failure before any target was attempted,
	// such as insufficient disk for pkgacct.
	StagingError string `json:"staging_error,omitempty"`
	// Missing is what the payload could not include, one line each. A
	// backup with a hole in it is still worth having and still worth
	// being told about; what it must never be is counted as a complete
	// account.
	Missing []string `json:"missing,omitempty"`
	// Warnings is what the payload holds but a restore may not be able
	// to put back, one line each. Unlike Missing it is not a hole in the
	// backup and does not make the run partial.
	Warnings []string `json:"warnings,omitempty"`
}

// ErrorResponse is the body of any non-2xx reply.
type ErrorResponse struct {
	Error string `json:"error"`
}
