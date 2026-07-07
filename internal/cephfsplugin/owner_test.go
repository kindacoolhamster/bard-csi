package cephfsplugin

import (
	"context"
	"testing"
)

// setOwnerMetadata labels the subvolume with the owning PVC from the provisioner's
// --extra-create-metadata, and is a no-op without those parameters.
func TestCephFSSetOwnerMetadata(t *testing.T) {
	b := snapBackend()
	run := b.run.(*cephRunner)
	b.setOwnerMetadata(context.Background(), nil, "cephfs", "csi", "bard-x", map[string]string{
		"csi.storage.k8s.io/pvc/name":      "data",
		"csi.storage.k8s.io/pvc/namespace": "team-a",
	})
	if !run.ran("fs", "subvolume", "metadata", "set", "cephfs", "bard-x", "bard.pvcname", "data") {
		t.Fatalf("expected subvolume metadata set for the pvc name; calls: %v", run.calls)
	}
	if !run.ran("fs", "subvolume", "metadata", "set", "cephfs", "bard-x", "bard.pvcnamespace", "team-a") {
		t.Fatalf("expected subvolume metadata set for the namespace; calls: %v", run.calls)
	}

	b2 := snapBackend()
	b2.setOwnerMetadata(context.Background(), nil, "cephfs", "csi", "bard-x", nil)
	if b2.run.(*cephRunner).ran("metadata", "set") {
		t.Fatalf("no labels without metadata params; calls: %v", b2.run.(*cephRunner).calls)
	}
}
