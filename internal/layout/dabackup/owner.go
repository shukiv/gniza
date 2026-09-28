package dabackup

import (
	"path"
	"strings"
)

// accountOwner is who an account is in an archive's own words: the owner
// and the group its files carry.
type accountOwner struct {
	UID, GID     int
	Uname, Gname string
}

// ownerOf reads the account's identity off the archive's own headers.
//
// DirectAdmin writes an account's records as the account, so user.conf
// says who that is; an archive whose records are somebody else's says it
// on the first member of the home archive, which is the home directory
// itself. Nothing outside the archive is asked: a restore is often onto
// a server where the account does not exist yet, and where a number
// from /etc/passwd would be some other account's. An archive that names
// the account nowhere is left as it is.
func ownerOf(manifest *Manifest) (accountOwner, bool) {
	named := func(member Member) (accountOwner, bool) {
		if member.Uname != manifest.Account || member.Gname != manifest.Account {
			return accountOwner{}, false
		}
		return accountOwner{
			UID: member.UID, GID: member.GID, Uname: member.Uname, Gname: member.Gname,
		}, true
	}
	for _, member := range manifest.Outer.Members {
		if path.Clean(member.Name) == path.Join(BackupDir, UserConf) {
			if owner, ok := named(member); ok {
				return owner, true
			}
			break
		}
	}
	if manifest.Home != nil {
		for _, member := range manifest.Home.Members {
			if path.Clean(member.Name) == "." {
				return named(member)
			}
		}
	}
	return accountOwner{}, false
}

// giveToAccount makes the account the owner of every one of its own
// files that the archive says belongs to somebody else, and reports how
// many that was.
//
// DirectAdmin's restore unpacks an account's files as the account. tar
// cannot then give a file to anybody else, and the first member it
// cannot chown ends the restore with everything after it left out. On
// one server 66 of 144 accounts had such a file -- a directory JetBackup
// left, a site unpacked by root, a backup an administrator made in the
// home -- and none of those accounts could have been restored.
//
// The files are in the account's home, under a directory the account
// owns, so they are the account's to rename or remove already. What it
// gains here is the right to read one that was closed to it, and a
// restore that finishes. DirectAdmin's own records under backup/ are not
// touched: the restore puts those back itself.
func giveToAccount(manifest *Manifest) int {
	owner, known := ownerOf(manifest)
	if !known {
		return 0
	}
	given := 0
	give := func(member *Member) {
		if member.Nested {
			return
		}
		if member.UID == owner.UID && (member.Uname == "" || member.Uname == owner.Uname) {
			return
		}
		member.UID, member.GID = owner.UID, owner.GID
		member.Uname, member.Gname = owner.Uname, owner.Gname
		given++
	}
	if manifest.Home != nil {
		for i := range manifest.Home.Members {
			give(&manifest.Home.Members[i])
		}
	}
	for i := range manifest.Outer.Members {
		first, _, _ := strings.Cut(path.Clean(manifest.Outer.Members[i].Name), "/")
		if first == DomainsDir || first == MailDir {
			give(&manifest.Outer.Members[i])
		}
	}
	return given
}
