package resticrun

import (
	"context"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/destination"
)

// TestARefusedDeletionIsNotASuccessfulOne.
//
// restic exits zero from a forget in which every removal was refused: it
// prints "unable to remove ... from the repository" and stops there. Read
// by the exit code alone, retention against an append-only endpoint
// reports that it removed a snapshot which is still in the repository --
// and a maintenance run that says it pruned, against storage that grows
// forever, is the worst shape this failure can take.
func TestARefusedDeletionIsNotASuccessfulOne(t *testing.T) {
	const id = "a99c69680cc6e8a34dd956c94a3993e78cd787f03d684c60dbd89e430678e8c3"
	refused := ExecFunc(func(ctx context.Context, cmd Command) (CommandResult, error) {
		return CommandResult{
			ExitCode: 0,
			Stdout:   []byte("[0:00] 0.00%  0 / 1 files deleted\n"),
			Stderr: []byte("Remove(<snapshot/a99c69680c>) failed: " +
				"unexpected HTTP response (403): 403 Forbidden\n" +
				"unable to remove snapshot/" + id + " from the repository\n"),
		}, nil
	})
	runner := New(Config{}, refused)
	repo := Repository{Dest: &destination.Local{Root: "/srv/b"}, Path: "cp01", Password: "p"}

	err := runner.ForgetSnapshots(context.Background(), repo, []string{id})
	if err == nil {
		t.Fatal("a forget whose removals were all refused reported success")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error = %v, want the reason the storage gave", err)
	}

	// Prune is the other half. It reports the same way and matters more:
	// it is what an operator runs when a repository is too big.
	if err := runner.Prune(context.Background(), repo); err == nil {
		t.Fatal("a prune that could remove nothing reported success")
	}
}

// TestAForgetThatRemovedWhatItNamedIsStillASuccess guards the other side:
// the check reads restic's own words, and restic says plenty on a healthy
// run that must not be mistaken for a refusal.
func TestAForgetThatRemovedWhatItNamedIsStillASuccess(t *testing.T) {
	const id = "a99c69680cc6e8a34dd956c94a3993e78cd787f03d684c60dbd89e430678e8c3"
	fine := ExecFunc(func(ctx context.Context, cmd Command) (CommandResult, error) {
		return CommandResult{
			ExitCode: 0,
			Stdout:   []byte("[0:00] 100.00%  1 / 1 files deleted\n"),
			Stderr:   []byte("removing 1 snapshot\n"),
		}, nil
	})
	runner := New(Config{}, fine)
	repo := Repository{Dest: &destination.Local{Root: "/srv/b"}, Path: "cp01", Password: "p"}

	if err := runner.ForgetSnapshots(context.Background(), repo, []string{id}); err != nil {
		t.Fatalf("a forget that worked was read as a failure: %v", err)
	}
}
