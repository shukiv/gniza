package node

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/staging"
)

// restoreManager is the staging manager for a restore directory of its
// own, under the same margins as the staging directory.
func restoreManager(settings nodestore.Settings) *staging.Manager {
	return &staging.Manager{
		Root:              settings.RestoreRoot,
		SafetyMarginRatio: settings.SafetyMargin,
		MaxConcurrent:     settings.MaxConcurrent,
	}
}

// restores is where a restore or a rehearsal is rebuilt now.
func (e *Engine) restores() *staging.Manager {
	if e.restoreStaging != nil {
		if manager := e.restoreStaging.Load(); manager != nil {
			return manager
		}
	}
	return e.staging
}

// RestoreRoot is the directory restores are rebuilt in.
func (e *Engine) RestoreRoot() string { return e.restores().Root }

// workManagers is every directory this server works in: the staging
// directory, and the one for restores when that is another.
func (e *Engine) workManagers() []*staging.Manager {
	managers := []*staging.Manager{e.staging}
	if restores := e.restores(); restores != e.staging {
		managers = append(managers, restores)
	}
	return managers
}

// retainedOutput is finished output and the directory it is in, which is
// the one that may remove it.
type retainedOutput struct {
	staging.Output
	in *staging.Manager
}

// retained lists finished output wherever it was left.
func (e *Engine) retained() ([]retainedOutput, error) {
	var found []retainedOutput
	for _, manager := range e.workManagers() {
		outputs, err := manager.Retained()
		if err != nil {
			if manager != e.staging {
				// A volume that is not mounted holds nothing to list.
				e.log.Error("read the directory restores are rebuilt in", "error", err)
				continue
			}
			return nil, err
		}
		for _, output := range outputs {
			found = append(found, retainedOutput{Output: output, in: manager})
		}
	}
	return found, nil
}

// SetRestoreRoot moves where restores and rehearsals are rebuilt. An
// empty path moves them back to the staging directory.
//
// The staging directory itself is fixed, because its path is recorded in
// every backup. This one is in none of them. It is changed when nothing
// is running and nothing finished is waiting in the directory being
// left, since a download somebody was told to collect would otherwise be
// somewhere this server no longer looks.
func (e *Engine) SetRestoreRoot(path string) error {
	if path != "" {
		cleaned, err := e.validRestoreRoot(path)
		if err != nil {
			return err
		}
		path = cleaned
	}
	if path == e.settings.StagingRoot {
		path = ""
	}

	busy, err := e.anyJobRunning()
	if err != nil {
		return err
	}
	if busy {
		return fmt.Errorf("node: a backup or a restore is running; " +
			"change where restores are rebuilt when it has finished")
	}
	current := e.restores()
	if current != e.staging && current.Root != path {
		waiting, err := current.Retained()
		if err == nil && len(waiting) > 0 {
			return fmt.Errorf(
				"node: %d restored archives are waiting to be collected in %s; "+
					"collect or delete them first", len(waiting), current.Root)
		}
	}

	settings, err := e.store.Settings()
	if err != nil {
		return err
	}
	settings.RestoreRoot = path
	if err := e.store.SaveSettings(settings); err != nil {
		return err
	}
	e.settings.RestoreRoot = path
	next := e.staging
	if path != "" {
		next = restoreManager(e.settings)
	}
	e.restoreStaging.Store(next)
	e.log.Info("restores are rebuilt somewhere else", "path", next.Root)
	return nil
}

// validRestoreRoot checks a directory an operator named, makes it if it
// is not there, and returns it cleaned.
//
// Whole accounts are written here as root before they are handed to the
// panel, so it has to be a place no account can reach: every directory
// from the root of the filesystem down to it belongs to root, or to
// whoever this service runs as, and can be written by nobody else. That
// rules out an account's home.
func (e *Engine) validRestoreRoot(path string) (string, error) {
	if len(path) > 1024 {
		return "", fmt.Errorf("node: that path is too long")
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("node: give the whole path, starting with /")
	}
	cleaned := filepath.Clean(path)
	if cleaned == "/" {
		return "", fmt.Errorf("node: the top of the filesystem is not a directory to work in")
	}
	for _, r := range cleaned {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("node: that path has a character in it that is not printable")
		}
	}
	for _, taken := range []string{e.settings.ResticCache, e.settings.ConfigDir} {
		if taken != "" && (cleaned == taken || within(taken, cleaned)) {
			return "", fmt.Errorf("node: %s is where Gniza keeps something else", taken)
		}
	}
	if staging := e.settings.StagingRoot; staging != "" && cleaned != staging &&
		within(staging, cleaned) {
		return "", fmt.Errorf("node: %s is inside the staging directory, "+
			"which is on the same volume", cleaned)
	}

	// The deepest part of it that exists is resolved first, so that what
	// is checked is where the files would really go.
	existing := cleaned
	var missing []string
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("node: read %s: %w", existing, err)
		}
		missing = append([]string{filepath.Base(existing)}, missing...)
		existing = filepath.Dir(existing)
	}
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", fmt.Errorf("node: read %s: %w", existing, err)
	}
	for at := resolved; ; at = filepath.Dir(at) {
		if err := ownedAndClosed(at); err != nil {
			return "", err
		}
		if at == "/" {
			break
		}
	}
	target := filepath.Join(append([]string{resolved}, missing...)...)
	if len(missing) == 0 {
		if err := nothingElseIn(target); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		return "", fmt.Errorf("node: create %s: %w", target, err)
	}
	info, err := os.Stat(target)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("node: %s is not a directory", target)
	}
	probe, err := os.CreateTemp(target, ".gniza-write-*")
	if err != nil {
		return "", fmt.Errorf("node: %s cannot be written to: %w", target, err)
	}
	probe.Close()
	_ = os.Remove(probe.Name())
	return target, nil
}

// nothingElseIn refuses a directory that holds anything but this
// server's own work. One that is there already has to be one set aside
// for this: work directories made beside the accounts in /home, or
// beside whatever else a directory holds, are in somebody's way, and
// what is in the directory is in theirs.
func nothingElseIn(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("node: read %s: %w", dir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() && staging.Owns(entry.Name()) {
			continue
		}
		return fmt.Errorf("node: %s has %s in it; name a directory that is "+
			"used for nothing else, which Gniza makes if it is not there",
			dir, entry.Name())
	}
	return nil
}

// ownedAndClosed reports whether a directory belongs to root or to this
// service, and can be written by nobody else.
func ownedAndClosed(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("node: read %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("node: %s is not a directory", dir)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("node: who owns %s cannot be read", dir)
	}
	if stat.Uid != 0 && int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf(
			"node: %s belongs to an account, and a restore must be rebuilt where no "+
				"account can reach it", dir)
	}
	// A directory anybody may write to is closed enough when it is
	// sticky: nobody can rename or remove what somebody else put there,
	// and the directory under it is checked in its own right.
	if info.Mode().Perm()&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
		return fmt.Errorf(
			"node: %s can be written by others, and a restore must be rebuilt where "+
				"nobody else can write", dir)
	}
	return nil
}

// within reports whether path is inside dir.
func within(dir, path string) bool {
	relative, err := filepath.Rel(dir, path)
	return err == nil && relative != "." && relative != ".." &&
		!filepath.IsAbs(relative) && len(relative) > 0 &&
		(len(relative) < 3 || relative[:3] != ".."+string(filepath.Separator))
}
