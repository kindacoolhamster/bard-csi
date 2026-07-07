package driver

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kindacoolhamster/bard-csi/internal/backend"
	"github.com/kindacoolhamster/bard-csi/internal/volumeid"
)

// fakeBackend implements backend.Backend with overridable behaviour. Only the
// methods a given test exercises need a func; the rest no-op or return
// ErrUnsupported. It exists so the CSI controller logic can be tested without a
// real plugin.
type fakeBackend struct {
	health          func() (*backend.VolumeHealth, error)
	expand          func() (int64, bool, error)
	modify          func() error
	reclaim         func() (*backend.SpaceUsage, error)
	nodeReclaim     func() (*backend.SpaceUsage, error)
	publish         func(nodeID string) (map[string]string, error)
	requiresPublish bool // advertised via Capabilities
	listVolumes     func() ([]backend.VolumeListEntry, error)
	listSnapshots   func() ([]backend.SnapshotListEntry, error)
	fence           func(instance string, cidrs []string) error // also enables NetworkFence cap
	fenced          [][]string                                  // recorded {op, instance, cidr...}
	group           func(op string) error                       // also enables VolumeGroup cap
	grouped         [][]string                                  // recorded {op, ...}
	replicate       func(op string, h volumeid.Handle) error    // also enables Replication cap
	replicated      []string                                    // recorded "op:<volume-id>"
	rotateKey       func(h volumeid.Handle, path string) error  // also enables EncryptionKeyRotation cap
	rotated         []string                                    // recorded "<volume-id>@<path>"
	unpublished     []string                                    // nodeIDs passed to ControllerUnpublish
	deletedSnaps    []string
}

func (f *fakeBackend) Type() string { return "ceph-rbd" }
func (f *fakeBackend) Capabilities() backend.Capabilities {
	return backend.Capabilities{
		VolumeHealth:              true,
		RequiresControllerPublish: f.requiresPublish,
		ListVolumes:               f.listVolumes != nil,
		ListSnapshots:             f.listSnapshots != nil,
		NetworkFence:              f.fence != nil,
		Replication:               f.replicate != nil,
		EncryptionKeyRotation:     f.rotateKey != nil,
		VolumeGroup:               f.group != nil,
	}
}

func (f *fakeBackend) CreateVolume(context.Context, *backend.CreateVolumeRequest) (*backend.Volume, error) {
	return nil, backend.ErrUnsupported
}
func (f *fakeBackend) DeleteVolume(context.Context, volumeid.Handle, map[string]string) error {
	return nil
}
func (f *fakeBackend) ExpandVolume(context.Context, volumeid.Handle, int64, map[string]string) (int64, bool, error) {
	if f.expand != nil {
		return f.expand()
	}
	return 0, false, backend.ErrUnsupported
}
func (f *fakeBackend) CreateSnapshot(_ context.Context, req *backend.CreateSnapshotRequest) (*backend.Snapshot, error) {
	h := req.SourceVolume
	h.Name = h.Name + "@" + req.Name // mirror the cephplugin's "image@snap" encoding
	return &backend.Snapshot{Handle: h, SourceVolumeID: req.SourceVolume.String(), SizeBytes: 1 << 30, ReadyToUse: true}, nil
}
func (f *fakeBackend) DeleteSnapshot(_ context.Context, h volumeid.Handle, _ map[string]string) error {
	f.deletedSnaps = append(f.deletedSnaps, h.String())
	return nil
}
func (f *fakeBackend) GetCapacity(context.Context, string, map[string]string) (int64, error) {
	return 0, backend.ErrUnsupported
}
func (f *fakeBackend) GetVolumeHealth(context.Context, volumeid.Handle, map[string]string) (*backend.VolumeHealth, error) {
	if f.health != nil {
		return f.health()
	}
	return nil, backend.ErrUnsupported
}
func (f *fakeBackend) ModifyVolume(context.Context, volumeid.Handle, map[string]string, map[string]string) error {
	if f.modify != nil {
		return f.modify()
	}
	return backend.ErrUnsupported
}
func (f *fakeBackend) ReclaimSpace(context.Context, volumeid.Handle, map[string]string) (*backend.SpaceUsage, error) {
	if f.reclaim != nil {
		return f.reclaim()
	}
	return nil, backend.ErrUnsupported
}
func (f *fakeBackend) NodeReclaimSpace(context.Context, volumeid.Handle, string, string, bool, map[string]string) (*backend.SpaceUsage, error) {
	if f.nodeReclaim != nil {
		return f.nodeReclaim()
	}
	return nil, backend.ErrUnsupported
}
func (f *fakeBackend) ControllerPublish(_ context.Context, _ volumeid.Handle, nodeID string, _ bool, _, _ map[string]string) (map[string]string, error) {
	if f.publish != nil {
		return f.publish(nodeID)
	}
	return nil, nil // node-mapped backend: no-op publish
}
func (f *fakeBackend) ControllerUnpublish(_ context.Context, _ volumeid.Handle, nodeID string, _ map[string]string) error {
	f.unpublished = append(f.unpublished, nodeID)
	return nil
}
func (f *fakeBackend) ListVolumes(context.Context) ([]backend.VolumeListEntry, error) {
	if f.listVolumes != nil {
		return f.listVolumes()
	}
	return nil, backend.ErrUnsupported
}
func (f *fakeBackend) ListSnapshots(context.Context) ([]backend.SnapshotListEntry, error) {
	if f.listSnapshots != nil {
		return f.listSnapshots()
	}
	return nil, backend.ErrUnsupported
}
func (f *fakeBackend) NodeStage(context.Context, *backend.NodeStageRequest) error     { return nil }
func (f *fakeBackend) NodeUnstage(context.Context, volumeid.Handle, string) error     { return nil }
func (f *fakeBackend) NodePublish(context.Context, *backend.NodePublishRequest) error { return nil }
func (f *fakeBackend) NodeUnpublish(context.Context, volumeid.Handle, string) error   { return nil }
func (f *fakeBackend) NodeExpand(context.Context, volumeid.Handle, string) (int64, error) {
	return 0, nil
}

// NetworkFencer (optional): records fence/unfence/list; the cap is gated on f.fence.
func (f *fakeBackend) FenceClusterNetwork(_ context.Context, instance string, cidrs []string, _, _ map[string]string) error {
	f.fenced = append(f.fenced, append([]string{"fence", instance}, cidrs...))
	return f.fence(instance, cidrs)
}
func (f *fakeBackend) UnfenceClusterNetwork(_ context.Context, instance string, cidrs []string, _, _ map[string]string) error {
	f.fenced = append(f.fenced, append([]string{"unfence", instance}, cidrs...))
	return f.fence(instance, cidrs)
}
func (f *fakeBackend) ListClusterFence(_ context.Context, instance string, _, _ map[string]string) ([]string, error) {
	f.fenced = append(f.fenced, []string{"list", instance})
	return []string{"10.9.9.0/24"}, nil
}
func (f *fakeBackend) GetFenceClients(_ context.Context, instance string, _, _ map[string]string) ([]backend.FenceClient, error) {
	f.fenced = append(f.fenced, []string{"getclients", instance})
	return []backend.FenceClient{{ID: "fsid-" + instance, CIDRs: []string{"10.5.5.5/32"}}}, nil
}

// VolumeGrouper (optional): records each op; the cap is gated on f.group.
func (f *fakeBackend) CreateVolumeGroup(_ context.Context, instance, pool, name string, members []volumeid.Handle, _, _ map[string]string) (backend.VolumeGroup, error) {
	f.grouped = append(f.grouped, []string{"create", instance, name})
	if err := f.group("create"); err != nil {
		return backend.VolumeGroup{}, err
	}
	return backend.VolumeGroup{Group: volumeid.Handle{Backend: "ceph-rbd", Instance: instance, Location: pool, Name: "csi-group-" + name}, Members: members}, nil
}
func (f *fakeBackend) ModifyVolumeGroup(_ context.Context, g volumeid.Handle, members []volumeid.Handle, _, _ map[string]string) (backend.VolumeGroup, error) {
	f.grouped = append(f.grouped, []string{"modify", g.String()})
	return backend.VolumeGroup{Group: g, Members: members}, f.group("modify")
}
func (f *fakeBackend) DeleteVolumeGroup(_ context.Context, g volumeid.Handle, _ map[string]string) error {
	f.grouped = append(f.grouped, []string{"delete", g.String()})
	return f.group("delete")
}
func (f *fakeBackend) GetVolumeGroup(_ context.Context, g volumeid.Handle, _ map[string]string) (backend.VolumeGroup, error) {
	f.grouped = append(f.grouped, []string{"get", g.String()})
	return backend.VolumeGroup{Group: g}, f.group("get")
}
func (f *fakeBackend) ListVolumeGroups(_ context.Context, _ map[string]string) ([]backend.VolumeGroup, error) {
	f.grouped = append(f.grouped, []string{"list"})
	return []backend.VolumeGroup{{Group: volumeid.Handle{Backend: "ceph-rbd", Instance: "east", Location: "p", Name: "csi-group-x"}}}, f.group("list")
}

// VolumeReplicator (optional): records each op; the cap is gated on f.replicate.
func (f *fakeBackend) rec(op string, h volumeid.Handle) error {
	f.replicated = append(f.replicated, op+":"+h.String())
	return f.replicate(op, h)
}
func (f *fakeBackend) EnableVolumeReplication(_ context.Context, h volumeid.Handle, _, _ map[string]string) error {
	return f.rec("enable", h)
}
func (f *fakeBackend) DisableVolumeReplication(_ context.Context, h volumeid.Handle, _, _ map[string]string) error {
	return f.rec("disable", h)
}
func (f *fakeBackend) PromoteVolume(_ context.Context, h volumeid.Handle, _ bool, _, _ map[string]string) error {
	return f.rec("promote", h)
}
func (f *fakeBackend) DemoteVolume(_ context.Context, h volumeid.Handle, _ bool, _, _ map[string]string) error {
	return f.rec("demote", h)
}
func (f *fakeBackend) ResyncVolume(_ context.Context, h volumeid.Handle, _ bool, _, _ map[string]string) (bool, error) {
	return true, f.rec("resync", h)
}
func (f *fakeBackend) GetVolumeReplicationInfo(_ context.Context, h volumeid.Handle, _ map[string]string) (time.Time, error) {
	_ = f.rec("info", h)
	return time.Unix(1700000200, 0), nil
}

// EncryptionKeyRotator (optional): records the rotation; the cap is gated on f.rotateKey.
func (f *fakeBackend) RotateEncryptionKey(_ context.Context, h volumeid.Handle, path string, _, _ map[string]string) error {
	f.rotated = append(f.rotated, h.String()+"@"+path)
	return f.rotateKey(h, path)
}

// controllerWith builds a controllerServer whose registry holds fb under the
// ceph-rbd type, plus a valid volume id routed to it.
func controllerWith(t *testing.T, fb *fakeBackend) (*controllerServer, string) {
	t.Helper()
	reg := backend.NewRegistry()
	reg.Register(fb)
	d := New(Options{Registry: reg, Dispatch: mustDisp(t)})
	h := volumeid.Handle{Backend: "ceph-rbd", Instance: "east", Location: "replicapool", Name: "csi-vol-abc"}
	if err := h.Validate(); err != nil {
		t.Fatal(err)
	}
	return &controllerServer{driver: d}, h.String()
}

func TestControllerGetVolumeHealthy(t *testing.T) {
	cs, id := controllerWith(t, &fakeBackend{
		health: func() (*backend.VolumeHealth, error) {
			return &backend.VolumeHealth{Abnormal: false, Message: "ok"}, nil
		},
	})
	resp, err := cs.ControllerGetVolume(context.Background(), &csi.ControllerGetVolumeRequest{VolumeId: id})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetVolume().GetVolumeId() != id {
		t.Fatalf("volume id not echoed: %q", resp.GetVolume().GetVolumeId())
	}
	cond := resp.GetStatus().GetVolumeCondition()
	if cond == nil || cond.GetAbnormal() {
		t.Fatalf("expected a healthy (not abnormal) condition, got %+v", cond)
	}
}

func TestControllerGetVolumeAbnormal(t *testing.T) {
	cs, id := controllerWith(t, &fakeBackend{
		health: func() (*backend.VolumeHealth, error) {
			return &backend.VolumeHealth{Abnormal: true, Message: "image gone"}, nil
		},
	})
	resp, err := cs.ControllerGetVolume(context.Background(), &csi.ControllerGetVolumeRequest{VolumeId: id})
	if err != nil {
		t.Fatal(err)
	}
	cond := resp.GetStatus().GetVolumeCondition()
	if cond == nil || !cond.GetAbnormal() || cond.GetMessage() != "image gone" {
		t.Fatalf("expected abnormal condition with message, got %+v", cond)
	}
}

// A backend without health support must still answer (volume info only), not error.
func TestControllerGetVolumeUnsupported(t *testing.T) {
	cs, id := controllerWith(t, &fakeBackend{}) // health func nil -> ErrUnsupported
	resp, err := cs.ControllerGetVolume(context.Background(), &csi.ControllerGetVolumeRequest{VolumeId: id})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetVolume().GetVolumeId() != id {
		t.Fatalf("volume id not echoed: %q", resp.GetVolume().GetVolumeId())
	}
	if resp.GetStatus().GetVolumeCondition() != nil {
		t.Fatalf("expected no condition for an unsupported backend, got %+v", resp.GetStatus().GetVolumeCondition())
	}
}

func TestControllerGetVolumeBadInput(t *testing.T) {
	cs, _ := controllerWith(t, &fakeBackend{})
	for name, tc := range map[string]struct {
		id   string
		code codes.Code
	}{
		"empty id":       {"", codes.InvalidArgument},
		"unparseable id": {"not-a-bard-handle", codes.NotFound},
	} {
		_, err := cs.ControllerGetVolume(context.Background(), &csi.ControllerGetVolumeRequest{VolumeId: tc.id})
		if status.Code(err) != tc.code {
			t.Errorf("%s: expected %v, got %v", name, tc.code, status.Code(err))
		}
	}
}
