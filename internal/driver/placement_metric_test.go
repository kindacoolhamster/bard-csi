package driver

import (
	"context"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/kindacoolhamster/bard-csi/internal/backend"
	"github.com/kindacoolhamster/bard-csi/internal/dispatch"
	"github.com/kindacoolhamster/bard-csi/internal/metrics"
)

// placementServer builds a controllerServer with a dispatcher that can actually
// resolve ceph-rbd, so CreateVolume gets past dispatch and into the paths that
// follow it.
func placementServer(t *testing.T, fb *fakeBackend) *controllerServer {
	t.Helper()
	reg := backend.NewRegistry()
	reg.Register(fb)
	disp, err := dispatch.New(dispatch.Config{
		Instances: map[string]map[string]string{"ceph-rbd": {"east": "zone-a"}},
		Defaults:  map[string]string{"ceph-rbd": "east"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &controllerServer{driver: New(Options{Registry: reg, Dispatch: disp})}
}

func exposition(t *testing.T) string {
	t.Helper()
	var sb strings.Builder
	metrics.WriteTo(&sb)
	return sb.String()
}

// A CreateVolume that fails AFTER dispatch chose an instance must still be
// counted. Validation failures between Resolve and the backend call (here a
// malformed content source) used to record nothing at all, so the placement
// panel under-reported exactly the failures it exists to surface.
func TestPlacementRecordedOnPostDispatchFailure(t *testing.T) {
	cs := placementServer(t, &fakeBackend{})

	_, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-post-dispatch",
		Parameters:         map[string]string{dispatch.BackendParamKey: "ceph-rbd"},
		VolumeCapabilities: []*csi.VolumeCapability{mountCap(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)},
		VolumeContentSource: &csi.VolumeContentSource{
			Type: &csi.VolumeContentSource_Snapshot{
				Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: "not-a-valid-handle"},
			},
		},
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound for a malformed source snapshot, got %v", err)
	}

	want := `bard_csi_volume_placement_attempts_total{backend="ceph-rbd",instance="east",zone="zone-a",decision="default",result="error"}`
	if out := exposition(t); !strings.Contains(out, want) {
		t.Fatalf("a post-dispatch failure recorded no placement attempt\nwant a series like: %s\n---\n%s", want, out)
	}
}

// A request that never gets past dispatch is recorded as unresolved, and the
// backend label is bounded: an unknown type from a StorageClass must not become
// a permanent new series.
func TestPlacementDispatchErrorBoundsBackendLabel(t *testing.T) {
	cs := placementServer(t, &fakeBackend{})

	_, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-typo",
		Parameters:         map[string]string{dispatch.BackendParamKey: "ceph-rbdd"}, // typo
		VolumeCapabilities: []*csi.VolumeCapability{mountCap(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for an unknown backend, got %v", err)
	}

	out := exposition(t)
	want := `bard_csi_volume_placement_attempts_total{backend="unknown",instance="none",zone="none",decision="unresolved",result="dispatch_error"}`
	if !strings.Contains(out, want) {
		t.Fatalf("dispatch error not recorded under the bounded label\nwant: %s\n---\n%s", want, out)
	}
	if strings.Contains(out, `backend="ceph-rbdd"`) {
		t.Fatal("the misspelled StorageClass backend leaked into a metric label -- cardinality is caller-controlled")
	}
}
