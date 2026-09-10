package update

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// TopDir is the one directory a release tarball may contain, and where
// install.sh is found afterwards.
// It keeps the name from before the rename to Gniza, for the reason given
// at update.TarballName: an agent from an older release looks for exactly
// this directory in the tarball it downloaded.
const TopDir = "cprest-plugin"

// What a release tarball may hold. It is a plugin of a dozen files and a
// static binary, so these are bounds on what is obviously wrong rather
// than a fit to what is in there today.
const (
	maxUnpacked = 256 << 20
	maxEntries  = 2000
)

// Unpack writes the contents of a verified release tarball into dir.
//
// Only ordinary files and directories under the package's own top
// directory are written.
// A tar can name anything -- a path climbing out of the directory, a
// symlink into /etc, a device node -- and this one has been checked
// against a signature but is still an archive that arrived over a
// network, so every entry is judged on its own.
//
// The caller has already checked the signature and the checksum. Nothing
// here treats the archive as trusted; it only makes it a directory.
func Unpack(tarball, dir string, want Package) error {
	file, err := os.Open(tarball)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	unzipped, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("update: read %s: %w", filepath.Base(tarball), err)
	}
	defer func() { _ = unzipped.Close() }()

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	var written, entries int64
	archive := tar.NewReader(unzipped)
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("update: read the release: %w", err)
		}
		entries++
		if entries > maxEntries {
			return fmt.Errorf("update: the release has more than %d files in it", maxEntries)
		}
		name, err := safeName(header.Name, want.TopDir)
		if err != nil {
			return err
		}
		if name == "" {
			continue
		}
		target := filepath.Join(dir, filepath.FromSlash(name))

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			// Only root reads any of this, and only install.sh and the
			// binary are ever run -- by name, by a shell this server
			// starts. Nothing needs a mode of its own out of the archive.
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
			if err != nil {
				return err
			}
			copied, err := io.Copy(out, io.LimitReader(archive, maxUnpacked-written+1))
			if closeErr := out.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				return err
			}
			written += copied
			if written > maxUnpacked {
				return fmt.Errorf("update: the release unpacks to more than %d bytes", int64(maxUnpacked))
			}
		default:
			// Symlinks, hard links, devices, fifos: a plugin needs none of
			// them, and each is a way for an archive to write somewhere
			// this is not.
			return fmt.Errorf("update: %s is not an ordinary file or directory", header.Name)
		}
	}

	installer := filepath.Join(dir, want.TopDir, "install.sh")
	if _, err := os.Stat(installer); err != nil {
		return fmt.Errorf("update: that release has no %s/install.sh in it", want.TopDir)
	}
	return nil
}

// safeName is the path an entry may be written to, or an error saying why
// it may not be written at all. An empty name is the top directory itself.
func safeName(name, top string) (string, error) {
	clean := path.Clean(strings.TrimPrefix(strings.ReplaceAll(name, `\`, "/"), "./"))
	if clean == "." || clean == top {
		return "", nil
	}
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("update: the release names a path outside itself: %s", name)
	}
	if !strings.HasPrefix(clean, top+"/") {
		return "", fmt.Errorf("update: the release holds %s, which is not under %s/", name, top)
	}
	return clean, nil
}
