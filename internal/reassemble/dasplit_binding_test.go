package reassemble

import (
	"path/filepath"
	"testing"

	"github.com/shukiv/gniza/internal/layout/dabackup"
)

// A DirectAdmin account archive is taken apart into two directories, and
// what a restore reads back out of a snapshot is those two paths and the
// roles Classify reads them as. The names are dabackup's; the rule that
// decides what a name means is here. They have to agree, and nothing
// about the archive says so: a metadata part named after the directory
// inside the archive would come back as a second home directory, and the
// restore would refuse the snapshot rather than rebuild it.
func TestTheDirectAdminPartsAreReadAsTheRolesTheyAre(t *testing.T) {
	staged := "/var/lib/gniza/staging/gzv0908a"
	metadata := dabackup.MetadataPart(staged)
	homedir := dabackup.HomedirPart(staged)

	found, err := Classify([]string{metadata, homedir})
	if err != nil {
		t.Fatalf("the parts of an unpacked DirectAdmin archive were not classified: %v", err)
	}
	if found.Metadata != metadata {
		t.Errorf("the metadata part was read as %q", found.Metadata)
	}
	if found.Homedir != homedir {
		t.Errorf("the home directory part was read as %q", found.Homedir)
	}
	if found.Archive != "" {
		t.Errorf("a split payload was read as carrying a whole-account archive: %q", found.Archive)
	}
	// And the manifest travels inside the metadata part, because restic
	// is handed the parts and nothing else. A tree restored without it
	// cannot be repacked into an archive DirectAdmin would restore.
	if manifest := filepath.Join(metadata, dabackup.ManifestFile); filepath.Dir(manifest) != found.Metadata {
		t.Errorf("the manifest at %s is not inside the metadata part", manifest)
	}
}
