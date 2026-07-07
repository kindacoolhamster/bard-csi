package driver

import (
	"context"
	"io"
	"os"
	"syscall"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/kindacoolhamster/bard-csi/internal/backend"
	"github.com/kindacoolhamster/bard-csi/internal/dispatch"
	"github.com/kindacoolhamster/bard-csi/internal/volumeid"
)

type nodeServer struct {
	csi.UnimplementedNodeServer
	driver *Driver
}

func (s *nodeServer) NodeGetInfo(_ context.Context, _ *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	resp := &csi.NodeGetInfoResponse{NodeId: s.driver.nodeID}
	if s.driver.maxVolumes > 0 {
		// Cap how many volumes the scheduler places here -- e.g. rbd-nbd is bounded
		// by the node's /dev/nbdN device count, so over-committing wedges staging.
		resp.MaxVolumesPerNode = s.driver.maxVolumes
	}
	if s.driver.zone != "" {
		// This is how the provisioner learns which zone a scheduled node is in,
		// and thus which backend instance CreateVolume should target.
		resp.AccessibleTopology = &csi.Topology{
			Segments: map[string]string{dispatch.TopologyKeyZone: s.driver.zone},
		}
	}
	return resp, nil
}

func (s *nodeServer) NodeGetCapabilities(_ context.Context, _ *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	caps := []csi.NodeServiceCapability_RPC_Type{
		csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
		csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
		csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
		// Lets NodeGetVolumeStats also report a VolumeCondition (volume health).
		csi.NodeServiceCapability_RPC_VOLUME_CONDITION,
		// Signals the driver understands the newer access modes, which is what
		// gates ReadWriteOncePod (SINGLE_NODE_SINGLE_WRITER) support in Kubernetes.
		csi.NodeServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER,
	}
	out := make([]*csi.NodeServiceCapability, 0, len(caps))
	for _, c := range caps {
		out = append(out, &csi.NodeServiceCapability{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{Type: c},
			},
		})
	}
	return &csi.NodeGetCapabilitiesResponse{Capabilities: out}, nil
}

func (s *nodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	if req.GetStagingTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "staging target path is required")
	}
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "volume capability is required")
	}
	h, be, err := s.resolve(req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	// One in-flight node op per volume: kubelet serializes per volume, but a
	// deadline-expired retry can race the still-running original (map/format/
	// LUKS-open must not interleave with themselves).
	done, err := s.driver.claim("node", req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	defer done()
	fsType, flags, block := capabilityDetails(req.GetVolumeCapability())
	if err := be.NodeStage(ctx, &backend.NodeStageRequest{
		Handle:         h,
		StagingPath:    req.GetStagingTargetPath(),
		FsType:         fsType,
		MountFlags:     flags,
		Block:          block,
		Exclusive:      exclusiveAccess(req.GetVolumeCapability()),
		Readonly:       readOnlyAccess(req.GetVolumeCapability()),
		Context:        req.GetVolumeContext(),
		PublishContext: req.GetPublishContext(),
		CrushLocation:  s.driver.crushLocation,
		Secrets:        req.GetSecrets(),
	}); err != nil {
		// toStatus maps backend sentinel errors (e.g. an invalid fsType or an
		// unsupported encryption combination -> InvalidArgument) so kubelet can
		// tell a permanent misconfiguration from a retryable failure.
		return nil, toStatus(err, "node stage")
	}
	return &csi.NodeStageVolumeResponse{}, nil
}

func (s *nodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	if req.GetStagingTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "staging target path is required")
	}
	h, be, err := s.resolve(req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	done, err := s.driver.claim("node", req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	defer done()
	if err := be.NodeUnstage(ctx, h, req.GetStagingTargetPath()); err != nil {
		return nil, toStatus(err, "node unstage")
	}
	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (s *nodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	if req.GetTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "volume capability is required")
	}
	h, be, err := s.resolve(req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	done, err := s.driver.claim("node", req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	defer done()
	fsType, flags, block := capabilityDetails(req.GetVolumeCapability())
	if err := be.NodePublish(ctx, &backend.NodePublishRequest{
		Handle:      h,
		StagingPath: req.GetStagingTargetPath(),
		TargetPath:  req.GetTargetPath(),
		FsType:      fsType,
		MountFlags:  flags,
		Readonly:    req.GetReadonly(),
		Block:       block,
		Context:     req.GetVolumeContext(),
	}); err != nil {
		return nil, toStatus(err, "node publish")
	}
	return &csi.NodePublishVolumeResponse{}, nil
}

func (s *nodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	if req.GetTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}
	h, be, err := s.resolve(req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	done, err := s.driver.claim("node", req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	defer done()
	if err := be.NodeUnpublish(ctx, h, req.GetTargetPath()); err != nil {
		return nil, toStatus(err, "node unpublish")
	}
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (s *nodeServer) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	if req.GetVolumePath() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume path is required")
	}
	// A raw block volume has no filesystem to grow -- the resized device is
	// already visible to the pod -- so node expansion is a successful no-op
	// (belt to ControllerExpandVolume's suspenders: it reports
	// node_expansion_required=false for block, but an older CO may call anyway).
	if req.GetVolumeCapability().GetBlock() != nil {
		return &csi.NodeExpandVolumeResponse{}, nil
	}
	h, be, err := s.resolve(req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	done, err := s.driver.claim("node", req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	defer done()
	newBytes, err := be.NodeExpand(ctx, h, req.GetVolumePath())
	if err != nil {
		return nil, toStatus(err, "node expand")
	}
	return &csi.NodeExpandVolumeResponse{CapacityBytes: newBytes}, nil
}

// NodeGetVolumeStats reports usage for a staged/published volume. It is backend
// agnostic: a filesystem volume is statfs'd for byte + inode usage; a raw block
// volume reports only its total size. The node core must be able to see the
// volume path (it mounts the kubelet dir read-only with HostToContainer
// propagation, so the plugin's mounts are visible here).
func (s *nodeServer) NodeGetVolumeStats(_ context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	volPath := req.GetVolumePath()
	if volPath == "" {
		return nil, status.Error(codes.InvalidArgument, "volume path is required")
	}
	info, err := os.Stat(volPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, status.Errorf(codes.NotFound, "volume path %q not found", volPath)
		}
		return nil, status.Errorf(codes.Internal, "stat %q: %v", volPath, err)
	}

	// Raw block: a device node, not a directory -> report total size only.
	if info.Mode()&os.ModeDir == 0 {
		total, _ := blockDeviceSize(volPath)
		return &csi.NodeGetVolumeStatsResponse{
			Usage:           []*csi.VolumeUsage{{Unit: csi.VolumeUsage_BYTES, Total: total}},
			VolumeCondition: &csi.VolumeCondition{Abnormal: false, Message: "block device is accessible"},
		}, nil
	}

	var st syscall.Statfs_t
	if err := syscall.Statfs(volPath, &st); err != nil {
		// The path exists but cannot be statfs'd: the mount is present yet not
		// healthy. Report it as an abnormal condition rather than erroring, so the
		// health monitor can surface it.
		return &csi.NodeGetVolumeStatsResponse{
			VolumeCondition: &csi.VolumeCondition{Abnormal: true, Message: "statfs failed: " + err.Error()},
		}, nil
	}
	bs := int64(st.Bsize)
	return &csi.NodeGetVolumeStatsResponse{
		VolumeCondition: &csi.VolumeCondition{Abnormal: false, Message: "mounted"},
		Usage: []*csi.VolumeUsage{
			{
				Unit:      csi.VolumeUsage_BYTES,
				Total:     int64(st.Blocks) * bs,
				Used:      int64(st.Blocks-st.Bfree) * bs,
				Available: int64(st.Bavail) * bs,
			},
			{
				Unit:      csi.VolumeUsage_INODES,
				Total:     int64(st.Files),
				Used:      int64(st.Files - st.Ffree),
				Available: int64(st.Ffree),
			},
		},
	}, nil
}

// blockDeviceSize returns a block device's size in bytes by seeking to its end.
// Best-effort: an unprivileged node core may not be able to open the device.
func blockDeviceSize(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return f.Seek(0, io.SeekEnd)
}

// resolve parses a volume id and looks up its backend, the common preamble to
// every node-plane call. An empty id is an InvalidArgument; a non-empty id we
// cannot parse or route is a volume we do not know about (NotFound). Callers
// must validate other required fields (paths, capabilities) *before* calling
// resolve, so a missing field is reported as InvalidArgument ahead of NotFound.
func (s *nodeServer) resolve(volumeID string) (volumeid.Handle, backend.Backend, error) {
	if volumeID == "" {
		return volumeid.Handle{}, nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	h, err := volumeid.Parse(volumeID)
	if err != nil {
		return volumeid.Handle{}, nil, status.Errorf(codes.NotFound, "unknown volume id: %v", err)
	}
	be, err := s.driver.snapshot().registry.Get(h.Backend)
	if err != nil {
		return volumeid.Handle{}, nil, status.Errorf(codes.NotFound, "%v", err)
	}
	return h, be, nil
}

// exclusiveAccess reports whether a volume capability is a single-node *writer*
// mode (ReadWriteOnce and the single-node RWOP/RWX variants). For those, only one
// node may write at a time, so the backend may fence a stale writer from a prior
// node on takeover. Multi-node and reader-only modes return false.
func exclusiveAccess(c *csi.VolumeCapability) bool {
	switch c.GetAccessMode().GetMode() {
	case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER:
		return true
	default:
		return false
	}
}

// readOnlyAccess reports whether the access mode is read-only (ReadOnlyMany or the
// single-node reader-only variant). A backend uses this to stage the volume
// read-only -- e.g. the Ceph RBD plugin maps the image `--read-only` so a ROX
// volume is write-protected at the Ceph client for every consumer, regardless of
// whether a particular pod set readOnly. This is the volume-level read-only
// contract (the access mode), distinct from a consumer mounting a writable volume
// read-only (the per-publish NodePublishVolumeRequest.readonly flag).
func readOnlyAccess(c *csi.VolumeCapability) bool {
	switch c.GetAccessMode().GetMode() {
	case csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY:
		return true
	default:
		return false
	}
}

// capabilityDetails extracts fsType, mount flags and whether a raw block device
// was requested from a CSI VolumeCapability.
func capabilityDetails(c *csi.VolumeCapability) (fsType string, flags []string, block bool) {
	if c == nil {
		return "", nil, false
	}
	if c.GetBlock() != nil {
		return "", nil, true
	}
	if m := c.GetMount(); m != nil {
		return m.GetFsType(), m.GetMountFlags(), false
	}
	return "", nil, false
}
