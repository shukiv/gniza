package directadmin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/shukiv/gniza/internal/bugreport"
	"github.com/shukiv/gniza/internal/layout/dabackup"
)

const defaultNativeRoot = "/var/lib/gniza-directadmin-native"

// A native workspace is separate from the root-private state/staging tree.
// DirectAdmin switches between admin and the selected account during backup.
// Only admin can list/write the job directory; only that account's group can
// traverse it. The parent contains no credentials and is never group writable.
type nativeWorkspace struct {
	path, destination  string
	adminUID, adminGID int
	lock               *os.File
}

// nativeRoot is where each account's job directory and lock live.
func (r *Real) nativeRoot() string {
	if r.NativeRoot != "" {
		return r.NativeRoot
	}
	return defaultNativeRoot
}

// SweepNativeWorkspaces removes job directories that no run owns any more,
// and reports how many it removed.
//
// A run removes its own directory when it finishes. A process that is
// killed does not, and what it leaves is not a stray password file but the
// account's archive: a live server was found holding 1.9 GiB from a run
// the service was restarted out from under, on a disk whose next night's
// backups were refused for want of room.
//
// Each directory is removed only while this holds that account's own lock,
// which is the same lock a run takes. A workspace being worked in is
// therefore never swept out from under it, and this is safe to call at any
// time rather than only at startup.
func (r *Real) SweepNativeWorkspaces() (int, error) {
	root := r.nativeRoot()
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("directadmin: read native workspace root: %w", err)
	}
	swept := 0
	var failures []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		cut := strings.LastIndex(entry.Name(), "-")
		if cut <= 0 {
			continue
		}
		account := entry.Name()[:cut]
		if usableAccountName(account) != nil {
			continue
		}
		removed, err := r.sweepOne(root, account, filepath.Join(root, entry.Name()))
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if removed {
			swept++
		}
	}
	return swept, errors.Join(failures...)
}

// sweepOne removes one job directory while holding its account's lock, and
// says whether it did. A lock it cannot take belongs to a run in progress.
func (r *Real) sweepOne(root, account, dir string) (bool, error) {
	lock, err := os.OpenFile(filepath.Join(root, account+".lock"),
		os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return false, nil
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if err := os.RemoveAll(dir); err != nil {
		return false, err
	}
	return true, nil
}

func (r *Real) nativeWorkspace(account string) (_ *nativeWorkspace, err error) {
	if err := usableAccountName(account); err != nil {
		return nil, err
	}
	lookup := r.lookupUser
	if lookup == nil {
		lookup = user.Lookup
	}
	admin, err := lookup("admin")
	if err != nil {
		return nil, fmt.Errorf("directadmin: resolve native backup owner: %w", err)
	}
	adminUID, err := strconv.Atoi(admin.Uid)
	if err != nil {
		return nil, err
	}
	adminGID, err := strconv.Atoi(admin.Gid)
	if err != nil {
		return nil, err
	}
	// The workspace is traversable by the account's group so that what
	// DirectAdmin writes into it as the account can be read. An account
	// that is not on the server yet -- a restore is about to create it --
	// has no group, and the workspace is the administrator's alone.
	// DirectAdmin reads the archive as the account -- for an account it
	// is creating, as the user it has just made, whose group did not
	// exist when this directory was -- so an account not on the server
	// gets a directory anyone may pass through and nobody else may list.
	// The archive inside stays the administrator's, mode 0600, and the
	// directory's name is random under a root nobody else can list.
	// Measured on 1.709, 2026-09-11: "File does not exist or you don't
	// have access to file ... File being read as 'gzdrill0911'".
	accountGID, pathMode := adminGID, os.FileMode(0o710)
	var unknown user.UnknownUserError
	if selected, err := lookup(account); err == nil {
		if accountGID, err = strconv.Atoi(selected.Gid); err != nil {
			return nil, err
		}
	} else if errors.As(err, &unknown) {
		pathMode = 0o711
	} else {
		return nil, fmt.Errorf("directadmin: resolve account identity: %w", err)
	}
	root := r.nativeRoot()
	if !filepath.IsAbs(root) || filepath.Clean(root) == "/" {
		return nil, fmt.Errorf("directadmin: native workspace root must be a dedicated absolute directory")
	}
	if err := os.Mkdir(root, 0o711); err == nil {
		// The service has umask 0077. Open traversal only on this newly
		// created, dedicated native root, never on an existing directory.
		if err := os.Chmod(root, 0o711); err != nil {
			return nil, err
		}
	} else if !os.IsExist(err) {
		return nil, err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm() != 0o711 {
		return nil, fmt.Errorf("directadmin: native workspace root must be service-owned mode 0711, not a symlink")
	}
	lock, err := os.OpenFile(filepath.Join(root, account+".lock"),
		os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			lock.Close()
		}
	}()
	if _, err := checkedFile(lock, uint32(os.Geteuid())); err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, fmt.Errorf("directadmin: another native operation holds account %s: %w", account, err)
	}
	dir, err := os.MkdirTemp(root, account+"-")
	if err != nil {
		return nil, err
	}
	w := &nativeWorkspace{path: dir, destination: filepath.Join(dir, "output"),
		adminUID: adminUID, adminGID: adminGID, lock: lock}
	defer func() {
		if err != nil {
			w.close()
		}
	}()
	if err := os.Mkdir(w.destination, 0o700); err != nil {
		return nil, err
	}
	for _, entry := range []struct {
		path     string
		uid, gid int
		mode     os.FileMode
	}{
		{w.destination, adminUID, adminGID, 0o711},
		{w.path, adminUID, accountGID, pathMode},
	} {
		if err := os.Chown(entry.path, entry.uid, entry.gid); err != nil {
			return nil, err
		}
		if err := os.Chmod(entry.path, entry.mode); err != nil {
			return nil, err
		}
	}
	return w, nil
}

func (w *nativeWorkspace) seal() error {
	if err := os.Chown(w.path, os.Geteuid(), os.Getegid()); err != nil {
		return err
	}
	return os.Chmod(w.path, 0o700)
}

func (w *nativeWorkspace) close() error {
	defer w.lock.Close()
	if err := w.seal(); err != nil {
		return err
	}
	// Only the fresh, locked job directory is temporary. Never remove the
	// native root, another job, the input archive, or panel backup data.
	return os.RemoveAll(w.path)
}

func checkedFile(f *os.File, owner uint32) (os.FileInfo, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || stat.Uid != owner || stat.Nlink != 1 || info.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("directadmin: expected a singly linked regular file owned by uid %d, without group/other write access", owner)
	}
	return info, nil
}

// copyArchive never follows links or overwrites a prior result. The caller
// validates this private copy, not bytes that the native workspace can change.
func copyArchive(ctx context.Context, source, destination string, owner uint32) error {
	in, err := os.OpenFile(source, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	before, err := checkedFile(in, owner)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, &contextReader{ctx, in})
	closeErr := out.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		os.Remove(destination)
		return err
	}
	after, err := in.Stat()
	if err != nil || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		os.Remove(destination)
		return fmt.Errorf("directadmin: archive changed during handoff")
	}
	return nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// DirectAdmin 1.709 can exit zero on a failed task. Require a matching
// start/completion pair AND no error records, inspecting the whole transcript.
// Unrecognised/truncated output fails closed, not as an assumed success.
var nativeRecord = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}\s+(info|error|warn)\s+(.*)$`)

type nativeOutput struct {
	bytes.Buffer
	overflow bool
}

func (b *nativeOutput) Write(p []byte) (int, error) {
	const limit = 1 << 20
	n := len(p)
	if len(p) > limit-b.Len() {
		p = p[:limit-b.Len()]
		b.overflow = true
	}
	b.Buffer.Write(p)
	return n, nil
}

func (r *Real) runNative(ctx context.Context, action, selection, destination string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, r.binary(), args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	// Cancel the whole group, not just DirectAdmin with mysqldump/tar still
	// using the directory after its account lock has been released.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	var output nativeOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	transcript := bugreport.Redact(output.String())
	// A native command must not leave a background writer behind after its
	// parent exits (including an output-pipe WaitDelay failure). Do not turn
	// a detached operation into a reported completion and release its lock.
	if cmd.Process != nil && syscall.Kill(-cmd.Process.Pid, 0) == nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == nil {
			err = fmt.Errorf("native task left running child processes")
		}
	}
	if ctx.Err() != nil {
		return transcript, ctx.Err()
	}
	if err != nil {
		return transcript, fmt.Errorf("directadmin: native %s process: %w", action, err)
	}
	if output.overflow {
		return transcript, fmt.Errorf("directadmin: native %s transcript exceeded 1 MiB; completion is unverified", action)
	}
	started, finished := 0, 0
	for _, line := range strings.Split(output.String(), "\n") {
		record := nativeRecord.FindStringSubmatch(line)
		if record == nil {
			continue
		}
		if record[1] == "error" {
			return transcript, fmt.Errorf("directadmin: native %s reported an error; inspect its transcript", action)
		}
		message := record[2]
		isStart, isEnd := strings.HasPrefix(message, "executing task "), strings.HasPrefix(message, "finished task ")
		if !isStart && !isEnd {
			continue
		}
		_, encoded, found := strings.Cut(message, "task=")
		if !found {
			return transcript, fmt.Errorf("directadmin: native task identity is missing")
		}
		task, err := url.ParseQuery(encoded)
		if err != nil {
			return transcript, fmt.Errorf("directadmin: invalid native task record")
		}
		for key, values := range task {
			if len(values) != 1 || (strings.HasPrefix(key, "select") && key != "select0") {
				return transcript, fmt.Errorf("directadmin: native task selected unexpected accounts")
			}
		}
		for key, expected := range map[string]string{"action": action, "select0": selection, "local_path": destination, "owner": "admin", "type": "admin", "value": "multiple", "where": "local", "when": "now"} {
			if task.Get(key) != expected {
				return transcript, fmt.Errorf("directadmin: native task %s did not match the requested operation", key)
			}
		}
		// A backup that asked for less than the whole account has to have
		// asked for exactly the set Gniza can put back. A task line that
		// arrived with one option missing is a backup missing that part
		// of the account, and it is refused rather than stored.
		if task.Has("what") && !selectedTheLeanSet(task) {
			return transcript, fmt.Errorf(
				"directadmin: native task asked for a set of data this cannot put back together")
		}
		if isStart {
			started++
		} else {
			if started != 1 {
				return transcript, fmt.Errorf("directadmin: native task completed without a unique start")
			}
			finished++
		}
	}
	if started != 1 || finished != 1 {
		return transcript, fmt.Errorf("directadmin: native %s did not confirm exactly one completed task", action)
	}
	return transcript, nil
}

// leanOptions is every value DirectAdmin's backup page offers under
// "What" except "domain", which is the one that carries the account's own
// files: domains/ and the nested home archive both go with it. "email"
// stays because leaving it out takes the mailboxes' own passwords and
// quotas as well, and those are nowhere in the home directory. See ADR
// 0021 and testdata/lean-account.tar.list.
var leanOptions = []string{
	"subdomain", "email", "emailsettings", "forwarder", "autoresponder",
	"vacation", "list", "ftp", "ftpsettings", "database", "database_data",
	"trash",
}

// backupTask is the line DirectAdmin's own backup page posts, which
// "directadmin taskq --run=" takes. lean asks for the chosen set rather
// than all of it.
func backupTask(account, destination string, lean bool) url.Values {
	task := url.Values{
		"action": {"backup"}, "local_path": {destination}, "owner": {"admin"},
		"select0": {account}, "type": {"admin"}, "value": {"multiple"},
		"when": {"now"}, "where": {"local"},
	}
	if !lean {
		return task
	}
	task.Set("what", "select")
	// What DirectAdmin's own backup page sends, and it is not decoration.
	// A client that says it knows what email_data is and leaves it out
	// gets no messages; one that does not say so is taken for a client
	// written before the option existed and gets them anyway. Without
	// these two lines every "lean" archive carried the whole mailbox --
	// 732,924,695 of 1,281,932,189 bytes on the validation host -- and
	// with them the same request came back at 15,124,712 bytes instead
	// of 310,915,651. Measured on 1.709, 2026-09-10.
	task.Set("database_data_aware", "yes")
	task.Set("email_data_aware", "yes")
	for i, option := range leanOptions {
		task.Set("option"+strconv.Itoa(i), option)
	}
	return task
}

// stageNative runs DirectAdmin's own backup and returns the archive it
// wrote, copied into Gniza's private staging.
//
// reserveBytes is what the caller expects the archive to cost. A whole
// account archive costs the account; one asked for without the account's
// files costs its mail, which is the only bulk left in it.
func (r *Real) stageNative(ctx context.Context, account, staging string, reserveBytes uint64, lean bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !filepath.IsAbs(staging) {
		return "", fmt.Errorf("directadmin: private staging must be an absolute directory")
	}
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return "", err
	}
	info, err := os.Lstat(staging)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return "", fmt.Errorf("directadmin: handoff staging must be service-owned and private")
	}
	w, err := r.nativeWorkspace(account)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := w.close(); err != nil {
			r.debug("native workspace cleanup failed", "path", w.path, "error", err)
		}
	}()
	// Native working data, its completed archive and the private handoff can
	// coexist. Check BOTH filesystems; the ordinary staging preflight only
	// knows about the latter. This is an estimate, not a disk reservation.
	// A whole-account run holds three: the account's own files copied
	// into the working directory, the nested home archive compressed
	// beside them, and the outer archive around both. A lean run holds
	// one: DirectAdmin assembles the records and the dumps in its own
	// backup_tmpdir, and what lands here is the compressed archive --
	// 15,124,712 bytes for 549,591,471 of dumps on the validation host.
	// Reserving two copies of the dumps for that refused a real account
	// with 10 GiB of databases on a disk with 16.5 GiB free.
	copies := uint64(3)
	if lean {
		copies = 1
	}
	if err := nativeSpace(w.destination, reserveBytes, copies); err != nil {
		return "", err
	}
	if err := nativeSpace(staging, reserveBytes, 1); err != nil {
		return "", err
	}
	// The whole-account backup goes through the command DirectAdmin
	// documents for it. A selective one has no command -- admin-backup
	// takes only --destination and --user -- so it goes through the task
	// line the backup page posts, which is how a native restore already
	// runs.
	args := []string{"admin-backup", "--destination=" + w.destination, "--user=" + account}
	if lean {
		args = []string{"taskq", "--run=" + backupTask(account, w.destination, true).Encode()}
	}
	transcript, err := r.runNative(ctx, "backup", account, w.destination, args...)
	if err != nil {
		// Unlike Apply, Stage has no transcript return field. Keep a bounded,
		// redacted diagnostic in its error so the job record remains useful.
		return "", fmt.Errorf("%w\n%s", err, bugreport.Clip(transcript, 16<<10))
	}
	if err := w.seal(); err != nil {
		return "", err
	}
	source, err := soleArchive(w.destination)
	if err != nil {
		return "", err
	}
	archive := filepath.Join(staging, filepath.Base(source))
	if err := copyArchive(ctx, source, archive, uint32(w.adminUID)); err != nil {
		return "", err
	}
	if err := (dabackup.Layout{}).ValidateArchive(ctx, archive, account); err != nil {
		os.Remove(archive)
		return "", err
	}
	return archive, nil
}

// leanStagingFloor is what a lean archive costs before its dumps: the
// account's records and the webmail data. 153,600 bytes on the fixture ADR 0021
// measured, and a margin for a server whose records are larger.
const leanStagingFloor = 64 << 20

func nativeSpace(dir string, size, copies uint64) error {
	const reserve = 1 << 30
	if size > (^uint64(0)-reserve)/copies {
		return fmt.Errorf("directadmin: staging estimate overflow")
	}
	required := size*copies + reserve
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return err
	}
	available := stat.Bavail * uint64(stat.Bsize)
	if required > available {
		return fmt.Errorf("directadmin: native staging needs %d bytes including reserve; %s has %d available", required, dir, available)
	}
	return nil
}

// selectedTheLeanSet says whether this task asked for exactly what a
// backup that reads the home directory in place asks for.
func selectedTheLeanSet(task url.Values) bool {
	if task.Get("what") != "select" {
		return false
	}
	wanted := map[string]bool{}
	for _, option := range leanOptions {
		wanted[option] = true
	}
	for key, values := range task {
		if !strings.HasPrefix(key, "option") || len(values) != 1 {
			continue
		}
		if !wanted[values[0]] {
			return false
		}
		delete(wanted, values[0])
	}
	return len(wanted) == 0
}
