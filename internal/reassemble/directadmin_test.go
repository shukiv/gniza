package reassemble

import (
	"testing"
)

func TestDirectAdminZstdSnapshotsAreMonolithic(t *testing.T) {
	archive := "/staging/studio/user.admin.studio.tar.zst"
	parts, err := Classify([]string{archive})
	if err != nil || parts.Archive != archive || parts.Homedir != "" {
		t.Fatalf("wrong classification: %+v %v", parts, err)
	}
	for _, paths := range [][]string{
		{archive, "/staging/victim.tar.zst"},
		{archive, "/staging/databases"},
		{archive, "/staging/metadata"},
		{archive, "/home/studio"},
	} {
		if _, err := Classify(paths); err == nil {
			t.Fatalf("ambiguous snapshot accepted: %v", paths)
		}
	}
}
