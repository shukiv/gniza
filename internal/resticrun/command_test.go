package resticrun_test

import (
	"context"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/resticrun"
)

// TestTruncatedOutputSaysSo covers a listing that was cut off to stay
// inside the memory cap. It parses cleanly and is short, so nothing
// downstream can tell it from a complete answer: an account browsing its
// backup would be shown some of its files and told that was all of them.
func TestTruncatedOutputSaysSo(t *testing.T) {
	exec := &resticrun.OSExec{MaxOutputBytes: 64}
	result, err := exec.Exec(context.Background(), resticrun.Command{
		Path: "/bin/sh",
		Args: []string{"-c", "head -c 4096 /dev/zero | tr '\\0' 'x'"},
	})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !result.Truncated {
		t.Fatal("output was cut off and the result did not say so")
	}
	if len(result.Stdout) > 64 {
		t.Fatalf("the cap did not hold: %d bytes", len(result.Stdout))
	}

	whole, err := exec.Exec(context.Background(), resticrun.Command{
		Path: "/bin/sh", Args: []string{"-c", "echo short"},
	})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if whole.Truncated {
		t.Fatal("output that fitted was reported as cut off")
	}
}

// TestTheDefaultWaitDelayIsNotForever guards the value production uses.
// Nothing outside the tests sets WaitDelay, so if the zero value reached
// exec unchanged, every caller would keep the behaviour this package
// changed to avoid.
func TestTheDefaultWaitDelayIsNotForever(t *testing.T) {
	if delay := resticrun.WaitDelayOf(&resticrun.OSExec{}); delay <= 0 {
		t.Errorf("default wait delay = %v, want a positive bound", delay)
	}
	want := 250 * time.Millisecond
	if delay := resticrun.WaitDelayOf(&resticrun.OSExec{WaitDelay: want}); delay != want {
		t.Errorf("wait delay = %v, want the one that was set (%v)", delay, want)
	}
}
