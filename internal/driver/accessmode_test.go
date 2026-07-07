package driver

import (
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"

	"github.com/kindacoolhamster/bard-csi/internal/backend"
)

func mountCap(m csi.VolumeCapability_AccessMode_Mode) *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: m},
	}
}

func blockCap(m csi.VolumeCapability_AccessMode_Mode) *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: m},
	}
}

// A block-device backend (RBD) may serve a multi-node mode only as raw block; a
// shared-filesystem backend (cephfs/nfs) may serve it as a filesystem. Single-node
// modes are always fine.
func TestAccessModeSupported(t *testing.T) {
	const (
		rwo = csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER
		rwx = csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER
		rox = csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY
	)
	block := backend.Capabilities{BlockDevice: true} // rbd-like
	shared := backend.Capabilities{}                 // cephfs/nfs-like

	cases := []struct {
		name string
		cap  *csi.VolumeCapability
		caps backend.Capabilities
		want bool
	}{
		{"rwo filesystem on block backend", mountCap(rwo), block, true},
		{"rwx filesystem on block backend", mountCap(rwx), block, false}, // unsafe shared mount
		{"rox filesystem on block backend", mountCap(rox), block, false},
		{"rwx raw block on block backend", blockCap(rwx), block, true},
		{"rwx filesystem on shared-fs backend", mountCap(rwx), shared, true},
		{"rwo filesystem on shared-fs backend", mountCap(rwo), shared, true},
	}
	for _, c := range cases {
		if got := accessModeSupported(c.cap, c.caps); got != c.want {
			t.Errorf("%s: accessModeSupported = %v, want %v", c.name, got, c.want)
		}
	}
}

// readOnlyAccess must be true exactly for the read-only access modes, so core maps
// such volumes read-only at NodeStage (RBD --read-only).
func TestReadOnlyAccess(t *testing.T) {
	ro := []csi.VolumeCapability_AccessMode_Mode{
		csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
	}
	rw := []csi.VolumeCapability_AccessMode_Mode{
		csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER,
		csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER,
		csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
	}
	for _, m := range ro {
		if !readOnlyAccess(mountCap(m)) || !readOnlyAccess(blockCap(m)) {
			t.Errorf("%v must be read-only", m)
		}
	}
	for _, m := range rw {
		if readOnlyAccess(mountCap(m)) || readOnlyAccess(blockCap(m)) {
			t.Errorf("%v must not be read-only", m)
		}
	}
}
