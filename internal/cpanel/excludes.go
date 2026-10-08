package cpanel

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// This file reads the exclusions cPanel's own backups obey.
//
// An operator who has written a path into cpbackup-exclude.conf has said
// they do not want it leaving the server. Gniza ignoring that file meant
// the very files they excluded were the ones being uploaded to a remote
// destination -- and the operator had no way to know, because the file
// they wrote it in is the one cPanel documents for exactly this.

// ExcludeConfName is the file an account keeps its own exclusions in, in
// its home directory. cPanel reads the same name.
const ExcludeConfName = "cpbackup-exclude.conf"

// serverExcludeConf is where the server-wide list lives.
var serverExcludeConf = "/etc/cpbackup-exclude.conf"

// gnizaSkips is what Gniza leaves out of every account's home on top of
// cPanel's lists, whether or not anybody wrote an exclusion file: the
// archives Installatron, Softaculous and WP Toolkit write under the home
// on every automatic update. Each is a fresh compression of a site and a
// dump of its database, which shares almost no chunk with the night
// before, so it is gigabytes of ingest every night for a backup of what
// the snapshot already holds directly. On one DirectAdmin server one
// site's tool wrote 27 GiB of them in two weeks. The DirectAdmin provider
// has carried the same three names since ADR 0021; the operator asked
// for application_backups to be left out everywhere, by default.
var gnizaSkips = []string{"application_backups", "softaculous_backups", "wordpress-backups"}

// NativeExcludes is what cPanel would leave out of a backup of this
// account: the server-wide list, plus the account's own, plus gnizaSkips.
//
// The patterns are returned as restic exclude patterns anchored under the
// account's home directory. cPanel matches them anywhere below the home,
// and anchoring keeps them from also matching inside the staged metadata
// and database dumps, which are this program's own files rather than the
// account's.
func (r *Real) NativeExcludes(home string) []string {
	if home == "" {
		return nil
	}
	// The account's own list is a file the account writes, read by a
	// process running as root. Whose it must be is therefore whoever owns
	// the home directory it is in -- read from the directory rather than
	// assumed, because that is the only thing here that says which
	// customer this is.
	owner, ownerKnown := ownerOf(home)

	seen := map[string]bool{}
	var excludes []string
	for _, name := range gnizaSkips {
		mapped := filepath.Join(home, name)
		seen[mapped] = true
		excludes = append(excludes, mapped)
	}
	for _, source := range []excludeFile{
		{path: r.serverExcludes()},
		{path: filepath.Join(home, ExcludeConfName), owner: owner, ownerKnown: ownerKnown},
	} {
		for _, pattern := range readExcludeConf(source) {
			for _, mapped := range anchorExclude(home, pattern) {
				if !seen[mapped] {
					seen[mapped] = true
					excludes = append(excludes, mapped)
				}
			}
		}
	}
	return excludes
}

func (r *Real) serverExcludes() string {
	if r.ServerExcludeConf != "" {
		return r.ServerExcludeConf
	}
	return serverExcludeConf
}

// excludeFile is one list of exclusions and who is allowed to have
// written it. The server-wide list has no owner named: it lives in /etc
// and only root can write there.
type excludeFile struct {
	path       string
	owner      uint32
	ownerKnown bool
}

// What one of these files may be. cPanel's own is a few dozen lines; a
// file larger than this is not somebody's exclusion list.
const (
	maxExcludeBytes = 256 << 10
	maxExcludeLines = 2000
)

// readExcludeConf reads one file's worth of patterns.
//
// A missing file is not an error: most servers have no per-account list,
// and an account that has never written one has excluded nothing.
//
// The account's file is the one thing this program reads from a place its
// own customers can write, as root, so it is opened the way anything from
// there has to be. Not through a symlink, because a link to /root/.my.cnf
// would put cPanel's MySQL password into the patterns this returns, and
// from there into a command line. Not blocking, because a named pipe with
// nobody at the other end would stop the open for ever, and the backup
// engine works through accounts one at a time: one customer could hold up
// every backup on the server. Only an ordinary file, only from the account
// that owns the home directory, and only so much of it.
func readExcludeConf(source excludeFile) []string {
	file, err := os.OpenFile(source.path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		// A directory, a device, a pipe: whatever it is, it is not the
		// list of exclusions this is here to read.
		return nil
	}
	if source.ownerKnown {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || (stat.Uid != source.owner && stat.Uid != 0) {
			// A hard link to somebody else's file, put where this one
			// belongs. What it says is not this account's to say.
			return nil
		}
	}

	var patterns []string
	scanner := bufio.NewScanner(io.LimitReader(file, maxExcludeBytes))
	for scanner.Scan() {
		if len(patterns) >= maxExcludeLines {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	return patterns
}

// ownerOf is who a directory belongs to.
func ownerOf(dir string) (uint32, bool) {
	info, err := os.Stat(dir)
	if err != nil {
		return 0, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return stat.Uid, true
}

// anchorExclude turns one cPanel pattern into restic patterns under the
// account's home.
//
// cPanel matches a bare name at any depth below the home, so that is what
// this produces: the name directly in the home, and the same name
// anywhere below it. A pattern that already begins with a slash is
// relative to the home and matches only there.
func anchorExclude(home, pattern string) []string {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return nil
	}
	if strings.HasPrefix(pattern, "/") {
		return []string{filepath.Join(home, pattern)}
	}
	// Leading "*/" is cPanel's way of saying "below the top", which is
	// already what matching at any depth means here.
	trimmed := strings.TrimPrefix(pattern, "*/")
	if trimmed == "" {
		return nil
	}
	return []string{
		filepath.Join(home, trimmed),
		filepath.Join(home, "**", trimmed),
	}
}
