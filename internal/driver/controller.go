package driver

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kindacoolhamster/bard-csi/internal/backend"
	"github.com/kindacoolhamster/bard-csi/internal/dispatch"
	"github.com/kindacoolhamster/bard-csi/internal/volumeid"
)

// toStatus maps a backend error to the appropriate gRPC status. Backend sentinel
// errors carry the semantic meaning; everything else is an Internal error.
func toStatus(err error, op string) error {
	switch {
	case errors.Is(err, backend.ErrAlreadyExists):
		return status.Errorf(codes.AlreadyExists, "%s: %v", op, err)
	case errors.Is(err, backend.ErrNotFound):
		return status.Errorf(codes.NotFound, "%s: %v", op, err)
	case errors.Is(err, backend.ErrInvalidArgument):
		return status.Errorf(codes.InvalidArgument, "%s: %v", op, err)
	case errors.Is(err, backend.ErrUnsupported):
		return status.Errorf(codes.Unimplemented, "%s: %v", op, err)
	default:
		return status.Errorf(codes.Internal, "%s: %v", op, err)
	}
}

type controllerServer struct {
	csi.UnimplementedControllerServer
	driver *Driver
}

func (s *controllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume name is required")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume capabilities are required")
	}
	// One in-flight create per CO volume name (its idempotency key).
	done, err := s.driver.claim("volume-name", req.GetName())
	if err != nil {
		return nil, err
	}
	defer done()

	bk := s.driver.snapshot() // one consistent registry+dispatcher pair for this call
	preferred, requisite := zonesFrom(req.GetAccessibilityRequirements())
	res, err := bk.disp.Resolve(req.GetParameters(), preferred, requisite)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "dispatch: %v", err)
	}
	be, err := bk.registry.Get(res.Backend)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	caps := be.Capabilities()
	for _, c := range req.GetVolumeCapabilities() {
		if !accessModeSupported(c, caps) {
			return nil, status.Errorf(codes.InvalidArgument,
				"backend %q cannot serve access mode %s for a filesystem volume (multi-node requires volumeMode: Block)",
				res.Backend, c.GetAccessMode().GetMode())
		}
	}

	srcSnap, srcVol, err := contentSource(req.GetVolumeContentSource())
	if err != nil {
		return nil, err
	}

	out, err := be.CreateVolume(ctx, &backend.CreateVolumeRequest{
		Name:           req.GetName(),
		CapacityBytes:  req.GetCapacityRange().GetRequiredBytes(),
		Instance:       res.Instance,
		FsType:         req.GetParameters()["fsType"],
		Parameters:     req.GetParameters(),
		MutableParams:  req.GetMutableParameters(),
		Secrets:        req.GetSecrets(),
		SourceSnapshot: srcSnap,
		SourceVolume:   srcVol,
	})
	if err != nil {
		return nil, toStatus(err, "create volume")
	}

	vol := &csi.Volume{
		VolumeId:      out.Handle.String(),
		CapacityBytes: out.CapacityBytes,
		VolumeContext: out.Context,
		ContentSource: req.GetVolumeContentSource(),
	}
	// Pin the volume's accessible topology to the zone of its backend instance,
	// so the scheduler only places pods on nodes that can reach it.
	if res.Zone != "" {
		vol.AccessibleTopology = []*csi.Topology{
			{Segments: map[string]string{dispatch.TopologyKeyZone: res.Zone}},
		}
	}
	return &csi.CreateVolumeResponse{Volume: vol}, nil
}

func (s *controllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	h, err := volumeid.Parse(req.GetVolumeId())
	if err != nil {
		// A non-empty but unparseable id is one we never created; per the CSI
		// spec, deleting an unknown volume is a successful no-op.
		return &csi.DeleteVolumeResponse{}, nil
	}
	done, err := s.driver.claim("volume", req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	defer done()
	be, err := s.driver.snapshot().registry.Get(h.Backend)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	if err := be.DeleteVolume(ctx, h, req.GetSecrets()); err != nil {
		return nil, toStatus(err, "delete volume")
	}
	return &csi.DeleteVolumeResponse{}, nil
}

func (s *controllerServer) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	if req.GetCapacityRange() == nil {
		return nil, status.Error(codes.InvalidArgument, "capacity range is required")
	}
	h, err := volumeid.Parse(req.GetVolumeId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "unknown volume id: %v", err)
	}
	done, err := s.driver.claim("volume", req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	defer done()
	be, err := s.driver.snapshot().registry.Get(h.Backend)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	newBytes, nodeExpansion, err := be.ExpandVolume(ctx, h, req.GetCapacityRange().GetRequiredBytes(), req.GetSecrets())
	if err != nil {
		return nil, toStatus(err, "expand volume")
	}
	// A raw block volume has no filesystem to grow: the pod sees the enlarged
	// device as soon as the backend resize lands, so never ask the CO for a node
	// expansion (kubelet would call NodeExpandVolume, which has nothing to do).
	if req.GetVolumeCapability().GetBlock() != nil {
		nodeExpansion = false
	}
	return &csi.ControllerExpandVolumeResponse{
		CapacityBytes:         newBytes,
		NodeExpansionRequired: nodeExpansion,
	}, nil
}

func (s *controllerServer) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "snapshot name is required")
	}
	if req.GetSourceVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "source volume id is required")
	}
	src, err := volumeid.Parse(req.GetSourceVolumeId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "source volume id: %v", err)
	}
	// One in-flight create per CO snapshot name (its idempotency key).
	done, err := s.driver.claim("snapshot-name", req.GetName())
	if err != nil {
		return nil, err
	}
	defer done()
	be, err := s.driver.snapshot().registry.Get(src.Backend)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	snap, err := be.CreateSnapshot(ctx, &backend.CreateSnapshotRequest{
		Name:         req.GetName(),
		SourceVolume: src,
		Parameters:   req.GetParameters(),
		Secrets:      req.GetSecrets(),
	})
	if err != nil {
		return nil, toStatus(err, "create snapshot")
	}
	return &csi.CreateSnapshotResponse{
		Snapshot: &csi.Snapshot{
			SnapshotId:     snap.Handle.String(),
			SourceVolumeId: snap.SourceVolumeID,
			SizeBytes:      snap.SizeBytes,
			CreationTime:   timestamppb.New(snap.CreationTime),
			ReadyToUse:     snap.ReadyToUse,
		},
	}, nil
}

func (s *controllerServer) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	if req.GetSnapshotId() == "" {
		return nil, status.Error(codes.InvalidArgument, "snapshot id is required")
	}
	h, err := volumeid.Parse(req.GetSnapshotId())
	if err != nil {
		return &csi.DeleteSnapshotResponse{}, nil // non-empty but unknown => no-op
	}
	done, err := s.driver.claim("snapshot", req.GetSnapshotId())
	if err != nil {
		return nil, err
	}
	defer done()
	be, err := s.driver.snapshot().registry.Get(h.Backend)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	if err := be.DeleteSnapshot(ctx, h, req.GetSecrets()); err != nil {
		return nil, toStatus(err, "delete snapshot")
	}
	return &csi.DeleteSnapshotResponse{}, nil
}

func (s *controllerServer) ValidateVolumeCapabilities(_ context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume capabilities are required")
	}
	h, err := volumeid.Parse(req.GetVolumeId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "unknown volume id: %v", err)
	}
	be, err := s.driver.snapshot().registry.Get(h.Backend)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "unknown volume id: %v", err)
	}
	caps := be.Capabilities()
	for _, c := range req.GetVolumeCapabilities() {
		if !accessModeSupported(c, caps) {
			// Leaving Confirmed unset (with a message) is how CSI reports that the
			// driver does NOT support the requested capability set.
			return &csi.ValidateVolumeCapabilitiesResponse{
				Message: fmt.Sprintf("backend %q cannot serve access mode %s for a filesystem volume (multi-node requires volumeMode: Block)",
					h.Backend, c.GetAccessMode().GetMode()),
			}, nil
		}
	}
	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: req.GetVolumeCapabilities(),
		},
	}, nil
}

// accessModeSupported reports whether a backend can honor a volume capability's
// access mode. A multi-node mode (RWX/ROX) is safe for a raw block volume on any
// backend, and for a backend that is itself a shared filesystem (cephfs/nfs,
// BlockDevice=false), but NOT for a filesystem on a block-device backend: a single
// rbd image cannot back a shared cross-node mount -- each node's filesystem would
// corrupt the others. Single-node modes are always supported.
func accessModeSupported(c *csi.VolumeCapability, caps backend.Capabilities) bool {
	switch c.GetAccessMode().GetMode() {
	case csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER,
		csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER:
		return c.GetBlock() != nil || !caps.BlockDevice
	default:
		return true
	}
}

func (s *controllerServer) ControllerGetCapabilities(_ context.Context, _ *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	caps := []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
		csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
		csi.ControllerServiceCapability_RPC_CLONE_VOLUME,
		csi.ControllerServiceCapability_RPC_GET_CAPACITY,
		csi.ControllerServiceCapability_RPC_GET_VOLUME,
		csi.ControllerServiceCapability_RPC_VOLUME_CONDITION,
		csi.ControllerServiceCapability_RPC_MODIFY_VOLUME,
	}
	// Advertise PUBLISH_UNPUBLISH_VOLUME only when a registered backend actually
	// attaches (e.g. iSCSI). Claiming it otherwise would make the external-attacher
	// run a no-op for every volume and assert node-existence semantics no
	// node-mapped backend can satisfy. The deploy's attachRequired + attacher
	// sidecar are toggled to match.
	if s.anyBackend(func(c backend.Capabilities) bool { return c.RequiresControllerPublish }) {
		caps = append(caps, csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME)
	}
	// List* are advertised only when a backend can enumerate, so csi-sanity's list
	// assertions run only where they can be satisfied.
	if s.anyBackend(func(c backend.Capabilities) bool { return c.ListVolumes }) {
		caps = append(caps, csi.ControllerServiceCapability_RPC_LIST_VOLUMES)
	}
	if s.anyBackend(func(c backend.Capabilities) bool { return c.ListSnapshots }) {
		caps = append(caps, csi.ControllerServiceCapability_RPC_LIST_SNAPSHOTS)
	}
	out := make([]*csi.ControllerServiceCapability, 0, len(caps))
	for _, c := range caps {
		out = append(out, &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{Type: c},
			},
		})
	}
	return &csi.ControllerGetCapabilitiesResponse{Capabilities: out}, nil
}

// GetCapacity reports bytes available to provision for the given StorageClass
// parameters + topology. It resolves the backend instance the same way
// CreateVolume does, then asks that backend for real numbers.
//
// When capacity is not tracked here -- the class/topology resolves to nothing,
// or the backend doesn't implement capacity reporting -- it returns a very large
// value rather than 0 or an error. A present 0 makes the scheduler treat the
// class as FULL (blocking pods), and an error leaves the provisioner retrying a
// stale entry; "effectively unlimited" correctly means "capacity is not a
// scheduling constraint for this backend" while real provisioning errors still
// surface at CreateVolume.
func (s *controllerServer) GetCapacity(ctx context.Context, req *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	const unlimited = math.MaxInt64

	bk := s.driver.snapshot()
	var preferred []string
	if seg := req.GetAccessibleTopology().GetSegments(); seg != nil {
		if z := seg[dispatch.TopologyKeyZone]; z != "" {
			preferred = []string{z}
		}
	}
	res, err := bk.disp.Resolve(req.GetParameters(), preferred, nil)
	if err != nil {
		return &csi.GetCapacityResponse{AvailableCapacity: unlimited}, nil
	}
	be, err := bk.registry.Get(res.Backend)
	if err != nil {
		return &csi.GetCapacityResponse{AvailableCapacity: unlimited}, nil
	}
	avail, err := be.GetCapacity(ctx, res.Instance, req.GetParameters())
	if err != nil {
		if errors.Is(err, backend.ErrUnsupported) {
			return &csi.GetCapacityResponse{AvailableCapacity: unlimited}, nil
		}
		return nil, status.Errorf(codes.Internal, "get capacity: %v", err)
	}
	return &csi.GetCapacityResponse{AvailableCapacity: avail}, nil
}

// ControllerGetVolume reports a volume's existence and, when the backend
// supports health monitoring, its condition. The external-health-monitor
// sidecar polls this to flag volumes whose backing storage has gone abnormal
// (e.g. an rbd image deleted out of band). A backend that doesn't report health
// returns the volume with no condition rather than an error, so health is purely
// additive.
func (s *controllerServer) ControllerGetVolume(ctx context.Context, req *csi.ControllerGetVolumeRequest) (*csi.ControllerGetVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	h, err := volumeid.Parse(req.GetVolumeId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "unknown volume id: %v", err)
	}
	be, err := s.driver.snapshot().registry.Get(h.Backend)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	resp := &csi.ControllerGetVolumeResponse{
		Volume: &csi.Volume{VolumeId: req.GetVolumeId()},
		Status: &csi.ControllerGetVolumeResponse_VolumeStatus{},
	}
	// ControllerGetVolume carries no secrets; the plugin resolves its own
	// per-instance cephx key (like every other control-plane call here).
	health, err := be.GetVolumeHealth(ctx, h, nil)
	if err != nil {
		if errors.Is(err, backend.ErrUnsupported) {
			return resp, nil // backend has no health signal; volume info only
		}
		return nil, toStatus(err, "get volume")
	}
	resp.Status.VolumeCondition = &csi.VolumeCondition{
		Abnormal: health.Abnormal,
		Message:  health.Message,
	}
	return resp, nil
}

// ControllerModifyVolume changes a volume's mutable parameters (CSI
// VolumeAttributesClass). The mutable parameters are backend-defined; the plugin
// validates them and rejects an unsupported key with InvalidArgument.
func (s *controllerServer) ControllerModifyVolume(ctx context.Context, req *csi.ControllerModifyVolumeRequest) (*csi.ControllerModifyVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	h, err := volumeid.Parse(req.GetVolumeId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "unknown volume id: %v", err)
	}
	done, err := s.driver.claim("volume", req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	defer done()
	be, err := s.driver.snapshot().registry.Get(h.Backend)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	if err := be.ModifyVolume(ctx, h, req.GetMutableParameters(), req.GetSecrets()); err != nil {
		return nil, toStatus(err, "modify volume")
	}
	return &csi.ControllerModifyVolumeResponse{}, nil
}

// anyBackend reports whether any currently-registered backend satisfies pred --
// used to gate optional controller capabilities to backends that support them.
func (s *controllerServer) anyBackend(pred func(backend.Capabilities) bool) bool {
	bk := s.driver.snapshot()
	for _, t := range bk.registry.Types() {
		be, err := bk.registry.Get(t)
		if err == nil && pred(be.Capabilities()) {
			return true
		}
	}
	return false
}

// ControllerPublishVolume attaches a volume to a node (CSI ControllerPublishVolume).
// Bard is one CSI driver serving many backends, so the external-attacher calls
// this for every volume when attachRequired is on; node-mapped backends (Ceph
// RBD, LVM) return an empty publish context as an immediate no-op, while an
// attach backend (iSCSI) does the real per-node masking and returns the
// connection context that the matching NodeStage will consume.
func (s *controllerServer) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	if req.GetNodeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "node id is required")
	}
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "volume capability is required")
	}
	h, err := volumeid.Parse(req.GetVolumeId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "unknown volume id: %v", err)
	}
	// Keyed volume@node: publishes of one volume to different nodes may run
	// concurrently (legitimate for multi-node volumes); duplicates may not.
	done, err := s.driver.claim("attach", req.GetVolumeId()+"@"+req.GetNodeId())
	if err != nil {
		return nil, err
	}
	defer done()
	be, err := s.driver.snapshot().registry.Get(h.Backend)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	pubCtx, err := be.ControllerPublish(ctx, h, req.GetNodeId(), req.GetReadonly(), req.GetVolumeContext(), req.GetSecrets())
	if err != nil {
		return nil, toStatus(err, "publish volume")
	}
	return &csi.ControllerPublishVolumeResponse{PublishContext: pubCtx}, nil
}

// ControllerUnpublishVolume detaches a volume from a node (CSI
// ControllerUnpublishVolume). Idempotent and a no-op for node-mapped backends.
func (s *controllerServer) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	h, err := volumeid.Parse(req.GetVolumeId())
	if err != nil {
		// Unknown volume id: nothing we created is attached, so detach is a no-op.
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}
	done, err := s.driver.claim("attach", req.GetVolumeId()+"@"+req.GetNodeId())
	if err != nil {
		return nil, err
	}
	defer done()
	be, err := s.driver.snapshot().registry.Get(h.Backend)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	if err := be.ControllerUnpublish(ctx, h, req.GetNodeId(), req.GetSecrets()); err != nil {
		return nil, toStatus(err, "unpublish volume")
	}
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

// ListVolumes enumerates volumes across all backends that support listing,
// sorted by volume id and paginated by the CSI starting_token/max_entries.
// Backends that don't implement listing are simply skipped.
func (s *controllerServer) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	bk := s.driver.snapshot()
	var entries []*csi.ListVolumesResponse_Entry
	for _, t := range bk.registry.Types() {
		be, err := bk.registry.Get(t)
		if err != nil {
			continue
		}
		vols, err := be.ListVolumes(ctx)
		if err != nil {
			if errors.Is(err, backend.ErrUnsupported) {
				continue
			}
			return nil, toStatus(err, "list volumes")
		}
		for _, v := range vols {
			entries = append(entries, &csi.ListVolumesResponse_Entry{
				Volume: &csi.Volume{VolumeId: v.Handle.String(), CapacityBytes: v.CapacityBytes},
			})
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].GetVolume().GetVolumeId() < entries[j].GetVolume().GetVolumeId()
	})
	page, next, err := paginate(entries, req.GetStartingToken(), req.GetMaxEntries())
	if err != nil {
		return nil, err
	}
	return &csi.ListVolumesResponse{Entries: page, NextToken: next}, nil
}

// ListSnapshots enumerates snapshots across listing-capable backends, optionally
// filtered by snapshot id or source volume id, sorted by snapshot id and
// paginated. An unknown filter id yields an empty list (per CSI).
func (s *controllerServer) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	bk := s.driver.snapshot()
	var entries []*csi.ListSnapshotsResponse_Entry
	for _, t := range bk.registry.Types() {
		be, err := bk.registry.Get(t)
		if err != nil {
			continue
		}
		snaps, err := be.ListSnapshots(ctx)
		if err != nil {
			if errors.Is(err, backend.ErrUnsupported) {
				continue
			}
			return nil, toStatus(err, "list snapshots")
		}
		for _, sn := range snaps {
			sid, srcID := sn.Handle.String(), sn.SourceVolume.String()
			if f := req.GetSnapshotId(); f != "" && f != sid {
				continue
			}
			if f := req.GetSourceVolumeId(); f != "" && f != srcID {
				continue
			}
			entries = append(entries, &csi.ListSnapshotsResponse_Entry{
				Snapshot: &csi.Snapshot{
					SnapshotId:     sid,
					SourceVolumeId: srcID,
					SizeBytes:      sn.SizeBytes,
					CreationTime:   timestamppb.New(sn.CreationTime),
					ReadyToUse:     sn.ReadyToUse,
				},
			})
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].GetSnapshot().GetSnapshotId() < entries[j].GetSnapshot().GetSnapshotId()
	})
	page, next, err := paginate(entries, req.GetStartingToken(), req.GetMaxEntries())
	if err != nil {
		return nil, err
	}
	return &csi.ListSnapshotsResponse{Entries: page, NextToken: next}, nil
}

// paginate slices a sorted entry list by the CSI starting_token (an integer
// offset) and max_entries, returning the page and the next token ("" when
// exhausted). An unparseable or out-of-range token is a CSI Aborted error.
func paginate[T any](entries []T, startingToken string, maxEntries int32) ([]T, string, error) {
	offset := 0
	if startingToken != "" {
		n, err := strconv.Atoi(startingToken)
		if err != nil || n < 0 {
			return nil, "", status.Errorf(codes.Aborted, "invalid starting_token %q", startingToken)
		}
		offset = n
	}
	if offset > len(entries) {
		return nil, "", status.Errorf(codes.Aborted, "starting_token %q out of range", startingToken)
	}
	page := entries[offset:]
	next := ""
	if maxEntries > 0 && int(maxEntries) < len(page) {
		next = strconv.Itoa(offset + int(maxEntries))
		page = page[:maxEntries]
	}
	return page, next, nil
}

// ---- helpers -------------------------------------------------------------

func zonesFrom(tr *csi.TopologyRequirement) (preferred, requisite []string) {
	if tr == nil {
		return nil, nil
	}
	for _, t := range tr.GetPreferred() {
		if z, ok := t.GetSegments()[dispatch.TopologyKeyZone]; ok {
			preferred = append(preferred, z)
		}
	}
	for _, t := range tr.GetRequisite() {
		if z, ok := t.GetSegments()[dispatch.TopologyKeyZone]; ok {
			requisite = append(requisite, z)
		}
	}
	return preferred, requisite
}

func contentSource(cs *csi.VolumeContentSource) (snap, vol *volumeid.Handle, err error) {
	if cs == nil {
		return nil, nil, nil
	}
	if src := cs.GetSnapshot(); src != nil {
		h, perr := volumeid.Parse(src.GetSnapshotId())
		if perr != nil {
			return nil, nil, status.Errorf(codes.NotFound, "source snapshot id: %v", perr)
		}
		snap = &h
	}
	if src := cs.GetVolume(); src != nil {
		h, perr := volumeid.Parse(src.GetVolumeId())
		if perr != nil {
			return nil, nil, status.Errorf(codes.NotFound, "source volume id: %v", perr)
		}
		vol = &h
	}
	return snap, vol, nil
}
