package nodestore

import (
	"errors"
	"fmt"
	"strings"
)

// legacyDirectories are the directories this program used when it was
// called cprest, and what the installer renames them to.
//
// The state file records absolute paths -- where staging is, where the
// restic cache is, and, for every SFTP destination, which private key and
// known-hosts file it connects with. Moving the directories on disk does
// not move what the state file says about them, and a destination naming
// a key under a directory that no longer exists cannot be reached at all:
// the backups are still there, but nothing on this server can read them.
var legacyDirectories = [][2]string{
	{"/etc/cprest", "/etc/gniza"},
	{"/var/lib/cprest", "/var/lib/gniza"},
	{"/var/cache/cprest", "/var/cache/gniza"},
	{"/var/run/cprest", "/var/run/gniza"},
	{"/run/cprest", "/run/gniza"},
}

// MigrateLegacyPaths rewrites stored paths that name the old directories,
// and reports how many records it changed.
//
// It is safe to run on every start: a server that was installed as Gniza
// has nothing that matches, and a server that has already been migrated
// has nothing left to match either.
func (s *Store) MigrateLegacyPaths() (int, error) {
	changed := 0

	// Only a settings record that was actually saved. Reading settings
	// returns the defaults when there is none, and writing those back
	// would freeze today's defaults into a server that was following
	// them.
	settings, err := s.rawSettings()
	switch {
	case err == nil:
		moved := settings
		moved.StagingRoot = underNewName(settings.StagingRoot)
		moved.ResticCache = underNewName(settings.ResticCache)
		moved.ConfigDir = underNewName(settings.ConfigDir)
		// An operator's own file rather than one Gniza made, and usually
		// put beside the keys that are: a certificate left pointing into
		// the old directory is handed to every restic run as a file that
		// is not there, so every backup to that destination fails after
		// the rename.
		moved.ResticCACert = underNewName(settings.ResticCACert)
		if moved.StagingRoot != settings.StagingRoot ||
			moved.ResticCache != settings.ResticCache ||
			moved.ConfigDir != settings.ConfigDir ||
			moved.ResticCACert != settings.ResticCACert {
			if err := s.SaveSettings(moved); err != nil {
				return changed, err
			}
			changed++
		}
	case errors.Is(err, ErrNotFound):
	default:
		return changed, err
	}

	destinations, err := s.Destinations()
	if err != nil {
		return changed, err
	}
	for _, dest := range destinations {
		// A new map rather than an edit in place: the destination that
		// gets written is a whole record, and one built beside the old
		// one cannot half-change it if writing fails.
		moved := make(map[string]string, len(dest.Config))
		differs := false
		for key, value := range dest.Config {
			moved[key] = underNewName(value)
			if moved[key] != value {
				differs = true
			}
		}
		if !differs {
			continue
		}
		dest.Config = moved
		if _, err := s.PutDestination(dest); err != nil {
			return changed, fmt.Errorf("nodestore: move destination %s to the new directory names: %w", dest.ID, err)
		}
		changed++
	}

	// The upgrade that installs this release is still in flight while the
	// directories move. The process that started it is stopped by the
	// installer, so the one that replaces it reads the installer's exit
	// status back off disk, at the path the old one wrote down -- and
	// that path moved with everything else. An upgrade whose status
	// cannot be found is one that reports itself as still installing
	// until it times out half an hour later, and no other upgrade can be
	// started while one says it is running.
	upgrade, err := s.UpgradeState()
	if err != nil {
		return changed, err
	}
	if moved := underNewName(upgrade.Dir); moved != upgrade.Dir {
		upgrade.Dir = moved
		if err := s.SaveUpgradeState(upgrade); err != nil {
			return changed, err
		}
		changed++
	}

	// Restore history says where an archive was left for the operator to
	// collect, which is what the page tells them to fetch. Those are
	// under the staging root, so they moved too.
	restores, err := s.Restores(0)
	if err != nil {
		return changed, err
	}
	for _, restore := range restores {
		target := underNewName(restore.TargetDir)
		archive := underNewName(restore.ArchivePath)
		to := underNewName(restore.RestoredTo)
		if target == restore.TargetDir && archive == restore.ArchivePath && to == restore.RestoredTo {
			continue
		}
		restore.TargetDir, restore.ArchivePath, restore.RestoredTo = target, archive, to
		if _, err := s.PutRestore(restore); err != nil {
			return changed, fmt.Errorf("nodestore: move restore %s to the new directory names: %w", restore.ID, err)
		}
		changed++
	}
	return changed, nil
}

// rawSettings reads the settings record as stored, so that "never saved"
// can be told apart from "saved, and happens to equal the defaults".
func (s *Store) rawSettings() (Settings, error) {
	var settings Settings
	if err := s.get(bucketSettings, settingsKey, &settings); err != nil {
		return Settings{}, err
	}
	return settings, nil
}

// underNewName is path with a renamed directory at the front of it, or
// path unchanged.
//
// The whole directory name has to match. "/var/lib/cprest" is a prefix of
// "/var/lib/cprest-archive", which is somewhere else entirely and was
// never moved; rewriting it would point a working destination at a
// directory that does not exist.
func underNewName(path string) string {
	for _, pair := range legacyDirectories {
		old, current := pair[0], pair[1]
		if path == old {
			return current
		}
		if strings.HasPrefix(path, old+"/") {
			return current + strings.TrimPrefix(path, old)
		}
	}
	return path
}
