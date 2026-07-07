// CSI-Addons is an out-of-CSI-spec extension API (github.com/csi-addons) served
// over its own gRPC endpoint and driven by the csi-addons sidecar + controller
// (e.g. a ReclaimSpaceJob). Bard serves the Identity service plus the operation
// services it can back; today that is controller-side ReclaimSpace, dispatched
// to whichever backend owns the volume (only ceph-rbd implements it). Serving the
// real csi-addons protos means a ceph-csi user's existing ReclaimSpaceJobs work
// against Bard unchanged.
package driver

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"

	"github.com/csi-addons/spec/lib/go/encryptionkeyrotation"
	"github.com/csi-addons/spec/lib/go/fence"
	"github.com/csi-addons/spec/lib/go/identity"
	"github.com/csi-addons/spec/lib/go/reclaimspace"
	"github.com/csi-addons/spec/lib/go/replication"
	"github.com/csi-addons/spec/lib/go/volumegroup"

	"github.com/kindacoolhamster/bard-csi/internal/backend"
	"github.com/kindacoolhamster/bard-csi/internal/volumeid"
)

// registerCSIAddons wires the csi-addons Identity plus the ReclaimSpace services
// onto a gRPC server, by mode: the controller serves the offline (controller)
// ReclaimSpace, the node the online (node, fstrim) ReclaimSpace. Each plane runs
// its own csi-addons sidecar, so a process registers only the half it runs.
func (d *Driver) registerCSIAddons(srv *grpc.Server) {
	identity.RegisterIdentityServer(srv, &csiAddonsIdentityServer{driver: d})
	if d.mode.Controller {
		reclaimspace.RegisterReclaimSpaceControllerServer(srv, &reclaimSpaceControllerServer{driver: d})
		// NetworkFence is a control-plane (controller) operation: node fencing for
		// failover/DR, driven by a NetworkFence CR. The capability is gated in
		// GetCapabilities, so the sidecar only invokes it when a backend can fence.
		fence.RegisterFenceControllerServer(srv, &fenceControllerServer{driver: d})
		// VolumeReplication (RBD mirroring for DR) is also controller-side, driven by
		// a VolumeReplication CR. Same capability gating.
		replication.RegisterControllerServer(srv, &replicationServer{driver: d})
		// VolumeGroup (consistency groups, e.g. rbd group) is controller-side too,
		// driven by VolumeGroup(Replication) CRs. Same capability gating.
		volumegroup.RegisterControllerServer(srv, &volumeGroupServer{driver: d})
	}
	if d.mode.Node {
		reclaimspace.RegisterReclaimSpaceNodeServer(srv, &reclaimSpaceNodeServer{driver: d})
		// EncryptionKeyRotation is a node-plane op (it needs the staged dm-crypt
		// device), despite the csi-addons service being named ...Controller.
		encryptionkeyrotation.RegisterEncryptionKeyRotationControllerServer(srv, &encryptionKeyRotationServer{driver: d})
	}
}

// anyBackendCap reports whether any currently-registered backend satisfies pred.
func (d *Driver) anyBackendCap(pred func(backend.Capabilities) bool) bool {
	bk := d.snapshot()
	for _, t := range bk.registry.Types() {
		if be, err := bk.registry.Get(t); err == nil && pred(be.Capabilities()) {
			return true
		}
	}
	return false
}

// csiAddonsIdentityServer answers the csi-addons Identity service the sidecar
// uses to discover which operations Bard supports.
type csiAddonsIdentityServer struct {
	identity.UnimplementedIdentityServer
	driver *Driver
}

func (s *csiAddonsIdentityServer) GetIdentity(context.Context, *identity.GetIdentityRequest) (*identity.GetIdentityResponse, error) {
	return &identity.GetIdentityResponse{Name: s.driver.name, VendorVersion: s.driver.version}, nil
}

func (s *csiAddonsIdentityServer) GetCapabilities(context.Context, *identity.GetCapabilitiesRequest) (*identity.GetCapabilitiesResponse, error) {
	var caps []*identity.Capability
	if s.driver.mode.Controller {
		caps = append(caps,
			service(identity.Capability_Service_CONTROLLER_SERVICE),
			// OFFLINE == the controller does the reclaim (rbd sparsify).
			reclaim(identity.Capability_ReclaimSpace_OFFLINE),
		)
		// NetworkFence only when a registered backend can fence (e.g. ceph-rbd via
		// osd blocklist range); otherwise the csi-addons controller would create a
		// no-op fencing path for a NetworkFence CR no backend can honour.
		if s.driver.anyBackendCap(func(c backend.Capabilities) bool { return c.NetworkFence }) {
			caps = append(caps, networkFence(), networkFenceGetClients())
		}
		// VolumeReplication only when a registered backend can mirror (ceph-rbd).
		if s.driver.anyBackendCap(func(c backend.Capabilities) bool { return c.Replication }) {
			caps = append(caps, volumeReplication())
		}
		// VolumeGroup only when a registered backend can manage consistency groups
		// (ceph-rbd via rbd group). Advertise the sub-capabilities Bard supports:
		// modify + get, never delete member volumes, one group per volume.
		if s.driver.anyBackendCap(func(c backend.Capabilities) bool { return c.VolumeGroup }) {
			caps = append(caps,
				volumeGroup(identity.Capability_VolumeGroup_VOLUME_GROUP),
				volumeGroup(identity.Capability_VolumeGroup_MODIFY_VOLUME_GROUP),
				volumeGroup(identity.Capability_VolumeGroup_GET_VOLUME_GROUP),
				volumeGroup(identity.Capability_VolumeGroup_DO_NOT_ALLOW_VG_TO_DELETE_VOLUMES),
				volumeGroup(identity.Capability_VolumeGroup_LIMIT_VOLUME_TO_ONE_VOLUME_GROUP),
			)
		}
	}
	if s.driver.mode.Node {
		caps = append(caps,
			service(identity.Capability_Service_NODE_SERVICE),
			// ONLINE == the node does the reclaim (fstrim) on a live mount.
			reclaim(identity.Capability_ReclaimSpace_ONLINE),
		)
		// EncryptionKeyRotation (node-plane) only when a registered backend can rotate
		// an encrypted volume's key (ceph-rbd LUKS via a stored-key KMS provider).
		if s.driver.anyBackendCap(func(c backend.Capabilities) bool { return c.EncryptionKeyRotation }) {
			caps = append(caps, encryptionKeyRotation())
		}
	}
	return &identity.GetCapabilitiesResponse{Capabilities: caps}, nil
}

func service(t identity.Capability_Service_Type) *identity.Capability {
	return &identity.Capability{Type: &identity.Capability_Service_{Service: &identity.Capability_Service{Type: t}}}
}

func reclaim(t identity.Capability_ReclaimSpace_Type) *identity.Capability {
	return &identity.Capability{Type: &identity.Capability_ReclaimSpace_{ReclaimSpace: &identity.Capability_ReclaimSpace{Type: t}}}
}

func networkFence() *identity.Capability {
	return &identity.Capability{Type: &identity.Capability_NetworkFence_{NetworkFence: &identity.Capability_NetworkFence{Type: identity.Capability_NetworkFence_NETWORK_FENCE}}}
}

func networkFenceGetClients() *identity.Capability {
	return &identity.Capability{Type: &identity.Capability_NetworkFence_{NetworkFence: &identity.Capability_NetworkFence{Type: identity.Capability_NetworkFence_GET_CLIENTS_TO_FENCE}}}
}

func volumeGroup(t identity.Capability_VolumeGroup_Type) *identity.Capability {
	return &identity.Capability{Type: &identity.Capability_VolumeGroup_{VolumeGroup: &identity.Capability_VolumeGroup{Type: t}}}
}

func volumeReplication() *identity.Capability {
	return &identity.Capability{Type: &identity.Capability_VolumeReplication_{VolumeReplication: &identity.Capability_VolumeReplication{Type: identity.Capability_VolumeReplication_VOLUME_REPLICATION}}}
}

func encryptionKeyRotation() *identity.Capability {
	return &identity.Capability{Type: &identity.Capability_EncryptionKeyRotation_{EncryptionKeyRotation: &identity.Capability_EncryptionKeyRotation{Type: identity.Capability_EncryptionKeyRotation_ENCRYPTIONKEYROTATION}}}
}

func (s *csiAddonsIdentityServer) Probe(context.Context, *identity.ProbeRequest) (*identity.ProbeResponse, error) {
	return &identity.ProbeResponse{}, nil
}

// reclaimSpaceControllerServer implements the csi-addons controller ReclaimSpace
// operation by dispatching to the backend that owns the volume.
type reclaimSpaceControllerServer struct {
	reclaimspace.UnimplementedReclaimSpaceControllerServer
	driver *Driver
}

func (s *reclaimSpaceControllerServer) ControllerReclaimSpace(ctx context.Context, req *reclaimspace.ControllerReclaimSpaceRequest) (*reclaimspace.ControllerReclaimSpaceResponse, error) {
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
	usage, err := be.ReclaimSpace(ctx, h, req.GetSecrets())
	if err != nil {
		return nil, toStatus(err, "reclaim space")
	}
	klog.V(2).Infof("reclaimed space on %s (used %d -> %d bytes)", req.GetVolumeId(), usage.PreUsageBytes, usage.PostUsageBytes)
	return &reclaimspace.ControllerReclaimSpaceResponse{
		PreUsage:  storageConsumption(usage.PreUsageBytes),
		PostUsage: storageConsumption(usage.PostUsageBytes),
	}, nil
}

// reclaimSpaceNodeServer implements the csi-addons node (online) ReclaimSpace
// operation by dispatching to the backend that owns the volume (e.g. fstrim).
type reclaimSpaceNodeServer struct {
	reclaimspace.UnimplementedReclaimSpaceNodeServer
	driver *Driver
}

func (s *reclaimSpaceNodeServer) NodeReclaimSpace(ctx context.Context, req *reclaimspace.NodeReclaimSpaceRequest) (*reclaimspace.NodeReclaimSpaceResponse, error) {
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
	block := req.GetVolumeCapability().GetBlock() != nil
	usage, err := be.NodeReclaimSpace(ctx, h, req.GetVolumePath(), req.GetStagingTargetPath(), block, req.GetSecrets())
	if err != nil {
		return nil, toStatus(err, "node reclaim space")
	}
	klog.V(2).Infof("node-reclaimed space on %s (path %s)", req.GetVolumeId(), req.GetVolumePath())
	return &reclaimspace.NodeReclaimSpaceResponse{
		PreUsage:  storageConsumption(usage.PreUsageBytes),
		PostUsage: storageConsumption(usage.PostUsageBytes),
	}, nil
}

// storageConsumption wraps a usage figure for the csi-addons response, or nil
// when the backend couldn't determine it (a negative value).
func storageConsumption(bytes int64) *reclaimspace.StorageConsumption {
	if bytes < 0 {
		return nil
	}
	return &reclaimspace.StorageConsumption{UsageBytes: bytes}
}

// fenceControllerServer implements the csi-addons NetworkFence operation by
// dispatching to the backend that can fence (e.g. ceph-rbd -> osd blocklist
// range). NetworkFence is cluster-scoped, not volume-scoped: the target backend
// cluster comes from the CR parameters (clusterID/instance), not a volume id.
type fenceControllerServer struct {
	fence.UnimplementedFenceControllerServer
	driver *Driver
}

func (s *fenceControllerServer) FenceClusterNetwork(ctx context.Context, req *fence.FenceClusterNetworkRequest) (*fence.FenceClusterNetworkResponse, error) {
	fencer, instance, err := s.driver.resolveFencer(req.GetParameters())
	if err != nil {
		return nil, err
	}
	if err := fencer.FenceClusterNetwork(ctx, instance, cidrStrings(req.GetCidrs()), req.GetParameters(), req.GetSecrets()); err != nil {
		return nil, toStatus(err, "fence cluster network")
	}
	klog.V(2).Infof("fenced %d network range(s) on instance %q", len(req.GetCidrs()), instance)
	return &fence.FenceClusterNetworkResponse{}, nil
}

func (s *fenceControllerServer) UnfenceClusterNetwork(ctx context.Context, req *fence.UnfenceClusterNetworkRequest) (*fence.UnfenceClusterNetworkResponse, error) {
	fencer, instance, err := s.driver.resolveFencer(req.GetParameters())
	if err != nil {
		return nil, err
	}
	if err := fencer.UnfenceClusterNetwork(ctx, instance, cidrStrings(req.GetCidrs()), req.GetParameters(), req.GetSecrets()); err != nil {
		return nil, toStatus(err, "unfence cluster network")
	}
	klog.V(2).Infof("unfenced %d network range(s) on instance %q", len(req.GetCidrs()), instance)
	return &fence.UnfenceClusterNetworkResponse{}, nil
}

func (s *fenceControllerServer) ListClusterFence(ctx context.Context, req *fence.ListClusterFenceRequest) (*fence.ListClusterFenceResponse, error) {
	fencer, instance, err := s.driver.resolveFencer(req.GetParameters())
	if err != nil {
		return nil, err
	}
	cidrs, err := fencer.ListClusterFence(ctx, instance, req.GetParameters(), req.GetSecrets())
	if err != nil {
		return nil, toStatus(err, "list cluster fence")
	}
	out := make([]*fence.CIDR, 0, len(cidrs))
	for _, c := range cidrs {
		out = append(out, &fence.CIDR{Cidr: c})
	}
	return &fence.ListClusterFenceResponse{Cidrs: out}, nil
}

func (s *fenceControllerServer) GetFenceClients(ctx context.Context, req *fence.GetFenceClientsRequest) (*fence.GetFenceClientsResponse, error) {
	fencer, instance, err := s.driver.resolveFencer(req.GetParameters())
	if err != nil {
		return nil, err
	}
	clients, err := fencer.GetFenceClients(ctx, instance, req.GetParameters(), req.GetSecrets())
	if err != nil {
		return nil, toStatus(err, "get fence clients")
	}
	out := make([]*fence.ClientDetails, 0, len(clients))
	for _, cl := range clients {
		addrs := make([]*fence.CIDR, 0, len(cl.CIDRs))
		for _, c := range cl.CIDRs {
			addrs = append(addrs, &fence.CIDR{Cidr: c})
		}
		out = append(out, &fence.ClientDetails{Id: cl.ID, Addresses: addrs})
	}
	return &fence.GetFenceClientsResponse{Clients: out}, nil
}

// resolveFencer finds the registered backend that can fence and the target
// instance from the NetworkFence CR parameters. The instance selector is
// clusterID (ceph-csi-compatible) or instance; backend optionally disambiguates
// when more than one backend can fence.
func (d *Driver) resolveFencer(params map[string]string) (backend.NetworkFencer, string, error) {
	instance := params["clusterID"]
	if instance == "" {
		instance = params["instance"]
	}
	wantType := params["backend"]
	bk := d.snapshot()
	var found backend.Backend
	for _, t := range bk.registry.Types() {
		be, err := bk.registry.Get(t)
		if err != nil || !be.Capabilities().NetworkFence {
			continue
		}
		if wantType != "" && t != wantType {
			continue
		}
		if found != nil {
			return nil, "", status.Error(codes.InvalidArgument, "multiple backends can fence; set parameters.backend")
		}
		found = be
	}
	if found == nil {
		return nil, "", status.Error(codes.Unimplemented, "no registered backend supports NetworkFence")
	}
	fencer, ok := found.(backend.NetworkFencer)
	if !ok {
		return nil, "", status.Error(codes.Unimplemented, "backend advertises NetworkFence but does not implement it")
	}
	return fencer, instance, nil
}

func cidrStrings(cidrs []*fence.CIDR) []string {
	out := make([]string, 0, len(cidrs))
	for _, c := range cidrs {
		if v := c.GetCidr(); v != "" {
			out = append(out, v)
		}
	}
	return out
}
