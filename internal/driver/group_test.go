package driver

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kindacoolhamster/bard-csi/internal/backend"
	"github.com/kindacoolhamster/bard-csi/internal/volumeid"
)

func groupServerWith(t *testing.T, fb *fakeBackend) *groupControllerServer {
	t.Helper()
	reg := backend.NewRegistry()
	reg.Register(fb)
	return &groupControllerServer{driver: New(Options{Registry: reg, Dispatch: mustDisp(t)})}
}

func vid(instance, name string) string {
	h := volumeid.Handle{Backend: "ceph-rbd", Instance: instance, Location: "replicapool", Name: name}
	return h.String()
}

// A group snapshot of N volumes produces N members, each an individual snapshot
// tagged with the same group id; members reuse the per-backend CreateSnapshot.
func TestCreateVolumeGroupSnapshot(t *testing.T) {
	gs := groupServerWith(t, &fakeBackend{})
	// Two source volumes -- note they can be on different instances/clusters.
	srcs := []string{vid("east", "vol-a"), vid("west", "vol-b")}
	resp, err := gs.CreateVolumeGroupSnapshot(context.Background(), &csi.CreateVolumeGroupSnapshotRequest{
		Name: "grp1", SourceVolumeIds: srcs,
	})
	if err != nil {
		t.Fatal(err)
	}
	g := resp.GetGroupSnapshot()
	if g.GetGroupSnapshotId() != groupSnapshotID("grp1") {
		t.Fatalf("group id = %q", g.GetGroupSnapshotId())
	}
	if len(g.GetSnapshots()) != 2 {
		t.Fatalf("want 2 members, got %d", len(g.GetSnapshots()))
	}
	seen := map[string]bool{}
	for _, s := range g.GetSnapshots() {
		if s.GetGroupSnapshotId() != g.GetGroupSnapshotId() {
			t.Errorf("member not tagged with the group id: %q", s.GetGroupSnapshotId())
		}
		if !s.GetReadyToUse() {
			t.Errorf("member not ready: %s", s.GetSnapshotId())
		}
		seen[s.GetSnapshotId()] = true
	}
	if len(seen) != 2 {
		t.Fatalf("members must be distinct, got %v", seen)
	}

	// Idempotent: a retry yields the same member ids (CreateSnapshot is keyed on
	// a deterministic per-member name).
	resp2, _ := gs.CreateVolumeGroupSnapshot(context.Background(), &csi.CreateVolumeGroupSnapshotRequest{Name: "grp1", SourceVolumeIds: srcs})
	for _, s := range resp2.GetGroupSnapshot().GetSnapshots() {
		if !seen[s.GetSnapshotId()] {
			t.Fatalf("retry produced a new member id %q", s.GetSnapshotId())
		}
	}
}

// DeleteVolumeGroupSnapshot deletes each member snapshot it is given.
func TestDeleteVolumeGroupSnapshot(t *testing.T) {
	fb := &fakeBackend{}
	gs := groupServerWith(t, fb)
	create, err := gs.CreateVolumeGroupSnapshot(context.Background(), &csi.CreateVolumeGroupSnapshotRequest{
		Name: "grp1", SourceVolumeIds: []string{vid("east", "vol-a"), vid("east", "vol-b")},
	})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range create.GetGroupSnapshot().GetSnapshots() {
		ids = append(ids, s.GetSnapshotId())
	}
	if _, err := gs.DeleteVolumeGroupSnapshot(context.Background(), &csi.DeleteVolumeGroupSnapshotRequest{
		GroupSnapshotId: create.GetGroupSnapshot().GetGroupSnapshotId(), SnapshotIds: ids,
	}); err != nil {
		t.Fatal(err)
	}
	if len(fb.deletedSnaps) != 2 {
		t.Fatalf("expected 2 member deletes, got %v", fb.deletedSnaps)
	}
}

func TestGroupSnapshotBadInput(t *testing.T) {
	gs := groupServerWith(t, &fakeBackend{})
	ctx := context.Background()

	if _, err := gs.CreateVolumeGroupSnapshot(ctx, &csi.CreateVolumeGroupSnapshotRequest{SourceVolumeIds: []string{vid("east", "v")}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("create without name: want InvalidArgument, got %v", status.Code(err))
	}
	if _, err := gs.CreateVolumeGroupSnapshot(ctx, &csi.CreateVolumeGroupSnapshotRequest{Name: "g"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("create without sources: want InvalidArgument, got %v", status.Code(err))
	}
	if _, err := gs.GetVolumeGroupSnapshot(ctx, &csi.GetVolumeGroupSnapshotRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("get without id: want InvalidArgument, got %v", status.Code(err))
	}
	if _, err := gs.GetVolumeGroupSnapshot(ctx, &csi.GetVolumeGroupSnapshotRequest{GroupSnapshotId: "not-ours"}); status.Code(err) != codes.NotFound {
		t.Errorf("get unknown id: want NotFound, got %v", status.Code(err))
	}
	// Delete tolerates an unknown id (idempotent success).
	if _, err := gs.DeleteVolumeGroupSnapshot(ctx, &csi.DeleteVolumeGroupSnapshotRequest{GroupSnapshotId: "not-ours"}); err != nil {
		t.Errorf("delete unknown id should succeed, got %v", err)
	}
}
