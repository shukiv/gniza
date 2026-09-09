// Package resticrun executes the restic binary.
//
// There is one runner for every destination type. Destinations supply the
// repository URI and backend environment (see internal/destination); this
// package owns argument construction, credential handling, execution and
// JSON parsing, so that logic exists once rather than once per backend.
package resticrun

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"time"
)

// Command is a single restic invocation.
type Command struct {
	Path string
	Args []string
	// Env is the complete environment for the child process, in "K=V"
	// form. Credentials travel here and never in Args, because
	// /proc/<pid>/cmdline is world-readable.
	Env []string
	Dir string
	// OnLine, when set, is called with each complete line of stdout as it
	// arrives. restic reports backup progress as one JSON object per line,
	// and an operator watching a five-minute upload should not have to
	// wait for the summary to know it is moving.
	OnLine func(line []byte)
	// Stdout, when set, receives the process's standard output instead of
	// it being captured in the result. What comes out of "restic dump" is
	// as large as the file it dumps, which for an account's metadata
	// archive is tens of megabytes -- more than the capture cap holds, and
	// more than belongs in memory to read a list of names out of.
	Stdout io.Writer
}

// CommandResult is the outcome of an invocation that actually ran. A
// non-zero ExitCode is reported here rather than as an error, so callers
// can distinguish restic's partial-success codes from a failure to start.
type CommandResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	// Truncated says output was dropped to stay inside the cap. It
	// matters because a caller parsing a listing would otherwise read a
	// cut-off answer as a complete one, and quietly show an account
	// fewer files than its backup holds.
	Truncated bool
}

// Execer runs a Command. Tests substitute a fake so the suite needs no
// restic binary.
type Execer interface {
	Exec(ctx context.Context, cmd Command) (CommandResult, error)
}

// ExecFunc adapts a function to Execer.
type ExecFunc func(ctx context.Context, cmd Command) (CommandResult, error)

func (f ExecFunc) Exec(ctx context.Context, cmd Command) (CommandResult, error) {
	return f(ctx, cmd)
}

// OSExec runs commands as real child processes.
type OSExec struct {
	// MaxOutputBytes caps captured stdout and stderr. Zero means 8 MiB.
	MaxOutputBytes int
	// WaitDelay bounds how long a killed process's output is still read
	// after the context is cancelled. Zero means ten seconds.
	WaitDelay time.Duration
}

var _ Execer = (*OSExec)(nil)

func (o *OSExec) Exec(ctx context.Context, cmd Command) (CommandResult, error) {
	limit := o.MaxOutputBytes
	if limit <= 0 {
		limit = 8 << 20
	}

	c := exec.CommandContext(ctx, cmd.Path, cmd.Args...)
	c.Env = cmd.Env
	c.Dir = cmd.Dir
	// restic's sftp backend runs ssh as a child of its own, and that
	// grandchild inherits the pipe this reads restic's output through.
	// Killing restic on cancellation leaves the pipe open, so a Wait
	// without a deadline waits for a process nobody is going to stop.
	c.WaitDelay = o.waitDelay()

	var stdout, stderr bytes.Buffer
	capped := &cappedWriter{buf: &stdout, limit: limit}
	var out io.Writer = capped
	if cmd.Stdout != nil {
		out = cmd.Stdout
	}
	if cmd.OnLine != nil {
		out = io.MultiWriter(out, &lineWriter{emit: cmd.OnLine})
	}
	c.Stdout = out
	cappedErr := &cappedWriter{buf: &stderr, limit: limit}
	c.Stderr = cappedErr

	err := c.Run()
	result := CommandResult{
		Stdout: stdout.Bytes(), Stderr: stderr.Bytes(),
		Truncated: capped.dropped || cappedErr.dropped,
	}

	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return result, nil
	case errors.As(err, &exitErr):
		// The process ran and exited non-zero. That is a result, not a
		// failure to execute; the caller decides what the code means.
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	default:
		return result, err
	}
}

// waitDelay is how long output is still read after the child is killed.
// It is never zero: zero is exec's "wait forever", which is the thing
// this exists to prevent.
func (o *OSExec) waitDelay() time.Duration {
	if o.WaitDelay > 0 {
		return o.WaitDelay
	}
	return 10 * time.Second
}

// cappedWriter discards output past a limit so a pathological restic run
// cannot exhaust the agent's memory.
type cappedWriter struct {
	buf   *bytes.Buffer
	limit int
	// dropped records that this writer threw output away, so a caller
	// can tell a short answer from a complete one.
	dropped bool
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	remaining := w.limit - w.buf.Len()
	switch {
	case remaining <= 0:
		w.dropped = true
	case len(p) > remaining:
		w.buf.Write(p[:remaining])
		w.dropped = true
	default:
		w.buf.Write(p)
	}
	// Always report a full write: truncation is intentional and must not
	// look like a broken pipe to the child process.
	return len(p), nil
}

// lineWriter calls emit once per complete line written through it.
//
// A partial line is held until the rest arrives, so a caller parsing JSON
// never sees half an object. Anything left unterminated when the process
// exits is dropped: restic's last line is the summary, which is read from
// the captured output rather than from here.
type lineWriter struct {
	emit    func(line []byte)
	pending []byte
}

// maxPendingLine bounds the held partial line, so output with no newline
// at all cannot grow without limit.
const maxPendingLine = 1 << 20

func (w *lineWriter) Write(p []byte) (int, error) {
	written := len(p)
	for len(p) > 0 {
		newline := bytes.IndexByte(p, '\n')
		if newline < 0 {
			if len(w.pending)+len(p) <= maxPendingLine {
				w.pending = append(w.pending, p...)
			}
			return written, nil
		}
		line := p[:newline]
		if len(w.pending) > 0 {
			line = append(w.pending, line...)
			w.pending = w.pending[:0]
		}
		if len(line) > 0 {
			w.emit(line)
		}
		p = p[newline+1:]
	}
	return written, nil
}
