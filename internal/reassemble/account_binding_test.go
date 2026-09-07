package reassemble

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/shukiv/gniza/internal/layout/cpmove"
)

func TestAccountArchiveCannotContradictTheSnapshotTag(t *testing.T) {
	for _, monolithic := range []bool{false, true} {
		for _, member := range []string{"cpmove-victim/cp/victim", "cpmove-customer1/cp/victim", "cpmove-customer1/meta/user"} {
			t.Run(member+map[bool]string{false: "/split", true: "/monolithic"}[monolithic], func(t *testing.T) {
				r, root := buildSplitSnapshot(t)
				metadata := r.source[r.snapshot.Paths[0]]
				archive := filepath.Join(metadata, "cpmove-customer1.tar")
				writeTestTar(t, archive, map[string]string{member: "victim\n"})
				if monolithic {
					r.snapshot.Paths = []string{"/stage/metadata/cpmove-customer1.tar"}
					r.source = map[string]string{"/stage/metadata": metadata}
				}
				_, err := Run(context.Background(), r, Request{
					Layout:  cpmove.Layout{},
					Account: "customer1", SnapshotID: r.snapshot.ID, WorkDir: filepath.Join(root, "work"),
				})
				if err == nil {
					t.Fatal("accepted another account's archive under customer1's snapshot tag")
				}
			})
		}
	}
}
