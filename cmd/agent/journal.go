package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// journalStreamSocket is where a process connects to hand journald its
// output, which is what systemd does on a service's behalf at start.
const journalStreamSocket = "/run/systemd/journal/stdout"

// journalHeader opens a stream: identifier, unit, priority, level prefix, and
// whether to forward to syslog, the kernel log and the console. The unit stays
// empty because journald reads it from the connecting process's cgroup, which
// is what "journalctl -u gniza" matches on.
const journalHeader = "gniza-agent\n\n6\n0\n0\n0\n0\n"

// surviveBrokenLog stops a dead log from ending the service.
//
// The agent's output is a socket to journald, and cPanel's nightly update
// restarts journald. The next line written to it fails, and a Go program whose
// write to stdout or stderr fails that way dies of SIGPIPE, which systemd
// reports as a clean exit. It took backups and upgrades with it, for the sake
// of a log line.
//
// Asking for the signal is what turns it into an ordinary write error. Nothing
// reads the channel, and nothing needs to: a full one drops the signal.
// signal.Ignore would do it too, but a process started from here inherits an
// ignored signal, and restic and pkgacct are not to be changed by this.
//
// Only for the service. A command run by hand and piped into head should still
// stop when head does.
func surviveBrokenLog() {
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)
}

// logWriter writes the log to stderr, and connects back to journald when that
// connection has gone. Staying alive is not enough: the log would be empty from
// then until somebody restarted the service, and that is the night it is read.
type logWriter struct {
	mu sync.Mutex
	// socket is journald's, or empty when this process was not started with
	// its output going there and has nothing to go back to.
	socket string
}

// newLogWriter connects back only if systemd connected it to the journal in
// the first place, which it says in JOURNAL_STREAM.
func newLogWriter() *logWriter {
	if os.Getenv("JOURNAL_STREAM") == "" {
		return &logWriter{}
	}
	return &logWriter{socket: journalStreamSocket}
}

// Write does not report a line it could not deliver. The logger has nowhere to
// say so, and the next line tries again.
func (w *logWriter) Write(line []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := os.Stderr.Write(line)
	if !brokenPipe(err) {
		return n, err
	}
	if w.socket != "" && reconnectJournal(w.socket) == nil {
		_, _ = os.Stderr.Write(line)
	}
	return len(line), nil
}

// brokenPipe reports whether a write failed because the far end has gone.
func brokenPipe(err error) bool {
	return errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET)
}

// reconnectJournal puts a new stream to journald on stdout and stderr, in
// place, so the standard library's own logger and a panic's stack trace reach
// the journal as well as this package's lines.
func reconnectJournal(socket string) error {
	conn, err := net.DialTimeout("unix", socket, 2*time.Second)
	if err != nil {
		return fmt.Errorf("connect to journald: %w", err)
	}
	defer conn.Close()
	if err := conn.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return err
	}
	if _, err := conn.Write([]byte(journalHeader)); err != nil {
		return fmt.Errorf("open a journald stream: %w", err)
	}
	stream, err := conn.(*net.UnixConn).File()
	if err != nil {
		return err
	}
	defer stream.Close()
	// Fd is also what puts the descriptor back into blocking mode, which is
	// how stdout and stderr are expected to behave.
	fd := int(stream.Fd())
	for _, target := range []int{int(os.Stdout.Fd()), int(os.Stderr.Fd())} {
		if err := unix.Dup3(fd, target, 0); err != nil {
			return fmt.Errorf("put the journald stream on descriptor %d: %w", target, err)
		}
	}
	return nil
}
