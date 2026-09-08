package reassemble

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/shukiv/gniza/internal/panel"
	"github.com/shukiv/gniza/internal/pkgacct"
)

// Databases names the account databases a rebuilt tree holds, read from
// the dumps beside the archive.
//
// It reports what the archive is about to put back, so a caller can check
// afterwards that cPanel actually did. Empty for a monolithic snapshot:
// its databases are inside pkgacct's own archive, where nothing here can
// see them without unpacking cPanel's format.
func (r Result) Databases() []string {
	if r.Mode == pkgacct.ModeMonolithic || r.TreeDir == "" || r.Layout == nil {
		return nil
	}
	root, err := r.Layout.AccountRoot(r.TreeDir, r.Account)
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(root, r.Layout.DatabaseDir()))
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if name, found := strings.CutSuffix(entry.Name(), ".sql"); found && name != "" {
			names = append(names, name)
		}
	}
	return names
}

// Verify applies the structural checks a rehearsed restore must pass,
// returning the ones that succeeded.
//
// The checks are structural on purpose. Nothing here can tell you cPanel
// would accept the archive — only a real restorepkg on a real host can —
// but a rehearsal that fails means the backup certainly cannot be
// restored, which is the question worth answering nightly.
func Verify(ctx context.Context, rebuilt Result) ([]string, error) {
	var passed []string

	// A backup taken of less than the whole account is checked against
	// what it claims to hold, and says so. Without this a schedule that
	// skips databases rehearses clean every night and the outcome is
	// indistinguishable from a full account that verified.
	skipped := make(map[string]bool, len(rebuilt.Skipped))
	for _, part := range rebuilt.Skipped {
		skipped[part] = true
	}
	if len(rebuilt.Skipped) > 0 {
		passed = append(passed, "taken without "+strings.Join(rebuilt.Skipped, ", ")+
			", so this is not a backup of the whole account")
	}

	// A rehearsal is run without repacking the tree, because the tar
	// answers nothing the tree does not and costs the same disk again.
	// There is then no archive to check, and claiming one would be a
	// check that never ran.
	if rebuilt.ArchivePath != "" {
		info, err := os.Stat(rebuilt.ArchivePath)
		if err != nil {
			return passed, fmt.Errorf("reassemble: rebuilt archive is missing: %w", err)
		}
		if info.Size() == 0 {
			return passed, fmt.Errorf("reassemble: rebuilt archive is empty")
		}
		passed = append(passed, "archive present")
	} else if rebuilt.TreeDir == "" {
		return passed, fmt.Errorf("reassemble: the restore produced neither an archive nor a tree")
	}

	if rebuilt.Mode == pkgacct.ModeMonolithic {
		// There is no tree to walk: the archive is the panel's own, and
		// its restore is what reads inside it. A panel that can read
		// inside its own archive says what that proved; one that cannot
		// -- cPanel's format, so far -- adds nothing, and the rehearsal
		// reports only what it did check.
		drill, ok := rebuilt.Layout.(panel.ArchiveDrill)
		if !ok || rebuilt.ArchivePath == "" || rebuilt.Account == "" {
			return passed, nil
		}
		inside, err := drill.DrillArchive(ctx, rebuilt.ArchivePath, rebuilt.Account)
		if err != nil {
			return passed, err
		}
		return append(passed, inside...), nil
	}

	if rebuilt.Layout == nil {
		return passed, fmt.Errorf("reassemble: this restore records no panel layout to check it against")
	}
	root, err := rebuilt.Layout.AccountRoot(rebuilt.TreeDir, rebuilt.Account)
	if err != nil {
		return passed, err
	}
	passed = append(passed, "account tree present")

	if !skipped["homedir"] {
		homedir := filepath.Join(root, rebuilt.Layout.HomedirDir())
		files, err := countFiles(homedir)
		if err != nil {
			return passed, fmt.Errorf("reassemble: home directory: %w", err)
		}
		if files == 0 {
			return passed, fmt.Errorf("reassemble: restored home directory is empty")
		}
		passed = append(passed, fmt.Sprintf("%d files in the home directory", files))
	}

	// Databases are optional: an account may genuinely have none. What
	// cannot be optional is a backup that was taken with databases and
	// came back without them -- that is the case this used to read as
	// "the account has none".
	dumps, err := os.ReadDir(filepath.Join(root, rebuilt.Layout.DatabaseDir()))
	if err != nil && !os.IsNotExist(err) {
		return passed, fmt.Errorf("reassemble: database directory: %w", err)
	}
	var checked int
	for _, dump := range dumps {
		if dump.IsDir() || !strings.HasSuffix(dump.Name(), ".sql") {
			continue
		}
		path := filepath.Join(root, rebuilt.Layout.DatabaseDir(), dump.Name())
		body, err := os.ReadFile(path)
		if err != nil {
			return passed, fmt.Errorf("reassemble: read dump %s: %w", dump.Name(), err)
		}
		// A truncated or empty dump restores an empty database, which is
		// worse than an obvious failure.
		if len(body) == 0 {
			return passed, fmt.Errorf("reassemble: dump %s is empty", dump.Name())
		}
		if !strings.Contains(strings.ToUpper(string(body)), "CREATE") {
			return passed, fmt.Errorf("reassemble: dump %s has no CREATE statement", dump.Name())
		}
		checked++
	}
	if checked > 0 {
		passed = append(passed, fmt.Sprintf("%d database dumps parse", checked))
	}
	if checked == 0 && skipped["databases"] {
		passed = append(passed, "no database dumps, which is what this backup was taken as")
	}
	return passed, nil
}

func countFiles(root string) (int, error) {
	var count int
	err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			count++
		}
		return nil
	})
	return count, err
}
