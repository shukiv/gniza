package resticrun

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// BackupSummary is restic's final "summary" message from "backup --json".
//
// Field names follow restic's documented scripting output. Unknown fields
// are ignored: restic states that new message types and fields may be added
// over time, so the parser must tolerate them.
type BackupSummary struct {
	MessageType         string    `json:"message_type"`
	FilesNew            uint64    `json:"files_new"`
	FilesChanged        uint64    `json:"files_changed"`
	FilesUnmodified     uint64    `json:"files_unmodified"`
	DirsNew             uint64    `json:"dirs_new"`
	DirsChanged         uint64    `json:"dirs_changed"`
	DirsUnmodified      uint64    `json:"dirs_unmodified"`
	DataBlobs           int64     `json:"data_blobs"`
	TreeBlobs           int64     `json:"tree_blobs"`
	DataAdded           uint64    `json:"data_added"`
	DataAddedPacked     uint64    `json:"data_added_packed"`
	TotalFilesProcessed uint64    `json:"total_files_processed"`
	TotalBytesProcessed uint64    `json:"total_bytes_processed"`
	TotalDuration       float64   `json:"total_duration"`
	BackupStart         time.Time `json:"backup_start"`
	BackupEnd           time.Time `json:"backup_end"`
	SnapshotID          string    `json:"snapshot_id"`
	DryRun              bool      `json:"dry_run"`
}

// ErrNoSummary means restic produced no summary message. That happens when
// the run failed before completing, and must never be reported as success.
var ErrNoSummary = errors.New("resticrun: no summary message in restic output")

// ParseBackupSummary extracts the summary from a "backup --json" stream.
//
// The stream is newline-delimited JSON carrying many status messages before
// the single summary. Malformed lines are skipped rather than fatal: a
// progress line restic garbled under a narrow terminal must not lose us the
// snapshot ID.
func ParseBackupSummary(stdout []byte) (BackupSummary, error) {
	summary, found, err := lastMessage[BackupSummary](stdout, "summary")
	if err != nil {
		return BackupSummary{}, err
	}
	if !found {
		return BackupSummary{}, ErrNoSummary
	}
	return summary, nil
}

// Exit codes restic documents. Anything else is treated as a plain failure.
const (
	exitOK = 0
	// exitIncompleteRead means "backup" could not read some source files.
	// A snapshot was still created, so the copy exists but is incomplete.
	//
	// The same code means something quite different for other commands —
	// "forget" uses it for "failed to remove one or more snapshots" — so
	// it is only forgiven where a snapshot really was produced.
	exitIncompleteRead = 3
)

// classifyExit maps a restic exit code to an error.
//
// incompleteOK must only be set for "backup". On a live cPanel server,
// transient unreadable files (a session file deleted mid-run, a socket) are
// routine, and the resulting snapshot is still worth keeping; the caller
// records Incomplete and the job is surfaced as a warning. For every other
// command exit 3 is a failure, and treating it as success would let a
// prune blocked by an append-only destination look like a completed
// maintenance run while the repository grows without bound.
func classifyExit(code int, stderr []byte, incompleteOK bool) error {
	if code == exitOK || (incompleteOK && code == exitIncompleteRead) {
		return nil
	}
	return fmt.Errorf("resticrun: restic exited %d: %s", code, explain(stderr, 5))
}

// refusedRemoval reports what restic could not delete, and is checked
// separately from the exit code because restic does not use one for it.
//
// A forget whose every removal was refused -- which is what an append-only
// endpoint does to anything holding agent credentials -- prints "unable to
// remove <file> from the repository" and exits zero. Read by the exit code
// alone, retention says it removed a snapshot that is still there, and a
// repository that can never be pruned grows for months while the
// maintenance run reports success every night.
func refusedRemoval(stderr []byte) error {
	for _, line := range bytes.Split(stderr, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("unable to remove ")) ||
			(bytes.HasPrefix(line, []byte("Remove(")) && bytes.Contains(line, []byte(") failed:"))) {
			return fmt.Errorf("resticrun: the repository refused a deletion: %s",
				explain(stderr, 5))
		}
	}
	return nil
}

// explain summarises stderr for an error message: the last few meaningful
// lines, with Go stack frames dropped. restic prints a trace on fatal
// errors, and the frames crowd out the line that says what went wrong.
func explain(b []byte, n int) string {
	lines := bytes.Split(bytes.TrimSpace(b), []byte("\n"))
	kept := make([][]byte, 0, n)
	for i := len(lines) - 1; i >= 0 && len(kept) < n; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 || isStackFrame(lines[i]) {
			continue
		}
		kept = append([][]byte{line}, kept...)
	}
	if len(kept) == 0 {
		return "(no output)"
	}
	return string(bytes.Join(kept, []byte("; ")))
}

// isStackFrame recognises the two shapes Go traces take: an indented file
// and line, and the qualified function name above it.
func isStackFrame(line []byte) bool {
	if len(line) > 0 && (line[0] == '\t' || line[0] == ' ') {
		return bytes.Contains(line, []byte(".go:")) || bytes.Contains(line, []byte(".s:"))
	}
	trimmed := bytes.TrimSpace(line)
	return bytes.HasPrefix(trimmed, []byte("runtime.")) ||
		bytes.HasPrefix(trimmed, []byte("main.init")) ||
		bytes.HasPrefix(trimmed, []byte("github.com/restic/restic/"))
}

// Progress is restic's account of a backup while it is still running.
//
// restic prints one of these per second on the "backup --json" stream. It
// is what the interface shows instead of an unmoving "Running" pill: a
// nightly run of a large account takes long enough that "is it stuck?" is
// a fair question to ask of it.
type Progress struct {
	PercentDone    float64 `json:"percent_done"`
	TotalFiles     uint64  `json:"total_files"`
	FilesDone      uint64  `json:"files_done"`
	TotalBytes     uint64  `json:"total_bytes"`
	BytesDone      uint64  `json:"bytes_done"`
	SecondsElapsed float64 `json:"seconds_elapsed"`
	// CurrentFiles is what restic is reading right now. Kept out of the
	// interface: an operator watching a backup does not need to be told
	// which of an account's files is open at this instant, and a path can
	// be a customer's private business.
	CurrentFiles []string `json:"current_files,omitempty"`
}

// progressReader turns restic's status lines into Progress calls. It
// returns nil when nobody is listening, so nothing is parsed for a caller
// that does not want it.
func progressReader(onProgress func(Progress)) func([]byte) {
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
		var progress Progress
		if err := json.Unmarshal(line, &progress); err != nil {
			// A status line this program cannot read is not worth failing
			// a backup over; the summary is what the job is judged on.
			return
		}
		onProgress(progress)
	}
}
