package resticrun

import (
	"context"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/destination"
)

func TestRestoreArgs(t *testing.T) {
	const snapshotID = "40dc15203b1cf9aa"

	subpath, err := RestoreArgs(RestoreSpec{
		SnapshotID: snapshotID,
		Subpath:    "/home/customer1",
		Target:     "/restore/work/homedir",
	})
	if err != nil {
		t.Fatalf("RestoreArgs: %v", err)
	}
	// The subpath form places the subtree at the target rather than
	// recreating its leading directories, which is what reassembly needs.
	assertArgs(t, subpath, []string{
		"restore", snapshotID + ":/home/customer1",
		"--target", "/restore/work/homedir", "--json",
		"--exclude-xattr", "security.selinux",
	})

	include, err := RestoreArgs(RestoreSpec{
		SnapshotID: snapshotID,
		Target:     "/root/recovered",
		Include:    []string{"/home/customer1/public_html/index.php"},
	})
	if err != nil {
		t.Fatalf("RestoreArgs: %v", err)
	}
	assertArgs(t, include, []string{
		"restore", snapshotID, "--target", "/root/recovered", "--json",
		"--exclude-xattr", "security.selinux",
		"--include", "/home/customer1/public_html/index.php",
	})
}

func TestRestoreArgsValidation(t *testing.T) {
	const snapshotID = "40dc15203b1cf9aa"

	cases := map[string]RestoreSpec{
		"no target":           {SnapshotID: snapshotID},
		"no snapshot":         {Target: "/restore"},
		"relative subpath":    {SnapshotID: snapshotID, Target: "/restore", Subpath: "home/c1"},
		"subpath and include": {SnapshotID: snapshotID, Target: "/restore", Subpath: "/home/c1", Include: []string{"/x"}},
	}
	for name, spec := range cases {
		if _, err := RestoreArgs(spec); err == nil {
			t.Errorf("%s should have been rejected", name)
		}
	}
}

func TestValidateSnapshotID(t *testing.T) {
	for _, id := range []string{"40dc1520", "latest", strings.Repeat("a", 64)} {
		if err := validateSnapshotID(id); err != nil {
			t.Errorf("validateSnapshotID(%q) = %v", id, err)
		}
	}
	// Identifiers are concatenated with a subpath before becoming an
	// argument, so anything unexpected is refused rather than passed on.
	for _, id := range []string{"", "short", "40dc1520; rm -rf /", "../../etc", strings.Repeat("a", 65)} {
		if err := validateSnapshotID(id); err == nil {
			t.Errorf("validateSnapshotID(%q) was accepted", id)
		}
	}
}

func TestParseRestoreSummary(t *testing.T) {
	const stream = `{"message_type":"status","percent_done":0.5}
{"message_type":"summary","total_files":9,"files_restored":9,"total_bytes":11,"bytes_restored":11}
`
	summary, err := ParseRestoreSummary([]byte(stream))
	if err != nil {
		t.Fatalf("ParseRestoreSummary: %v", err)
	}
	if summary.FilesRestored != 9 || summary.BytesRestored != 11 {
		t.Errorf("summary = %+v", summary)
	}

	if _, err := ParseRestoreSummary([]byte(`{"message_type":"status"}`)); err == nil {
		t.Error("a restore with no summary should be an error, not a silent success")
	}
}

func TestSnapshotTagHelpers(t *testing.T) {
	snapshot := Snapshot{Tags: []string{"account:customer1", "mode:split"}}
	if snapshot.Account() != "customer1" {
		t.Errorf("Account = %q", snapshot.Account())
	}
	if snapshot.PayloadMode() != "split" {
		t.Errorf("PayloadMode = %q", snapshot.PayloadMode())
	}

	untagged := Snapshot{}
	if untagged.Account() != "" || untagged.PayloadMode() != "" {
		t.Error("an untagged snapshot should report nothing rather than guess")
	}
}

func TestSnapshotsParsesListing(t *testing.T) {
	const listing = `[{"id":"40dc15203b1cf9aa","short_id":"40dc1520",
	  "time":"2026-08-30T02:00:00Z","hostname":"cp01","tags":["account:customer1"],
	  "paths":["/home/customer1"],"summary":{"total_bytes_processed":4096}}]`

	fake := &fakeExec{result: CommandResult{Stdout: []byte(listing)}}
	runner := New(Config{}, fake)
	repo := Repository{Dest: &destination.Local{Root: "/srv/b"}, Path: "cp01", Password: "p"}

	snapshots, err := runner.Snapshots(context.Background(), repo, SnapshotFilter{
		Tags: []string{"account:customer1"}, Latest: 5,
	})
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	if len(snapshots) != 1 || snapshots[0].ShortID != "40dc1520" {
		t.Fatalf("snapshots = %+v", snapshots)
	}
	if snapshots[0].Summary.TotalBytesProcessed != 4096 {
		t.Errorf("size = %d", snapshots[0].Summary.TotalBytesProcessed)
	}
	assertArgs(t, fake.got.Args, []string{
		"snapshots", "--json", "--tag", "account:customer1",
	})
}

func TestLsSkipsTheSnapshotHeader(t *testing.T) {
	// The stream opens with a snapshot header and continues with one node
	// per entry, so entries are selected by message type.
	const stream = `{"message_type":"snapshot","id":"40dc1520","paths":["/home/c1"]}
{"message_type":"node","name":"home","type":"dir","path":"/home"}
{"message_type":"node","name":"index.php","type":"file","path":"/home/c1/index.php","size":42}
`
	fake := &fakeExec{result: CommandResult{Stdout: []byte(stream)}}
	runner := New(Config{}, fake)
	repo := Repository{Dest: &destination.Local{Root: "/srv/b"}, Path: "cp01", Password: "p"}

	entries, err := runner.Ls(context.Background(), repo, "40dc15203b1cf9aa", "/home/c1")
	if err != nil {
		t.Fatalf("Ls: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if !entries[0].IsDir() {
		t.Error("the first entry should be a directory")
	}
	if entries[1].Size != 42 || entries[1].Path != "/home/c1/index.php" {
		t.Errorf("entry = %+v", entries[1])
	}
	assertArgs(t, fake.got.Args, []string{"ls", "--json", "40dc15203b1cf9aa", "/home/c1"})
}

func TestRestoreRejectsFailureExitCode(t *testing.T) {
	fake := &fakeExec{result: CommandResult{ExitCode: 1, Stderr: []byte("Fatal: repository does not exist")}}
	runner := New(Config{}, fake)
	repo := Repository{Dest: &destination.Local{Root: "/srv/b"}, Path: "cp01", Password: "p"}

	if _, err := runner.Restore(context.Background(), repo, RestoreSpec{
		SnapshotID: "40dc15203b1cf9aa", Target: "/restore",
	}); err == nil {
		t.Fatal("a failed restore should be an error")
	}
}

// A restore counts in different words from a backup: bytes_restored and
// files_restored where a backup says bytes_done and files_done. Reading
// one with the other's struct gives a bar that never moves.
func TestRestoreProgressIsReadFromResticsOwnWords(t *testing.T) {
	var seen []RestoreProgress
	read := restoreProgressReader(func(p RestoreProgress) {
		seen = append(seen, p)
	})

	for _, line := range []string{
		`{"message_type":"status","seconds_elapsed":3,"percent_done":0.574,` +
			`"total_files":120,"files_restored":69,"total_bytes":1000,"bytes_restored":574}`,
		`{"message_type":"summary","total_bytes":1000,"bytes_restored":1000}`,
		`not json at all`,
	} {
		read([]byte(line))
	}

	if len(seen) != 1 {
		t.Fatalf("read %d status lines, want 1: %+v", len(seen), seen)
	}
	if seen[0].PercentDone != 0.574 {
		t.Errorf("percent_done is %v, want 0.574", seen[0].PercentDone)
	}
	if seen[0].BytesRestored != 574 || seen[0].TotalBytes != 1000 {
		t.Errorf("bytes are %d of %d, want 574 of 1000",
			seen[0].BytesRestored, seen[0].TotalBytes)
	}
	if seen[0].FilesRestored != 69 {
		t.Errorf("files_restored is %d, want 69", seen[0].FilesRestored)
	}
}
