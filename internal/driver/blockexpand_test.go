package driver

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kindacoolhamster/bard-csi/internal/backend"
)

// blockCap/mountCap come from accessmode_test.go.
const rwo = csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER

// A raw block volume has no filesystem to grow: ControllerExpandVolume must not
// request node expansion for it (kubelet would call NodeExpandVolume, which has
// nothing to do and previously ran resize2fs against nonsense, wedging the PVC).
func TestControllerExpandBlockSkipsNodeExpansion(t *testing.T) {
	cs, id := controllerWith(t, &fakeBackend{
		expand: func() (int64, bool, error) { return 2 << 30, true, nil }, // backend asks for node expansion
	})
	req := &csi.ControllerExpandVolumeRequest{
		VolumeId:      id,
		CapacityRange: &csi.CapacityRange{RequiredBytes: 2 << 30},
	}

	req.VolumeCapability = blockCap(rwo)
	resp, err := cs.ControllerExpandVolume(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetNodeExpansionRequired() {
		t.Fatal("block volume expansion must not require node expansion")
	}

	// A mount volume keeps the backend's verdict.
	req.VolumeCapability = mountCap(rwo)
	resp, err = cs.ControllerExpandVolume(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.GetNodeExpansionRequired() {
		t.Fatal("mount volume expansion must keep the backend's node-expansion verdict")
	}
}

// NodeExpandVolume for a block volume is a successful no-op, before any backend
// resolution (an older CO may call it even though the controller said not to).
func TestNodeExpandBlockIsNoOp(t *testing.T) {
	// Empty registry: succeeding proves the block no-op returns before resolve.
	d := New(Options{Registry: backend.NewRegistry(), Dispatch: mustDisp(t)})
	ns := &nodeServer{driver: d}
	req := &csi.NodeExpandVolumeRequest{
		VolumeId:   "swsk|1|ceph-rbd|east|replicapool|csi-vol-abc",
		VolumePath: "/var/lib/kubelet/some/device",
	}

	req.VolumeCapability = blockCap(rwo)
	if _, err := ns.NodeExpandVolume(context.Background(), req); err != nil {
		t.Fatalf("block node expansion must be a no-op success, got %v", err)
	}

	// Without the block capability the same request resolves (and here fails
	// NotFound on the empty registry) -- the no-op is block-scoped.
	req.VolumeCapability = mountCap(rwo)
	_, err := ns.NodeExpandVolume(context.Background(), req)
	if status.Code(err) != codes.NotFound {
		t.Fatalf("mount node expansion must resolve the backend, got %v", err)
	}
}
