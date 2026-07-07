package driver

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

// NodeGetInfo reports MaxVolumesPerNode only when a positive limit is configured
// (e.g. for the rbd-nbd device cap); 0 means unlimited and the field stays zero.
func TestNodeGetInfoMaxVolumes(t *testing.T) {
	for _, tc := range []struct {
		limit int64
		want  int64
	}{
		{0, 0},
		{16, 16},
	} {
		d := New(Options{NodeID: "node-a", MaxVolumes: tc.limit})
		ns := &nodeServer{driver: d}
		resp, err := ns.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if resp.GetNodeId() != "node-a" {
			t.Fatalf("node id = %q", resp.GetNodeId())
		}
		if resp.GetMaxVolumesPerNode() != tc.want {
			t.Fatalf("limit %d: MaxVolumesPerNode = %d, want %d", tc.limit, resp.GetMaxVolumesPerNode(), tc.want)
		}
	}
}
