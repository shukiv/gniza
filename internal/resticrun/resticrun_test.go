package resticrun

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/destination"
)

func TestBackupArgs(t *testing.T) {
	args, err := BackupArgs(BackupSpec{
		Paths:          []string{"/stage/metadata", "/home/customer1"},
		Tags:           []string{"account:customer1", "job:42"},
		Host:           "cp01",
		Exclude:        []string{"*.sock"},
		LimitUploadKiB: 4096,
	})
	if err != nil {
		t.Fatalf("BackupArgs: %v", err)
	}
	want := []string{
		"backup", "--json",
		"--host", "cp01",
		"--tag", "account:customer1",
		"--tag", "job:42",
		"--exclude", "*.sock",
		"--limit-upload", "4096",
		"/stage/metadata", "/home/customer1",
	}
	assertArgs(t, args, want)

	if _, err := BackupArgs(BackupSpec{}); err == nil {
		t.Error("backup with no paths should be rejected")
	}
}

func TestInitArgsCopiesChunkerParams(t *testing.T) {
	assertArgs(t, InitArgs(""), []string{"init", "--repository-version", "2"})

	// Chunker parameters are immutable after creation, so every repository
	// after the first must be seeded from the server's chunker source.
	assertArgs(t, InitArgs("rest:https://backup/cp01/"), []string{
		"init", "--repository-version", "2",
		"--from-repo", "rest:https://backup/cp01/",
		"--copy-chunker-params",
	})
}

func TestForgetArgsRequiresKeepPolicy(t *testing.T) {
	if _, err := ForgetArgs(ForgetSpec{Prune: true}); err == nil {
		t.Fatal("forget with no keep policy should be rejected: it would delete everything")
	}
	if _, err := ForgetArgs(ForgetSpec{KeepDaily: -1}); err == nil {
		t.Error("negative keep count should be rejected")
	}

	args, err := ForgetArgs(ForgetSpec{
		KeepDaily: 7, KeepMonthly: 6, Tags: []string{"account:c1"},
		GroupBy: "host,tags", Prune: true,
	})
	if err != nil {
		t.Fatalf("ForgetArgs: %v", err)
	}
	assertArgs(t, args, []string{
		"forget", "--json",
		"--keep-daily", "7",
		"--keep-monthly", "6",
		"--tag", "account:c1",
		"--group-by", "host,tags",
		"--prune",
	})
}

func TestCheckArgs(t *testing.T) {
	structureOnly, err := CheckArgs(CheckSpec{})
	if err != nil {
		t.Fatalf("CheckArgs: %v", err)
	}
	assertArgs(t, structureOnly, []string{"check"})

	withData, err := CheckArgs(CheckSpec{ReadDataSubsetPercent: 5})
	if err != nil {
		t.Fatalf("CheckArgs: %v", err)
	}
	assertArgs(t, withData, []string{"check", "--read-data-subset", "5%"})

	if _, err := CheckArgs(CheckSpec{ReadDataSubsetPercent: 101}); err == nil {
		t.Error("out-of-range subset should be rejected")
	}
}

const sampleBackupJSON = `{"message_type":"status","percent_done":0.5}
not json at all
{"message_type":"verbose_status","action":"new","item":"/home/c1/x"}
{"message_type":"summary","files_new":12,"files_changed":3,"files_unmodified":900,` +
	`"data_blobs":40,"tree_blobs":5,"data_added":1048576,"data_added_packed":524288,` +
	`"total_files_processed":915,"total_bytes_processed":201850880,"total_duration":42.5,` +
	`"snapshot_id":"40dc15203b1cf9"}
`

func TestParseBackupSummary(t *testing.T) {
	summary, err := ParseBackupSummary([]byte(sampleBackupJSON))
	if err != nil {
		t.Fatalf("ParseBackupSummary: %v", err)
	}
	if summary.SnapshotID != "40dc15203b1cf9" {
		t.Errorf("SnapshotID = %q", summary.SnapshotID)
	}
	if summary.DataAdded != 1048576 {
		t.Errorf("DataAdded = %d, want 1048576", summary.DataAdded)
	}
	if summary.TotalBytesProcessed != 201850880 {
		t.Errorf("TotalBytesProcessed = %d, want 201850880", summary.TotalBytesProcessed)
	}
}

func TestParseBackupSummaryMissing(t *testing.T) {
	_, err := ParseBackupSummary([]byte(`{"message_type":"status","percent_done":0.5}` + "\n"))
	if !errors.Is(err, ErrNoSummary) {
		t.Fatalf("err = %v, want ErrNoSummary", err)
	}
}

// fakeExec records the command it was given and replays a canned result.
type fakeExec struct {
	got    Command
	result CommandResult
	err    error
}

func (f *fakeExec) Exec(_ context.Context, cmd Command) (CommandResult, error) {
	f.got = cmd
	return f.result, f.err
}

func TestRunnerBackupPassesSecretsByEnvironmentOnly(t *testing.T) {
	fake := &fakeExec{result: CommandResult{Stdout: []byte(sampleBackupJSON)}}
	runner := New(Config{Binary: "/usr/bin/restic", Compression: "max"}, fake)

	repo := Repository{
		Dest: &destination.S3{
			Endpoint: "s3.us-east-1.wasabisys.com", Bucket: "cp-backups",
			AccessKeyID: "AKIA-TEST", SecretAccessKey: "SECRET-TEST",
		},
		Path:     "cp01",
		Password: "repo-password",
	}

	result, err := runner.Backup(context.Background(), repo, BackupSpec{Paths: []string{"/stage"}})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if result.Summary.SnapshotID != "40dc15203b1cf9" {
		t.Errorf("SnapshotID = %q", result.Summary.SnapshotID)
	}
	if result.Incomplete {
		t.Error("Incomplete should be false for exit 0")
	}

	// Credentials must never reach the argument list: /proc/<pid>/cmdline
	// is world-readable on the servers this runs on.
	joined := strings.Join(fake.got.Args, " ")
	for _, secret := range []string{"repo-password", "SECRET-TEST", "AKIA-TEST"} {
		if strings.Contains(joined, secret) {
			t.Errorf("args %q leak secret %q", joined, secret)
		}
	}

	env := envMap(fake.got.Env)
	if env["RESTIC_REPOSITORY"] != "s3:https://s3.us-east-1.wasabisys.com/cp-backups/cp01" {
		t.Errorf("RESTIC_REPOSITORY = %q", env["RESTIC_REPOSITORY"])
	}
	if env["AWS_SECRET_ACCESS_KEY"] != "SECRET-TEST" {
		t.Errorf("AWS_SECRET_ACCESS_KEY missing from environment")
	}
	if env["RESTIC_COMPRESSION"] != "max" {
		t.Errorf("RESTIC_COMPRESSION = %q, want max", env["RESTIC_COMPRESSION"])
	}
	// The password goes in a file, not RESTIC_PASSWORD, so restic's
	// backend helpers do not inherit it.
	if _, set := env["RESTIC_PASSWORD"]; set {
		t.Error("RESTIC_PASSWORD should not be set; use RESTIC_PASSWORD_FILE")
	}
	if env["RESTIC_PASSWORD_FILE"] == "" {
		t.Error("RESTIC_PASSWORD_FILE should be set")
	}
}

func TestRunnerBackupIncompleteRead(t *testing.T) {
	fake := &fakeExec{result: CommandResult{Stdout: []byte(sampleBackupJSON), ExitCode: 3}}
	runner := New(Config{}, fake)
	repo := Repository{Dest: &destination.Local{Root: "/srv/b"}, Path: "cp01", Password: "p"}

	result, err := runner.Backup(context.Background(), repo, BackupSpec{Paths: []string{"/stage"}})
	if err != nil {
		t.Fatalf("exit 3 still produces a snapshot and must not be an error: %v", err)
	}
	if !result.Incomplete {
		t.Error("Incomplete should be true for exit 3")
	}
}

func TestRunnerBackupFailure(t *testing.T) {
	fake := &fakeExec{result: CommandResult{ExitCode: 1, Stderr: []byte("Fatal: unable to open config file")}}
	runner := New(Config{}, fake)
	repo := Repository{Dest: &destination.Local{Root: "/srv/b"}, Path: "cp01", Password: "p"}

	if _, err := runner.Backup(context.Background(), repo, BackupSpec{Paths: []string{"/stage"}}); err == nil {
		t.Fatal("exit 1 should be an error")
	} else if !strings.Contains(err.Error(), "unable to open config file") {
		t.Errorf("error should carry stderr context, got %v", err)
	}
}

func TestRunnerRejectsEmptyPassword(t *testing.T) {
	runner := New(Config{}, &fakeExec{})
	repo := Repository{Dest: &destination.Local{Root: "/srv/b"}, Path: "cp01"}
	if _, err := runner.Backup(context.Background(), repo, BackupSpec{Paths: []string{"/s"}}); err == nil {
		t.Error("empty repository password should be rejected")
	}
}

func TestPasswordFileIsRemoved(t *testing.T) {
	var seen string
	runner := New(Config{RuntimeDir: t.TempDir()}, ExecFunc(
		func(_ context.Context, cmd Command) (CommandResult, error) {
			seen = envMap(cmd.Env)["RESTIC_PASSWORD_FILE"]
			return CommandResult{Stdout: []byte(sampleBackupJSON)}, nil
		}))
	repo := Repository{Dest: &destination.Local{Root: "/srv/b"}, Path: "cp01", Password: "p"}

	if _, err := runner.Backup(context.Background(), repo, BackupSpec{Paths: []string{"/s"}}); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if seen == "" {
		t.Fatal("no password file was created")
	}
	if _, err := os.Stat(seen); err == nil {
		t.Errorf("password file %q still exists after the run", seen)
	}
}

func assertArgs(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("args = %v, want %v", got, want)
		}
	}
}

func envMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, entry := range env {
		if key, value, found := strings.Cut(entry, "="); found {
			out[key] = value
		}
	}
	return out
}

func TestRunnerInitSeedsChunkerParams(t *testing.T) {
	fake := &fakeExec{}
	runner := New(Config{}, fake)

	primary := Repository{
		Dest:     &destination.REST{BaseURL: "https://backup.example.com", Username: "cp01"},
		Path:     "cp01",
		Password: "p",
	}
	secondary := Repository{
		Dest:     &destination.Local{Root: "/srv/backups"},
		Path:     "cp01",
		Password: "p",
	}

	if err := runner.Init(context.Background(), secondary, &primary); err != nil {
		t.Fatalf("Init: %v", err)
	}
	assertArgs(t, fake.got.Args, []string{
		"init", "--repository-version", "2",
		"--from-repo", "rest:https://backup.example.com/cp01/",
		"--copy-chunker-params",
	})

	// The repository being created is the one in the environment; the
	// chunker source only appears as --from-repo.
	if got := envMap(fake.got.Env)["RESTIC_REPOSITORY"]; got != "/srv/backups/cp01" {
		t.Errorf("RESTIC_REPOSITORY = %q, want the new repository", got)
	}
}

func TestRunnerCheckAndForget(t *testing.T) {
	fake := &fakeExec{}
	runner := New(Config{}, fake)
	repo := Repository{Dest: &destination.Local{Root: "/srv/b"}, Path: "cp01", Password: "p"}

	if err := runner.Check(context.Background(), repo, CheckSpec{ReadDataSubsetPercent: 5}); err != nil {
		t.Fatalf("Check: %v", err)
	}
	assertArgs(t, fake.got.Args, []string{"check", "--read-data-subset", "5%"})

	if err := runner.Forget(context.Background(), repo, ForgetSpec{KeepDaily: 7, Prune: true}); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	assertArgs(t, fake.got.Args, []string{"forget", "--json", "--keep-daily", "7", "--group-by", "host,tags", "--dry-run"})

	// A forget with no keep policy would delete every snapshot, so it must
	// fail before restic is ever invoked.
	fake.got = Command{}
	if err := runner.Forget(context.Background(), repo, ForgetSpec{Prune: true}); err == nil {
		t.Error("forget without a keep policy should be rejected")
	}
	if fake.got.Path != "" {
		t.Error("restic must not be invoked for an invalid forget spec")
	}
}

func TestRunnerRejectsMissingDestination(t *testing.T) {
	runner := New(Config{}, &fakeExec{})
	if _, err := runner.Backup(context.Background(),
		Repository{Path: "cp01", Password: "p"},
		BackupSpec{Paths: []string{"/s"}}); err == nil {
		t.Error("repository without a destination should be rejected")
	}
}

func TestOSExecReportsExitCodeNotError(t *testing.T) {
	execer := &OSExec{}
	result, err := execer.Exec(context.Background(), Command{
		Path: "/bin/sh",
		Args: []string{"-c", "echo out; echo err >&2; exit 3"},
		Env:  []string{"PATH=/bin:/usr/bin"},
	})
	if err != nil {
		t.Fatalf("a process that ran and exited non-zero is a result, not an error: %v", err)
	}
	if result.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", result.ExitCode)
	}
	if strings.TrimSpace(string(result.Stdout)) != "out" {
		t.Errorf("Stdout = %q", result.Stdout)
	}
	if strings.TrimSpace(string(result.Stderr)) != "err" {
		t.Errorf("Stderr = %q", result.Stderr)
	}
}

func TestOSExecFailsWhenBinaryMissing(t *testing.T) {
	execer := &OSExec{}
	if _, err := execer.Exec(context.Background(), Command{
		Path: "/nonexistent/restic", Env: []string{},
	}); err == nil {
		t.Error("a binary that cannot start should be an error, not exit code 0")
	}
}

func TestCappedWriterTruncatesWithoutFailing(t *testing.T) {
	execer := &OSExec{MaxOutputBytes: 16}
	result, err := execer.Exec(context.Background(), Command{
		Path: "/bin/sh",
		Args: []string{"-c", "printf 'x%.0s' $(seq 1 10000)"},
		Env:  []string{"PATH=/bin:/usr/bin"},
	})
	if err != nil {
		t.Fatalf("truncation must not look like a broken pipe: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if len(result.Stdout) != 16 {
		t.Errorf("captured %d bytes, want the 16-byte cap", len(result.Stdout))
	}
}

func TestRunnerPassesBackendOptionsAsGlobalFlags(t *testing.T) {
	fake := &fakeExec{result: CommandResult{Stdout: []byte(sampleBackupJSON)}}
	runner := New(Config{}, fake)

	repo := Repository{
		Dest: &destination.SFTP{
			Host: "backup.example.com", User: "cpbackup", Root: "/backup",
			IdentityFile:   "/etc/gniza/id_ed25519",
			KnownHostsFile: "/etc/gniza/known_hosts",
		},
		Path:     "cp01",
		Password: "p",
	}
	if _, err := runner.Backup(context.Background(), repo, BackupSpec{Paths: []string{"/stage"}}); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	// Extended options are global flags and must precede the subcommand.
	if fake.got.Args[0] != "-o" {
		t.Fatalf("args = %v, want -o flags before the subcommand", fake.got.Args)
	}
	joined := strings.Join(fake.got.Args, " ")
	if !strings.Contains(joined, "sftp.args=-i /etc/gniza/id_ed25519") {
		t.Errorf("args = %q, missing the ssh identity", joined)
	}
	if !strings.Contains(joined, "backup --json") {
		t.Errorf("args = %q, subcommand should follow the options", joined)
	}
}

func TestRunnerRejectsConflictingBackendOptions(t *testing.T) {
	runner := New(Config{}, &fakeExec{})

	sftpRepo := func(identity string) Repository {
		return Repository{
			Dest: &destination.SFTP{
				Host: "h", User: "u", Root: "/b",
				IdentityFile: identity, KnownHostsFile: "/etc/known_hosts",
			},
			Path: "cp01", Password: "p",
		}
	}
	target := sftpRepo("/etc/key-a")
	source := sftpRepo("/etc/key-b")

	// restic applies -o globally, so one invocation cannot give two
	// repositories different ssh identities. Failing loudly beats
	// silently using the wrong key.
	err := runner.Init(context.Background(), target, &source)
	if err == nil || !strings.Contains(err.Error(), "conflicting backend option") {
		t.Fatalf("err = %v, want a conflict complaint", err)
	}
}

func TestExitThreeIsOnlyForgivenForBackup(t *testing.T) {
	// restic reuses exit code 3 for "backup could not read some files"
	// (a snapshot still exists) and for "forget failed to remove one or
	// more snapshots" (nothing was pruned). Forgiving it everywhere would
	// let a prune blocked by an append-only destination look like a
	// completed maintenance run.
	const forgetFailure = "Remove(<snapshot/78273572>) failed: unexpected HTTP response (403): 403 Forbidden\n" +
		"unable to remove snapshot/78273572 from the repository\n" +
		"failed to remove one or more snapshots\n" +
		"main.init\n" +
		"\t/home/user/go/pkg/mod/github.com/restic/restic@v0.19.1/cmd/restic/cmd_forget.go:67\n" +
		"runtime.doInit1\n" +
		"\t/usr/local/go/src/runtime/proc.go:8103\n"

	repo := Repository{Dest: &destination.Local{Root: "/srv/b"}, Path: "cp01", Password: "p"}

	forget := &fakeExec{result: CommandResult{ExitCode: 3, Stderr: []byte(forgetFailure)}}
	err := New(Config{}, forget).Forget(context.Background(), repo, ForgetSpec{KeepLast: 1, Prune: true})
	if err == nil {
		t.Fatal("forget exiting 3 should be an error: nothing was pruned")
	}
	// The message must carry the reason, not restic's stack trace.
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error = %v, want the 403 that explains it", err)
	}
	if strings.Contains(err.Error(), "runtime.doInit1") {
		t.Errorf("error = %v, stack frames should be dropped", err)
	}

	check := &fakeExec{result: CommandResult{ExitCode: 3, Stderr: []byte("some error")}}
	if err := New(Config{}, check).Check(context.Background(), repo, CheckSpec{}); err == nil {
		t.Error("check exiting 3 should be an error")
	}

	backup := &fakeExec{result: CommandResult{Stdout: []byte(sampleBackupJSON), ExitCode: 3}}
	if _, err := New(Config{}, backup).Backup(context.Background(), repo,
		BackupSpec{Paths: []string{"/s"}}); err != nil {
		t.Errorf("backup exiting 3 still produced a snapshot: %v", err)
	}
}

// While a backup runs, restic reports progress once a second. An operator
// watching a large account should see it move rather than wait for the
// summary, so those lines are parsed as they arrive.
func TestProgressIsReportedWhileTheBackupRuns(t *testing.T) {
	var seen []Progress
	read := progressReader(func(p Progress) { seen = append(seen, p) })
	if read == nil {
		t.Fatal("a caller that wants progress got no reader")
	}

	for _, line := range []string{
		`{"message_type":"status","percent_done":0.25,"total_files":10,"files_done":2,"total_bytes":2048,"bytes_done":512,"seconds_elapsed":3}`,
		`{"message_type":"verbose_status","percent_done":0.9}`,
		`not json at all`,
		`{"message_type":"summary","snapshot_id":"abc"}`,
		`{"message_type":"status","percent_done":1,"total_bytes":2048,"bytes_done":2048}`,
	} {
		read([]byte(line))
	}

	if len(seen) != 2 {
		t.Fatalf("read %d status lines, want 2: %+v", len(seen), seen)
	}
	if seen[0].PercentDone != 0.25 || seen[0].BytesDone != 512 || seen[0].FilesDone != 2 {
		t.Errorf("first status = %+v", seen[0])
	}
	if seen[1].PercentDone != 1 {
		t.Errorf("last status = %+v", seen[1])
	}
}

// Nobody listening means nothing is parsed.
func TestProgressReaderIsNilWhenNoOneIsWatching(t *testing.T) {
	if progressReader(nil) != nil {
		t.Error("a reader was built for a caller that wants no progress")
	}
}

// restic's output arrives in whatever chunks the pipe delivers, which is
// not the same as one line per write.
func TestLineWriterEmitsWholeLinesOnly(t *testing.T) {
	var lines []string
	w := &lineWriter{emit: func(line []byte) { lines = append(lines, string(line)) }}

	for _, chunk := range []string{"{\"a\":1}\n{\"b\"", ":2}\n{\"c\":3}", "\n"} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{`{"a":1}`, `{"b":2}`, `{"c":3}`}
	if len(lines) != len(want) {
		t.Fatalf("lines = %v, want %v", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, lines[i], want[i])
		}
	}
}

// TestACancelledRunDoesNotWaitOnAGrandchild is about the sftp backend.
// restic runs ssh as a child of its own, and that grandchild inherits the
// pipe this package reads restic's output through. Killing restic when the
// context is cancelled does not close the pipe -- the grandchild still
// holds the write end -- so a Wait that has no deadline never returns and
// the job that was cancelled hangs the agent instead of ending.
func TestACancelledRunDoesNotWaitOnAGrandchild(t *testing.T) {
	started := make(chan struct{})
	once := sync.Once{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := (&OSExec{WaitDelay: 200 * time.Millisecond}).Exec(ctx, Command{
			Path: "/bin/sh",
			// The backgrounded sleep keeps the write end of the pipe open
			// after the shell itself is killed.
			Args:   []string{"-c", "sleep 60 & echo up; sleep 60"},
			Env:    []string{"PATH=/bin:/usr/bin"},
			OnLine: func([]byte) { once.Do(func() { close(started) }) },
		})
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the child never reported that it had started")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Exec did not return after the context was cancelled")
	}
}
