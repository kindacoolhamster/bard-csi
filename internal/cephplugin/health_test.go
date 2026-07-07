package cephplugin

import (
	"context"
	"testing"

	"github.com/kindacoolhamster/bard-csi/internal/fakerun"
	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// TestGetVolumeHealth proves the health probe reflects the backing rbd image:
// an existing image is healthy; once deleted it is reported abnormal.
func TestGetVolumeHealth(t *testing.T) {
	run := fakerun.New()
	b := New(map[string]ClusterConfig{"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin"}}, "", "", run)
	ctx := context.Background()

	create, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: "vol-health", CapacityBytes: 1 << 30, Instance: "east",
		Parameters: map[string]string{"pool": "replicapool"},
	})
	if err != nil {
		t.Fatal(err)
	}
	vol := bardplugin.VolumeRef{Instance: "east", Location: create.Location, Name: create.Name}

	health, err := b.GetVolumeHealth(ctx, &bardplugin.GetVolumeHealthRequest{Volume: vol})
	if err != nil {
		t.Fatal(err)
	}
	if health.Abnormal {
		t.Fatalf("existing image reported abnormal: %q", health.Message)
	}

	// Delete the image out from under the volume; health must flip to abnormal.
	if err := b.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{Volume: vol}); err != nil {
		t.Fatal(err)
	}
	health, err = b.GetVolumeHealth(ctx, &bardplugin.GetVolumeHealthRequest{Volume: vol})
	if err != nil {
		t.Fatal(err)
	}
	if !health.Abnormal {
		t.Fatalf("deleted image reported healthy: %q", health.Message)
	}
}

// The plugin must advertise VolumeHealth so Bard wires the /volume/health route.
func TestHealthReporterAdvertised(t *testing.T) {
	var _ bardplugin.HealthReporter = New(nil, "", "", fakerun.New())
}
