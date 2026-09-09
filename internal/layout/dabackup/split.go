package dabackup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/shukiv/gniza/internal/panel"
)

// Taking an archive apart and putting it back together are one thing, not
// two: a backup that could be split and not rebuilt is a backup nothing
// can restore.
var _ panel.ArchivePacker = Layout{}

// Split mode on DirectAdmin.
//
// Restic deduplicates files, not compressed archives, so a nightly backup
// that stores what admin-backup produced stores close to a full copy every
// night (docs/DESIGN.md §4). DirectAdmin has no documented way to be asked
// for the parts separately -- that is what ADR 0019 was waiting on -- so
// the parts are taken out of the archive it does write.
//
// The archive is two archives: an outer tar with backup/, domains/ and
// imap/ in it, and a second compressed tar at backup/home.tar.zst holding
// the rest of the home directory. Taking it apart gives restic real files
// at paths that are the same tonight as they were last night. Putting it
// back has to give DirectAdmin's own restore the archive it wrote, which
// is why the headers are kept rather than rebuilt: the entries carry
// owners and groups as names, and not always the account's own --
// gzv0908a/apache on .php/, gzv0908a/mail on Maildir/, measured on 1.709.
// A tree walked and repacked from what is on disk would restore an
// account whose mail directory Dovecot cannot write.
//
// So the unpacked form is two things: a manifest holding every tar header
// in order, and a tree holding the file bodies. Everything that is not a
// file body -- directories, symlinks, hard links, devices -- is in the
// manifest alone, and the repack is driven entirely by it.

const (
	// MetadataTreeDir and HomeTreeDir are the two directories an
	// unpacked archive becomes. They are the two parts restic is pointed
	// at, and they are named for what reassemble.Classify reads a
	// snapshot's paths as: the metadata part is recognised by its name,
	// and the other one is the home directory.
	//
	// HomeTreeDir is where domains/, imap/ and the nested archive are put
	// back together, because they are three views of one directory.
	MetadataTreeDir = "metadata"
	HomeTreeDir     = "home"
	// ManifestFile is where the headers of both archives are written. It
	// lives in the metadata part rather than beside it because a part is
	// one path handed to restic, and a manifest outside both parts would
	// not be backed up at all -- leaving a tree that cannot be repacked
	// into anything DirectAdmin would restore.
	ManifestFile = ".gniza-manifest.json"

	// manifestVersion is what the reader checks. A tree written by a
	// later Gniza is refused rather than misread.
	manifestVersion = 1
	// leanManifestVersion is the version of a tree whose home directory
	// was read where it lies (ADR 0021). It is a separate number rather
	// than a field on version 1 because a Gniza that does not know to
	// rebuild the account's own files out of the tree would repack an
	// archive without them and call it a restore.
	leanManifestVersion = 2
	// reservedPrefix is what everything in the tree that is Gniza's
	// rather than DirectAdmin's is named. An archive whose own members
	// would land there is refused; see reserveBody for the other thing
	// kept under it.
	reservedPrefix = MetadataTreeDir + "/.gniza-"
	// maxMemberSize bounds one file taken out of the archive, for the
	// same reason reassemble bounds one: the archive is only as
	// trustworthy as the machine that wrote it.
	maxMemberSize = 1 << 40 // 1 TiB
)

// Manifest is what the two tars said, in the order they said it.
type Manifest struct {
	Version int    `json:"version"`
	Account string `json:"account"`
	// ArchiveName is what DirectAdmin called the archive this came out
	// of. The repack writes it back under that name because
	// DirectAdmin's own restore reads the account out of the filename
	// before it reads anything inside, and a name Gniza made up is one
	// its restore refuses.
	ArchiveName string `json:"archive_name"`
	// Outer is the archive DirectAdmin wrote. Home is the one inside it,
	// absent for an account with nothing outside its domains.
	Outer ArchivePart  `json:"outer"`
	Home  *ArchivePart `json:"home,omitempty"`

	// Lean says the account's own files were never in this archive: they
	// were read from the home directory in place, and the members for
	// them are built out of the restored tree when the archive is put
	// back. See ADR 0021.
	Lean bool `json:"lean,omitempty"`
	// HomeArchiveName and HomeCompression are what the nested archive is
	// written back as, since there was none to copy the name from.
	HomeArchiveName string `json:"home_archive_name,omitempty"`
	HomeCompression string `json:"home_compression,omitempty"`
}

// ArchivePart is one tar.
type ArchivePart struct {
	// Name is the member the nested archive arrived as, empty for the
	// outer one. Compression is gz, zst, or empty for a plain tar: a host
	// configured for gzip writes home.tar.gz, and it is put back the way
	// it came.
	Name        string   `json:"name,omitempty"`
	Compression string   `json:"compression,omitempty"`
	Members     []Member `json:"members"`
}

// Member is one tar header.
//
// The name is kept exactly as it arrived, trailing slash and leading ./
// included, because that is what goes back into the archive. Everything
// safety is decided on is decided on a cleaned copy that is never written
// down.
type Member struct {
	Name     string    `json:"name"`
	Typeflag byte      `json:"typeflag"`
	Mode     int64     `json:"mode"`
	UID      int       `json:"uid"`
	GID      int       `json:"gid"`
	Uname    string    `json:"uname,omitempty"`
	Gname    string    `json:"gname,omitempty"`
	ModTime  time.Time `json:"mtime"`
	Size     int64     `json:"size,omitempty"`
	Linkname string    `json:"linkname,omitempty"`
	Devmajor int64     `json:"devmajor,omitempty"`
	Devminor int64     `json:"devminor,omitempty"`
	// Xattrs are the extended attributes tar recorded, which is where an
	// ACL lives. Only these pax records are kept: every other one Go's
	// tar writer works out again from the fields above, and handing it
	// back its own bookkeeping is how a header stops agreeing with
	// itself.
	Xattrs map[string]string `json:"xattrs,omitempty"`
	// Body is where this member's file lives under the tree, empty for
	// anything that is not a regular file.
	Body string `json:"body,omitempty"`
	// Nested marks the member the home archive arrived as. Its body is
	// not in the tree: it was opened, and it is written again from the
	// members in Manifest.Home.
	Nested bool `json:"nested,omitempty"`
}

// UnpackArchive takes a DirectAdmin account archive apart into a manifest
// and a tree of file bodies, so restic sees files rather than one
// compressed blob.
//
// dir is created if it is not there, and must not already hold either
// part: a tree from a previous run is refused rather than written into.
// What it ends up holding is MetadataTreeDir and HomeTreeDir, with
// ManifestFile inside the first of them. Anything else already in dir is
// left alone -- the archive being taken apart is usually one of them.
func (Layout) UnpackArchive(ctx context.Context, archivePath, account, dir string) error {
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
	// Both parts are made whether or not the archive has anything to put
	// in them, because a part that is not there is a backup that is
	// missing one. A part already there is a tree from a previous run:
	// writing into it would keep files the account has since deleted and
	// would fail wherever a directory has become a file, so it is
	// refused rather than merged into.
	for _, part := range []string{MetadataPart(dir), HomedirPart(dir)} {
		if _, err := os.Lstat(part); err == nil {
			return fmt.Errorf("dabackup: %s already holds a %s tree", dir, filepath.Base(part))
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("dabackup: %s: %w", part, err)
		}
		if err := os.MkdirAll(part, 0o700); err != nil {
			return fmt.Errorf("dabackup: create %s: %w", part, err)
		}
	}
	// Every body below is written through this. os.Root resolves each
	// path against the directory itself rather than against the string,
	// so a member that walks out through a symlink an account planted in
	// its own home is refused by the operating system -- which matters,
	// because this runs as root on an archive a customer can influence.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("dabackup: open %s: %w", dir, err)
	}
	defer func() { _ = root.Close() }()

	out := &unpacker{root: root, bodies: map[string]bool{}}
	manifest := Manifest{
		Version:     manifestVersion,
		Account:     account,
		ArchiveName: filepath.Base(archivePath),
		Outer:       ArchivePart{Compression: compressionOf(archivePath)},
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
	return os.WriteFile(filepath.Join(MetadataPart(dir), ManifestFile), append(body, '\n'), 0o600)
}

// unpacker carries the state one archive is taken apart with.
type unpacker struct {
	root *os.Root
	// bodies is every path a file has already been written to. Two
	// members wanting the same one is not expected -- the nested archive
	// held the complement of domains/ and imap/ on the host this was
	// measured on -- but expected is not the same as promised, and the
	// repack writes both members back.
	bodies map[string]bool
	// ordinal counts members across both archives, so a body that has to
	// be put somewhere else has somewhere unique to go.
	ordinal int
	// lean marks an archive that was asked for without the account's own
	// files. See leanMember for what that changes.
	lean bool
}

// readTar walks one archive, recording every header and writing every
// file body. home says which of the two archives this is.
func (u *unpacker) readTar(ctx context.Context, tr *tar.Reader, manifest *Manifest, home bool) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("dabackup: read account archive: %w", err)
		}
		clean, err := safeMemberName(header.Name)
		if err != nil {
			return err
		}
		if u.lean && !home {
			skip, err := leanMember(clean, header.Typeflag)
			if err != nil {
				return err
			}
			if skip {
				continue
			}
		}
		u.ordinal++
		member := Member{
			Name: header.Name, Typeflag: header.Typeflag, Mode: header.Mode,
			UID: header.Uid, GID: header.Gid, Uname: header.Uname, Gname: header.Gname,
			ModTime: header.ModTime, Linkname: header.Linkname,
			Devmajor: header.Devmajor, Devminor: header.Devminor,
			Xattrs: xattrsOf(header),
		}
		if !home && isNestedHomeArchive(clean, header.Typeflag) {
			if manifest.Home != nil {
				return fmt.Errorf("dabackup: the archive carries two nested home archives")
			}
			member.Nested = true
			manifest.Outer.Members = append(manifest.Outer.Members, member)
			manifest.Home = &ArchivePart{Name: header.Name, Compression: compressionOf(clean)}
			if err := u.readNested(ctx, tr, clean, manifest); err != nil {
				return err
			}
			continue
		}
		if err := u.place(&member, tr, clean, header.Size, home); err != nil {
			return err
		}
		if home {
			manifest.Home.Members = append(manifest.Home.Members, member)
		} else {
			manifest.Outer.Members = append(manifest.Outer.Members, member)
		}
	}
}

// readNested opens the archive inside the archive and walks it in place,
// without writing the compressed blob anywhere: storing it is what split
// mode exists to avoid.
func (u *unpacker) readNested(ctx context.Context, tr *tar.Reader, name string, manifest *Manifest) error {
	reader, closer, err := decompressed(tr, name)
	if err != nil {
		return fmt.Errorf("dabackup: read %s: %w", name, err)
	}
	defer closer()
	return u.readTar(ctx, tar.NewReader(reader), manifest, true)
}

// place puts one member's file body in the tree and records where.
//
// Only regular files have a body. A directory is made so the tree is a
// directory tree rather than a heap of paths; a symlink, a hard link, a
// device or a fifo is left to the manifest alone, which is both faithful
// and the reason nothing here ever creates a link for a later member to
// be written through.
func (u *unpacker) place(member *Member, tr *tar.Reader, clean string, size int64, home bool) error {
	if clean == "." {
		return nil
	}
	target := treePath(clean, home)
	if strings.HasPrefix(target, reservedPrefix) {
		return fmt.Errorf("dabackup: archive member %q is named the same as one of Gniza's own files", member.Name)
	}
	switch member.Typeflag {
	case tar.TypeDir:
		if err := u.root.MkdirAll(target, 0o700); err != nil {
			return fmt.Errorf("dabackup: create %s: %w", member.Name, err)
		}
	case tar.TypeReg:
		if size > maxMemberSize {
			return fmt.Errorf("dabackup: member %s is %d bytes, refusing to unpack", member.Name, size)
		}
		body := u.reserveBody(target)
		if parent := path.Dir(body); parent != "." {
			if err := u.root.MkdirAll(parent, 0o700); err != nil {
				return fmt.Errorf("dabackup: create %s: %w", parent, err)
			}
		}
		// The mode on disk is Gniza's, not DirectAdmin's: the manifest
		// carries what the archive said, and a staged tree readable by
		// nobody but root is the right thing for a copy of an account.
		file, err := u.root.OpenFile(body, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("dabackup: create %s: %w", member.Name, err)
		}
		written, copyErr := io.Copy(file, io.LimitReader(tr, maxMemberSize))
		closeErr := file.Close()
		if copyErr != nil {
			return fmt.Errorf("dabackup: write %s: %w", member.Name, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("dabackup: close %s: %w", member.Name, closeErr)
		}
		if written != size {
			return fmt.Errorf("dabackup: %s is %d bytes, its header said %d",
				member.Name, written, size)
		}
		member.Size = size
		member.Body = body
	}
	return nil
}

// reserveBody answers where a file may be written.
//
// Almost always where its name says. When two members of the two
// archives name the same file, the second goes somewhere of Gniza's own
// choosing instead, because dropping one of them would repack an archive
// whose second copy of that file is the first one's contents.
func (u *unpacker) reserveBody(preferred string) string {
	if !u.bodies[preferred] {
		u.bodies[preferred] = true
		return preferred
	}
	duplicate := reservedPrefix + "duplicate/" + strconv.Itoa(u.ordinal)
	u.bodies[duplicate] = true
	return duplicate
}

// PackArchive writes the archive back from the manifest and the tree,
// into outDir under the name DirectAdmin gave it, and reports where it
// put it.
//
// What comes out is the archive DirectAdmin wrote, header for header. The
// compression is not byte for byte -- compressing the same bytes twice
// does not produce the same file -- and does not need to be: what reads
// it is tar.
func (Layout) PackArchive(ctx context.Context, dir, account, outDir string) (string, error) {
	manifest, err := readManifest(dir)
	if err != nil {
		return "", err
	}
	if manifest.Account != account {
		return "", fmt.Errorf("dabackup: this tree was taken from %s, not from %s",
			manifest.Account, account)
	}
	// The name comes out of the manifest, so it is checked the way a name
	// from outside is checked: it has to be one filename, and it has to
	// be this account's.
	name := manifest.ArchiveName
	if name == "" || filepath.Base(name) != name || !nameMatchesArchive(name, account) {
		return "", fmt.Errorf("dabackup: the manifest does not name an archive belonging to %s", account)
	}
	archivePath := filepath.Join(outDir, name)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", fmt.Errorf("dabackup: open %s: %w", dir, err)
	}
	defer func() { _ = root.Close() }()

	// An account read where it lies has no members in the manifest for
	// its own files. They are built from the tree restic restored, and
	// the archive says it holds a whole account, because it does.
	if manifest.Lean {
		if err := hydrate(dir, &manifest); err != nil {
			return "", err
		}
		if err := sayItHoldsEverything(root, &manifest); err != nil {
			return "", err
		}
	}

	// The nested archive is built first, into a file beside the one being
	// written, because its length is a header in the outer archive and is
	// not known until it is finished.
	nested := ""
	if manifest.Home != nil {
		nested, err = writeNested(ctx, root, manifest.Home, archivePath)
		if err != nil {
			return "", err
		}
		defer func() { _ = os.Remove(nested) }()
	}

	out, err := os.OpenFile(archivePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("dabackup: create %s: %w", archivePath, err)
	}
	defer out.Close()
	writer, finish, err := compressed(out, manifest.Outer.Compression)
	if err != nil {
		return "", err
	}
	tw := tar.NewWriter(writer)
	for _, member := range manifest.Outer.Members {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if member.Nested {
			if err := copyNested(tw, member, nested); err != nil {
				return "", err
			}
			continue
		}
		if err := writeBack(tw, root, member); err != nil {
			return "", err
		}
	}
	if err := tw.Close(); err != nil {
		return "", fmt.Errorf("dabackup: finish %s: %w", archivePath, err)
	}
	if err := finish(); err != nil {
		return "", fmt.Errorf("dabackup: finish %s: %w", archivePath, err)
	}
	if err := out.Close(); err != nil {
		return "", fmt.Errorf("dabackup: finish %s: %w", archivePath, err)
	}
	return archivePath, nil
}

// writeNested rebuilds the archive that goes inside the archive and
// reports the file it is in.
func writeNested(ctx context.Context, root *os.Root, home *ArchivePart, archivePath string) (string, error) {
	file, err := os.CreateTemp(filepath.Dir(archivePath), ".gniza-home-*")
	if err != nil {
		return "", fmt.Errorf("dabackup: stage the home archive: %w", err)
	}
	defer file.Close()
	writer, finish, err := compressed(file, home.Compression)
	if err != nil {
		_ = os.Remove(file.Name())
		return "", err
	}
	tw := tar.NewWriter(writer)
	for _, member := range home.Members {
		if err := ctx.Err(); err != nil {
			_ = os.Remove(file.Name())
			return "", err
		}
		if err := writeBack(tw, root, member); err != nil {
			_ = os.Remove(file.Name())
			return "", err
		}
	}
	if err := tw.Close(); err == nil {
		err = finish()
		if err == nil {
			err = file.Close()
		}
		if err != nil {
			_ = os.Remove(file.Name())
			return "", fmt.Errorf("dabackup: finish the home archive: %w", err)
		}
	} else {
		_ = os.Remove(file.Name())
		return "", fmt.Errorf("dabackup: finish the home archive: %w", err)
	}
	return file.Name(), nil
}

// copyNested writes the rebuilt home archive back into the outer one
// under the header it arrived with, at whatever length it came out.
func copyNested(tw *tar.Writer, member Member, nested string) error {
	if nested == "" {
		return fmt.Errorf("dabackup: the manifest names a nested home archive it does not describe")
	}
	file, err := os.Open(nested)
	if err != nil {
		return fmt.Errorf("dabackup: read the home archive: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("dabackup: read the home archive: %w", err)
	}
	header := member.header()
	header.Size = info.Size()
	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("dabackup: write header %s: %w", member.Name, err)
	}
	if _, err := io.Copy(tw, file); err != nil {
		return fmt.Errorf("dabackup: write %s: %w", member.Name, err)
	}
	return nil
}

// writeBack puts one member back, with its body from the tree when it has
// one.
func writeBack(tw *tar.Writer, root *os.Root, member Member) error {
	header := member.header()
	if member.Body == "" {
		header.Size = 0
		if err := tw.WriteHeader(header); err != nil {
			return fmt.Errorf("dabackup: write header %s: %w", member.Name, err)
		}
		return nil
	}
	file, err := root.Open(member.Body)
	if err != nil {
		return fmt.Errorf("dabackup: read %s: %w", member.Name, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("dabackup: read %s: %w", member.Name, err)
	}
	// A body that is not the length its header said produces a tar whose
	// headers and contents disagree, which tar reads as a corrupt archive
	// somewhere else entirely. It is caught here, named.
	if info.Size() != member.Size {
		return fmt.Errorf("dabackup: %s is %d bytes in the tree, the archive said %d",
			member.Name, info.Size(), member.Size)
	}
	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("dabackup: write header %s: %w", member.Name, err)
	}
	if _, err := io.Copy(tw, file); err != nil {
		return fmt.Errorf("dabackup: write %s: %w", member.Name, err)
	}
	return nil
}

// header turns a recorded member back into a tar header.
//
// The format is left to Go, which picks one that can carry these fields:
// a long name or an mtime with nanoseconds on it gets pax, a short one
// does not. What DirectAdmin's tar chose is not reproduced because
// nothing reads it but tar, and every field it carried is here.
func (m Member) header() *tar.Header {
	header := &tar.Header{
		Name: m.Name, Typeflag: m.Typeflag, Mode: m.Mode,
		Uid: m.UID, Gid: m.GID, Uname: m.Uname, Gname: m.Gname,
		ModTime: m.ModTime, Linkname: m.Linkname,
		Devmajor: m.Devmajor, Devminor: m.Devminor,
		Format: tar.FormatUnknown,
	}
	if m.Typeflag == tar.TypeReg {
		header.Size = m.Size
	}
	if len(m.Xattrs) > 0 {
		header.PAXRecords = map[string]string{}
		for key, value := range m.Xattrs {
			header.PAXRecords[key] = value
		}
	}
	return header
}

// readManifest reads what UnpackArchive wrote.
func readManifest(dir string) (Manifest, error) {
	body, err := os.ReadFile(filepath.Join(MetadataPart(dir), ManifestFile))
	if err != nil {
		return Manifest{}, fmt.Errorf("dabackup: this is not an unpacked account archive: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("dabackup: read the archive manifest: %w", err)
	}
	if manifest.Version != manifestVersion && manifest.Version != leanManifestVersion {
		return Manifest{}, fmt.Errorf(
			"dabackup: this tree was written by another version of Gniza (manifest %d, this reads %d and %d)",
			manifest.Version, manifestVersion, leanManifestVersion)
	}
	if manifest.Lean != (manifest.Version == leanManifestVersion) {
		return Manifest{}, fmt.Errorf(
			"dabackup: manifest %d does not agree with itself about where this account's files were read",
			manifest.Version)
	}
	if len(manifest.Outer.Members) == 0 {
		return Manifest{}, fmt.Errorf("dabackup: the archive manifest describes no archive")
	}
	return manifest, nil
}

// xattrsOf keeps the extended attributes and discards the rest of the pax
// records. See Member.Xattrs.
func xattrsOf(header *tar.Header) map[string]string {
	var kept map[string]string
	for key, value := range header.PAXRecords {
		if !strings.HasPrefix(key, "SCHILY.xattr.") {
			continue
		}
		if kept == nil {
			kept = map[string]string{}
		}
		kept[key] = value
	}
	return kept
}

// isNestedHomeArchive says whether this member is the archive inside the
// archive. The name is matched by prefix because a host configured for
// gzip writes home.tar.gz where this one wrote home.tar.zst.
func isNestedHomeArchive(clean string, typeflag byte) bool {
	return typeflag == tar.TypeReg && path.Dir(clean) == BackupDir &&
		strings.HasPrefix(path.Base(clean), "home.tar")
}

// MetadataPart and HomedirPart are the two directories UnpackArchive
// writes, and the two paths a split payload hands restic.
func MetadataPart(dir string) string { return filepath.Join(dir, MetadataTreeDir) }
func HomedirPart(dir string) string  { return filepath.Join(dir, HomeTreeDir) }

// treePath answers where a member's body belongs in the tree.
//
// domains/ and imap/ from the outer archive and everything in the nested
// one are parts of the same directory -- /home/<account> -- and are put
// back into it, so what restic sees is a home directory rather than the
// shape of the archive it arrived in. What is left is DirectAdmin's own
// records of the account, which are the metadata part.
func treePath(clean string, home bool) string {
	if home {
		return path.Join(HomeTreeDir, clean)
	}
	if first, _, _ := strings.Cut(clean, "/"); first == DomainsDir || first == MailDir {
		return path.Join(HomeTreeDir, clean)
	}
	return path.Join(MetadataTreeDir, clean)
}

// safeMemberName refuses a member that would be written outside the tree
// and returns the cleaned name everything else is decided on.
//
// The rules are inspect's, for the same reason: an awkward name is not a
// dangerous one, and refusing an account's archive over a backslash in a
// filename stops that account being backed up at all.
func safeMemberName(name string) (string, error) {
	if path.IsAbs(name) || filepath.IsAbs(name) || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("dabackup: unsafe account archive member %q", name)
	}
	// Checked before cleaning as well as after: cleaning alone would
	// conceal backup/../../etc.
	for _, component := range strings.Split(name, "/") {
		if component == ".." {
			return "", fmt.Errorf("dabackup: unsafe account archive member %q", name)
		}
	}
	clean := path.Clean(name)
	if path.IsAbs(clean) {
		return "", fmt.Errorf("dabackup: unsafe account archive member %q", name)
	}
	for _, component := range strings.Split(clean, "/") {
		if component == ".." {
			return "", fmt.Errorf("dabackup: unsafe account archive member %q", name)
		}
	}
	return clean, nil
}

// compressionOf reads how an archive is compressed from its name, which
// is how DirectAdmin says so.
func compressionOf(name string) string {
	switch {
	case strings.HasSuffix(name, ".zst"):
		return "zst"
	case strings.HasSuffix(name, ".gz"):
		return "gz"
	default:
		return ""
	}
}

// decompressed wraps a reader in whatever the name says it is compressed
// with, and returns the closer for it.
func decompressed(reader io.Reader, name string) (io.Reader, func(), error) {
	switch compressionOf(name) {
	case "gz":
		z, err := gzip.NewReader(reader)
		if err != nil {
			return nil, nil, fmt.Errorf("dabackup: read compressed account archive: %w", err)
		}
		return z, func() { _ = z.Close() }, nil
	case "zst":
		z, err := zstd.NewReader(reader,
			zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(128<<20))
		if err != nil {
			return nil, nil, fmt.Errorf("dabackup: read zstd account archive: %w", err)
		}
		return z, z.Close, nil
	default:
		return reader, func() {}, nil
	}
}

// compressed wraps a writer the same way, and returns what has to be
// called before the file underneath is closed.
func compressed(writer io.Writer, compression string) (io.Writer, func() error, error) {
	switch compression {
	case "gz":
		z := gzip.NewWriter(writer)
		return z, z.Close, nil
	case "zst":
		z, err := zstd.NewWriter(writer, zstd.WithEncoderConcurrency(1))
		if err != nil {
			return nil, nil, fmt.Errorf("dabackup: write zstd account archive: %w", err)
		}
		return z, z.Close, nil
	case "":
		return writer, func() error { return nil }, nil
	default:
		return nil, nil, fmt.Errorf("dabackup: the manifest names a compression this does not write: %q", compression)
	}
}
