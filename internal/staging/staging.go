// Package staging manages the scratch space pkgacct writes into.
//
// pkgacct needs free disk roughly equal to the account's size, on the
// server we are trying not to disrupt. Filling that volume takes the
// customer's sites down, so space is checked before a job starts rather
// than discovered when a write fails. See docs/DESIGN.md §12.
package staging

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Manager allocates and reclaims staging directories under one root.
type Manager struct {
	// Root is the staging parent directory. Operators size this volume
	// deliberately; it should not be /tmp, and it should not share a
	// filesystem with the accounts being backed up.
	Root string
	// SafetyMarginRatio is extra free space required beyond the estimated
	// payload size, as a fraction. 0.2 means require 120% of the estimate.
	SafetyMarginRatio float64
	// MaxConcurrent caps simultaneously staged accounts so several large
	// accounts cannot collectively exhaust the volume.
	MaxConcurrent int

	// mu makes the space check and the directory that follows it one
	// operation, so two accounts cannot each be told there is room.
	mu sync.Mutex
	// reserved is what each allocated directory said it would need,
	// by path. Space committed to and not yet written is not free for
	// the next account: two accounts each checked against the whole
	// volume both passed and together needed more than it holds.
	reserved map[string]uint64
}

// Dir is an allocated staging directory.
type Dir struct {
	Path string
	// Retained marks finished output kept for collection rather than work
	// in progress.
	Retained bool
	// Key identifies what is staged here. It is stable across runs — the
	// account name, not the job id — so the paths restic records in a
	// snapshot are the same every night. Paths that changed per job would
	// give every run its own "restic forget" group, and a per-run group
	// of one is never pruned.
	Key string
}

// ErrInsufficientSpace is returned when the staging volume cannot safely
// hold the estimated payload.
type ErrInsufficientSpace struct {
	Required  uint64
	Available uint64
}

func (e *ErrInsufficientSpace) Error() string {
	// Bytes are what the check compares; an operator deciding what to do
	// about it is thinking in gigabytes and in what else is on the disk.
	return fmt.Sprintf(
		"not enough room to stage this account: it needs %s free and there is %s",
		human(e.Required), human(e.Available))
}

// human renders a size the way the interface does, so an error read in a
// log and a number read on a page agree with each other.
func human(bytes uint64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	value, exponent := float64(bytes), 0
	for value >= unit && exponent < 4 {
		value /= unit
		exponent++
	}
	return fmt.Sprintf("%.1f %s", value, [...]string{"B", "KiB", "MiB", "GiB", "TiB"}[exponent])
}

// Allocate reserves a staging directory after checking space.
//
// key must be stable for the thing being staged, normally the account
// name. estimatedBytes should come from the account's recorded size.
// Callers that have no estimate must not pass zero to bypass the check;
// they should refuse the job instead, because an unbounded pkgacct on a
// nearly full volume is exactly the failure this guards against.
func (m *Manager) Allocate(key string, estimatedBytes uint64) (*Dir, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	if m.Root == "" {
		return nil, fmt.Errorf("staging: root is not configured")
	}
	if estimatedBytes == 0 {
		return nil, fmt.Errorf("staging: refusing to stage %s without a size estimate", key)
	}

	required := estimatedBytes
	if m.SafetyMarginRatio > 0 {
		required = uint64(float64(estimatedBytes) * (1 + m.SafetyMarginRatio))
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	available, err := AvailableBytes(m.Root)
	if err != nil {
		return nil, err
	}
	// What another account has committed to and not yet written is not
	// there for this one, however much the filesystem still says is free.
	available -= min(available, m.outstanding())
	if available < required {
		return nil, &ErrInsufficientSpace{Required: required, Available: available}
	}

	if m.MaxConcurrent > 0 {
		// Only work actually in progress counts. A rebuilt archive waiting
		// to be downloaded is finished, and letting it hold a slot meant
		// one uncollected download blocked every other account.
		active, err := m.Active()
		if err != nil {
			return nil, err
		}
		if len(active) >= m.MaxConcurrent {
			return nil, fmt.Errorf("staging: %d accounts already being worked on, limit is %d",
				len(active), m.MaxConcurrent)
		}
	}

	path := filepath.Join(m.Root, dirPrefix+key)
	if err := os.Mkdir(path, 0o700); err != nil {
		// An existing directory means crash debris the startup sweep has
		// not cleaned up, since the controller leases only one running
		// job per account. Failing loudly beats staging into a
		// half-written tree.
		return nil, fmt.Errorf("staging: create %s: %w", path, err)
	}
	if m.reserved == nil {
		m.reserved = map[string]uint64{}
	}
	m.reserved[path] = required
	return &Dir{Path: path, Key: key}, nil
}

// outstanding is what allocated directories have committed to and have
// not written yet. Called with mu held.
//
// A reservation shrinks as its directory fills: the bytes already on
// disk are counted by the filesystem too, and counting them twice would
// refuse work the volume has room for. A directory that has gone takes
// its reservation with it.
func (m *Manager) outstanding() uint64 {
	var total uint64
	for path, required := range m.reserved {
		// Ask whether the directory is there before measuring it: a walk
		// that starts at a name which is not there tolerates the missing
		// name like any other and reports an empty tree, which is
		// indistinguishable from a reservation nothing has written to yet.
		if _, err := os.Stat(path); os.IsNotExist(err) {
			// The directory is gone, so nothing is coming to it.
			delete(m.reserved, path)
			continue
		}
		written, err := treeBytes(path)
		if err != nil {
			// It is still there but could not be measured, so how much
			// of the reservation is already on disk is unknown. Hold all
			// of it rather than hand the same bytes out twice.
			total += required
			continue
		}
		if required > written {
			total += required - written
		}
	}
	return total
}

// forget drops a directory's reservation: what it holds is on the disk
// now, or gone from it.
func (m *Manager) forget(path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.reserved, path)
}

// List returns every staging directory, in progress or retained.
func (m *Manager) List() ([]Dir, error) { return m.list(true) }

// Active returns only the directories being worked in.
//
// The agent clears these at startup: work interrupted by a crash leaves
// them behind, and cleaning up only on the success path is not enough,
// because the failure path is precisely when the volume is under pressure.
func (m *Manager) Active() ([]Dir, error) { return m.list(false) }

func (m *Manager) list(includeRetained bool) ([]Dir, error) {
	entries, err := os.ReadDir(m.Root)
	if err != nil {
		return nil, fmt.Errorf("staging: read root: %w", err)
	}
	var dirs []Dir
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		switch {
		case strings.HasPrefix(name, dirPrefix):
			dirs = append(dirs, Dir{
				Path: filepath.Join(m.Root, name),
				Key:  strings.TrimPrefix(name, dirPrefix),
			})
		case includeRetained && strings.HasPrefix(name, keepPrefix):
			dirs = append(dirs, Dir{
				Path:     filepath.Join(m.Root, name),
				Key:      strings.TrimPrefix(name, keepPrefix),
				Retained: true,
			})
		}
	}
	return dirs, nil
}

// Output is finished work kept for collection: a rebuilt archive, or what
// a granular restore recovered.
type Output struct {
	Dir
	// Bytes is what it occupies. Measured rather than recorded: the
	// operator is deciding whether to delete it, and a stale number is
	// worse than none.
	Bytes uint64
	// At is when the output was produced. Retaining renames the
	// directory, which leaves its modification time alone.
	At time.Time
}

// Retained lists finished output, newest first, with what each costs.
func (m *Manager) Retained() ([]Output, error) {
	dirs, err := m.list(true)
	if err != nil {
		return nil, err
	}
	var outputs []Output
	for _, dir := range dirs {
		if !dir.Retained {
			continue
		}
		info, err := os.Stat(dir.Path)
		if err != nil {
			return nil, fmt.Errorf("staging: stat %s: %w", dir.Path, err)
		}
		size, err := treeBytes(dir.Path)
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, Output{Dir: dir, Bytes: size, At: info.ModTime()})
	}
	sort.Slice(outputs, func(i, j int) bool { return outputs[i].At.After(outputs[j].At) })
	return outputs, nil
}

// treeBytes is what a directory occupies, following no symlinks.
// addSize counts regular files into total. A staging directory is written
// into while it is measured, so a name that has gone between the listing
// and the stat is ordinary, not a failure.
func addSize(total *uint64) filepath.WalkFunc {
	return func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.Mode().IsRegular() {
			*total += uint64(info.Size())
		}
		return nil
	}
}

func treeBytes(root string) (uint64, error) {
	var total uint64
	err := filepath.Walk(root, addSize(&total))
	if err != nil {
		return 0, fmt.Errorf("staging: measure %s: %w", root, err)
	}
	return total, nil
}

// Retain marks a finished directory as output to be collected, so it stops
// counting as work in progress and survives a restart.
//
// It returns the directory at its new location, with any previous output
// for the same key replaced: a newer rebuild supersedes an older one.
func (m *Manager) Retain(dir *Dir) (*Dir, error) {
	if dir == nil || dir.Retained {
		return dir, nil
	}
	if err := validateKey(dir.Key); err != nil {
		return nil, err
	}
	target := filepath.Join(m.Root, keepPrefix+dir.Key)
	if err := os.RemoveAll(target); err != nil {
		return nil, fmt.Errorf("staging: clear previous output: %w", err)
	}
	if err := os.Rename(dir.Path, target); err != nil {
		return nil, fmt.Errorf("staging: retain %s: %w", dir.Path, err)
	}
	// Finished output rather than work in progress: what it holds is
	// written, and the filesystem counts it from here on.
	m.forget(dir.Path)
	return &Dir{Path: target, Key: dir.Key, Retained: true}, nil
}

// Reclaim removes a leftover staging directory for a key, if one exists,
// and reports whether it did.
//
// Allocate deliberately refuses a directory that is already there, so
// anything that legitimately supersedes a previous run — a second restore
// of the same account, whose rebuilt archive replaces the last one — calls
// this first rather than failing.
func (m *Manager) Reclaim(key string) (bool, error) {
	if err := validateKey(key); err != nil {
		return false, err
	}
	var reclaimed bool
	for _, prefix := range []string{dirPrefix, keepPrefix} {
		path := filepath.Join(m.Root, prefix+key)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return false, fmt.Errorf("staging: stat %s: %w", path, err)
		}
		if err := m.Release(&Dir{Path: path, Key: key}); err != nil {
			return false, err
		}
		reclaimed = true
	}
	return reclaimed, nil
}

// SupersedeOutputs removes finished output whose key begins with prefix,
// and reports the keys it removed.
//
// Output is named after the restore that produced it, so that a stored
// restore's archive path stays that restore's own: a second rebuild of
// the same account writes somewhere else, and the first record's path
// stops resolving instead of quietly resolving to the newer archive. The
// download page can then say the archive is gone, which is true, rather
// than handing over a different backup under the old one's date.
//
// Keeping all of them would fill the disk the next night's backup needs,
// so the newer restore clears what it supersedes. Only finished output is
// touched: a directory still being worked in belongs to a restore that
// has not finished.
func (m *Manager) SupersedeOutputs(prefix string) ([]string, error) {
	if err := validateKey(prefix); err != nil {
		return nil, err
	}
	outputs, err := m.Retained()
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, output := range outputs {
		if !strings.HasPrefix(output.Key, prefix) {
			continue
		}
		dir := output.Dir
		if err := m.Release(&dir); err != nil {
			return removed, err
		}
		removed = append(removed, output.Key)
	}
	return removed, nil
}

// Release removes a staging directory. It is called only once every target
// of the job has reached a terminal state, so that a retry against a slow
// destination does not have to re-run pkgacct.
func (m *Manager) Release(dir *Dir) error {
	if dir == nil {
		return nil
	}
	if !strings.HasPrefix(filepath.Clean(dir.Path), filepath.Clean(m.Root)+string(os.PathSeparator)) {
		return fmt.Errorf("staging: refusing to remove %q outside root %q", dir.Path, m.Root)
	}
	if err := os.RemoveAll(dir.Path); err != nil {
		return fmt.Errorf("staging: remove %s: %w", dir.Path, err)
	}
	m.forget(dir.Path)
	return nil
}

// AvailableBytes reports free space on the filesystem holding path,
// counting only space available to unprivileged writes.
func AvailableBytes(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, fmt.Errorf("staging: statfs %s: %w", path, err)
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}

const (
	// dirPrefix marks a directory being worked in right now.
	dirPrefix = "stage-"
	// keepPrefix marks output that is finished and waiting to be
	// collected — a rebuilt archive somebody was told to download. It is
	// deliberately not counted against the concurrency limit, and it
	// survives a restart, because it is a result rather than debris.
	keepPrefix = "keep-"
)

// validateKey rejects identifiers that could escape the staging root once
// joined into a path.
func validateKey(key string) error {
	if key == "" {
		return fmt.Errorf("staging: key is empty")
	}
	for _, r := range key {
		// "@" is allowed for one reason: the server's own configuration is
		// staged under a name a cPanel account cannot have, and cPanel
		// usernames cannot contain it. It is as safe in a path as the
		// rest of these.
		isAllowed := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '@'
		if !isAllowed {
			return fmt.Errorf("staging: key %q contains an unsupported character", key)
		}
	}
	return nil
}
