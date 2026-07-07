// Package backend defines the abstraction every storage backend implements.
//
// The CSI gRPC layer is intentionally backend-agnostic: it parses the request,
// resolves which backend instance should service it, and then calls through
// this interface. Backends are heterogeneous (Ceph RBD maps a block device on
// the node; NFS just mounts an export; LVM is node-local), so a backend
// advertises Capabilities that tell the generic layer how to treat its volumes
// rather than the layer assuming one shape.
package backend

import (
	"context"
	"errors"
	"time"

	"github.com/kindacoolhamster/bard-csi/internal/volumeid"
)

// Sentinel errors backends return so the CSI layer can translate them to the
// correct gRPC status codes. Wrap them with fmt.Errorf("...: %w", Err...) to add
// context; the CSI layer matches with errors.Is.
var (
	// ErrAlreadyExists means an object with the requested name already exists
	// but with incompatible properties (e.g. a different size). Maps to
	// codes.AlreadyExists.
	ErrAlreadyExists = errors.New("already exists with different properties")
	// ErrNotFound means the referenced backend object does not exist. Maps to
	// codes.NotFound.
	ErrNotFound = errors.New("not found")
	// ErrUnsupported means the backend does not implement an optional operation
	// (e.g. GetCapacity). Maps to codes.Unimplemented.
	ErrUnsupported = errors.New("operation not supported by backend")
	// ErrInvalidArgument means the request is malformed for this backend (e.g. a
	// mutable parameter key the backend does not support). Maps to
	// codes.InvalidArgument.
	ErrInvalidArgument = errors.New("invalid argument")
)

// Capabilities describes what a backend can do and how the CSI layer must
// treat its volumes.
type Capabilities struct {
	// RequiresControllerPublish is true when attaching a volume to a node is a
	// control-plane operation (e.g. cloud-disk attach). Ceph RBD maps on the
	// node itself, so this is false.
	RequiresControllerPublish bool
	// NodeLocal is true for backends whose volumes live on a single node (e.g.
	// LVM). Such volumes are topology-pinned to their creating node.
	NodeLocal bool
	// BlockDevice is true when the backend exposes a raw block device that the
	// node layer must format and mount (rbd, iscsi, cloud disk). File backends
	// (NFS, CephFS) are false.
	BlockDevice bool
	Snapshots   bool
	Expand      bool
	// GetCapacity is true when the backend can report available capacity (the
	// plugin implements CapacityReporter). Field order/types must mirror
	// bardplugin.Capabilities so plugin.Client can convert between them.
	GetCapacity bool
	// VolumeHealth is true when the backend can report a volume's condition (the
	// plugin implements HealthReporter). Field order/types must mirror
	// bardplugin.Capabilities so plugin.Client can convert between them.
	VolumeHealth bool
	// ModifyVolume is true when the backend can change a volume's mutable
	// parameters (the plugin implements VolumeModifier). Field order/types must
	// mirror bardplugin.Capabilities so plugin.Client can convert between them.
	ModifyVolume bool
	// ReclaimSpace is true when the backend can reclaim unused space from a
	// volume's backing store (the plugin implements SpaceReclaimer).
	ReclaimSpace bool
	// NodeReclaimSpace is true when the backend can reclaim space from the node
	// side of a mounted volume (the plugin implements NodeSpaceReclaimer).
	NodeReclaimSpace bool
	// ListVolumes is true when the backend can enumerate its volumes (the plugin
	// implements VolumeLister).
	ListVolumes bool
	// ListSnapshots is true when the backend can enumerate its snapshots (the
	// plugin implements SnapshotLister).
	ListSnapshots bool
	// NetworkFence is true when the backend can fence/unfence client network ranges
	// at the storage layer (the plugin implements NetworkFencer); Bard then serves
	// the csi-addons NetworkFence operation.
	NetworkFence bool
	// Replication is true when the backend can mirror a volume to a peer cluster
	// (the plugin implements VolumeReplicator); Bard then serves the csi-addons
	// VolumeReplication operation.
	Replication bool
	// EncryptionKeyRotation is true when the backend can rotate an encrypted volume's
	// key (the plugin implements EncryptionKeyRotator); Bard then serves the csi-addons
	// EncryptionKeyRotation operation.
	EncryptionKeyRotation bool
	// VolumeGroup is true when the backend can manage volume groups (the plugin
	// implements VolumeGrouper, e.g. ceph-rbd consistency groups); Bard then serves the
	// csi-addons VolumeGroup operation. Keep this last to match the
	// bardplugin.Capabilities field order (struct conversion).
	VolumeGroup bool
}

// CreateVolumeRequest is the backend-facing form of CSI CreateVolume. The CSI
// layer has already resolved topology to a concrete Instance before calling.
type CreateVolumeRequest struct {
	Name           string            // desired volume name (CSI-provided, idempotent)
	CapacityBytes  int64             // requested size
	Instance       string            // resolved backend instance / zone
	FsType         string            // requested filesystem ("" => backend default)
	Parameters     map[string]string // StorageClass parameters
	MutableParams  map[string]string // VolumeAttributesClass parameters (mutable)
	Secrets        map[string]string // provisioner secrets
	SourceSnapshot *volumeid.Handle  // optional: clone from this snapshot
	SourceVolume   *volumeid.Handle  // optional: clone from this volume
}

// Volume is the result of a successful CreateVolume.
type Volume struct {
	Handle             volumeid.Handle
	CapacityBytes      int64
	Context            map[string]string // returned to CSI as volume_context
	AccessibleTopology map[string]string // topology segment(s) the volume lives in
}

// CreateSnapshotRequest is the backend-facing form of CSI CreateSnapshot.
type CreateSnapshotRequest struct {
	Name         string
	SourceVolume volumeid.Handle
	Parameters   map[string]string
	Secrets      map[string]string
}

// VolumeHealth is a volume's reported condition (CSI VolumeCondition). Abnormal
// is true when the backing storage is unhealthy; Message explains either way.
type VolumeHealth struct {
	Abnormal bool
	Message  string
}

// SpaceUsage is a volume's backing-store usage before and after a reclaim, for
// the csi-addons ReclaimSpace pre/post report. A negative value means unknown.
type SpaceUsage struct {
	PreUsageBytes  int64
	PostUsageBytes int64
}

// Snapshot is the result of a successful CreateSnapshot.
type Snapshot struct {
	Handle         volumeid.Handle
	SourceVolumeID string
	SizeBytes      int64
	CreationTime   time.Time
	ReadyToUse     bool
}

// VolumeListEntry is one volume from ListVolumes. The CSI layer encodes Handle
// into the volume id.
type VolumeListEntry struct {
	Handle        volumeid.Handle
	CapacityBytes int64
}

// SnapshotListEntry is one snapshot from ListSnapshots. The CSI layer encodes
// Handle/SourceVolume into the snapshot id / source volume id.
type SnapshotListEntry struct {
	Handle       volumeid.Handle
	SourceVolume volumeid.Handle
	SizeBytes    int64
	CreationTime time.Time
	ReadyToUse   bool
}

// NodeStageRequest carries everything the node layer needs to make a volume
// available at a per-node staging path (map/attach + format + mount).
type NodeStageRequest struct {
	Handle      volumeid.Handle
	StagingPath string
	FsType      string
	MountFlags  []string
	Readonly    bool
	Block       bool // raw block volume requested (skip filesystem)
	// Exclusive is true for single-node-writer access modes; a backend may fence
	// a stale writer from a previous node before taking the volume over.
	Exclusive bool
	Context   map[string]string
	// PublishContext is the result of a prior ControllerPublish for this volume
	// on this node (CSI publish_context). Empty for backends that don't attach.
	PublishContext map[string]string
	// CrushLocation is the staging node's topology as a Ceph CRUSH location
	// ("region:r1|zone:z1"), derived by core from node labels. Node-level (same
	// for every volume staged on this node); a backend may use it for read
	// locality. Empty when no crush-location labels are configured.
	CrushLocation string
	Secrets       map[string]string
}

// NodePublishRequest carries everything needed to expose a staged volume at a
// pod's target path (typically a bind mount of the staging path).
type NodePublishRequest struct {
	Handle      volumeid.Handle
	StagingPath string
	TargetPath  string
	FsType      string
	MountFlags  []string
	Readonly    bool
	Block       bool
	Context     map[string]string
}

// Backend is the contract every storage backend implements. Methods are split
// into control-plane (run in the controller Deployment) and node-plane (run in
// the node DaemonSet) groups; a given process only exercises the half its mode
// enables.
type Backend interface {
	// Type returns the stable backend identifier, e.g. "ceph-rbd". It must
	// match the Backend field encoded in volume handles.
	Type() string
	Capabilities() Capabilities

	// Control-plane.
	CreateVolume(ctx context.Context, req *CreateVolumeRequest) (*Volume, error)
	DeleteVolume(ctx context.Context, h volumeid.Handle, secrets map[string]string) error
	ExpandVolume(ctx context.Context, h volumeid.Handle, newSizeBytes int64, secrets map[string]string) (newSizeBytes2 int64, nodeExpansionRequired bool, err error)
	CreateSnapshot(ctx context.Context, req *CreateSnapshotRequest) (*Snapshot, error)
	DeleteSnapshot(ctx context.Context, h volumeid.Handle, secrets map[string]string) error
	// GetCapacity reports bytes available to provision on the given instance for
	// the StorageClass parameters. Returns ErrUnsupported when the backend's
	// Capabilities.GetCapacity is false.
	GetCapacity(ctx context.Context, instance string, params map[string]string) (int64, error)
	// GetVolumeHealth reports the condition of an existing volume. Returns
	// ErrUnsupported when the backend's Capabilities.VolumeHealth is false.
	GetVolumeHealth(ctx context.Context, h volumeid.Handle, secrets map[string]string) (*VolumeHealth, error)
	// ModifyVolume changes a volume's mutable parameters (VolumeAttributesClass).
	// Returns ErrInvalidArgument for an unsupported parameter, ErrNotFound for a
	// missing volume, and ErrUnsupported when Capabilities.ModifyVolume is false.
	ModifyVolume(ctx context.Context, h volumeid.Handle, mutableParams, secrets map[string]string) error
	// ReclaimSpace reclaims unused space from a volume's backing store (the
	// csi-addons controller ReclaimSpace operation). Returns ErrUnsupported when
	// Capabilities.ReclaimSpace is false.
	ReclaimSpace(ctx context.Context, h volumeid.Handle, secrets map[string]string) (*SpaceUsage, error)
	// NodeReclaimSpace reclaims space from the node side of a mounted volume (the
	// csi-addons node ReclaimSpace operation, e.g. fstrim). Returns ErrUnsupported
	// when Capabilities.NodeReclaimSpace is false.
	NodeReclaimSpace(ctx context.Context, h volumeid.Handle, volumePath, stagingPath string, block bool, secrets map[string]string) (*SpaceUsage, error)
	// ControllerPublish attaches a volume to a node as a control-plane operation
	// (e.g. iSCSI LUN masking). It returns the publish context to thread into
	// NodeStage on that node. For backends whose Capabilities.RequiresControllerPublish
	// is false it is a no-op returning an empty context. Must be idempotent.
	ControllerPublish(ctx context.Context, h volumeid.Handle, nodeID string, readonly bool, volCtx, secrets map[string]string) (publishContext map[string]string, err error)
	// ControllerUnpublish detaches a volume from a node. A no-op for backends that
	// don't attach. Must be idempotent.
	ControllerUnpublish(ctx context.Context, h volumeid.Handle, nodeID string, secrets map[string]string) error
	// ListVolumes returns all of the backend's volumes; the CSI layer sorts,
	// paginates and aggregates across backends. Returns ErrUnsupported when
	// Capabilities.ListVolumes is false.
	ListVolumes(ctx context.Context) ([]VolumeListEntry, error)
	// ListSnapshots returns all of the backend's snapshots; the CSI layer filters,
	// sorts and paginates. Returns ErrUnsupported when Capabilities.ListSnapshots
	// is false.
	ListSnapshots(ctx context.Context) ([]SnapshotListEntry, error)

	// Node-plane.
	NodeStage(ctx context.Context, req *NodeStageRequest) error
	NodeUnstage(ctx context.Context, h volumeid.Handle, stagingPath string) error
	NodePublish(ctx context.Context, req *NodePublishRequest) error
	NodeUnpublish(ctx context.Context, h volumeid.Handle, targetPath string) error
	NodeExpand(ctx context.Context, h volumeid.Handle, volumePath string) (int64, error)
}

// NetworkFencer is an OPTIONAL interface a Backend may also implement when it can
// fence client network ranges at the storage layer (the csi-addons NetworkFence
// operation, e.g. Ceph `osd blocklist range`). It is cluster-scoped, not volume-
// scoped: instance selects the backend cluster and cidrs are the ranges. The CSI
// layer type-asserts for this and serves the FenceController service only when a
// registered backend implements it (Capabilities.NetworkFence). All idempotent.
type NetworkFencer interface {
	FenceClusterNetwork(ctx context.Context, instance string, cidrs []string, params, secrets map[string]string) error
	UnfenceClusterNetwork(ctx context.Context, instance string, cidrs []string, params, secrets map[string]string) error
	ListClusterFence(ctx context.Context, instance string, params, secrets map[string]string) ([]string, error)
	// GetFenceClients returns the client(s) a Fence/Unfence should target (the
	// csi-addons GET_CLIENTS_TO_FENCE capability) -- a pure read used by a DR
	// orchestrator to discover what to fence.
	GetFenceClients(ctx context.Context, instance string, params, secrets map[string]string) ([]FenceClient, error)
}

// FenceClient is one client a NetworkFence should target: an identity (the storage
// cluster's id, e.g. the Ceph FSID) and the client's local address ranges.
type FenceClient struct {
	ID    string
	CIDRs []string
}

// VolumeReplicator is an OPTIONAL interface a Backend may also implement when it
// can mirror a volume to a peer cluster (the csi-addons VolumeReplication
// operation, e.g. Ceph `rbd mirror image`). Volume-scoped: the CSI layer parses
// the volume handle and dispatches to the owning backend, and serves the
// Replication service only when a registered backend implements it
// (Capabilities.Replication). Enable/Disable/Promote/Demote must be idempotent.
type VolumeReplicator interface {
	EnableVolumeReplication(ctx context.Context, h volumeid.Handle, params, secrets map[string]string) error
	DisableVolumeReplication(ctx context.Context, h volumeid.Handle, params, secrets map[string]string) error
	PromoteVolume(ctx context.Context, h volumeid.Handle, force bool, params, secrets map[string]string) error
	DemoteVolume(ctx context.Context, h volumeid.Handle, force bool, params, secrets map[string]string) error
	ResyncVolume(ctx context.Context, h volumeid.Handle, force bool, params, secrets map[string]string) (ready bool, err error)
	GetVolumeReplicationInfo(ctx context.Context, h volumeid.Handle, secrets map[string]string) (lastSyncTime time.Time, err error)
}

// EncryptionKeyRotator is an OPTIONAL interface a Backend may also implement when it
// can rotate an encrypted volume's key in place (the csi-addons EncryptionKeyRotation
// operation). Node-scoped: the volume must be staged on the serving node, whose
// volumePath is its published path. The CSI layer type-asserts for this and serves
// the EncryptionKeyRotation service only when a registered backend implements it
// (Capabilities.EncryptionKeyRotation).
type EncryptionKeyRotator interface {
	RotateEncryptionKey(ctx context.Context, h volumeid.Handle, volumePath string, params, secrets map[string]string) error
}

// VolumeGroup is a backend consistency group: the group's own handle (Name is the
// backend group name, Location its pool) and the handles of its current members.
type VolumeGroup struct {
	Group   volumeid.Handle
	Members []volumeid.Handle
}

// VolumeGrouper is an OPTIONAL interface a Backend may also implement when it can
// manage consistency groups (the csi-addons VolumeGroup operation, e.g. Ceph `rbd
// group`). Cluster-scoped and controller-side; the CSI layer serves the VolumeGroup
// service only when a registered backend implements it (Capabilities.VolumeGroup). A
// group lives in ONE instance, so all members share the group's Instance (enforced by
// core). All ops idempotent; DeleteVolumeGroup must NOT delete member volumes.
type VolumeGrouper interface {
	CreateVolumeGroup(ctx context.Context, instance, pool, name string, members []volumeid.Handle, params, secrets map[string]string) (VolumeGroup, error)
	ModifyVolumeGroup(ctx context.Context, group volumeid.Handle, members []volumeid.Handle, params, secrets map[string]string) (VolumeGroup, error)
	DeleteVolumeGroup(ctx context.Context, group volumeid.Handle, secrets map[string]string) error
	GetVolumeGroup(ctx context.Context, group volumeid.Handle, secrets map[string]string) (VolumeGroup, error)
	ListVolumeGroups(ctx context.Context, secrets map[string]string) ([]VolumeGroup, error)
}
