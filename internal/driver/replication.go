package driver

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/klog/v2"

	"github.com/csi-addons/spec/lib/go/replication"

	"github.com/kindacoolhamster/bard-csi/internal/backend"
	"github.com/kindacoolhamster/bard-csi/internal/volumeid"
)

// replicationServer implements the csi-addons Replication Controller service
// (RBD mirroring for DR) by dispatching each volume-scoped op to the backend that
// owns the volume. ceph-rbd implements VolumeReplicator; other backends don't, so
// the operation is Unimplemented there (and the capability is gated accordingly).
type replicationServer struct {
	replication.UnimplementedControllerServer
	driver *Driver
}

// srcVolumeID extracts the CSI volume id from a request, preferring the newer
// replication_source.volume field that recent csi-addons controller-managers
// populate and falling back to the deprecated top-level volume_id.
func srcVolumeID(deprecatedVolumeID string, src *replication.ReplicationSource) string {
	if id := src.GetVolume().GetVolumeId(); id != "" {
		return id
	}
	return deprecatedVolumeID
}

// replicator resolves the volume id to its owning backend and asserts that the
// backend can replicate. Mirrors the ReclaimSpace dispatch (volume-scoped).
func (s *replicationServer) replicator(volumeID string) (backend.VolumeReplicator, volumeid.Handle, error) {
	if volumeID == "" {
		return nil, volumeid.Handle{}, status.Error(codes.InvalidArgument, "volume id is required")
	}
	h, err := volumeid.Parse(volumeID)
	if err != nil {
		return nil, volumeid.Handle{}, status.Errorf(codes.NotFound, "unknown volume id: %v", err)
	}
	be, err := s.driver.snapshot().registry.Get(h.Backend)
	if err != nil {
		return nil, h, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	vr, ok := be.(backend.VolumeReplicator)
	if !ok || !be.Capabilities().Replication {
		return nil, h, status.Errorf(codes.Unimplemented, "backend %q does not support volume replication", h.Backend)
	}
	return vr, h, nil
}

func (s *replicationServer) EnableVolumeReplication(ctx context.Context, req *replication.EnableVolumeReplicationRequest) (*replication.EnableVolumeReplicationResponse, error) {
	vr, h, err := s.replicator(srcVolumeID(req.GetVolumeId(), req.GetReplicationSource()))
	if err != nil {
		return nil, err
	}
	if err := vr.EnableVolumeReplication(ctx, h, req.GetParameters(), req.GetSecrets()); err != nil {
		return nil, toStatus(err, "enable volume replication")
	}
	klog.V(2).Infof("enabled replication on %s", req.GetVolumeId())
	return &replication.EnableVolumeReplicationResponse{}, nil
}

func (s *replicationServer) DisableVolumeReplication(ctx context.Context, req *replication.DisableVolumeReplicationRequest) (*replication.DisableVolumeReplicationResponse, error) {
	vr, h, err := s.replicator(srcVolumeID(req.GetVolumeId(), req.GetReplicationSource()))
	if err != nil {
		return nil, err
	}
	if err := vr.DisableVolumeReplication(ctx, h, req.GetParameters(), req.GetSecrets()); err != nil {
		return nil, toStatus(err, "disable volume replication")
	}
	klog.V(2).Infof("disabled replication on %s", req.GetVolumeId())
	return &replication.DisableVolumeReplicationResponse{}, nil
}

func (s *replicationServer) PromoteVolume(ctx context.Context, req *replication.PromoteVolumeRequest) (*replication.PromoteVolumeResponse, error) {
	vr, h, err := s.replicator(srcVolumeID(req.GetVolumeId(), req.GetReplicationSource()))
	if err != nil {
		return nil, err
	}
	if err := vr.PromoteVolume(ctx, h, req.GetForce(), req.GetParameters(), req.GetSecrets()); err != nil {
		return nil, toStatus(err, "promote volume")
	}
	klog.V(2).Infof("promoted %s (force=%v)", req.GetVolumeId(), req.GetForce())
	return &replication.PromoteVolumeResponse{}, nil
}

func (s *replicationServer) DemoteVolume(ctx context.Context, req *replication.DemoteVolumeRequest) (*replication.DemoteVolumeResponse, error) {
	vr, h, err := s.replicator(srcVolumeID(req.GetVolumeId(), req.GetReplicationSource()))
	if err != nil {
		return nil, err
	}
	if err := vr.DemoteVolume(ctx, h, req.GetForce(), req.GetParameters(), req.GetSecrets()); err != nil {
		return nil, toStatus(err, "demote volume")
	}
	klog.V(2).Infof("demoted %s (force=%v)", req.GetVolumeId(), req.GetForce())
	return &replication.DemoteVolumeResponse{}, nil
}

func (s *replicationServer) ResyncVolume(ctx context.Context, req *replication.ResyncVolumeRequest) (*replication.ResyncVolumeResponse, error) {
	vr, h, err := s.replicator(srcVolumeID(req.GetVolumeId(), req.GetReplicationSource()))
	if err != nil {
		return nil, err
	}
	ready, err := vr.ResyncVolume(ctx, h, req.GetForce(), req.GetParameters(), req.GetSecrets())
	if err != nil {
		return nil, toStatus(err, "resync volume")
	}
	klog.V(2).Infof("resynced %s (ready=%v)", req.GetVolumeId(), ready)
	return &replication.ResyncVolumeResponse{Ready: ready}, nil
}

func (s *replicationServer) GetVolumeReplicationInfo(ctx context.Context, req *replication.GetVolumeReplicationInfoRequest) (*replication.GetVolumeReplicationInfoResponse, error) {
	vr, h, err := s.replicator(srcVolumeID(req.GetVolumeId(), req.GetReplicationSource()))
	if err != nil {
		return nil, err
	}
	last, err := vr.GetVolumeReplicationInfo(ctx, h, req.GetSecrets())
	if err != nil {
		return nil, toStatus(err, "get volume replication info")
	}
	resp := &replication.GetVolumeReplicationInfoResponse{}
	if !last.IsZero() {
		resp.LastSyncTime = timestamppb.New(last)
	}
	return resp, nil
}
