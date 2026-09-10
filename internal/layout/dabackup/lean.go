package dabackup

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Reading the account where it lies. See ADR 0021.
//
// DirectAdmin's backup task takes a chosen set of data rather than all of
// it, and leaving "domain" out of that set leaves out both domains/ and
// the nested home archive -- every one of the account's own files. What
// comes back is DirectAdmin's records of the account and, because asking
// for the mailboxes' passwords means asking for "email", the messages as
// well.
//
// So the archive is unpacked into the metadata part alone, the messages
// are dropped because they are under the home path restic reads in place,
// and the home part is not a tree Gniza wrote at all: it is
// /home/<account>, handed to restic as a path.
//
// Putting the archive back therefore has to build the members that were
// never in it. That is what hydrate does, out of the tree restic
// restored -- which is a copy of the home directory, with the owners the
// account's own files have. The objection that stopped a repack from
// walking a tree (see split.go: gzv0908a/apache on .php/, gzv0908a/mail
// on Maildir/) does not apply to walking a faithful copy of the
// directory those owners are on.

// optionsList is the member DirectAdmin's own restore reads to decide
// what it is being handed. A rebuilt archive is a whole account, so it
// says so, whatever the backup that produced it asked for.
const optionsList = BackupDir + "/backup_options.list"

// wholeAccountOptions is that file's contents for an archive holding
// everything, taken from a real whole-account archive on 1.709.
var wholeAccountOptions = []string{
	"autoresponder", "database", "database_data", "database_data_aware",
	"dns", "domain", "email", "email_data", "email_data_aware",
	"emailsettings", "forwarder", "ftp", "ftpsettings", "list",
	"subdomain", "trash", "trash_aware", "vacation",
}

// UnpackLeanArchive takes apart the archive DirectAdmin writes when the
// backup task asked for everything except the account's own files.
//
// Only the metadata part is written. There is no home tree: the home
// directory is read where it lies, so writing one here would be a second
// copy of the thing this exists to stop copying.
func (Layout) UnpackLeanArchive(ctx context.Context, archivePath, account, dir string) error {
	if !nameMatchesArchive(filepath.Base(archivePath), account) {
		return fmt.Errorf("dabackup: archive filename %q does not belong to %s",
			filepath.Base(archivePath), account)
	}
	f, err := os.OpenFile(archivePath, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("dabackup: account archive is not a regular file")
	}
	part := MetadataPart(dir)
	if _, err := os.Lstat(part); err == nil {
		return fmt.Errorf("dabackup: %s already holds a %s tree", dir, filepath.Base(part))
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("dabackup: %s: %w", part, err)
	}
	if err := os.MkdirAll(part, 0o700); err != nil {
		return fmt.Errorf("dabackup: create %s: %w", part, err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("dabackup: open %s: %w", dir, err)
	}
	defer func() { _ = root.Close() }()

	compression := compressionOf(archivePath)
	out := &unpacker{root: root, bodies: map[string]bool{}, lean: true}
	manifest := Manifest{
		Version:     leanManifestVersion,
		Account:     account,
		ArchiveName: filepath.Base(archivePath),
		Outer:       ArchivePart{Compression: compression},
		Lean:        true,
		// The nested archive is written back under the name and the
		// compression DirectAdmin would have used, which is the one it
		// used for the archive around it: both come from backup_gzip.
		HomeArchiveName: BackupDir + "/home.tar" + extensionOf(compression),
		HomeCompression: compression,
	}
	reader, closer, err := decompressed(contextReader{ctx, f}, archivePath)
	if err != nil {
		return err
	}
	defer closer()
	if err := out.readTar(ctx, tar.NewReader(reader), &manifest, false); err != nil {
		return err
	}
	body, err := json.MarshalIndent(manifest, "", "\t")
	if err != nil {
		return fmt.Errorf("dabackup: write the archive manifest: %w", err)
	}
	return os.WriteFile(filepath.Join(part, ManifestFile), append(body, '\n'), 0o600)
}

// leanMember decides what readTar does with one member of an archive
// that was meant to hold no account files. It reports whether the member
// is to be skipped.
//
// An archive that still carries the account's files is one DirectAdmin
// produced without honouring the selection -- an older panel, or one
// that read the options and ignored them. Unpacking it as though it were
// a metadata archive would leave the home part to be read in place as
// well, which is right, but the run that produced it read and wrote the
// whole account, which is what the selection existed to prevent, and the
// staging space was reserved for a small archive. It is refused, by
// name, so the server is recorded as one this mode does not work on.
func leanMember(clean string, typeflag byte) (skip bool, err error) {
	if isNestedHomeArchive(clean, typeflag) {
		return false, fmt.Errorf(
			"dabackup: the archive carries %s, so DirectAdmin ignored the backup selection", clean)
	}
	switch first, _, _ := strings.Cut(clean, "/"); first {
	case DomainsDir:
		return false, fmt.Errorf(
			"dabackup: the archive carries %s, so DirectAdmin ignored the backup selection", DomainsDir)
	case MailDir:
		return true, nil
	}
	return false, nil
}

// hydrate fills in the members that were never in the archive.
//
// The manifest describes what DirectAdmin wrote. Everything the account
// owns was read in place instead, so it arrives as a tree, and the
// members for it are made here: domains/ and imap/ into the outer
// archive where DirectAdmin put them, and the rest of the home directory
// into the nested one.
func hydrate(dir string, manifest *Manifest) error {
	home := HomedirPart(dir)
	if _, err := os.Lstat(home); err != nil {
		return fmt.Errorf(
			"dabackup: this account's files were read where they lie, and %s is not here: %w",
			HomeTreeDir, err)
	}
	names := newNameCache()
	nestedLinks, outerLinks := newLinkIndex(), newLinkIndex()
	var nested, outer []Member
	err := filepath.WalkDir(home, func(full string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(home, full)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		member, ok, err := memberOf(full, rel, entry, names)
		if err != nil {
			return err
		}
		if !ok {
			// A socket, which tar does not carry and restic does not
			// store. Nothing under it either, since a socket has no
			// contents.
			return nil
		}
		member.Body = path.Join(HomeTreeDir, rel)
		if entry.Type() != 0 {
			// Only a regular file has a body to copy back; everything
			// else is the header alone, as it is for the outer archive.
			member.Body = ""
		}
		first, _, _ := strings.Cut(rel, "/")
		own := first == DomainsDir || first == MailDir
		links := nestedLinks
		if own {
			links = outerLinks
		}
		if err := links.link(&member, entry); err != nil {
			return err
		}
		if own {
			outer = append(outer, member)
			return nil
		}
		nested = append(nested, member)
		return nil
	})
	if err != nil {
		return fmt.Errorf("dabackup: read the restored home directory: %w", err)
	}

	manifest.Home = &ArchivePart{
		Name:        manifest.HomeArchiveName,
		Compression: manifest.HomeCompression,
		Members:     nested,
	}
	manifest.Outer.Members = append(afterRecords(manifest.Outer.Members, Member{
		Name: manifest.HomeArchiveName, Typeflag: tar.TypeReg, Mode: 0o600,
		Uname: "", Gname: "", Nested: true,
	}), outer...)
	return nil
}

// afterRecords puts the nested home archive back where DirectAdmin had
// it: the last member of backup/, before the account's own directories.
func afterRecords(existing []Member, nested Member) []Member {
	last := 0
	for i, member := range existing {
		if first, _, _ := strings.Cut(path.Clean(member.Name), "/"); first == BackupDir {
			last = i + 1
		}
	}
	out := make([]Member, 0, len(existing)+1)
	out = append(out, existing[:last]...)
	out = append(out, nested)
	return append(out, existing[last:]...)
}

// linkIndex remembers the name each file was first carried under, so a
// second name for the same file is carried as a link to the first rather
// than as another copy of its contents. DirectAdmin's own backup does
// this -- backup_hard_link_check is on by default -- and an archive that
// did not would restore a Maildir whose messages are linked from two
// folders at twice the size it was backed up at.
//
// There is one of these per archive, not one per account: a link's target
// is a name in the same tar, and the account's own directories go into
// the outer archive while the rest of the home directory goes into the
// nested one.
type linkIndex struct{ first map[[2]uint64]string }

func newLinkIndex() *linkIndex { return &linkIndex{first: map[[2]uint64]string{}} }

// link turns a member into a link to a name already in this archive,
// when the file it names is the same file that name carried.
func (x *linkIndex) link(member *Member, entry fs.DirEntry) error {
	if member.Typeflag != tar.TypeReg {
		return nil
	}
	info, err := entry.Info()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink < 2 {
		return nil
	}
	key := [2]uint64{uint64(stat.Dev), uint64(stat.Ino)}
	target, seen := x.first[key]
	if !seen {
		x.first[key] = member.Name
		return nil
	}
	member.Typeflag, member.Linkname = tar.TypeLink, target
	member.Size, member.Body = 0, ""
	return nil
}

// memberOf reads one entry of the restored tree as a tar header. It
// reports false for anything tar does not carry.
func memberOf(full, rel string, entry fs.DirEntry, names *nameCache) (Member, bool, error) {
	info, err := entry.Info()
	if err != nil {
		return Member{}, false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return Member{}, false, fmt.Errorf("dabackup: %s has no ownership to read", rel)
	}
	member := Member{
		Name:  rel,
		Mode:  int64(info.Mode().Perm()) | permissionBits(info.Mode()),
		UID:   int(stat.Uid),
		GID:   int(stat.Gid),
		Uname: names.user(int(stat.Uid)),
		Gname: names.group(int(stat.Gid)),
		// Whole seconds, because that is what a tar header carries
		// without pax records DirectAdmin's own tar never wrote, and a
		// sub-second time here comes back rounded rather than kept.
		ModTime: info.ModTime().Truncate(time.Second),
		Xattrs:  xattrsOfFile(full),
	}
	switch {
	case entry.IsDir():
		member.Typeflag, member.Name = tar.TypeDir, rel+"/"
	case info.Mode()&fs.ModeSymlink != 0:
		member.Typeflag = tar.TypeSymlink
		if member.Linkname, err = os.Readlink(full); err != nil {
			return Member{}, false, err
		}
	case info.Mode().IsRegular():
		member.Typeflag, member.Size = tar.TypeReg, info.Size()
	case info.Mode()&fs.ModeNamedPipe != 0:
		member.Typeflag = tar.TypeFifo
	case info.Mode()&fs.ModeDevice != 0:
		member.Typeflag = tar.TypeBlock
		if info.Mode()&fs.ModeCharDevice != 0 {
			member.Typeflag = tar.TypeChar
		}
		member.Devmajor, member.Devminor = int64(unix.Major(uint64(stat.Rdev))), int64(unix.Minor(uint64(stat.Rdev)))
	default:
		return Member{}, false, nil
	}
	return member, true, nil
}

// permissionBits carries setuid, setgid and the sticky bit across, which
// fs.FileMode keeps outside the permission bits and tar does not.
func permissionBits(mode fs.FileMode) int64 {
	var bits int64
	if mode&fs.ModeSetuid != 0 {
		bits |= 0o4000
	}
	if mode&fs.ModeSetgid != 0 {
		bits |= 0o2000
	}
	if mode&fs.ModeSticky != 0 {
		bits |= 0o1000
	}
	return bits
}

// xattrsOfFile reads the extended attributes off one file, in the shape
// Member.Xattrs keeps them. An ACL is one of these, and an archive that
// dropped it restores a directory the web server can no longer read.
//
// A file whose attributes cannot be read is not an error: the restore is
// worth more than its access control list, and every other path here
// would refuse the whole archive over one file.
func xattrsOfFile(full string) map[string]string {
	size, err := unix.Llistxattr(full, nil)
	if err != nil || size == 0 {
		return nil
	}
	buffer := make([]byte, size)
	size, err = unix.Llistxattr(full, buffer)
	if err != nil {
		return nil
	}
	var kept map[string]string
	for _, name := range strings.Split(strings.TrimRight(string(buffer[:size]), "\x00"), "\x00") {
		if name == "" {
			continue
		}
		length, err := unix.Lgetxattr(full, name, nil)
		if err != nil {
			continue
		}
		value := make([]byte, length)
		if length, err = unix.Lgetxattr(full, name, value); err != nil {
			continue
		}
		if kept == nil {
			kept = map[string]string{}
		}
		kept["SCHILY.xattr."+name] = string(value[:length])
	}
	return kept
}

// nameCache answers what a numeric owner is called, once per owner.
type nameCache struct{ users, groups map[int]string }

func newNameCache() *nameCache {
	return &nameCache{users: map[int]string{}, groups: map[int]string{}}
}

func (c *nameCache) user(id int) string {
	if name, seen := c.users[id]; seen {
		return name
	}
	name := ""
	if found, err := user.LookupId(strconv.Itoa(id)); err == nil {
		name = found.Username
	}
	c.users[id] = name
	return name
}

func (c *nameCache) group(id int) string {
	if name, seen := c.groups[id]; seen {
		return name
	}
	name := ""
	if found, err := user.LookupGroupId(strconv.Itoa(id)); err == nil {
		name = found.Name
	}
	c.groups[id] = name
	return name
}

// AddMetadataMember records a file written into the metadata part after
// the archive was taken apart, so the archive built from the tree
// carries it. The name is the member as DirectAdmin's own archive would
// name it, under backup/, and the file has to be at that path in the
// metadata part already. A member already there by that name is
// replaced.
//
// Only a lean tree takes one. Its manifest holds nothing but backup/,
// and the account's own directories are built after it when the archive
// is packed, so the member goes at the end and is still where
// DirectAdmin's restore reads it: with the rest of backup/, before the
// account's own directories.
//
// What this is for is the webmail data: DirectAdmin files roundcube.xml
// under email_data, so an archive that leaves the messages out leaves
// that out too, and the provider exports it by hand after the unpack.
func (Layout) AddMetadataMember(dir, name string) error {
	clean, err := safeMemberName(name)
	if err != nil {
		return err
	}
	if first, _, _ := strings.Cut(clean, "/"); first != BackupDir || clean == BackupDir {
		return fmt.Errorf("dabackup: %s is not a member of %s/", name, BackupDir)
	}
	manifest, err := readManifest(dir)
	if err != nil {
		return err
	}
	if !manifest.Lean {
		return fmt.Errorf("dabackup: this archive was not read in place, and carries its own %s", name)
	}
	body := treePath(clean, false)
	stat, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(body)))
	if err != nil {
		return fmt.Errorf("dabackup: record %s: %w", name, err)
	}
	if !stat.Mode().IsRegular() {
		return fmt.Errorf("dabackup: record %s: not a regular file", name)
	}
	member := Member{
		Name: clean, Typeflag: tar.TypeReg, Mode: 0o600,
		Uname: "root", Gname: "root",
		ModTime: stat.ModTime(), Size: stat.Size(), Body: body,
	}
	for i, existing := range manifest.Outer.Members {
		if existingClean, err := safeMemberName(existing.Name); err == nil && existingClean == clean {
			manifest.Outer.Members[i] = member
			return writeManifest(dir, manifest)
		}
	}
	manifest.Outer.Members = append(manifest.Outer.Members, member)
	return writeManifest(dir, manifest)
}

// sayItHoldsEverything rewrites backup_options.list in the tree, so the
// archive built from it tells DirectAdmin's restore that it is holding a
// whole account rather than the selection the backup asked for.
func sayItHoldsEverything(root *os.Root, manifest *Manifest) error {
	body := []byte(strings.Join(wholeAccountOptions, "\n") + "\n")
	for i, member := range manifest.Outer.Members {
		if path.Clean(member.Name) != optionsList || member.Body == "" {
			continue
		}
		file, err := root.OpenFile(member.Body, os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("dabackup: rewrite %s: %w", optionsList, err)
		}
		_, err = file.Write(body)
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return fmt.Errorf("dabackup: rewrite %s: %w", optionsList, err)
		}
		manifest.Outer.Members[i].Size = int64(len(body))
		return nil
	}
	return fmt.Errorf("dabackup: the archive carried no %s to rewrite", optionsList)
}

// extensionOf is compressionOf backwards: what a name compressed this way
// ends in.
func extensionOf(compression string) string {
	if compression == "" {
		return ""
	}
	return "." + compression
}
