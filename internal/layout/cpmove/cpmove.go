// Package cpmove is cPanel's account archive, as Gniza has to read and
// rebuild it.
//
// The names here are cPanel's, not ours. They have been stable for a long
// time and were checked against what /scripts/pkgacct produces on cPanel
// 136 -- but they are still somebody else's format, so reassembly
// discovers the archive's own top-level directory rather than assuming it,
// and refuses a tree that does not look like a cpmove one.
//
// It imports nothing else of Gniza's on purpose: a layout describes a
// format, and a format cannot depend on the machinery that reads it.
package cpmove

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	// HomedirDir and DatabaseDir are where a cpmove tree keeps the home
	// directory and one dump per database.
	HomedirDir  = "homedir"
	DatabaseDir = "mysql"
	// GrantsFile is where a cpmove archive keeps the account's database
	// users and their grants: at the top of the tree, not with the
	// databases. Checked against what /scripts/pkgacct produces on cPanel
	// 136.
	GrantsFile = "mysql.sql"
	// authSuffix names the file cPanel keeps beside the grants, holding
	// each user's real password hash and authentication plugin.
	authSuffix = "-auth.json"
	// StagedGrantsFile and StagedRunnableFile are what Gniza names the
	// same things where it dumps them, beside the databases. They are
	// granular's constants spelt out rather than imported, because a
	// format package that imported the restore machinery could not be
	// used by it -- and a test holds the two spellings together.
	StagedGrantsFile   = "_users.sql"
	StagedRunnableFile = "_users-runnable.sql"
)

// Layout answers where cPanel keeps the parts of an account.
type Layout struct{}

// Panel names the panel this layout belongs to.
func (Layout) Panel() string { return "cpanel" }

// HomedirDir is homedir/, inside the account's own directory.
func (Layout) HomedirDir() string { return HomedirDir }

// DatabaseDir is mysql/, one file per database.
func (Layout) DatabaseDir() string { return DatabaseDir }

// AccountRoot returns the one directory inside an extracted tree, which
// for a cpmove archive is the account itself.
//
// The name is cPanel's to choose -- cpmove-fred, or fred -- so it is
// discovered rather than assumed, and a tree holding anything but one
// account directory is refused.
func (Layout) AccountRoot(treeDir, account string) (string, error) {
	entries, err := os.ReadDir(treeDir)
	if err != nil {
		return "", fmt.Errorf("cpmove: read %s: %w", treeDir, err)
	}
	var dirs []string
	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, filepath.Join(treeDir, entry.Name()))
		}
	}
	if len(dirs) != 1 {
		return "", fmt.Errorf("cpmove: found %d top-level directories, expected one", len(dirs))
	}
	return dirs[0], nil
}

// PlaceDatabaseUsers moves the grants file to the name cPanel's own
// restore reads.
//
// Gniza dumps the account's database users beside its databases, because
// that is where they are produced. A cpmove archive keeps them somewhere
// else: one file per database under mysql/, and the users and their grants
// in mysql.sql at the top of the tree. restorepkg reads the latter and
// nothing else, so an archive with the file under mysql/ was restored with
// every table in place and no user able to read them.
func (Layout) PlaceDatabaseUsers(root string) error {
	from := filepath.Join(root, DatabaseDir, StagedGrantsFile)
	if _, err := os.Stat(from); err != nil {
		// No users file: an account with no databases, or a backup taken
		// before Gniza dumped them.
		return nil
	}
	to := filepath.Join(root, GrantsFile)
	if err := os.Rename(from, to); err != nil {
		return fmt.Errorf("cpmove: place the database users where restorepkg reads them: %w", err)
	}
	// The authentication file travels with it. "IDENTIFIED BY PASSWORD"
	// is not valid on MySQL 8, so this is where the real hash and plugin
	// are read from, and a grants file without it restores users that
	// cannot authenticate.
	if err := os.Rename(from+authSuffix, to+authSuffix); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("cpmove: place the database authentication: %w", err)
	}
	// The readable copy of the same users is staged for whoever pulls
	// them out of a backup by hand. It has no business in an archive
	// handed to restorepkg, which would find a file it does not know in a
	// directory where it expects one file per database.
	runnable := filepath.Join(root, DatabaseDir, StagedRunnableFile)
	if err := os.Remove(runnable); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("cpmove: %w", err)
	}
	return nil
}

// ValidateArchive binds cPanel's embedded identity to the requested
// account. A restic tag and an archive filename are not authoritative:
// both can say customer1 while restorepkg reads cp/victim from inside the
// archive. Check the entire member list, including duplicate identity
// records, without extracting it. The caller must keep the archive in
// root-owned staging.
func (Layout) ValidateArchive(ctx context.Context, filename, account string) error {
	if account == "" || strings.ContainsAny(account, "/\\.\x00") {
		return fmt.Errorf("cpmove: invalid expected account %q", account)
	}
	base := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(filename), ".gz"), ".tar")
	if base != "cpmove-"+account && base != account {
		return fmt.Errorf("cpmove: archive filename does not belong to %s", account)
	}
	f, err := os.OpenFile(filename, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("cpmove: account archive is not a regular file")
	}
	var reader io.Reader = f
	if strings.HasSuffix(filename, ".gz") {
		z, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("cpmove: read compressed account archive: %w", err)
		}
		defer z.Close()
		reader = z
	}
	tr := tar.NewReader(reader)
	root, identity := "", false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("cpmove: read account archive: %w", err)
		}
		name := strings.TrimPrefix(h.Name, "./")
		if name == "." && h.Typeflag == tar.TypeDir {
			continue
		}
		for _, component := range strings.Split(name, "/") {
			if component == ".." {
				return fmt.Errorf("cpmove: unsafe account archive member %q", h.Name)
			}
		}
		name = path.Clean(name)
		top, within, _ := strings.Cut(name, "/")
		if top != "cpmove-"+account && top != account {
			return fmt.Errorf("cpmove: archive member %q does not belong to %s", h.Name, account)
		}
		if root != "" && root != top {
			return fmt.Errorf("cpmove: account archive has multiple roots")
		}
		root = top
		if (within == "" || within == "cp" || within == "meta") && h.Typeflag != tar.TypeDir {
			return fmt.Errorf("cpmove: account identity directory is not a directory: %s", h.Name)
		}
		if strings.HasPrefix(within, "cp/") || within == "meta/user" {
			if within != "cp/"+account && within != "meta/user" {
				return fmt.Errorf("cpmove: archive contains a different account record: %s", h.Name)
			}
			if h.Typeflag != tar.TypeReg || h.Size > 1<<20 {
				return fmt.Errorf("cpmove: invalid account identity record %s", h.Name)
			}
			body, err := io.ReadAll(tr)
			if err != nil {
				return err
			}
			if within == "meta/user" && strings.TrimSpace(string(body)) != account {
				return fmt.Errorf("cpmove: archive's account identity is not %s", account)
			}
			if within == "cp/"+account {
				for _, line := range strings.Split(string(body), "\n") {
					if value, ok := strings.CutPrefix(line, "USER="); ok && strings.TrimSpace(value) != account {
						return fmt.Errorf("cpmove: archive's USER field is not %s", account)
					}
				}
			}
			identity = true
		}
	}
	if !identity {
		return fmt.Errorf("cpmove: archive has no identity record for %s", account)
	}
	return nil
}
