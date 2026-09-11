package plain

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// copyPath copies a file or a tree, keeping modes and symlinks and
// following nothing.
func copyPath(source, target string, info os.FileInfo) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("plain: create %s: %w", filepath.Dir(target), err)
	}
	if !info.IsDir() {
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(source)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		return copyFile(source, target, info.Mode().Perm())
	}
	return filepath.Walk(source, func(path string, walked os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		into := filepath.Join(target, rel)
		switch {
		case walked.IsDir():
			return os.MkdirAll(into, 0o700)
		case walked.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_ = os.Remove(into)
			return os.Symlink(link, into)
		case !walked.Mode().IsRegular():
			return nil
		}
		return copyFile(path, into, walked.Mode().Perm())
	})
}

func copyFile(source, target string, mode os.FileMode) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("plain: read %s: %w", source, err)
	}
	if err := os.WriteFile(target, data, mode); err != nil {
		return fmt.Errorf("plain: write %s: %w", target, err)
	}
	return os.Chmod(target, mode)
}

// chownLike gives target the owner the restored file had, when this
// process may: the files of a site are the web server's or a deploy
// user's, and a restore that hands them all to root breaks the site.
func chownLike(target string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || os.Geteuid() != 0 {
		return nil
	}
	return os.Lchown(target, int(stat.Uid), int(stat.Gid))
}
