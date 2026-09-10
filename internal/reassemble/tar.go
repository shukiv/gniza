// Package reassemble rebuilds a cPanel account archive from the parts a
// split-mode backup stored separately.
//
// Split mode exists because restic cannot deduplicate a compressed archive
// (docs/DESIGN.md §4). The cost of that decision is paid here: what pkgacct
// produced as one file has to be put back together before cPanel's
// restorepkg will accept it.
package reassemble

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

// maxEntrySize bounds a single extracted file. An archive is only as
// trustworthy as the server that produced it, and a decompression bomb
// would otherwise fill the restore volume.
const maxEntrySize = 1 << 40 // 1 TiB

// extractTar unpacks an archive into dir.
func extractTar(archivePath, dir string) error {
	_, err := extractTarFiltered(archivePath, dir, nil)
	return err
}

// ExtractMembers unpacks only the named parts of a cpmove archive, and
// reports how many files that produced.
//
// Prefixes are relative to the archive's own top-level directory —
// "dnszones/" rather than "cpmove-studio/dnszones/" — because that
// directory is named after the account. A prefix ending in a slash matches
// a directory and everything under it; anything else matches one file.
//
// The count is the point of the return value: a granular restore that
// extracted nothing has failed, however cleanly it ran.
func ExtractMembers(archivePath, dir string, prefixes []string) (int, error) {
	if len(prefixes) == 0 {
		return 0, fmt.Errorf("reassemble: no archive members were asked for")
	}
	return extractTarFiltered(archivePath, dir, func(name string) bool {
		// Drop the archive's own top-level directory.
		rel := name
		if slash := strings.IndexByte(rel, '/'); slash >= 0 {
			rel = rel[slash+1:]
		}
		for _, prefix := range prefixes {
			if strings.HasSuffix(prefix, "/") {
				if strings.HasPrefix(rel, prefix) {
					return true
				}
				continue
			}
			if rel == prefix {
				return true
			}
		}
		return false
	})
}

// PackDir packs a directory into an uncompressed archive, so a granular
// restore can be handed over as one file.
func PackDir(dir, archivePath string) error { return createTar(dir, archivePath) }

// extractTarFiltered unpacks the entries keep accepts, or everything when
// keep is nil, and reports how many regular files it wrote.
func extractTarFiltered(archivePath, dir string, keep func(name string) bool) (int, error) {
	written := 0
	file, err := os.Open(archivePath)
	if err != nil {
		return 0, fmt.Errorf("reassemble: open %s: %w", archivePath, err)
	}
	defer file.Close()

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return written, fmt.Errorf("reassemble: create %s: %w", dir, err)
	}
	// Everything below is written through this, and os.Root resolves
	// every path against the directory itself rather than against the
	// string: a name that walks out through a symlink is refused by the
	// operating system, not by this program's reading of the name.
	//
	// It is not spare belt and braces. The archive is what a backup of
	// another server produced, and it is unpacked as root before cPanel's
	// restore -- restricted or not -- has seen any of it.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return written, fmt.Errorf("reassemble: open %s: %w", dir, err)
	}
	defer func() { _ = root.Close() }()

	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return written, nil
		}
		if err != nil {
			return written, fmt.Errorf("reassemble: read %s: %w", archivePath, err)
		}

		if keep != nil && !keep(header.Name) {
			continue
		}

		name, err := safeName(header.Name)
		if err != nil {
			return written, err
		}
		if name == "" {
			continue
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, entryMode(header, 0o700)); err != nil {
				return written, fmt.Errorf("reassemble: create %s: %w", header.Name, err)
			}
		case tar.TypeReg:
			if header.Size > maxEntrySize {
				return written, fmt.Errorf("reassemble: entry %s is %d bytes, refusing to extract",
					header.Name, header.Size)
			}
			if parent := path.Dir(name); parent != "." {
				if err := root.MkdirAll(parent, 0o700); err != nil {
					return written, fmt.Errorf("reassemble: create %s: %w", parent, err)
				}
			}
			out, err := root.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, entryMode(header, 0o600))
			if err != nil {
				return written, fmt.Errorf("reassemble: create %s: %w", header.Name, err)
			}
			_, copyErr := io.Copy(out, io.LimitReader(reader, maxEntrySize))
			closeErr := out.Close()
			if copyErr != nil {
				return written, fmt.Errorf("reassemble: write %s: %w", header.Name, copyErr)
			}
			if closeErr != nil {
				return written, fmt.Errorf("reassemble: close %s: %w", header.Name, closeErr)
			}
			written++
		case tar.TypeSymlink:
			// A link is recreated only where it cannot lead out of the
			// tree. An absolute target used to pass this: joining it with
			// the entry's own directory produced a path that read as
			// being inside, while the link written to disk still pointed
			// wherever it said. The next entry underneath it was then
			// written through it, outside, as root.
			if path.IsAbs(header.Linkname) || filepath.IsAbs(header.Linkname) {
				return written, fmt.Errorf(
					"reassemble: symlink %s points outside the archive", header.Name)
			}
			if _, err := safeName(path.Join(path.Dir(name), header.Linkname)); err != nil {
				return written, fmt.Errorf(
					"reassemble: symlink %s points outside the archive", header.Name)
			}
			if parent := path.Dir(name); parent != "." {
				if err := root.MkdirAll(parent, 0o700); err != nil {
					return written, fmt.Errorf("reassemble: create %s: %w", parent, err)
				}
			}
			if err := root.Symlink(header.Linkname, name); err != nil && !os.IsExist(err) {
				return written, fmt.Errorf("reassemble: symlink %s: %w", header.Name, err)
			}
		default:
			// Devices, fifos and hard links are not part of an account
			// payload and are skipped rather than reproduced.
		}
	}
}

// createTar packs dir into an uncompressed archive.
//
// Uncompressed on purpose: the result is handed straight to restorepkg,
// and compressing it would only cost time.
func createTar(dir, archivePath string) error {
	out, err := os.Create(archivePath)
	if err != nil {
		return fmt.Errorf("reassemble: create %s: %w", archivePath, err)
	}
	defer out.Close()

	writer := tar.NewWriter(out)
	root := filepath.Clean(dir)

	// A Maildir is a directory of hard links: Dovecot moves a message
	// between folders by linking it under the new name and unlinking the
	// old, and restic puts those links back as links. Packing each name
	// as a copy of its own makes an archive as large as the copies, and
	// an account restored from it larger than the account backed up --
	// against a reservation that counted the content once, so a repack
	// can run the disk out halfway through as well. The first name
	// carries the body and the rest link to it, which is what tar does.
	type inode struct{ device, number uint64 }
	linked := map[inode]string{}

	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}

		var link string
		if info.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(path); err != nil {
				return fmt.Errorf("reassemble: read symlink %s: %w", path, err)
			}
		}
		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return fmt.Errorf("reassemble: header for %s: %w", path, err)
		}
		header.Name = filepath.ToSlash(relative)
		if info.IsDir() {
			header.Name += "/"
		}
		if info.Mode().IsRegular() {
			if stat, ok := info.Sys().(*syscall.Stat_t); ok && uint64(stat.Nlink) > 1 {
				key := inode{uint64(stat.Dev), uint64(stat.Ino)}
				if first, seen := linked[key]; seen {
					header.Typeflag = tar.TypeLink
					header.Linkname = first
					header.Size = 0
				} else {
					linked[key] = header.Name
				}
			}
		}
		if err := writer.WriteHeader(header); err != nil {
			return fmt.Errorf("reassemble: write header %s: %w", header.Name, err)
		}
		if !info.Mode().IsRegular() || header.Typeflag == tar.TypeLink {
			return nil
		}

		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("reassemble: open %s: %w", path, err)
		}
		_, copyErr := io.Copy(writer, file)
		closeErr := file.Close()
		if copyErr != nil {
			return fmt.Errorf("reassemble: copy %s: %w", path, copyErr)
		}
		return closeErr
	})
	if walkErr != nil {
		return walkErr
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("reassemble: finish %s: %w", archivePath, err)
	}
	return nil
}

// safeJoin resolves name inside root and refuses anything that escapes it.
// safeName is where an entry may be written, relative to the extraction
// directory, or an error saying why it may not be written at all. An empty
// name is the directory itself.
//
// os.Root refuses an escape whatever this returns; this is what turns that
// refusal into a message naming the archive entry responsible, and what
// keeps a link target from being judged by its spelling alone.
func safeName(name string) (string, error) {
	clean := path.Clean(strings.TrimPrefix(filepath.ToSlash(name), "./"))
	if clean == "." || clean == "/" {
		return "", nil
	}
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("reassemble: archive entry %q escapes the extraction directory", name)
	}
	return clean, nil
}

// entryMode keeps an archive's permission bits within sane bounds.
//
// Only the permission bits are taken, so setuid, setgid and sticky never
// survive extraction, and the owner always keeps enough access to read the
// tree back when it is repacked.
func entryMode(header *tar.Header, ownerBits os.FileMode) os.FileMode {
	return os.FileMode(header.Mode).Perm() | ownerBits
}
