package main

import (
	"bufio"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// brokenLogHelper is set in the child these tests start. Writing to a broken
// stderr ends a Go process, and that has to be seen from outside it.
const brokenLogHelper = "GNIZA_TEST_BROKEN_LOG_HELPER"

// TestBrokenLogHelper is the child, and does nothing in an ordinary test run.
func TestBrokenLogHelper(t *testing.T) {
	if os.Getenv(brokenLogHelper) == "" {
		t.Skip("run by the tests below")
	}
	surviveBrokenLog()
	log := &logWriter{socket: os.Getenv("GNIZA_TEST_JOURNAL_SOCKET")}
	if _, err := log.Write([]byte("after the journal went away\n")); err != nil {
		os.Exit(3)
	}
	if _, err := os.Stdout.Write([]byte("stdout came back too\n")); err != nil && log.socket != "" {
		os.Exit(4)
	}
	if err := os.WriteFile(os.Getenv(brokenLogHelper), []byte("alive"), 0o600); err != nil {
		os.Exit(5)
	}
	os.Exit(0)
}

// startWithBrokenLog runs the child with stdout and stderr on a pipe nobody
// is reading, which is what the agent is left holding when journald goes.
func startWithBrokenLog(t *testing.T, socket string) (string, error) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "alive")
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestBrokenLogHelper$")
	child.Env = append(os.Environ(), brokenLogHelper+"="+marker, "GNIZA_TEST_JOURNAL_SOCKET="+socket)
	child.Stdout, child.Stderr = writer, writer
	return marker, child.Run()
}

// TestTheAgentOutlivesABrokenLog is the night cPanel restarted systemd-journald
// on .144, .148 and .163: the next log line killed the agent, and whatever it
// was doing with it, with a signal systemd reports as a clean exit.
func TestTheAgentOutlivesABrokenLog(t *testing.T) {
	marker, err := startWithBrokenLog(t, "")
	if err != nil {
		t.Fatalf("the agent did not survive its log going away: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the agent stopped before it finished: %v", err)
	}
}

// TestABrokenLogReconnectsToTheJournal is the other half: alive and silent
// would leave journalctl empty until somebody restarted the service. The bytes
// are the header systemd itself sends when it connects a service to journald.
func TestABrokenLogReconnectsToTheJournal(t *testing.T) {
	dir, err := os.MkdirTemp("", "gnz")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "stdout")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	received := make(chan []string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var lines []string
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
		}
		received <- lines
	}()

	marker, err := startWithBrokenLog(t, socket)
	if err != nil {
		t.Fatalf("the agent did not survive its log going away: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the agent stopped before it finished: %v", err)
	}

	select {
	case lines := <-received:
		want := []string{"gniza-agent", "", "6", "0", "0", "0", "0",
			"after the journal went away", "stdout came back too"}
		if strings.Join(lines, "|") != strings.Join(want, "|") {
			t.Fatalf("journald was sent %q, want %q", lines, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing reached the journal")
	}
}

func TestOnlyABrokenPipeCountsAsOne(t *testing.T) {
	if !brokenPipe(&os.PathError{Op: "write", Path: "/dev/stderr", Err: syscall.EPIPE}) {
		t.Error("a broken pipe was not recognised through the error os.File returns")
	}
	if !brokenPipe(&os.PathError{Op: "write", Path: "/dev/stderr", Err: syscall.ECONNRESET}) {
		t.Error("a reset connection was not recognised through the error os.File returns")
	}
	if brokenPipe(io.ErrShortWrite) || brokenPipe(nil) || brokenPipe(errors.New("disk full")) {
		t.Error("something other than a broken pipe was taken for one")
	}
}
