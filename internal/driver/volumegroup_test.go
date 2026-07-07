package driver

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/csi-addons/spec/lib/go/identity"
	"github.com/csi-addons/spec/lib/go/volumegroup"

	"github.com/kindacoolhamster/bard-csi/internal/backend"
	"github.com/kindacoolhamster/bard-csi/internal/volumeid"
)

func groupWith(t *testing.T, fb *fakeBackend) *volumeGroupServer {
	t.Helper()
	reg := backend.NewRegistry()
	reg.Register(fb)
	return &volumeGroupServer{driver: New(Options{Registry: reg, Dispatch: mustDisp(t), Mode: Mode{Controller: true}})}
}

func volID(instance, pool, name string) string {
	return volumeid.Handle{Backend: "ceph-rbd", Instance: instance, Location: pool, Name: name}.String()
}

// CreateVolumeGroup resolves the instance/pool from the member volumes, dispatches to
// the grouping backend, and returns a group id plus the member volume ids.
func TestCreateVolumeGroupDispatches(t *testing.T) {
	fb := &fakeBackend{group: func(string) error { return nil }}
	gs := groupWith(t, fb)
	resp, err := gs.CreateVolumeGroup(context.Background(), &volumegroup.CreateVolumeGroupRequest{
		Name:      "grp",
		VolumeIds: []string{volID("east", "replicapool", "csi-vol-a"), volID("east", "replicapool", "csi-vol-b")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fb.grouped) != 1 || fb.grouped[0][0] != "create" || fb.grouped[0][1] != "east" {
		t.Fatalf("expected one create on instance east, got %v", fb.grouped)
	}
	if resp.GetVolumeGroup().GetVolumeGroupId() == "" {
		t.Fatal("response must carry a volume group id")
	}
	if len(resp.GetVolumeGroup().GetVolumes()) != 2 {
		t.Fatalf("expected 2 member volumes echoed, got %v", resp.GetVolumeGroup().GetVolumes())
	}
	// The group id must be a parseable handle naming the derived group.
	gh, err := volumeid.Parse(resp.GetVolumeGroup().GetVolumeGroupId())
	if err != nil || gh.Name != "csi-group-grp" {
		t.Fatalf("group id should be a handle for the derived group, got %q (%v)", resp.GetVolumeGroup().GetVolumeGroupId(), err)
	}
}

// A group whose members span two instances is rejected (an rbd group is one cluster).
func TestCreateVolumeGroupRejectsMixedInstances(t *testing.T) {
	fb := &fakeBackend{group: func(string) error { return nil }}
	gs := groupWith(t, fb)
	_, err := gs.CreateVolumeGroup(context.Background(), &volumegroup.CreateVolumeGroupRequest{
		Name:      "grp",
		VolumeIds: []string{volID("east", "p", "csi-vol-a"), volID("west", "p", "csi-vol-b")},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("mixed-instance group must be InvalidArgument, got %v", err)
	}
	if len(fb.grouped) != 0 {
		t.Fatalf("a rejected create must not dispatch, got %v", fb.grouped)
	}
}

// Modify / Get / Delete dispatch by parsing the group id; Delete never touches members.
func TestVolumeGroupModifyGetDelete(t *testing.T) {
	fb := &fakeBackend{group: func(string) error { return nil }}
	gs := groupWith(t, fb)
	gid := volID("east", "replicapool", "csi-group-grp")

	if _, err := gs.ModifyVolumeGroupMembership(context.Background(), &volumegroup.ModifyVolumeGroupMembershipRequest{
		VolumeGroupId: gid, VolumeIds: []string{volID("east", "replicapool", "csi-vol-a")},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := gs.ControllerGetVolumeGroup(context.Background(), &volumegroup.ControllerGetVolumeGroupRequest{VolumeGroupId: gid}); err != nil {
		t.Fatal(err)
	}
	if _, err := gs.DeleteVolumeGroup(context.Background(), &volumegroup.DeleteVolumeGroupRequest{VolumeGroupId: gid}); err != nil {
		t.Fatal(err)
	}
	ops := []string{}
	for _, g := range fb.grouped {
		ops = append(ops, g[0])
	}
	want := map[string]bool{"modify": false, "get": false, "delete": false}
	for _, op := range ops {
		if _, ok := want[op]; ok {
			want[op] = true
		}
	}
	for op, seen := range want {
		if !seen {
			t.Fatalf("expected a %s dispatch; got %v", op, ops)
		}
	}
	// A modify whose member is in a different instance than the group is rejected.
	if _, err := gs.ModifyVolumeGroupMembership(context.Background(), &volumegroup.ModifyVolumeGroupMembershipRequest{
		VolumeGroupId: gid, VolumeIds: []string{volID("west", "replicapool", "csi-vol-z")},
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("cross-instance modify must be InvalidArgument, got %v", err)
	}
}

// Without a grouping backend, the VolumeGroup RPC is Unimplemented and the Identity
// does not advertise VOLUME_GROUP; with one, both flip on (incl. the sub-caps).
func TestVolumeGroupCapabilityGating(t *testing.T) {
	caps := func(fb *fakeBackend) map[identity.Capability_VolumeGroup_Type]bool {
		reg := backend.NewRegistry()
		if fb != nil {
			reg.Register(fb)
		}
		d := New(Options{Registry: reg, Dispatch: mustDisp(t), Mode: Mode{Controller: true}})
		resp, _ := (&csiAddonsIdentityServer{driver: d}).GetCapabilities(context.Background(), &identity.GetCapabilitiesRequest{})
		out := map[identity.Capability_VolumeGroup_Type]bool{}
		for _, c := range resp.GetCapabilities() {
			if vg := c.GetVolumeGroup(); vg != nil {
				out[vg.GetType()] = true
			}
		}
		return out
	}
	if len(caps(&fakeBackend{})) != 0 {
		t.Fatal("a non-grouping backend must not advertise VolumeGroup")
	}
	got := caps(&fakeBackend{group: func(string) error { return nil }})
	for _, want := range []identity.Capability_VolumeGroup_Type{
		identity.Capability_VolumeGroup_VOLUME_GROUP,
		identity.Capability_VolumeGroup_MODIFY_VOLUME_GROUP,
		identity.Capability_VolumeGroup_GET_VOLUME_GROUP,
		identity.Capability_VolumeGroup_DO_NOT_ALLOW_VG_TO_DELETE_VOLUMES,
		identity.Capability_VolumeGroup_LIMIT_VOLUME_TO_ONE_VOLUME_GROUP,
	} {
		if !got[want] {
			t.Fatalf("a grouping backend must advertise %v; got %v", want, got)
		}
	}
	// No grouping backend -> the RPC is Unimplemented.
	gs := groupWith(t, &fakeBackend{}) // group cap false
	_, err := gs.CreateVolumeGroup(context.Background(), &volumegroup.CreateVolumeGroupRequest{
		Name: "g", VolumeIds: []string{volID("east", "p", "csi-vol-a")},
	})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("no grouping backend should be Unimplemented, got %v", err)
	}
}
