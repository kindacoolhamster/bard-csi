package driver

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/kindacoolhamster/bard-csi/internal/backend"
	"github.com/kindacoolhamster/bard-csi/internal/volumeid"
)

func groupSnapWith(t *testing.T, fb *fakeBackend) *groupControllerServer {
	t.Helper()
	reg := backend.NewRegistry()
	reg.Register(fb)
	return &groupControllerServer{driver: New(Options{Registry: reg, Dispatch: mustDisp(t), Mode: Mode{Controller: true}})}
}

// gsHandle is a group snapshot id: the backing group and the snapshot within it.
func gsHandle(instance string) volumeid.Handle {
	return volumeid.Handle{Backend: "ceph-rbd", Instance: instance, Location: "replicapool", Name: "csi-gsnap-abc@csi-gs-def"}
}

// memberID is a member snapshot id in the "image@#<snap-id>" group-member form.
func memberID(image string, snapID string) string {
	return volumeid.Handle{Backend: "ceph-rbd", Instance: "east", Location: "replicapool", Name: image + "@#" + snapID}.String()
}

// fixedGroupSnapshot is a backend group snapshot over the two named images.
func fixedGroupSnapshot(images ...string) *backend.GroupSnapshot {
	g := &backend.GroupSnapshot{Handle: gsHandle("east"), CreationTime: time.Unix(1700000000, 0), ReadyToUse: true}
	for _, img := range images {
		g.Members = append(g.Members, backend.GroupSnapshotMember{
			Handle:       volumeid.Handle{Backend: "ceph-rbd", Instance: "east", Location: "replicapool", Name: img + "@#7"},
			SourceVolume: volumeid.Handle{Backend: "ceph-rbd", Instance: "east", Location: "replicapool", Name: img},
			SizeBytes:    1 << 30,
			ReadyToUse:   true,
		})
	}
	return g
}

// Create dispatches to the grouping backend and returns a group snapshot whose
// members each carry the group snapshot id -- which is how the CO learns they
// must be reclaimed through DeleteVolumeGroupSnapshot, not DeleteSnapshot.
func TestCreateVolumeGroupSnapshotDispatches(t *testing.T) {
	fb := &fakeBackend{groupSnap: func(string, volumeid.Handle) (*backend.GroupSnapshot, error) {
		return fixedGroupSnapshot("csi-vol-a", "csi-vol-b"), nil
	}}
	gs := groupSnapWith(t, fb)
	resp, err := gs.CreateVolumeGroupSnapshot(context.Background(), &csi.CreateVolumeGroupSnapshotRequest{
		Name:            "gs-1",
		SourceVolumeIds: []string{volID("east", "replicapool", "csi-vol-a"), volID("east", "replicapool", "csi-vol-b")},
	})
	if err != nil {
		t.Fatal(err)
	}
	groupID := resp.GetGroupSnapshot().GetGroupSnapshotId()
	if groupID != gsHandle("east").String() {
		t.Fatalf("group snapshot id not echoed: %q", groupID)
	}
	if len(resp.GetGroupSnapshot().GetSnapshots()) != 2 {
		t.Fatalf("expected 2 members, got %v", resp.GetGroupSnapshot().GetSnapshots())
	}
	for _, s := range resp.GetGroupSnapshot().GetSnapshots() {
		if s.GetGroupSnapshotId() != groupID {
			t.Fatalf("every member must carry the group snapshot id, got %q", s.GetGroupSnapshotId())
		}
		if s.GetSourceVolumeId() == "" || s.GetSnapshotId() == "" || !s.GetReadyToUse() {
			t.Fatalf("member is not fully described: %+v", s)
		}
	}
	if len(fb.groupSnapped) != 1 || fb.groupSnapped[0] != "create:gs-1" {
		t.Fatalf("expected one create dispatch, got %v", fb.groupSnapped)
	}
}

// The CSI error codes for CreateVolumeGroupSnapshot, taken from the spec's own
// table rather than inferred: a bad request is InvalidArgument, an unknown source
// volume is NotFound, and volumes the SP cannot group together (spanning
// instances, or on a backend without the guarantee) are FailedPrecondition -- the
// caller must change the request, and no retry of it can succeed.
func TestCreateVolumeGroupSnapshotErrorCodes(t *testing.T) {
	ok := func(string, volumeid.Handle) (*backend.GroupSnapshot, error) {
		return fixedGroupSnapshot("csi-vol-a"), nil
	}
	for name, tc := range map[string]struct {
		backend *fakeBackend
		req     *csi.CreateVolumeGroupSnapshotRequest
		want    codes.Code
	}{
		"no name": {
			&fakeBackend{groupSnap: ok},
			&csi.CreateVolumeGroupSnapshotRequest{SourceVolumeIds: []string{volID("east", "replicapool", "csi-vol-a")}},
			codes.InvalidArgument,
		},
		"no source volumes": {
			&fakeBackend{groupSnap: ok},
			&csi.CreateVolumeGroupSnapshotRequest{Name: "gs-1"},
			codes.InvalidArgument,
		},
		"unparseable source volume": {
			&fakeBackend{groupSnap: ok},
			&csi.CreateVolumeGroupSnapshotRequest{Name: "gs-1", SourceVolumeIds: []string{"not-a-bard-handle"}},
			codes.NotFound,
		},
		"sources span instances": {
			&fakeBackend{groupSnap: ok},
			&csi.CreateVolumeGroupSnapshotRequest{Name: "gs-1", SourceVolumeIds: []string{
				volID("east", "replicapool", "csi-vol-a"), volID("west", "replicapool", "csi-vol-b"),
			}},
			codes.FailedPrecondition,
		},
		"backend cannot group-snapshot": {
			&fakeBackend{}, // groupSnap nil -> capability off
			&csi.CreateVolumeGroupSnapshotRequest{Name: "gs-1", SourceVolumeIds: []string{volID("east", "replicapool", "csi-vol-a")}},
			codes.FailedPrecondition,
		},
	} {
		gs := groupSnapWith(t, tc.backend)
		_, err := gs.CreateVolumeGroupSnapshot(context.Background(), tc.req)
		if got := status.Code(err); got != tc.want {
			t.Errorf("%s: expected %v, got %v (%v)", name, tc.want, got, err)
		}
		if len(tc.backend.groupSnapped) != 0 {
			t.Errorf("%s: a rejected request must not dispatch, got %v", name, tc.backend.groupSnapped)
		}
	}
}

// A backend refusal maps through to the CSI code the spec asks for: a cluster too
// old to restore group members is FailedPrecondition (the operator must fix the
// configuration before retrying), not a generic Internal error.
func TestCreateVolumeGroupSnapshotBackendRefusal(t *testing.T) {
	fb := &fakeBackend{groupSnap: func(string, volumeid.Handle) (*backend.GroupSnapshot, error) {
		return nil, backend.ErrFailedPrecondition
	}}
	gs := groupSnapWith(t, fb)
	_, err := gs.CreateVolumeGroupSnapshot(context.Background(), &csi.CreateVolumeGroupSnapshotRequest{
		Name: "gs-1", SourceVolumeIds: []string{volID("east", "replicapool", "csi-vol-a")},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v (%v)", status.Code(err), err)
	}
}

// Delete's snapshot_ids check must NOT fire once the group snapshot is gone.
// CSI retries delete with the same snapshot_ids whenever the CO misses a
// response, so an unconditional check would reject every retry after a
// successful delete (zero live members can never equal a non-empty list) and
// leave the VolumeGroupSnapshotContent permanently undeletable. The retry must
// still reach the backend, because delete also finishes any leftover collection.
func TestDeleteVolumeGroupSnapshotIdempotencyCarveOuts(t *testing.T) {
	ids := []string{memberID("csi-vol-a", "7"), memberID("csi-vol-b", "7")}

	t.Run("group snapshot gone: succeeds and still runs the delete", func(t *testing.T) {
		fb := &fakeBackend{groupSnap: func(op string, _ volumeid.Handle) (*backend.GroupSnapshot, error) {
			return nil, nil // get -> ErrNotFound, delete -> success
		}}
		gs := groupSnapWith(t, fb)
		if _, err := gs.DeleteVolumeGroupSnapshot(context.Background(), &csi.DeleteVolumeGroupSnapshotRequest{
			GroupSnapshotId: gsHandle("east").String(), SnapshotIds: ids,
		}); err != nil {
			t.Fatalf("deleting an already-deleted group snapshot must succeed: %v", err)
		}
		if !containsPrefix(fb.groupSnapped, "delete:") {
			t.Fatalf("the delete must still reach the backend for leftover collection, got %v", fb.groupSnapped)
		}
	})

	t.Run("group snapshot present with matching members: deletes", func(t *testing.T) {
		fb := &fakeBackend{groupSnap: func(op string, _ volumeid.Handle) (*backend.GroupSnapshot, error) {
			if op == "get" {
				return fixedGroupSnapshot("csi-vol-a", "csi-vol-b"), nil
			}
			return nil, nil
		}}
		gs := groupSnapWith(t, fb)
		// Order must not matter: the CO may list the members either way.
		if _, err := gs.DeleteVolumeGroupSnapshot(context.Background(), &csi.DeleteVolumeGroupSnapshotRequest{
			GroupSnapshotId: gsHandle("east").String(), SnapshotIds: []string{ids[1], ids[0]},
		}); err != nil {
			t.Fatal(err)
		}
		if !containsPrefix(fb.groupSnapped, "delete:") {
			t.Fatalf("expected a delete dispatch, got %v", fb.groupSnapped)
		}
	})

	t.Run("group snapshot present with different members: mismatch", func(t *testing.T) {
		fb := &fakeBackend{groupSnap: func(op string, _ volumeid.Handle) (*backend.GroupSnapshot, error) {
			if op == "get" {
				return fixedGroupSnapshot("csi-vol-a", "csi-vol-zzz"), nil
			}
			return nil, nil
		}}
		gs := groupSnapWith(t, fb)
		_, err := gs.DeleteVolumeGroupSnapshot(context.Background(), &csi.DeleteVolumeGroupSnapshotRequest{
			GroupSnapshotId: gsHandle("east").String(), SnapshotIds: ids,
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("a snapshot list mismatch must be InvalidArgument, got %v (%v)", status.Code(err), err)
		}
		if containsPrefix(fb.groupSnapped, "delete:") {
			t.Fatalf("a mismatched delete must NOT destroy anything, got %v", fb.groupSnapped)
		}
	})

	t.Run("empty snapshot_ids asserts nothing", func(t *testing.T) {
		fb := &fakeBackend{groupSnap: func(op string, _ volumeid.Handle) (*backend.GroupSnapshot, error) {
			if op == "get" {
				return fixedGroupSnapshot("csi-vol-a", "csi-vol-b"), nil
			}
			return nil, nil
		}}
		gs := groupSnapWith(t, fb)
		if _, err := gs.DeleteVolumeGroupSnapshot(context.Background(), &csi.DeleteVolumeGroupSnapshotRequest{
			GroupSnapshotId: gsHandle("east").String(),
		}); err != nil {
			t.Fatalf("a delete that names no members must not be blocked: %v", err)
		}
		if !containsPrefix(fb.groupSnapped, "delete:") {
			t.Fatalf("expected a delete dispatch, got %v", fb.groupSnapped)
		}
	})

	t.Run("membership unreadable: deletes anyway rather than wedging", func(t *testing.T) {
		fb := &fakeBackend{groupSnap: func(op string, _ volumeid.Handle) (*backend.GroupSnapshot, error) {
			if op == "get" {
				return nil, errors.New("mon down")
			}
			return nil, nil
		}}
		gs := groupSnapWith(t, fb)
		if _, err := gs.DeleteVolumeGroupSnapshot(context.Background(), &csi.DeleteVolumeGroupSnapshotRequest{
			GroupSnapshotId: gsHandle("east").String(), SnapshotIds: ids,
		}); err != nil {
			t.Fatalf("an unreadable member list must not block the delete: %v", err)
		}
		if !containsPrefix(fb.groupSnapped, "delete:") {
			t.Fatalf("expected the delete to be dispatched, got %v", fb.groupSnapped)
		}
	})

	t.Run("delete failure surfaces", func(t *testing.T) {
		fb := &fakeBackend{groupSnap: func(op string, _ volumeid.Handle) (*backend.GroupSnapshot, error) {
			if op == "get" {
				return fixedGroupSnapshot("csi-vol-a", "csi-vol-b"), nil
			}
			return nil, errors.New("mon down")
		}}
		gs := groupSnapWith(t, fb)
		if _, err := gs.DeleteVolumeGroupSnapshot(context.Background(), &csi.DeleteVolumeGroupSnapshotRequest{
			GroupSnapshotId: gsHandle("east").String(), SnapshotIds: ids,
		}); status.Code(err) != codes.Internal {
			t.Fatalf("a failed delete must be reported so the CO retries, got %v", err)
		}
	})

	t.Run("id we never issued: succeeds without dispatching", func(t *testing.T) {
		fb := &fakeBackend{groupSnap: func(string, volumeid.Handle) (*backend.GroupSnapshot, error) {
			return nil, nil
		}}
		gs := groupSnapWith(t, fb)
		if _, err := gs.DeleteVolumeGroupSnapshot(context.Background(), &csi.DeleteVolumeGroupSnapshotRequest{
			GroupSnapshotId: "not-a-bard-handle",
		}); err != nil {
			t.Fatalf("an unknown group snapshot id must delete as a no-op: %v", err)
		}
		if len(fb.groupSnapped) != 0 {
			t.Fatalf("nothing should have been dispatched, got %v", fb.groupSnapped)
		}
	})

	t.Run("no id at all", func(t *testing.T) {
		gs := groupSnapWith(t, &fakeBackend{groupSnap: func(string, volumeid.Handle) (*backend.GroupSnapshot, error) {
			return nil, nil
		}})
		_, err := gs.DeleteVolumeGroupSnapshot(context.Background(), &csi.DeleteVolumeGroupSnapshotRequest{})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("expected InvalidArgument, got %v", status.Code(err))
		}
	})
}

// Get's own error table: a group snapshot that no longer exists is NOT_FOUND
// (whether the id is unparseable or simply gone), while one that exists with a
// member set differing from the request is INVALID_ARGUMENT -- the same
// snapshot-list-mismatch row Delete has, confirmed against Get's table rather
// than assumed from Delete's.
func TestGetVolumeGroupSnapshotErrorCodes(t *testing.T) {
	ids := []string{memberID("csi-vol-a", "7"), memberID("csi-vol-b", "7")}

	t.Run("matching members", func(t *testing.T) {
		gs := groupSnapWith(t, &fakeBackend{groupSnap: func(string, volumeid.Handle) (*backend.GroupSnapshot, error) {
			return fixedGroupSnapshot("csi-vol-a", "csi-vol-b"), nil
		}})
		resp, err := gs.GetVolumeGroupSnapshot(context.Background(), &csi.GetVolumeGroupSnapshotRequest{
			GroupSnapshotId: gsHandle("east").String(), SnapshotIds: ids,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.GetGroupSnapshot().GetSnapshots()) != 2 || !resp.GetGroupSnapshot().GetReadyToUse() {
			t.Fatalf("expected both members reported ready, got %+v", resp.GetGroupSnapshot())
		}
	})

	for name, tc := range map[string]struct {
		gsnap func(string, volumeid.Handle) (*backend.GroupSnapshot, error)
		req   *csi.GetVolumeGroupSnapshotRequest
		want  codes.Code
	}{
		"no id": {
			func(string, volumeid.Handle) (*backend.GroupSnapshot, error) { return fixedGroupSnapshot("a"), nil },
			&csi.GetVolumeGroupSnapshotRequest{},
			codes.InvalidArgument,
		},
		"unparseable id": {
			func(string, volumeid.Handle) (*backend.GroupSnapshot, error) { return fixedGroupSnapshot("a"), nil },
			&csi.GetVolumeGroupSnapshotRequest{GroupSnapshotId: "not-a-bard-handle"},
			codes.NotFound,
		},
		"group snapshot gone": {
			func(string, volumeid.Handle) (*backend.GroupSnapshot, error) { return nil, nil },
			&csi.GetVolumeGroupSnapshotRequest{GroupSnapshotId: gsHandle("east").String(), SnapshotIds: ids},
			codes.NotFound,
		},
		"member set mismatch": {
			func(string, volumeid.Handle) (*backend.GroupSnapshot, error) {
				return fixedGroupSnapshot("csi-vol-a"), nil
			},
			&csi.GetVolumeGroupSnapshotRequest{GroupSnapshotId: gsHandle("east").String(), SnapshotIds: ids},
			codes.InvalidArgument,
		},
	} {
		gs := groupSnapWith(t, &fakeBackend{groupSnap: tc.gsnap})
		_, err := gs.GetVolumeGroupSnapshot(context.Background(), tc.req)
		if got := status.Code(err); got != tc.want {
			t.Errorf("%s: expected %v, got %v (%v)", name, tc.want, got, err)
		}
	}
}

// The mismatch error names both sides, so an operator can see which member the CO
// and the backend disagree about without reading driver logs.
func TestGroupSnapshotMismatchErrorNamesBothSides(t *testing.T) {
	gs := groupSnapWith(t, &fakeBackend{groupSnap: func(string, volumeid.Handle) (*backend.GroupSnapshot, error) {
		return fixedGroupSnapshot("csi-vol-a"), nil
	}})
	_, err := gs.GetVolumeGroupSnapshot(context.Background(), &csi.GetVolumeGroupSnapshotRequest{
		GroupSnapshotId: gsHandle("east").String(), SnapshotIds: []string{memberID("csi-vol-zzz", "7")},
	})
	if !strings.Contains(err.Error(), "csi-vol-a") || !strings.Contains(err.Error(), "csi-vol-zzz") {
		t.Fatalf("the mismatch error should name both member sets: %v", err)
	}
}

// GroupController is advertised and served ONLY when a registered backend can cut
// a write-order-consistent snapshot: CSI has no way to say "there is a group
// controller here, but it cannot honour the guarantee", and Bard's earlier
// implementation was withdrawn precisely for advertising one that could not.
func TestGroupSnapshotCapabilityGating(t *testing.T) {
	advertises := func(fb *fakeBackend) bool {
		reg := backend.NewRegistry()
		if fb != nil {
			reg.Register(fb)
		}
		d := New(Options{Registry: reg, Dispatch: mustDisp(t), Mode: Mode{Controller: true}})
		resp, err := (&identityServer{driver: d}).GetPluginCapabilities(context.Background(), &csi.GetPluginCapabilitiesRequest{})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range resp.GetCapabilities() {
			if c.GetService().GetType() == csi.PluginCapability_Service_GROUP_CONTROLLER_SERVICE {
				return true
			}
		}
		return false
	}
	if advertises(&fakeBackend{}) {
		t.Fatal("a backend without group snapshots must not advertise GROUP_CONTROLLER_SERVICE")
	}
	if !advertises(&fakeBackend{groupSnap: func(string, volumeid.Handle) (*backend.GroupSnapshot, error) {
		return fixedGroupSnapshot("csi-vol-a"), nil
	}}) {
		t.Fatal("a group-snapshot-capable backend must advertise GROUP_CONTROLLER_SERVICE")
	}

	// The single capability the service reports is the one CSI defines for it.
	gs := groupSnapWith(t, &fakeBackend{groupSnap: func(string, volumeid.Handle) (*backend.GroupSnapshot, error) {
		return fixedGroupSnapshot("csi-vol-a"), nil
	}})
	caps, err := gs.GroupControllerGetCapabilities(context.Background(), &csi.GroupControllerGetCapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(caps.GetCapabilities()) != 1 ||
		caps.GetCapabilities()[0].GetRpc().GetType() != csi.GroupControllerServiceCapability_RPC_CREATE_DELETE_GET_VOLUME_GROUP_SNAPSHOT {
		t.Fatalf("unexpected group controller capabilities: %v", caps.GetCapabilities())
	}
}

func containsPrefix(ss []string, prefix string) bool {
	for _, s := range ss {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}
