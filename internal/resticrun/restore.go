package resticrun

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Snapshot is one entry from "restic snapshots --json".
type Snapshot struct {
	ID       string    `json:"id"`
	ShortID  string    `json:"short_id"`
	Time     time.Time `json:"time"`
	Paths    []string  `json:"paths"`
	Hostname string    `json:"hostname"`
	Tags     []string  `json:"tags"`
	// ReadComplete is derived from a separate repository receipt, not from
	// local job history that disappears in disaster recovery.
	ReadComplete bool `json:"-"`
	// ReadFailed also carries known pre-receipt failures from local history.
	ReadFailed bool `json:"-"`
	Summary    struct {
		TotalBytesProcessed uint64 `json:"total_bytes_processed"`
		TotalFilesProcessed uint64 `json:"total_files_processed"`
		// DataAdded is what this snapshot actually cost in the
		// repository. restic reports it from 0.17 on; older
		// repositories leave it zero and the page omits the column.
		DataAdded uint64 `json:"data_added"`
	} `json:"summary"`
}

// Account reads the account tag the agent attaches to every snapshot.
func (s Snapshot) Account() string {
	for _, tag := range s.Tags {
		if account, found := strings.CutPrefix(tag, "account:"); found {
			return account
		}
	}
	return ""
}

// PayloadMode reads the mode tag, which decides whether a restore has to
// reassemble split parts or just unpack one archive.
func (s Snapshot) PayloadMode() string {
	for _, tag := range s.Tags {
		if mode, found := strings.CutPrefix(tag, "mode:"); found {
			return mode
		}
	}
	return ""
}

// SkipTagPrefix marks a snapshot that was taken of less than the whole
// account. A schedule may leave the home directory, the databases or the
// mail out, and the resulting snapshot is not interchangeable with a full
// one: restoring it would silently hand back an account missing whatever
// was skipped.
const SkipTagPrefix = "skip:"

// Skipped is what a backup deliberately left out, in the words the tag
// uses ("databases", "homedir", "email"). A full backup returns nothing.
func (s Snapshot) Skipped() []string {
	var skipped []string
	if s.ReadFailed || (hasTag(s.Tags, NeedsCompletionTag) && !s.ReadComplete) {
		skipped = append(skipped, "unverified source reads")
	}
	for _, tag := range s.Tags {
		if part, found := strings.CutPrefix(tag, SkipTagPrefix); found && part != "" {
			skipped = append(skipped, part)
		}
	}
	return skipped
}

// Complete says the snapshot holds the whole account, so a whole-account
// restore made from it gives back everything the account had.
func (s Snapshot) Complete() bool {
	return len(s.Skipped()) == 0
}

// SnapshotFilter narrows a snapshot listing.
type SnapshotFilter struct {
	Tags []string
	Host string
	// Latest limits the result to the n most recent matches. Zero means all.
	Latest int
}

// Snapshots lists the snapshots in a repository.
func (r *Runner) Snapshots(ctx context.Context, repo Repository, filter SnapshotFilter) ([]Snapshot, error) {
	// Receipts are internal snapshots, not candidate account backups. Do
	// not let --latest pick a receipt and hide the backup immediately before it.
	latest := filter.Latest
	filter.Latest = 0
	snapshots, err := r.rawSnapshots(ctx, repo, filter)
	if err != nil {
		return nil, err
	}
	needsReceipts := false
	for _, snapshot := range snapshots {
		needsReceipts = needsReceipts || hasTag(snapshot.Tags, NeedsCompletionTag)
	}
	var receipts map[string]bool
	if needsReceipts {
		receipts, err = r.completionReceipts(ctx, repo)
		if err != nil {
			return nil, err
		}
	}
	var visible []Snapshot
	for _, snapshot := range snapshots {
		if hasTag(snapshot.Tags, CompletionReceiptTag) {
			continue
		}
		snapshot.ReadComplete = receipts[snapshot.ID]
		visible = append(visible, snapshot)
	}
	if latest > 0 && len(visible) > latest {
		sort.Slice(visible, func(i, j int) bool { return visible[i].Time.Before(visible[j].Time) })
		visible = visible[len(visible)-latest:]
	}
	return visible, nil
}

func (r *Runner) rawSnapshots(ctx context.Context, repo Repository, filter SnapshotFilter) ([]Snapshot, error) {
	args := []string{"snapshots", "--json"}
	for _, tag := range filter.Tags {
		args = append(args, "--tag", tag)
	}
	if filter.Host != "" {
		args = append(args, "--host", filter.Host)
	}
	if filter.Latest > 0 {
		args = append(args, "--latest", fmt.Sprint(filter.Latest))
	}

	result, err := r.run(ctx, repo, args, secondary{}, nil)
	if err != nil {
		return nil, err
	}
	if err := classifyExit(result.ExitCode, result.Stderr, false); err != nil {
		return nil, err
	}

	var snapshots []Snapshot
	if result.Truncated {
		return nil, fmt.Errorf("resticrun: snapshot listing was truncated")
	}
	if err := json.Unmarshal(result.Stdout, &snapshots); err != nil {
		return nil, fmt.Errorf("resticrun: decode snapshots: %w", err)
	}
	return snapshots, nil
}

// RestoreSpec describes one restore.
type RestoreSpec struct {
	SnapshotID string
	// Subpath restores one directory from the snapshot directly into
	// Target, without recreating the leading path components. This is how
	// a split payload's parts are placed into their slots.
	Subpath string
	Target  string
	// Include restores only matching paths, and unlike Subpath preserves
	// the original directory structure under Target. Use it when the
	// caller wants a file back at the path it came from.
	Include []string
	Exclude []string
	// Verify re-reads restored files and compares them against the
	// snapshot. It costs a second pass over the data.
	Verify bool
	// OnProgress, when set, is called about once a second while restic
	// works. It runs on the goroutine reading restic's output, so it must
	// not block.
	OnProgress func(RestoreProgress)
}

// RestoreResult is restic's summary of a completed restore.
type RestoreResult struct {
	MessageType   string `json:"message_type"`
	TotalFiles    uint64 `json:"total_files"`
	FilesRestored uint64 `json:"files_restored"`
	TotalBytes    uint64 `json:"total_bytes"`
	BytesRestored uint64 `json:"bytes_restored"`
}

// RestoreProgress is restic's account of a restore while it is running.
//
// It is a different shape from a backup's, and reading one with the
// other's struct yields zeroes: restic counts a restore in bytes_restored
// and files_restored where a backup counts bytes_done and files_done.
type RestoreProgress struct {
	PercentDone    float64 `json:"percent_done"`
	TotalFiles     uint64  `json:"total_files"`
	FilesRestored  uint64  `json:"files_restored"`
	TotalBytes     uint64  `json:"total_bytes"`
	BytesRestored  uint64  `json:"bytes_restored"`
	SecondsElapsed float64 `json:"seconds_elapsed"`
}

// restoreProgressReader turns restic's status lines into RestoreProgress
// calls. It returns nil when nobody is listening, so nothing is parsed for
// a caller that does not want it.
func restoreProgressReader(onProgress func(RestoreProgress)) func([]byte) {
	if onProgress == nil {
		return nil
	}
	return func(line []byte) {
		if len(line) == 0 || line[0] != '{' {
			return
		}
		var probe struct {
			MessageType string `json:"message_type"`
		}
		if err := json.Unmarshal(line, &probe); err != nil || probe.MessageType != "status" {
			return
		}
		var progress RestoreProgress
		if err := json.Unmarshal(line, &progress); err != nil {
			// A status line this program cannot read is not worth failing
			// a restore over; the summary is what it is judged on.
			return
		}
		onProgress(progress)
	}
}

// RestoreArgs builds the argument list for "restic restore".
func RestoreArgs(spec RestoreSpec) ([]string, error) {
	if err := validateSnapshotID(spec.SnapshotID); err != nil {
		return nil, err
	}
	if spec.Target == "" {
		return nil, fmt.Errorf("resticrun: restore needs a target directory")
	}
	if spec.Subpath != "" && len(spec.Include) > 0 {
		// They disagree about the shape of the output: a subpath restore
		// flattens, an include restore preserves the original paths.
		return nil, fmt.Errorf("resticrun: restore cannot combine a subpath with include patterns")
	}

	target := spec.SnapshotID
	if spec.Subpath != "" {
		if !strings.HasPrefix(spec.Subpath, "/") {
			return nil, fmt.Errorf("resticrun: restore subpath %q must be absolute", spec.Subpath)
		}
		target += ":" + spec.Subpath
	}

	// The SELinux label is left behind. It says what a file was on the
	// server it was backed up from, in a directory it is not being put
	// back into, and where SELinux is on restic may not set it: one file
	// it could not relabel fails the whole restore. On 182.54.236.163
	// that was 24 accounts in 54. A file restored without one takes the
	// label of where it lands, and the panel's own restore labels what
	// it puts into a home.
	args := []string{"restore", target, "--target", spec.Target, "--json",
		"--exclude-xattr", "security.selinux"}
	for _, pattern := range spec.Include {
		args = append(args, "--include", pattern)
	}
	for _, pattern := range spec.Exclude {
		args = append(args, "--exclude", pattern)
	}
	if spec.Verify {
		args = append(args, "--verify")
	}
	return args, nil
}

// Restore materialises a snapshot, or part of one, into a directory.
func (r *Runner) Restore(ctx context.Context, repo Repository, spec RestoreSpec) (RestoreResult, error) {
	args, err := RestoreArgs(spec)
	if err != nil {
		return RestoreResult{}, err
	}
	result, err := r.run(ctx, repo, args, secondary{}, restoreProgressReader(spec.OnProgress))
	if err != nil {
		return RestoreResult{}, err
	}
	if err := classifyExit(result.ExitCode, result.Stderr, false); err != nil {
		return RestoreResult{}, err
	}
	return ParseRestoreSummary(result.Stdout)
}

// ParseRestoreSummary extracts the summary from a "restore --json" stream.
func ParseRestoreSummary(stdout []byte) (RestoreResult, error) {
	summary, found, err := lastMessage[RestoreResult](stdout, "summary")
	if err != nil {
		return RestoreResult{}, err
	}
	if !found {
		return RestoreResult{}, ErrNoSummary
	}
	return summary, nil
}

// Entry is one node from "restic ls --json".
type Entry struct {
	Name        string    `json:"name"`
	Type        string    `json:"type"`
	Path        string    `json:"path"`
	Size        uint64    `json:"size"`
	Permissions string    `json:"permissions"`
	ModTime     time.Time `json:"mtime"`
}

// IsDir reports whether the entry is a directory.
func (e Entry) IsDir() bool { return e.Type == "dir" }

// Ls lists a snapshot's contents, so an operator can find the one file they
// need without restoring the whole account.
//
// subpaths limits the listing to those directories. Passing none lists
// everything, which for a large home directory is a great many lines.
func (r *Runner) Ls(ctx context.Context, repo Repository, snapshotID string, subpaths ...string) ([]Entry, error) {
	if err := validateSnapshotID(snapshotID); err != nil {
		return nil, err
	}
	args := append([]string{"ls", "--json", snapshotID}, subpaths...)

	result, err := r.run(ctx, repo, args, secondary{}, nil)
	if err != nil {
		return nil, err
	}
	if err := classifyExit(result.ExitCode, result.Stderr, false); err != nil {
		return nil, err
	}
	if result.Truncated {
		// A listing that was cut short parses perfectly and is wrong.
		// Silently showing part of a backup is how somebody restores what
		// they can see and never learns the rest was there.
		return nil, fmt.Errorf(
			"resticrun: this backup holds more files than can be listed at once. " +
				"Browse into a directory rather than listing the whole snapshot")
	}

	// The stream opens with a snapshot header and continues with one node
	// per entry, so entries are selected by message type rather than by
	// position.
	var entries []Entry
	scanner := bufio.NewScanner(bytes.NewReader(result.Stdout))
	scanner.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var probe struct {
			MessageType string `json:"message_type"`
		}
		if err := json.Unmarshal(line, &probe); err != nil || probe.MessageType != "node" {
			continue
		}
		var entry Entry
		if err := json.Unmarshal(line, &entry); err != nil {
			return nil, fmt.Errorf("resticrun: decode ls entry: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("resticrun: read ls output: %w", err)
	}
	return entries, nil
}

// lastMessage returns the final JSON-lines message of the given type.
func lastMessage[T any](stdout []byte, messageType string) (T, bool, error) {
	var (
		found  bool
		result T
	)
	scanner := bufio.NewScanner(bytes.NewReader(stdout))
	scanner.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var probe struct {
			MessageType string `json:"message_type"`
		}
		if err := json.Unmarshal(line, &probe); err != nil || probe.MessageType != messageType {
			continue
		}
		if err := json.Unmarshal(line, &result); err != nil {
			return result, false, fmt.Errorf("resticrun: decode %s message: %w", messageType, err)
		}
		found = true
	}
	if err := scanner.Err(); err != nil {
		return result, false, fmt.Errorf("resticrun: read restic output: %w", err)
	}
	return result, found, nil
}

// validateSnapshotID rejects anything that is not a snapshot identifier.
//
// Identifiers reach here from the database and from operators, and are
// concatenated with a subpath before becoming an argument, so the shape is
// checked rather than assumed.
func validateSnapshotID(id string) error {
	if id == "" {
		return fmt.Errorf("resticrun: snapshot id is required")
	}
	if id == "latest" {
		return nil
	}
	if len(id) < 8 || len(id) > 64 {
		return fmt.Errorf("resticrun: snapshot id %q has an implausible length", id)
	}
	for _, r := range id {
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
		if !isHex {
			return fmt.Errorf("resticrun: snapshot id %q is not hexadecimal", id)
		}
	}
	return nil
}

// Dump writes one file out of a snapshot, without restoring anything to
// disk.
//
// It exists so a page can say what a backup holds. The parts of an account
// that are not files -- its DNS zones, its certificates, its cron jobs, its
// database users -- are inside a single archive or a single SQL file in the
// snapshot, so "restic ls" can see the container and nothing in it. Reading
// that container is the only way to list what it holds, and restoring it to
// disk first would mean a restore to answer a question.
//
// The output is streamed rather than captured: a metadata archive runs to
// tens of megabytes, and the caller reads names out of it as it arrives.
func (r *Runner) Dump(ctx context.Context, repo Repository, snapshotID, path string, out io.Writer) error {
	if err := validateSnapshotID(snapshotID); err != nil {
		return err
	}
	if !strings.HasPrefix(path, "/") || strings.Contains(path, "..") {
		return fmt.Errorf("resticrun: %q is not a path inside a snapshot", path)
	}
	if out == nil {
		return fmt.Errorf("resticrun: dump has nowhere to write")
	}
	result, err := r.run(ctx, repo, []string{"dump", snapshotID, path}, secondary{}, nil, out)
	if err != nil {
		return err
	}
	return classifyExit(result.ExitCode, result.Stderr, false)
}
