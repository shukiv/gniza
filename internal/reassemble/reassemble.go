package reassemble

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/shukiv/gniza/internal/panel"
	"github.com/shukiv/gniza/internal/pkgacct"
	"github.com/shukiv/gniza/internal/resticrun"
)

// Request describes a restore of one account.
type Request struct {
	Account    string
	SnapshotID string
	// Layout says where the panel this backup came from keeps the parts
	// of an account. Reassembly puts them back in that shape, so there is
	// no default: a restore built to the wrong panel's layout is an
	// archive its own restore will not read.
	Layout panel.ArchiveLayout
	// WorkDir is scratch space. Everything under it is the caller's to
	// remove when the restore is finished.
	WorkDir string
	// Repo is the repository holding the snapshot.
	Repo resticrun.Repository
	// OnStage, when set, is told what the reassembly is doing as it moves
	// from one part of the account to the next.
	//
	// A split snapshot is several restic runs, each counting from zero,
	// so a percentage on its own would appear to go backwards twice. The
	// stage is what makes it read as three parts rather than as a fault.
	OnStage func(stage string)
	// OnProgress, when set, is handed restic's own account of whichever
	// part is being read now. It runs on the goroutine reading restic's
	// output, so it must not block.
	OnProgress func(resticrun.RestoreProgress)
	// TreeOnly stops after the account tree, without repacking it.
	//
	// A rehearsal asks whether this backup can be turned back into an
	// account, and the tree answers that. The tar is a second full copy
	// on the same disk, and asking for it is why a 48.7 GiB account could
	// not be rehearsed on a server with 63 GiB free. cPanel's own restore
	// takes a directory as readily as an archive -- see restorepkg's
	// usage, which lists /path/to/extracted-cpuser-file -- so the archive
	// is for whoever asked to download one.
	TreeOnly bool
}

// stage says what is happening now, for a caller that wants to show it.
func (r Request) stage(name string) {
	if r.OnStage != nil {
		r.OnStage(name)
	}
}

// Result is what a completed reassembly produced.
type Result struct {
	// Account is whose backup this is, carried so that whatever checks
	// the result reads it against the same account it was rebuilt for.
	Account string
	// ArchivePath is the cpmove archive, ready for restorepkg.
	ArchivePath string
	// TreeDir is the extracted tree the archive was built from, kept so an
	// operator can inspect it.
	TreeDir string
	// RootDir is the cpmove directory inside that tree -- the account
	// itself, as cPanel lays one out.
	//
	// restorepkg takes it as readily as an archive: its usage lists
	// "/path/to/extracted-cpuser-file", and it copies whatever it is
	// given into a temporary directory of its own either way. Handing it
	// the directory saves building a tar that would be a second full
	// copy of the account on the same disk.
	RootDir string
	// Mode records which payload shape was restored.
	Mode pkgacct.Mode
	// BytesRestored is what restic reported across every part.
	BytesRestored uint64
	// Layout is the panel shape this was rebuilt into, so that whoever
	// checks the result reads it the same way it was written.
	Layout panel.ArchiveLayout
	// Skipped is what the backup was taken without, in the words the
	// snapshot's tags use ("databases", "homedir", "email"). Empty means
	// the snapshot holds the whole account.
	Skipped []string
}

// Complete says the snapshot this was rebuilt from holds the whole
// account.
func (r Result) Complete() bool {
	return len(r.Skipped) == 0
}

// Restorer performs the restic side of a restore. The concrete
// implementation is *resticrun.Runner; the interface keeps reassembly
// testable without a repository.
type Restorer interface {
	Snapshots(ctx context.Context, repo resticrun.Repository, filter resticrun.SnapshotFilter) ([]resticrun.Snapshot, error)
	Restore(ctx context.Context, repo resticrun.Repository, spec resticrun.RestoreSpec) (resticrun.RestoreResult, error)
}

// Run rebuilds an account archive from a snapshot.
//
// A monolithic snapshot holds the archive already and is simply restored. A
// split snapshot is put back together: the metadata archive is extracted,
// then the home directory and the database dumps are restored straight into
// their slots inside it, and the tree is repacked.
func Run(ctx context.Context, restorer Restorer, req Request) (Result, error) {
	if req.Account == "" {
		return Result{}, fmt.Errorf("reassemble: account is required")
	}
	if req.WorkDir == "" {
		return Result{}, fmt.Errorf("reassemble: work directory is required")
	}
	if req.Layout == nil {
		return Result{}, fmt.Errorf("reassemble: the panel's archive layout is required")
	}

	snapshot, err := findSnapshot(ctx, restorer, req)
	if err != nil {
		return Result{}, err
	}
	found, err := classifyPaths(snapshot.Paths)
	if err != nil {
		return Result{}, err
	}

	var result Result
	if found.mode() == pkgacct.ModeMonolithic {
		result, err = restoreMonolithic(ctx, restorer, req, snapshot, found)
	} else {
		result, err = restoreSplit(ctx, restorer, req, snapshot, found)
	}
	if err != nil {
		return Result{}, err
	}
	// What the schedule left out, taken from the snapshot's own tags. A
	// rehearsal has to check what this backup claims to hold: a snapshot
	// taken without databases has no dumps in it, and reading that as
	// "the account has none" is how a partial backup passes as a full one.
	result.Account = req.Account
	result.Skipped = snapshot.Skipped()
	return result, nil
}

// FindSnapshot resolves the snapshot a request names, and checks it
// belongs to the account asking for it.
func FindSnapshot(ctx context.Context, restorer Restorer, req Request) (resticrun.Snapshot, error) {
	return findSnapshot(ctx, restorer, req)
}

func findSnapshot(ctx context.Context, restorer Restorer, req Request) (resticrun.Snapshot, error) {
	snapshots, err := restorer.Snapshots(ctx, req.Repo, resticrun.SnapshotFilter{
		Tags: []string{"account:" + req.Account},
	})
	if err != nil {
		return resticrun.Snapshot{}, err
	}
	for _, snapshot := range snapshots {
		if snapshot.ID != req.SnapshotID && snapshot.ShortID != req.SnapshotID {
			continue
		}
		// The listing was filtered by tag, but the match is re-checked
		// here: restoring one customer's data into another's account
		// would be about the worst thing this program could do.
		if account := snapshot.Account(); account != req.Account {
			return resticrun.Snapshot{}, fmt.Errorf(
				"reassemble: snapshot %s belongs to account %q, not %q",
				req.SnapshotID, account, req.Account)
		}
		return snapshot, nil
	}
	return resticrun.Snapshot{}, fmt.Errorf(
		"reassemble: snapshot %s does not belong to account %s in this repository",
		req.SnapshotID, req.Account)
}

// Parts maps a snapshot's recorded paths back to the roles they played.
type Parts struct {
	Metadata  string
	Homedir   string
	Databases string
	Archive   string
	// System is the server's own configuration, backed up under its own
	// name rather than as an account.
	System string
}

func (p Parts) mode() pkgacct.Mode {
	if p.Archive != "" {
		return pkgacct.ModeMonolithic
	}
	return pkgacct.ModeSplit
}

// classifyPaths works out what each snapshot path was.
//
// The agent stages metadata and database dumps in named subdirectories and
// backs up the home directory in place, so the roles are recoverable from
// the paths themselves. Nothing about the staging root is assumed.
// Classify is how a caller recovers the roles of a snapshot's paths.
func Classify(paths []string) (Parts, error) { return classifyPaths(paths) }

func classifyPaths(paths []string) (Parts, error) {
	var found Parts
	for _, path := range paths {
		switch {
		case strings.HasSuffix(path, "/metadata"):
			found.Metadata = path
		case strings.HasSuffix(path, "/databases"):
			found.Databases = path
		case strings.HasSuffix(path, "/system"):
			found.System = path
		case strings.HasSuffix(path, ".tar"), strings.HasSuffix(path, ".tar.gz"), strings.HasSuffix(path, ".tar.zst"):
			if found.Archive != "" {
				return Parts{}, fmt.Errorf("reassemble: snapshot has multiple account archives")
			}
			found.Archive = path
		default:
			if found.Homedir != "" {
				return Parts{}, fmt.Errorf(
					"reassemble: snapshot has two candidate home directories, %s and %s",
					found.Homedir, path)
			}
			found.Homedir = path
		}
	}

	switch {
	case found.System != "":
		// A backup of the server itself: one directory of configuration,
		// and none of the parts an account has.
		if found.Metadata != "" || found.Homedir != "" || found.Archive != "" {
			return Parts{}, fmt.Errorf(
				"reassemble: snapshot mixes the server's own settings with an account's parts")
		}
		return found, nil
	case found.Archive != "" && (found.Metadata != "" || found.Homedir != "" || found.Databases != ""):
		return Parts{}, fmt.Errorf("reassemble: snapshot mixes a monolithic archive with split Parts")
	case found.Archive != "":
		return found, nil
	case found.Metadata == "":
		return Parts{}, fmt.Errorf("reassemble: snapshot has no metadata part")
	case found.Homedir == "":
		return Parts{}, fmt.Errorf("reassemble: snapshot has no home directory part")
	}
	return found, nil
}

func restoreMonolithic(ctx context.Context, restorer Restorer, req Request,
	snapshot resticrun.Snapshot, found Parts) (Result, error) {

	dir := filepath.Join(req.WorkDir, "archive")
	req.stage("reading the account archive")
	restored, err := restorer.Restore(ctx, req.Repo, resticrun.RestoreSpec{
		SnapshotID: snapshot.ID,
		Subpath:    filepath.Dir(found.Archive),
		Target:     dir,
		OnProgress: req.OnProgress,
	})
	if err != nil {
		return Result{}, fmt.Errorf("reassemble: restore archive: %w", err)
	}

	archive := filepath.Join(dir, filepath.Base(found.Archive))
	if _, err := os.Stat(archive); err != nil {
		return Result{}, fmt.Errorf("reassemble: restored archive is missing: %w", err)
	}
	if err := req.Layout.ValidateArchive(ctx, archive, req.Account); err != nil {
		return Result{}, err
	}
	return Result{
		ArchivePath:   archive,
		Layout:        req.Layout,
		Mode:          pkgacct.ModeMonolithic,
		BytesRestored: restored.BytesRestored,
	}, nil
}

func restoreSplit(ctx context.Context, restorer Restorer, req Request,
	snapshot resticrun.Snapshot, found Parts) (Result, error) {

	var bytesRestored uint64
	restore := func(stage, subpath, target string) error {
		req.stage(stage)
		restored, err := restorer.Restore(ctx, req.Repo, resticrun.RestoreSpec{
			SnapshotID: snapshot.ID,
			Subpath:    subpath,
			Target:     target,
			OnProgress: req.OnProgress,
		})
		if err != nil {
			return err
		}
		bytesRestored += restored.BytesRestored
		return nil
	}

	// 1. The metadata part holds the pkgacct archive with everything except
	//    the home directory and the databases.
	metadataDir := filepath.Join(req.WorkDir, "metadata")
	if err := restore("reading the account settings", found.Metadata, metadataDir); err != nil {
		return Result{}, fmt.Errorf("reassemble: restore metadata: %w", err)
	}

	archive, err := soleArchive(metadataDir)
	if err != nil {
		return Result{}, err
	}
	if err := req.Layout.ValidateArchive(ctx, archive, req.Account); err != nil {
		return Result{}, err
	}
	treeDir := filepath.Join(req.WorkDir, "tree")
	req.stage("unpacking the account settings")
	if err := extractTar(archive, treeDir); err != nil {
		return Result{}, err
	}

	// 2. The archive's own top-level directory is discovered rather than
	//    assumed, because its name is the panel's to choose.
	root, err := req.Layout.AccountRoot(treeDir, req.Account)
	if err != nil {
		return Result{}, fmt.Errorf("reassemble: the metadata archive is not an account tree: %w", err)
	}

	// 3. Each remaining part is restored straight into its slot.
	if err := restore("reading the home directory", found.Homedir,
		filepath.Join(root, req.Layout.HomedirDir())); err != nil {
		return Result{}, fmt.Errorf("reassemble: restore home directory: %w", err)
	}
	if found.Databases != "" {
		if err := restore("reading the databases", found.Databases,
			filepath.Join(root, req.Layout.DatabaseDir())); err != nil {
			return Result{}, fmt.Errorf("reassemble: restore databases: %w", err)
		}
		if err := req.Layout.PlaceDatabaseUsers(root); err != nil {
			return Result{}, err
		}
	}

	// 4. Repack, for a caller that asked for an archive. A rehearsal did
	//    not, and the tar would double what it needs on disk.
	var rebuilt string
	if !req.TreeOnly {
		rebuilt = filepath.Join(req.WorkDir, filepath.Base(root)+".tar")
		req.stage("building the account archive")
		if err := createTar(treeDir, rebuilt); err != nil {
			return Result{}, err
		}
	}
	return Result{
		ArchivePath:   rebuilt,
		TreeDir:       treeDir,
		RootDir:       root,
		Layout:        req.Layout,
		Mode:          pkgacct.ModeSplit,
		BytesRestored: bytesRestored,
	}, nil
}

// soleArchive finds the single archive a metadata restore produced.
func soleArchive(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("reassemble: read %s: %w", dir, err)
	}
	var archives []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			continue
		}
		if strings.HasSuffix(name, ".tar") || strings.HasSuffix(name, ".tar.gz") {
			archives = append(archives, filepath.Join(dir, name))
		}
	}
	switch len(archives) {
	case 1:
		return archives[0], nil
	case 0:
		return "", fmt.Errorf("reassemble: no archive in the restored metadata part")
	default:
		return "", fmt.Errorf("reassemble: %d archives in the restored metadata part, expected one",
			len(archives))
	}
}
