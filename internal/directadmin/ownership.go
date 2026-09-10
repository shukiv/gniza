package directadmin

import (
	"fmt"
	"github.com/shukiv/gniza/internal/layout/dabackup"
	"io/fs"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
)

// foreignOwners counts the files in an account's home that belong to
// somebody else.
//
// DirectAdmin's restore extracts the nested home archive as the account,
// so tar cannot chown, and the first member it cannot chown ends the
// restore -- with everything after it left out. Six accounts on the
// validation host are in that state today. The backup still holds every
// byte and is still worth having; what it is not is a backup somebody
// can restore, and the night it is taken is when to say so.
type foreignOwners struct {
	// First is one of the paths that will stop the restore: an operator
	// given a name can go and look, where a count alone leaves them
	// searching a home directory by hand.
	First string
	Count int
}

// see records one file's owner. want is the account's own uid.
func (f *foreignOwners) see(name string, uid, want int) {
	if uid == want {
		return
	}
	if f.Count == 0 {
		f.First = name
	}
	f.Count++
}

// warning is what an operator is told, empty when there is nothing to
// tell them.
func (f foreignOwners) warning() string {
	switch f.Count {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf(
			"%s is owned by another account, so a restore of this backup stops there. "+
				"Everything is in the backup; DirectAdmin's restore unpacks the home as "+
				"the account and cannot give a file back to somebody else.", f.First)
	default:
		return fmt.Sprintf(
			"%d files are owned by another account, the first of them %s, so a restore "+
				"of this backup stops there. Everything is in the backup; DirectAdmin's "+
				"restore unpacks the home as the account and cannot give a file back to "+
				"somebody else.", f.Count, f.First)
	}
}

// homeOwners reads a home directory in place and reports what in it the
// account does not own.
//
// Nothing here fails a backup. A home is written into the whole time it
// is walked, and a check that costs an account its backup would be worse
// than the condition it looks for: what cannot be read is not counted.
func homeOwners(home string, want int) (foreignOwners, error) {
	var found foreignOwners
	err := filepath.WalkDir(home, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return nil
		}
		if path == home {
			return nil
		}
		name, err := filepath.Rel(home, path)
		if err != nil {
			name = path
		}
		found.see(name, int(stat.Uid), want)
		return nil
	})
	if err != nil {
		return found, err
	}
	return found, nil
}

// archiveOwners reads the manifest of an unpacked account archive and
// reports what in the account's own files it does not own. The files on
// disk are all root's -- Gniza wrote them -- so what the archive said is
// the only record of who they belong to.
func archiveOwners(dir string, want int) (foreignOwners, error) {
	var found foreignOwners
	err := dabackup.EachHomeMember(dir, func(name string, uid int) { found.see(name, uid, want) })
	return found, err
}

// ownershipWarning is what an operator is told about this account's
// files tonight, empty when there is nothing to tell them.
//
// A check that cannot be carried out says nothing rather than failing a
// backup: the backup is the thing that matters, and a warning that is not
// produced costs nobody a byte.
func (r *Real) ownershipWarning(account string, find func(want int) (foreignOwners, error)) string {
	uid, err := accountUID(r, account)
	if err != nil {
		r.debug("could not tell which user the account is", "account", account, "error", err)
		return ""
	}
	found, err := find(uid)
	if err != nil {
		r.debug("could not check who owns the account's files", "account", account, "error", err)
		return ""
	}
	return found.warning()
}

// accountUID answers who the account is, so what it does not own can be
// recognised.
func accountUID(r *Real, account string) (int, error) {
	lookup := r.lookupUser
	if lookup == nil {
		lookup = user.Lookup
	}
	found, err := lookup(account)
	if err != nil {
		return 0, fmt.Errorf("directadmin: resolve account identity: %w", err)
	}
	uid, err := strconv.Atoi(found.Uid)
	if err != nil {
		return 0, err
	}
	return uid, nil
}
