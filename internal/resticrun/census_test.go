package resticrun

import (
	"context"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/destination"
)

func TestARepositoryIsMeasuredWithoutTakingItsLock(t *testing.T) {
	var ran string
	r := New(Config{RuntimeDir: t.TempDir()}, ExecFunc(func(_ context.Context, cmd Command) (CommandResult, error) {
		ran = strings.Join(cmd.Args, " ")
		return CommandResult{Stdout: []byte(
			`{"total_size":103079215104,"total_blob_count":812345,"snapshots_count":294}`)}, nil
	}))
	census, err := r.Census(t.Context(), Repository{
		Dest: &destination.Local{Root: t.TempDir()}, Path: "repo", Password: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if census.SizeBytes != 103079215104 || census.Snapshots != 294 {
		t.Fatalf("census = %+v", census)
	}
	// raw-data is what the repository costs its destination, which is the
	// question an operator with a full disk is asking. And a measurement
	// must never be the reason a backup cannot start, so it takes no lock.
	for _, want := range []string{"stats", "--json", "--no-lock", "--mode raw-data"} {
		if !strings.Contains(ran, want) {
			t.Errorf("the measurement did not pass %s: %s", want, ran)
		}
	}
}

// A repository restic cannot read is not a repository of no size. Saying
// zero would draw an empty destination on the page and quietly retire the
// last figure anyone had.
func TestARepositoryThatCannotBeMeasuredSaysSoRatherThanZero(t *testing.T) {
	for _, output := range []string{`{}`, `{"total_size":12}`, `not json`} {
		r := New(Config{RuntimeDir: t.TempDir()}, ExecFunc(func(_ context.Context, cmd Command) (CommandResult, error) {
			return CommandResult{Stdout: []byte(output)}, nil
		}))
		if _, err := r.Census(t.Context(), Repository{
			Dest: &destination.Local{Root: t.TempDir()}, Path: "repo", Password: "test"}); err == nil {
			t.Errorf("%q was accepted as a measurement", output)
		}
	}
}

// An empty repository is a real answer, and one an operator needs: a
// destination that has been configured and never written to.
func TestARepositoryWithNothingInItMeasuresAsEmpty(t *testing.T) {
	r := New(Config{RuntimeDir: t.TempDir()}, ExecFunc(func(_ context.Context, cmd Command) (CommandResult, error) {
		return CommandResult{Stdout: []byte(`{"total_size":0,"snapshots_count":0,"total_blob_count":0}`)}, nil
	}))
	census, err := r.Census(t.Context(), Repository{
		Dest: &destination.Local{Root: t.TempDir()}, Path: "repo", Password: "test"})
	if err != nil || census.Snapshots != 0 || census.SizeBytes != 0 {
		t.Fatalf("census = %+v, err = %v", census, err)
	}
}

// restic that refused says why. A measurement that ignored the exit code
// would read a partial line of output as the whole repository.
func TestARepositoryMeasurementRespectsTheExitCode(t *testing.T) {
	r := New(Config{RuntimeDir: t.TempDir()}, ExecFunc(func(_ context.Context, cmd Command) (CommandResult, error) {
		return CommandResult{ExitCode: 1, Stderr: []byte("Fatal: unable to open config file"),
			Stdout: []byte(`{"total_size":0,"snapshots_count":0}`)}, nil
	}))
	if _, err := r.Census(t.Context(), Repository{
		Dest: &destination.Local{Root: t.TempDir()}, Path: "repo", Password: "test"}); err == nil {
		t.Fatal("a failed measurement was read as an empty repository")
	}
}
