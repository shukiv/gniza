package resticrun

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/destination"
)

const (
	firstLock  = "553a8a7a00000000000000000000000000000000000000000000000000000001"
	secondLock = "734614f100000000000000000000000000000000000000000000000000000002"
)

func lockedRepo(t *testing.T) Repository {
	t.Helper()
	return Repository{Dest: &destination.Local{Root: t.TempDir()}, Path: "repo", Password: "test"}
}

// The locks are read without taking one: they are asked for when
// something else could not take its own.
func TestLocksAreReadWithoutTakingOne(t *testing.T) {
	var ran []string
	r := New(Config{RuntimeDir: t.TempDir()}, ExecFunc(func(_ context.Context, cmd Command) (CommandResult, error) {
		line := strings.Join(cmd.Args, " ")
		ran = append(ran, line)
		switch {
		case strings.Contains(line, "list locks"):
			return CommandResult{Stdout: []byte(firstLock + "\n" + secondLock + "\n")}, nil
		case strings.Contains(line, firstLock):
			return CommandResult{Stdout: []byte(`{"time":"2026-09-09T02:08:11.5+03:00",` +
				`"exclusive":false,"hostname":"uscp","username":"root","pid":338155,"uid":0,"gid":0}`)}, nil
		default:
			return CommandResult{Stdout: []byte(`{"time":"2026-09-09T04:14:37+03:00",` +
				`"exclusive":true,"hostname":"uscp","username":"root","pid":390771}`)}, nil
		}
	}))
	locks, err := r.Locks(t.Context(), lockedRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(locks) != 2 || locks[0].PID != 338155 || locks[0].Hostname != "uscp" ||
		locks[0].ID != firstLock || !locks[1].Exclusive {
		t.Fatalf("locks = %+v", locks)
	}
	for _, line := range ran {
		if !strings.Contains(line, "--no-lock") {
			t.Errorf("%q would wait behind the lock it was sent to read", line)
		}
	}
}

// What restic lists is put on a command line, so anything that is not a
// lock's name stops the reading rather than becoming an argument.
func TestALockNameThatIsNotOneIsRefused(t *testing.T) {
	r := New(Config{RuntimeDir: t.TempDir()}, ExecFunc(func(_ context.Context, cmd Command) (CommandResult, error) {
		return CommandResult{Stdout: []byte("--remove-all\n")}, nil
	}))
	if _, err := r.Locks(t.Context(), lockedRepo(t)); err == nil {
		t.Fatal("a line that is not a lock id was passed on")
	}
}

func TestALockIsStaleByAgeOrByADeadProcessOnThisHost(t *testing.T) {
	now := time.Date(2026, 9, 28, 16, 0, 0, 0, time.UTC)
	dead := func(int) bool { return false }
	living := func(int) bool { return true }
	for _, c := range []struct {
		name  string
		lock  Lock
		host  string
		alive func(int) bool
		want  bool
	}{
		{"nineteen days old", Lock{Time: now.Add(-19 * 24 * time.Hour), Hostname: "uscp", PID: 1}, "uscp", living, true},
		{"old, from another host", Lock{Time: now.Add(-time.Hour), Hostname: "other", PID: 1}, "uscp", living, true},
		{"fresh, process gone", Lock{Time: now.Add(-time.Minute), Hostname: "uscp", PID: 1}, "uscp", dead, true},
		{"fresh, process running", Lock{Time: now.Add(-time.Minute), Hostname: "uscp", PID: 1}, "uscp", living, false},
		{"fresh, another host", Lock{Time: now.Add(-time.Minute), Hostname: "other", PID: 1}, "uscp", dead, false},
		{"fresh, host unknown", Lock{Time: now.Add(-time.Minute), Hostname: "uscp", PID: 1}, "", dead, false},
	} {
		if got := c.lock.Stale(now, c.host, c.alive); got != c.want {
			t.Errorf("%s: stale = %v, want %v", c.name, got, c.want)
		}
	}
}

// A locked repository is the failure a caller can act on, so it has to
// be recognisable however restic reported it.
func TestALockedRepositoryIsToldApartFromOtherFailures(t *testing.T) {
	if err := classifyExit(11, []byte("repository is already locked by PID 338155"), false); !errors.Is(err, ErrLocked) {
		t.Errorf("exit 11 = %v, want ErrLocked", err)
	}
	if err := classifyExit(1, []byte("no such host"), false); errors.Is(err, ErrLocked) {
		t.Error("an unreachable destination was reported as a lock")
	}
	_, err := ParseForgetPlan([]byte(`{"message_type":"exit_error","code":11,"message":"repository is already locked"}`))
	if !errors.Is(err, ErrLocked) {
		t.Errorf("a locked forget = %v, want ErrLocked", err)
	}
}

func TestUnlockAsksResticToRemoveOnlyWhatIsStale(t *testing.T) {
	var ran string
	r := New(Config{RuntimeDir: t.TempDir()}, ExecFunc(func(_ context.Context, cmd Command) (CommandResult, error) {
		ran = strings.Join(cmd.Args, " ")
		return CommandResult{}, nil
	}))
	if err := r.Unlock(t.Context(), lockedRepo(t)); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(ran, "unlock") || strings.Contains(ran, "remove-all") {
		t.Errorf("ran %q", ran)
	}
}
