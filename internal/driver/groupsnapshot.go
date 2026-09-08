package driver

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/klog/v2"

	"github.com/kindacoolhamster/bard-csi/internal/backend"
	"github.com/kindacoolhamster/bard-csi/internal/volumeid"
)

// groupControllerServer implements the CSI GroupController service
// (VolumeGroupSnapshot) by dispatching to a backend that can cut one
// write-order-consistent snapshot across several volumes (ceph-rbd, via `rbd
// group snap create`).
//
// CSI v1.12 requires that guarantee or a failure, so this service is registered
// ONLY when a registered backend advertises Capabilities.GroupSnapshot -- Bard's
// earlier per-volume-sequential implementation was withdrawn rather than kept,
// because no arrangement of independent CreateSnapshot calls can honour it.
//
// The CSI-level rules live here rather than in the backend: `snapshot_ids`
// verification and the delete/get idempotency carve-outs are stated in terms of
// CSI ids, and a backend may be an out-of-tree plugin that never sees one (core
// owns volume-id encoding). The backend reports what it holds; this layer decides
// what that means for the CO.
type groupControllerServer struct {
	csi.UnimplementedGroupControllerServer
	driver *Driver
}

func (s *groupControllerServer) GroupControllerGetCapabilities(_ context.Context, _ *csi.GroupControllerGetCapabilitiesRequest) (*csi.GroupControllerGetCapabilitiesResponse, error) {
	return &csi.GroupControllerGetCapabilitiesResponse{
		Capabilities: []*csi.GroupControllerServiceCapability{{
			Type: &csi.GroupControllerServiceCapability_Rpc{
				Rpc: &csi.GroupControllerServiceCapability_RPC{
					Type: csi.GroupControllerServiceCapability_RPC_CREATE_DELETE_GET_VOLUME_GROUP_SNAPSHOT,
				},
			},
		}},
	}, nil
}

// groupSnapshotter returns the registered backend of the given type when it can
// take group snapshots. A backend that cannot is a FailedPrecondition rather than
// Unimplemented: the CO named specific volumes, and per the CSI error table the
// remedy is to ask about volumes the SP can actually group -- not to conclude the
// whole RPC is unavailable (some other registered backend may serve it fine).
func (d *Driver) groupSnapshotter(backendType string) (backend.GroupSnapshotter, error) {
	be, err := d.snapshot().registry.Get(backendType)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	gs, ok := be.(backend.GroupSnapshotter)
	if !ok || !be.Capabilities().GroupSnapshot {
		return nil, status.Errorf(codes.FailedPrecondition,
			"backend %q cannot snapshot volumes together with a write-order consistency guarantee", backendType)
	}
	return gs, nil
}

// parseGroupSources parses the source volume ids and verifies they all live in
// one backend instance. Spanning instances is FailedPrecondition, not
// InvalidArgument: the ids are well-formed, but no storage system can cut one
// write-order-consistent snapshot across two independent clusters, so the caller
// must group different volumes.
func parseGroupSources(volumeIDs []string) (handles []volumeid.Handle, backendType string, err error) {
	instance := ""
	for _, id := range volumeIDs {
		h, perr := volumeid.Parse(id)
		if perr != nil {
			return nil, "", status.Errorf(codes.NotFound, "source volume id %q: %v", id, perr)
		}
		if backendType == "" {
			backendType, instance = h.Backend, h.Instance
		} else if h.Backend != backendType || h.Instance != instance {
			return nil, "", status.Error(codes.FailedPrecondition,
				"every volume in a group snapshot must be in the same backend instance: one write-order-consistent snapshot cannot span storage clusters")
		}
		handles = append(handles, h)
	}
	return handles, backendType, nil
}

func (s *groupControllerServer) CreateVolumeGroupSnapshot(ctx context.Context, req *csi.CreateVolumeGroupSnapshotRequest) (*csi.CreateVolumeGroupSnapshotResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "group snapshot name is required")
	}
	if len(req.GetSourceVolumeIds()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "at least one source volume id is required")
	}
	sources, backendType, err := parseGroupSources(req.GetSourceVolumeIds())
	if err != nil {
		return nil, err
	}
	gs, err := s.driver.groupSnapshotter(backendType)
	if err != nil {
		return nil, err
	}
	// One in-flight create per CO group-snapshot name (its idempotency key).
	done, err := s.driver.claim("group-snapshot-name", req.GetName())
	if err != nil {
		return nil, err
	}
	defer done()

	g, err := gs.CreateVolumeGroupSnapshot(ctx, &backend.CreateGroupSnapshotRequest{
		Name:          req.GetName(),
		SourceVolumes: sources,
		Parameters:    req.GetParameters(),
		Secrets:       req.GetSecrets(),
	})
	if err != nil {
		return nil, toStatus(err, "create volume group snapshot")
	}
	klog.V(2).Infof("created group snapshot %s with %d member(s)", g.Handle.String(), len(g.Members))
	return &csi.CreateVolumeGroupSnapshotResponse{GroupSnapshot: groupSnapshotResponse(*g)}, nil
}

func (s *groupControllerServer) DeleteVolumeGroupSnapshot(ctx context.Context, req *csi.DeleteVolumeGroupSnapshotRequest) (*csi.DeleteVolumeGroupSnapshotResponse, error) {
	if req.GetGroupSnapshotId() == "" {
		return nil, status.Error(codes.InvalidArgument, "group snapshot id is required")
	}
	gh, err := volumeid.Parse(req.GetGroupSnapshotId())
	if err != nil {
		// Not an id we ever issued, so there is nothing of ours to delete.
		return &csi.DeleteVolumeGroupSnapshotResponse{}, nil
	}
	gs, err := s.driver.groupSnapshotter(gh.Backend)
	if err != nil {
		return nil, err
	}
	done, err := s.driver.claim("group-snapshot", req.GetGroupSnapshotId())
	if err != nil {
		return nil, err
	}
	defer done()

	// The mismatch check is conditional ON THE GROUP SNAPSHOT STILL EXISTING.
	// Delete is retried with the same snapshot_ids whenever the CO misses a
	// response, so an unconditional check would permanently reject the retry that
	// follows a successful delete (zero live members can never match a non-empty
	// list) and leave the VolumeGroupSnapshotContent undeletable.
	switch current, err := gs.GetVolumeGroupSnapshot(ctx, gh, req.GetSecrets()); {
	case err == nil:
		if mErr := verifyGroupMembers(current, req.GetSnapshotIds()); mErr != nil {
			return nil, mErr
		}
	case errors.Is(err, backend.ErrNotFound):
		// Already gone. Fall through to the delete anyway: it is idempotent and
		// still performs the leftover collection (disbanding an emptied group) for
		// a delete interrupted between its two steps.
	default:
		// Membership could not be read. Delete anyway rather than refuse: the
		// check is a guard against id confusion, not a reason to make a group
		// snapshot undeletable, and a genuinely unreachable backend will fail the
		// delete below on its own. Only the guard is lost, never the delete.
		klog.Warningf("group snapshot %s: cannot verify members before delete (%v); deleting the named group snapshot anyway", gh.String(), err)
	}
	if err := gs.DeleteVolumeGroupSnapshot(ctx, gh, req.GetSecrets()); err != nil {
		return nil, toStatus(err, "delete volume group snapshot")
	}
	klog.V(2).Infof("deleted group snapshot %s", gh.String())
	return &csi.DeleteVolumeGroupSnapshotResponse{}, nil
}

func (s *groupControllerServer) GetVolumeGroupSnapshot(ctx context.Context, req *csi.GetVolumeGroupSnapshotRequest) (*csi.GetVolumeGroupSnapshotResponse, error) {
	if req.GetGroupSnapshotId() == "" {
		return nil, status.Error(codes.InvalidArgument, "group snapshot id is required")
	}
	gh, err := volumeid.Parse(req.GetGroupSnapshotId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "unknown group snapshot id %q: %v", req.GetGroupSnapshotId(), err)
	}
	gs, err := s.driver.groupSnapshotter(gh.Backend)
	if err != nil {
		return nil, err
	}
	current, err := gs.GetVolumeGroupSnapshot(ctx, gh, req.GetSecrets())
	if err != nil {
		return nil, toStatus(err, "get volume group snapshot")
	}
	if mErr := verifyGroupMembers(current, req.GetSnapshotIds()); mErr != nil {
		return nil, mErr
	}
	return &csi.GetVolumeGroupSnapshotResponse{GroupSnapshot: groupSnapshotResponse(*current)}, nil
}

// verifyGroupMembers enforces the spec's snapshot_ids check: the ids the CO
// believes are in the group snapshot must be exactly the ones it actually holds.
// An empty list from the CO asserts nothing and is accepted.
func verifyGroupMembers(g *backend.GroupSnapshot, snapshotIDs []string) error {
	if len(snapshotIDs) == 0 {
		return nil
	}
	actual := make([]string, 0, len(g.Members))
	for _, m := range g.Members {
		actual = append(actual, m.Handle.String())
	}
	want := append([]string(nil), snapshotIDs...)
	sort.Strings(actual)
	sort.Strings(want)
	if strings.Join(actual, ",") == strings.Join(want, ",") {
		return nil
	}
	return status.Errorf(codes.InvalidArgument,
		"snapshot list mismatch for group snapshot %s: it holds [%s], the request named [%s]",
		g.Handle.String(), strings.Join(actual, " "), strings.Join(want, " "))
}

// groupSnapshotResponse renders a backend group snapshot as the CSI message. Each
// member carries the group snapshot id, which is how the CO learns it must be
// reclaimed through DeleteVolumeGroupSnapshot rather than DeleteSnapshot.
func groupSnapshotResponse(g backend.GroupSnapshot) *csi.VolumeGroupSnapshot {
	groupID := g.Handle.String()
	snaps := make([]*csi.Snapshot, 0, len(g.Members))
	for _, m := range g.Members {
		snaps = append(snaps, &csi.Snapshot{
			SnapshotId:      m.Handle.String(),
			SourceVolumeId:  m.SourceVolume.String(),
			SizeBytes:       m.SizeBytes,
			CreationTime:    timestamppb.New(m.CreationTime),
			ReadyToUse:      m.ReadyToUse,
			GroupSnapshotId: groupID,
		})
	}
	return &csi.VolumeGroupSnapshot{
		GroupSnapshotId: groupID,
		Snapshots:       snaps,
		CreationTime:    timestamppb.New(g.CreationTime),
		ReadyToUse:      g.ReadyToUse,
	}
}
