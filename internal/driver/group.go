package driver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kindacoolhamster/bard-csi/internal/backend"
	"github.com/kindacoolhamster/bard-csi/internal/volumeid"
)

// groupControllerServer implements CSI VolumeGroupSnapshot. Bard takes a group
// snapshot by snapshotting each source volume individually -- reusing the
// per-backend CreateSnapshot/DeleteSnapshot -- and returning them as one group.
// Because each member is an ordinary, independently-restorable snapshot, a group
// can span volumes on DIFFERENT backend instances / Ceph clusters (Bard's
// multi-cluster model), which a single rbd consistency group cannot. The trade:
// members are snapshotted in sequence, so the group is per-volume crash
// consistent but not atomic across volumes (rbd group snapshots are atomic but
// their members are not independently clonable -- verified against Ceph).
type groupControllerServer struct {
	csi.UnimplementedGroupControllerServer
	driver *Driver
}

const groupSnapMagic = "swskgs"

// groupSnapshotID is a deterministic, opaque id for a group snapshot name, so a
// retried create is idempotent. Members travel as their own snapshot ids; the CO
// hands them back on delete/get, so the group id need not encode membership.
func groupSnapshotID(name string) string {
	return groupSnapMagic + "|1|" + name
}

// memberName derives a unique, deterministic CreateSnapshot name for one source
// volume in a group, so retries reuse the same backend snapshot.
func memberName(groupName, sourceVolumeID string) string {
	sum := sha256.Sum256([]byte(sourceVolumeID))
	return groupName + "-" + hex.EncodeToString(sum[:8])
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

func (s *groupControllerServer) CreateVolumeGroupSnapshot(ctx context.Context, req *csi.CreateVolumeGroupSnapshotRequest) (*csi.CreateVolumeGroupSnapshotResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "group snapshot name is required")
	}
	if len(req.GetSourceVolumeIds()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "at least one source volume id is required")
	}
	groupID := groupSnapshotID(req.GetName())
	// One in-flight create per CO group-snapshot name (its idempotency key).
	done, err := s.driver.claim("group-name", req.GetName())
	if err != nil {
		return nil, err
	}
	defer done()
	bk := s.driver.snapshot()

	snaps := make([]*csi.Snapshot, 0, len(req.GetSourceVolumeIds()))
	for _, vid := range req.GetSourceVolumeIds() {
		src, err := volumeid.Parse(vid)
		if err != nil {
			return nil, status.Errorf(codes.NotFound, "source volume id %q: %v", vid, err)
		}
		be, err := bk.registry.Get(src.Backend)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "%v", err)
		}
		// CreateSnapshot is idempotent on its name, so a retried group create
		// reuses each member rather than duplicating it.
		snap, err := be.CreateSnapshot(ctx, &backend.CreateSnapshotRequest{
			Name:         memberName(req.GetName(), vid),
			SourceVolume: src,
			Parameters:   req.GetParameters(),
			Secrets:      req.GetSecrets(),
		})
		if err != nil {
			return nil, toStatus(err, "create group snapshot member")
		}
		snaps = append(snaps, &csi.Snapshot{
			SnapshotId:      snap.Handle.String(),
			SourceVolumeId:  snap.SourceVolumeID,
			SizeBytes:       snap.SizeBytes,
			CreationTime:    timestamppb.New(snap.CreationTime),
			ReadyToUse:      snap.ReadyToUse,
			GroupSnapshotId: groupID,
		})
	}

	return &csi.CreateVolumeGroupSnapshotResponse{
		GroupSnapshot: &csi.VolumeGroupSnapshot{
			GroupSnapshotId: groupID,
			Snapshots:       snaps,
			CreationTime:    timestamppb.New(time.Now()),
			ReadyToUse:      true,
		},
	}, nil
}

func (s *groupControllerServer) DeleteVolumeGroupSnapshot(ctx context.Context, req *csi.DeleteVolumeGroupSnapshotRequest) (*csi.DeleteVolumeGroupSnapshotResponse, error) {
	if req.GetGroupSnapshotId() == "" {
		return nil, status.Error(codes.InvalidArgument, "group snapshot id is required")
	}
	done, err := s.driver.claim("group", req.GetGroupSnapshotId())
	if err != nil {
		return nil, err
	}
	defer done()
	bk := s.driver.snapshot()
	for _, sid := range req.GetSnapshotIds() {
		h, err := volumeid.Parse(sid)
		if err != nil {
			continue // unknown/unparseable member => already gone, idempotent
		}
		be, err := bk.registry.Get(h.Backend)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "%v", err)
		}
		if err := be.DeleteSnapshot(ctx, h, req.GetSecrets()); err != nil {
			return nil, toStatus(err, "delete group snapshot member")
		}
	}
	return &csi.DeleteVolumeGroupSnapshotResponse{}, nil
}

func (s *groupControllerServer) GetVolumeGroupSnapshot(_ context.Context, req *csi.GetVolumeGroupSnapshotRequest) (*csi.GetVolumeGroupSnapshotResponse, error) {
	if req.GetGroupSnapshotId() == "" {
		return nil, status.Error(codes.InvalidArgument, "group snapshot id is required")
	}
	// An id we did not issue is a group snapshot we do not know about.
	if !strings.HasPrefix(req.GetGroupSnapshotId(), groupSnapMagic+"|1|") {
		return nil, status.Errorf(codes.NotFound, "unknown group snapshot id %q", req.GetGroupSnapshotId())
	}
	snaps := make([]*csi.Snapshot, 0, len(req.GetSnapshotIds()))
	for _, sid := range req.GetSnapshotIds() {
		h, err := volumeid.Parse(sid)
		if err != nil {
			return nil, status.Errorf(codes.NotFound, "snapshot id %q: %v", sid, err)
		}
		snaps = append(snaps, &csi.Snapshot{
			SnapshotId:      sid,
			SourceVolumeId:  sourceVolumeOf(h).String(),
			ReadyToUse:      true,
			GroupSnapshotId: req.GetGroupSnapshotId(),
		})
	}
	return &csi.GetVolumeGroupSnapshotResponse{
		GroupSnapshot: &csi.VolumeGroupSnapshot{
			GroupSnapshotId: req.GetGroupSnapshotId(),
			Snapshots:       snaps,
			ReadyToUse:      true,
		},
	}, nil
}

// sourceVolumeOf reconstructs a snapshot's source volume handle: a snapshot
// handle's Name encodes "image@snap", so the source volume is the same handle
// with the "@snap" suffix dropped.
func sourceVolumeOf(snap volumeid.Handle) volumeid.Handle {
	src := snap
	if i := strings.LastIndex(snap.Name, "@"); i >= 0 {
		src.Name = snap.Name[:i]
	}
	return src
}
