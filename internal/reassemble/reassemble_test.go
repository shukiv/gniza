package reassemble

import (
	"archive/tar"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/layout/cpmove"
	"github.com/shukiv/gniza/internal/pkgacct"
	"github.com/shukiv/gniza/internal/resticrun"
)

// fakeRestorer serves a snapshot from a directory tree on disk, so
// reassembly can be exercised without a repository.
type fakeRestorer struct {
	snapshot resticrun.Snapshot
	// source maps a snapshot path to the local directory holding it.
	source map[string]string
	calls  []resticrun.RestoreSpec
}

// Snapshots honours the tag filter the way restic does.
func (f *fakeRestorer) Snapshots(_ context.Context, _ resticrun.Repository,
	filter resticrun.SnapshotFilter) ([]resticrun.Snapshot, error) {
	for _, want := range filter.Tags {
		if !contains(f.snapshot.Tags, want) {
			return nil, nil
		}
	}
	return []resticrun.Snapshot{f.snapshot}, nil
}

func (f *fakeRestorer) Restore(_ context.Context, _ resticrun.Repository,
	spec resticrun.RestoreSpec) (resticrun.RestoreResult, error) {
	f.calls = append(f.calls, spec)

	from, known := f.source[spec.Subpath]
	if !known {
		return resticrun.RestoreResult{}, os.ErrNotExist
	}
	bytes, err := copyTree(from, spec.Target)
	if err != nil {
		return resticrun.RestoreResult{}, err
	}
	return resticrun.RestoreResult{BytesRestored: bytes, FilesRestored: 1}, nil
}

func copyTree(from, to string) (uint64, error) {
	var total uint64
	err := filepath.Walk(from, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, relative)
		if info.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		total += uint64(len(body))
		return os.WriteFile(target, body, 0o600)
	})
	return total, err
}

// buildSplitSnapshot lays out on disk what a split-mode backup captured.
func buildSplitSnapshot(t *testing.T) (*fakeRestorer, string) {
	t.Helper()
	root := t.TempDir()

	metadataDir := filepath.Join(root, "captured", "metadata")
	if err := os.MkdirAll(metadataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestTar(t, filepath.Join(metadataDir, "cpmove-customer1.tar"), map[string]string{
		"cpmove-customer1/version":   "6\n",
		"cpmove-customer1/meta/user": "customer1\n",
	})

	homeDir := filepath.Join(root, "captured", "home")
	if err := os.MkdirAll(filepath.Join(homeDir, "public_html"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homeDir, "public_html", "index.html"),
		[]byte("<h1>hello</h1>"), 0o600); err != nil {
		t.Fatal(err)
	}

	databaseDir := filepath.Join(root, "captured", "databases")
	if err := os.MkdirAll(databaseDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(databaseDir, "customer1_wp.sql"),
		[]byte("CREATE TABLE posts (id int);\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	restorer := &fakeRestorer{
		snapshot: resticrun.Snapshot{
			ID:      "40dc15203b1cf9aa",
			ShortID: "40dc1520",
			Tags:    []string{"account:customer1", "mode:split"},
			Paths: []string{
				"/var/lib/gniza/staging/stage-customer1/metadata",
				"/home/customer1",
				"/var/lib/gniza/staging/stage-customer1/databases",
			},
		},
		source: map[string]string{
			"/var/lib/gniza/staging/stage-customer1/metadata": metadataDir,
			"/home/customer1": homeDir,
			"/var/lib/gniza/staging/stage-customer1/databases": databaseDir,
		},
	}
	return restorer, root
}

func TestRunSplitRebuildsCpmoveTree(t *testing.T) {
	restorer, root := buildSplitSnapshot(t)
	workDir := filepath.Join(root, "work")

	result, err := Run(context.Background(), restorer, Request{
		Layout:  cpmove.Layout{},
		Account: "customer1", SnapshotID: "40dc15203b1cf9aa", WorkDir: workDir,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Mode != pkgacct.ModeSplit {
		t.Errorf("mode = %q, want split", result.Mode)
	}
	if result.BytesRestored == 0 {
		t.Error("no bytes reported restored")
	}

	// The Parts must land in the slots restorepkg expects, inside the
	// archive's own top-level directory.
	tree := filepath.Join(workDir, "tree", "cpmove-customer1")
	for path, want := range map[string]string{
		filepath.Join(tree, "version"):                                      "6\n",
		filepath.Join(tree, "meta", "user"):                                 "customer1\n",
		filepath.Join(tree, cpmove.HomedirDir, "public_html", "index.html"): "<h1>hello</h1>",
		filepath.Join(tree, cpmove.DatabaseDir, "customer1_wp.sql"):         "CREATE TABLE posts (id int);\n",
	} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read %s: %v", path, err)
			continue
		}
		if string(body) != want {
			t.Errorf("%s = %q, want %q", path, body, want)
		}
	}

	// And the repacked archive must contain the same tree.
	names := tarNames(t, result.ArchivePath)
	for _, want := range []string{
		"cpmove-customer1/version",
		"cpmove-customer1/homedir/public_html/index.html",
		"cpmove-customer1/mysql/customer1_wp.sql",
	} {
		if !contains(names, want) {
			t.Errorf("rebuilt archive is missing %s; has %v", want, names)
		}
	}
}

func TestRunSplitRestoresPartsByDiscoveredRole(t *testing.T) {
	restorer, root := buildSplitSnapshot(t)

	if _, err := Run(context.Background(), restorer, Request{
		Layout:  cpmove.Layout{},
		Account: "customer1", SnapshotID: "40dc1520",
		WorkDir: filepath.Join(root, "work"),
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Every part is fetched with a subpath restore, which places the
	// subtree directly rather than recreating its leading directories.
	if len(restorer.calls) != 3 {
		t.Fatalf("made %d restore calls, want 3", len(restorer.calls))
	}
	for _, call := range restorer.calls {
		if call.Subpath == "" {
			t.Errorf("restore %+v used no subpath", call)
		}
		if len(call.Include) != 0 {
			t.Errorf("restore %+v mixed include patterns with a subpath", call)
		}
	}
}

func TestRunRejectsSnapshotFromAnotherAccount(t *testing.T) {
	restorer, root := buildSplitSnapshot(t)
	_, err := Run(context.Background(), restorer, Request{
		Layout:  cpmove.Layout{},
		Account: "customer2", SnapshotID: "40dc15203b1cf9aa",
		WorkDir: filepath.Join(root, "work"),
	})
	if err == nil {
		t.Fatal("a snapshot belonging to another account was accepted")
	}
	if !strings.Contains(err.Error(), "customer2") {
		t.Errorf("err = %v, want it to name the account", err)
	}

	// The same must hold when the listing is not filtered for us: the
	// account tag on the snapshot itself is checked before anything is
	// written.
	restorer.snapshot.Tags = []string{"account:customer1", "mode:split"}
	unfiltered := &fakeRestorer{snapshot: restorer.snapshot, source: restorer.source}
	if _, err := Run(context.Background(), unfilteredAll{unfiltered}, Request{
		Layout:  cpmove.Layout{},
		Account: "customer2", SnapshotID: "40dc15203b1cf9aa",
		WorkDir: filepath.Join(root, "work2"),
	}); err == nil {
		t.Error("an unfiltered listing must still not cross accounts")
	}
}

func TestClassifyPaths(t *testing.T) {
	split, err := classifyPaths([]string{
		"/stage/stage-c1/metadata", "/home/c1", "/stage/stage-c1/databases",
	})
	if err != nil {
		t.Fatalf("classifyPaths: %v", err)
	}
	if split.mode() != pkgacct.ModeSplit || split.Homedir != "/home/c1" {
		t.Errorf("split = %+v", split)
	}

	monolithic, err := classifyPaths([]string{"/stage/stage-c1/cpmove-c1.tar"})
	if err != nil {
		t.Fatalf("classifyPaths: %v", err)
	}
	if monolithic.mode() != pkgacct.ModeMonolithic {
		t.Errorf("monolithic = %+v", monolithic)
	}

	for _, paths := range [][]string{
		{"/home/c1"},        // no metadata
		{"/stage/metadata"}, // no home directory
		{"/stage/metadata", "/home/c1", "/home/c2"},     // ambiguous home
		{"/stage/c1.tar", "/stage/metadata", "/home/x"}, // mixed shapes
	} {
		if _, err := classifyPaths(paths); err == nil {
			t.Errorf("paths %v should have been rejected", paths)
		}
	}
}

func TestSoleArchiveAndDirectory(t *testing.T) {
	dir := t.TempDir()
	if _, err := soleArchive(dir); err == nil {
		t.Error("an empty metadata part should be rejected")
	}
	writeTestTar(t, filepath.Join(dir, "one.tar"), map[string]string{"a": "b"})
	if got, err := soleArchive(dir); err != nil || filepath.Base(got) != "one.tar" {
		t.Errorf("soleArchive = %q, %v", got, err)
	}
	writeTestTar(t, filepath.Join(dir, "two.tar"), map[string]string{"a": "b"})
	if _, err := soleArchive(dir); err == nil {
		t.Error("two archives should be rejected rather than guessed between")
	}

	tree := t.TempDir()
	layout := cpmove.Layout{}
	if _, err := layout.AccountRoot(tree, "x"); err == nil {
		t.Error("an archive with no top-level directory is not an account tree")
	}
	if err := os.Mkdir(filepath.Join(tree, "cpmove-x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got, err := layout.AccountRoot(tree, "x"); err != nil || filepath.Base(got) != "cpmove-x" {
		t.Errorf("AccountRoot = %q, %v", got, err)
	}
}

// unfilteredAll stands in for a restic that ignored the tag filter.
type unfilteredAll struct{ inner *fakeRestorer }

func (u unfilteredAll) Snapshots(context.Context, resticrun.Repository,
	resticrun.SnapshotFilter) ([]resticrun.Snapshot, error) {
	return []resticrun.Snapshot{u.inner.snapshot}, nil
}

func (u unfilteredAll) Restore(ctx context.Context, repo resticrun.Repository,
	spec resticrun.RestoreSpec) (resticrun.RestoreResult, error) {
	return u.inner.Restore(ctx, repo, spec)
}

func writeTestTar(t *testing.T, path string, files map[string]string) {
	t.Helper()
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()

	writer := tar.NewWriter(out)
	for name, body := range files {
		if err := writer.WriteHeader(&tar.Header{
			Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}

func tarNames(t *testing.T, path string) []string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	var names []string
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, strings.TrimSuffix(header.Name, "/"))
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// A backup of the server itself has one part and none of an account's, so
// it is recognised rather than rejected for having no metadata.
func TestClassifyRecognisesASystemSnapshot(t *testing.T) {
	found, err := Classify([]string{"/var/lib/gniza/staging/stage-@system/system"})
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if found.System == "" || found.Homedir != "" || found.Metadata != "" {
		t.Errorf("parts = %+v", found)
	}

	if _, err := Classify([]string{
		"/stage/stage-@system/system", "/home/customer1",
	}); err == nil {
		t.Error("a snapshot mixing the server's settings with an account was accepted")
	}
}

// A split snapshot is several restic runs, each counting from zero. The
// stage is what makes a percentage that starts again read as the next part
// of the account rather than as a fault.
func TestReassemblyNamesEachStageItIsWorkingOn(t *testing.T) {
	restorer, root := buildSplitSnapshot(t)
	var stages []string

	_, err := Run(context.Background(), restorer, Request{
		Layout:  cpmove.Layout{},
		Account: "customer1", SnapshotID: "40dc15203b1cf9aa",
		WorkDir: filepath.Join(root, "work"),
		OnStage: func(stage string) { stages = append(stages, stage) },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, want := range []string{
		"reading the account settings", "unpacking the account settings",
		"reading the home directory", "building the account archive",
	} {
		found := false
		for _, said := range stages {
			if said == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no stage said %q; got %v", want, stages)
		}
	}
}
