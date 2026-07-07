package driver

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/csi-addons/spec/lib/go/identity"
	"github.com/csi-addons/spec/lib/go/replication"

	"github.com/kindacoolhamster/bard-csi/internal/backend"
	"github.com/kindacoolhamster/bard-csi/internal/volumeid"
)

func replWith(t *testing.T, fb *fakeBackend) (*replicationServer, string) {
	t.Helper()
	reg := backend.NewRegistry()
	reg.Register(fb)
	d := New(Options{Registry: reg, Dispatch: mustDisp(t), Mode: Mode{Controller: true}})
	h := volumeid.Handle{Backend: "ceph-rbd", Instance: "galileo", Location: "k8s-csi-test", Name: "csi-vol-abc"}
	if err := h.Validate(); err != nil {
		t.Fatal(err)
	}
	return &replicationServer{driver: d}, h.String()
}

// Each Replication RPC dispatches to the owning backend's VolumeReplicator, and
// GetVolumeReplicationInfo maps the backend's last-sync time into the response.
func TestReplicationDispatches(t *testing.T) {
	fb := &fakeBackend{replicate: func(string, volumeid.Handle) error { return nil }}
	rs, id := replWith(t, fb)
	ctx := context.Background()

	if _, err := rs.EnableVolumeReplication(ctx, &replication.EnableVolumeReplicationRequest{VolumeId: id}); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.PromoteVolume(ctx, &replication.PromoteVolumeRequest{VolumeId: id, Force: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.DemoteVolume(ctx, &replication.DemoteVolumeRequest{VolumeId: id}); err != nil {
		t.Fatal(err)
	}
	resyncResp, err := rs.ResyncVolume(ctx, &replication.ResyncVolumeRequest{VolumeId: id})
	if err != nil {
		t.Fatal(err)
	}
	if !resyncResp.GetReady() {
		t.Fatal("resync should report ready")
	}
	if _, err := rs.DisableVolumeReplication(ctx, &replication.DisableVolumeReplicationRequest{VolumeId: id}); err != nil {
		t.Fatal(err)
	}
	infoResp, err := rs.GetVolumeReplicationInfo(ctx, &replication.GetVolumeReplicationInfoRequest{VolumeId: id})
	if err != nil {
		t.Fatal(err)
	}
	if infoResp.GetLastSyncTime().GetSeconds() != 1700000200 {
		t.Fatalf("last sync time not mapped, got %v", infoResp.GetLastSyncTime())
	}

	want := []string{"enable", "promote", "demote", "resync", "disable", "info"}
	if len(fb.replicated) != len(want) {
		t.Fatalf("expected ops %v, got %v", want, fb.replicated)
	}
	for i, op := range want {
		if got := fb.replicated[i]; got[:len(op)] != op {
			t.Errorf("op %d: want %s, got %s", i, op, got)
		}
	}
}

// A backend without replication support is Unimplemented; the Identity advertises
// VolumeReplication only when a registered backend can replicate.
func TestReplicationCapabilityGating(t *testing.T) {
	hasCap := func(fb *fakeBackend) bool {
		reg := backend.NewRegistry()
		if fb != nil {
			reg.Register(fb)
		}
		d := New(Options{Registry: reg, Dispatch: mustDisp(t), Mode: Mode{Controller: true}})
		resp, err := (&csiAddonsIdentityServer{driver: d}).GetCapabilities(context.Background(), &identity.GetCapabilitiesRequest{})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range resp.GetCapabilities() {
			if c.GetVolumeReplication().GetType() == identity.Capability_VolumeReplication_VOLUME_REPLICATION {
				return true
			}
		}
		return false
	}
	if hasCap(&fakeBackend{}) {
		t.Fatal("a non-replicating backend must not advertise VolumeReplication")
	}
	if !hasCap(&fakeBackend{replicate: func(string, volumeid.Handle) error { return nil }}) {
		t.Fatal("a replicating backend must advertise VolumeReplication")
	}

	rs, id := replWith(t, &fakeBackend{}) // replicate nil -> cap false
	if _, err := rs.EnableVolumeReplication(context.Background(), &replication.EnableVolumeReplicationRequest{VolumeId: id}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("non-replicating backend should be Unimplemented, got %v", err)
	}
	if _, err := rs.EnableVolumeReplication(context.Background(), &replication.EnableVolumeReplicationRequest{VolumeId: ""}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty id should be InvalidArgument, got %v", err)
	}
}
