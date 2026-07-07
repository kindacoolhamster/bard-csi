package cephplugin

import (
	"context"
	"testing"
)

// setOwnerMetadata labels the image with the owning PVC/PV from the provisioner's
// --extra-create-metadata, and is a no-op without those parameters.
func TestSetOwnerMetadata(t *testing.T) {
	run := &modifyRunner{}
	b := newModifyBackend(run)
	b.setOwnerMetadata(context.Background(), nil, "replicapool/img", map[string]string{
		"csi.storage.k8s.io/pvc/name":      "data",
		"csi.storage.k8s.io/pvc/namespace": "team-a",
		"csi.storage.k8s.io/pv/name":       "pvc-123",
	})
	for _, want := range [][]string{
		{"image-meta", "set", "replicapool/img", "bard.pvcname", "data"},
		{"image-meta", "set", "replicapool/img", "bard.pvcnamespace", "team-a"},
		{"image-meta", "set", "replicapool/img", "bard.pvname", "pvc-123"},
	} {
		if !run.ran(want...) {
			t.Fatalf("expected image-meta %v; calls: %v", want, run.calls)
		}
	}

	none := &modifyRunner{}
	newModifyBackend(none).setOwnerMetadata(context.Background(), nil, "replicapool/img", nil)
	if none.ran("image-meta", "set") {
		t.Fatalf("no owner labels should be set without metadata params; calls: %v", none.calls)
	}
}
