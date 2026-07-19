// Package bardplugin is the public SDK for writing out-of-tree Bard CSI storage
// backends. A plugin is just an HTTP server speaking JSON over a unix socket;
// Go authors can implement the Backend interface and call Serve, and authors in
// any other language can implement the same small HTTP+JSON contract directly.
//
// Bard discovers a plugin via config (a backend with a `plugin.endpoint` socket
// path) and proxies every backend operation to it. Adding a backend therefore
// needs no change to Bard's code or binary: ship a plugin container as a sidecar
// and add a config entry.
//
// # Wire contract
//
// Each operation is an HTTP POST to a fixed path with a JSON request body and a
// JSON response body. Success is HTTP 200. Failure is a non-200 status with an
// Error body; the Code field maps to CSI semantics (see ErrorCode).
//
//	POST /info               -> Info
//	POST /volume/create      CreateVolumeRequest      -> CreateVolumeResponse
//	POST /volume/delete      DeleteVolumeRequest      -> {}
//	POST /volume/expand      ExpandVolumeRequest      -> ExpandVolumeResponse
//	POST /snapshot/create    CreateSnapshotRequest    -> CreateSnapshotResponse
//	POST /snapshot/delete    DeleteSnapshotRequest    -> {}
//	POST /controller/publish   ControllerPublishRequest   -> ControllerPublishResponse (optional)
//	POST /controller/unpublish ControllerUnpublishRequest -> {}                         (optional)
//	POST /volume/list        ListVolumesRequest       -> ListVolumesResponse       (optional)
//	POST /snapshot/list      ListSnapshotsRequest     -> ListSnapshotsResponse     (optional)
//	POST /node/stage         NodeStageRequest         -> {}
//	POST /node/unstage       NodeUnstageRequest       -> {}
//	POST /node/publish       NodePublishRequest       -> {}
//	POST /node/unpublish     NodeUnpublishRequest     -> {}
//	POST /node/expand        NodeExpandRequest        -> NodeExpandResponse
//	POST /capacity           GetCapacityRequest       -> GetCapacityResponse       (optional)
//	POST /volume/health      GetVolumeHealthRequest   -> GetVolumeHealthResponse   (optional)
//	POST /volume/modify      ModifyVolumeRequest      -> ModifyVolumeResponse      (optional)
//	POST /volume/reclaimspace ReclaimSpaceRequest     -> ReclaimSpaceResponse      (optional)
//	POST /node/reclaimspace  NodeReclaimSpaceRequest  -> ReclaimSpaceResponse      (optional)
//	POST /networkfence/fence   FenceClusterNetworkRequest   -> {}                       (optional)
//	POST /networkfence/unfence UnfenceClusterNetworkRequest -> {}                       (optional)
//	POST /networkfence/list    ListClusterFenceRequest      -> ListClusterFenceResponse (optional)
//	POST /networkfence/clients GetFenceClientsRequest       -> GetFenceClientsResponse  (optional)
//	POST /replication/enable   EnableReplicationRequest     -> {}                        (optional)
//	POST /replication/disable  DisableReplicationRequest    -> {}                        (optional)
//	POST /replication/promote  PromoteVolumeRequest         -> {}                        (optional)
//	POST /replication/demote   DemoteVolumeRequest          -> {}                        (optional)
//	POST /replication/resync   ResyncVolumeRequest          -> ResyncVolumeResponse      (optional)
//	POST /replication/info     ReplicationInfoRequest       -> ReplicationInfoResponse   (optional)
//	POST /node/rotate-key      RotateEncryptionKeyRequest   -> {}                        (optional)
//	POST /volumegroup/create   CreateVolumeGroupRequest     -> VolumeGroupResponse       (optional)
//	POST /volumegroup/modify   ModifyVolumeGroupRequest     -> VolumeGroupResponse       (optional)
//	POST /volumegroup/delete   DeleteVolumeGroupRequest     -> {}                        (optional)
//	POST /volumegroup/get      GetVolumeGroupRequest        -> VolumeGroupResponse       (optional)
//	POST /volumegroup/list     ListVolumeGroupsRequest      -> ListVolumeGroupsResponse  (optional)
//
// # Contract version and compatibility
//
// The wire contract is versioned MAJOR.MINOR, independently of Bard releases;
// the current version is ContractVersion. A plugin reports the version it
// implements in Info.ContractVersion (the SDK fills it in automatically; an
// absent or empty value means "1.0", the pre-versioning contract). Bard
// refuses a plugin whose MAJOR it does not support.
//
// Within a MAJOR version the contract only grows, and only compatibly, so a
// plugin built against contract 1.0 keeps working, unchanged, with every Bard
// release that speaks major 1:
//
//   - Existing routes and fields are never removed, renamed, or given new
//     meaning.
//   - New operations arrive as new routes gated by new capability flags in
//     Info; Bard never calls an optional route the plugin did not advertise.
//   - New fields on existing messages are optional, and absent means the old
//     behavior. Both sides ignore JSON fields they do not know (the default
//     for Go's and Python's decoders -- do not enable a strict mode).
//
// A MINOR bump marks that new optional surface exists. A MAJOR bump is a
// breaking change: rare, announced in release notes ahead of time, and shipped
// with a transition period during which Bard accepts both majors.
//
// The bard-plugin-conformance tool (cmd/bard-plugin-conformance) drives a
// plugin over its socket and verifies the required semantics plus every
// optional capability the plugin declares. See docs/writing-a-plugin.md.
package bardplugin

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// ContractVersion is the wire-contract version this package defines, as
// "MAJOR.MINOR". The SDK reports it from /info when a Backend's Info does not
// set ContractVersion explicitly.
const ContractVersion = "1.0"

// ContractMajor is the contract MAJOR version this Bard supports. Core
// refuses a plugin that reports a different major.
const ContractMajor = 1

// ParseContractVersion parses an Info.ContractVersion of the form
// "MAJOR.MINOR". The empty string is the pre-versioning contract and parses
// as 1.0.
func ParseContractVersion(s string) (major, minor int, err error) {
	if s == "" {
		return 1, 0, nil
	}
	majS, minS, ok := strings.Cut(s, ".")
	if !ok {
		return 0, 0, fmt.Errorf("contract version %q: want MAJOR.MINOR", s)
	}
	if major, err = strconv.Atoi(majS); err != nil || major < 0 {
		return 0, 0, fmt.Errorf("contract version %q: bad major", s)
	}
	if minor, err = strconv.Atoi(minS); err != nil || minor < 0 {
		return 0, 0, fmt.Errorf("contract version %q: bad minor", s)
	}
	return major, minor, nil
}

// Route paths for the wire contract. Shared by the SDK server and Bard's client.
const (
	PathInfo                  = "/info"
	PathCreateVolume          = "/volume/create"
	PathDeleteVolume          = "/volume/delete"
	PathExpandVolume          = "/volume/expand"
	PathCreateSnapshot        = "/snapshot/create"
	PathDeleteSnapshot        = "/snapshot/delete"
	PathControllerPublish     = "/controller/publish"
	PathControllerUnpublish   = "/controller/unpublish"
	PathListVolumes           = "/volume/list"
	PathListSnapshots         = "/snapshot/list"
	PathNodeStage             = "/node/stage"
	PathNodeUnstage           = "/node/unstage"
	PathNodePublish           = "/node/publish"
	PathNodeUnpublish         = "/node/unpublish"
	PathNodeExpand            = "/node/expand"
	PathGetCapacity           = "/capacity"
	PathVolumeHealth          = "/volume/health"
	PathModifyVolume          = "/volume/modify"
	PathReclaimSpace          = "/volume/reclaimspace"
	PathNodeReclaimSpace      = "/node/reclaimspace"
	PathFenceClusterNetwork   = "/networkfence/fence"
	PathUnfenceClusterNetwork = "/networkfence/unfence"
	PathListClusterFence      = "/networkfence/list"
	PathGetFenceClients       = "/networkfence/clients"
	PathEnableReplication     = "/replication/enable"
	PathDisableReplication    = "/replication/disable"
	PathPromoteVolume         = "/replication/promote"
	PathDemoteVolume          = "/replication/demote"
	PathResyncVolume          = "/replication/resync"
	PathReplicationInfo       = "/replication/info"
	PathRotateEncryptionKey   = "/node/rotate-key"
	PathCreateVolumeGroup     = "/volumegroup/create"
	PathModifyVolumeGroup     = "/volumegroup/modify"
	PathDeleteVolumeGroup     = "/volumegroup/delete"
	PathGetVolumeGroup        = "/volumegroup/get"
	PathListVolumeGroups      = "/volumegroup/list"
)

// ErrorCode lets a plugin signal CSI-relevant outcomes; Bard maps these to gRPC
// status codes. Unknown/empty maps to Internal.
type ErrorCode string

const (
	CodeInternal      ErrorCode = "Internal"
	CodeNotFound      ErrorCode = "NotFound"
	CodeAlreadyExists ErrorCode = "AlreadyExists"
	CodeInvalidArg    ErrorCode = "InvalidArgument"
	// CodeUnsupported signals a request that is well-formed but permanently
	// unsupported by this backend/instance (e.g. an operation a particular
	// management mode cannot do safely). Bard maps it to a TERMINAL,
	// non-retried CSI failure -- unlike CodeInvalidArg this is not about a bad
	// request, but a capability the plugin will never grant on retry.
	CodeUnsupported ErrorCode = "Unsupported"
)

// Error is the JSON body returned with a non-200 status.
type Error struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

// Capabilities describes what a backend can do and how Bard must treat its
// volumes. Mirrors Bard's internal capability model.
type Capabilities struct {
	// RequiresControllerPublish is true when attaching a volume to a node is a
	// control-plane operation (e.g. iSCSI LUN masking, cloud-disk attach). Set
	// automatically when the backend implements ControllerPublisher; Bard then
	// calls /controller/publish before NodeStage and /controller/unpublish after
	// NodeUnstage. Node-mapped backends (Ceph RBD, LVM) leave it false.
	RequiresControllerPublish bool `json:"requiresControllerPublish"`
	NodeLocal                 bool `json:"nodeLocal"`
	BlockDevice               bool `json:"blockDevice"`
	Snapshots                 bool `json:"snapshots"`
	Expand                    bool `json:"expand"`
	// GetCapacity is true when the plugin implements CapacityReporter (the
	// /capacity route). Bard only calls it when this is set.
	GetCapacity bool `json:"getCapacity,omitempty"`
	// VolumeHealth is true when the plugin implements HealthReporter (the
	// /volume/health route). Bard only calls it when this is set.
	VolumeHealth bool `json:"volumeHealth,omitempty"`
	// ModifyVolume is true when the plugin implements VolumeModifier (the
	// /volume/modify route). Bard only calls it when this is set.
	ModifyVolume bool `json:"modifyVolume,omitempty"`
	// ReclaimSpace is true when the plugin implements SpaceReclaimer (the
	// /volume/reclaimspace route). Bard only calls it when this is set, and
	// advertises the csi-addons controller ReclaimSpace operation.
	ReclaimSpace bool `json:"reclaimSpace,omitempty"`
	// NodeReclaimSpace is true when the plugin implements NodeSpaceReclaimer (the
	// /node/reclaimspace route). Bard only calls it when this is set, and
	// advertises the csi-addons node (online) ReclaimSpace operation.
	NodeReclaimSpace bool `json:"nodeReclaimSpace,omitempty"`
	// ListVolumes is true when the plugin implements VolumeLister (the /volume/list
	// route). Bard aggregates + paginates across backends; the plugin just returns
	// all of its volumes.
	ListVolumes bool `json:"listVolumes,omitempty"`
	// ListSnapshots is true when the plugin implements SnapshotLister (the
	// /snapshot/list route).
	ListSnapshots bool `json:"listSnapshots,omitempty"`
	// NetworkFence is true when the plugin implements NetworkFencer (the
	// /networkfence/* routes). Bard then advertises the csi-addons NetworkFence
	// operation.
	NetworkFence bool `json:"networkFence,omitempty"`
	// Replication is true when the plugin implements VolumeReplicator (the
	// /replication/* routes). Bard then advertises the csi-addons VolumeReplication
	// operation.
	Replication bool `json:"replication,omitempty"`
	// EncryptionKeyRotation is true when the plugin implements EncryptionKeyRotator
	// (the /node/rotate-key route). Bard then advertises the csi-addons
	// EncryptionKeyRotation operation.
	EncryptionKeyRotation bool `json:"encryptionKeyRotation,omitempty"`
	// VolumeGroup is true when the plugin implements VolumeGrouper (the /volumegroup/*
	// routes). Bard then advertises the csi-addons VolumeGroup operation. Keep this LAST
	// to match backend.Capabilities field order (the plugin.Client converts between the
	// two structs positionally).
	VolumeGroup bool `json:"volumeGroup,omitempty"`
}

// Info is returned from /info and declares the backend's identity + capabilities.
type Info struct {
	Type string `json:"type"`
	// ContractVersion is the wire-contract version ("MAJOR.MINOR") the plugin
	// implements. Empty means 1.0. The Go SDK fills in the current version
	// automatically; plugins in other languages set it themselves.
	ContractVersion string       `json:"contractVersion,omitempty"`
	Capabilities    Capabilities `json:"capabilities"`
}

// VolumeRef identifies a volume to the plugin. Location and Name are chosen by
// the plugin on create and echoed back on every later call; together with the
// Instance they form the backend-owned identity. Neither may contain '|', and
// the combined encoding stays within the CSI 128-byte volume_id limit (Bard
// also encodes the backend type + instance), so keep them compact.
type VolumeRef struct {
	Instance string `json:"instance"`
	Location string `json:"location,omitempty"`
	Name     string `json:"name"`
}

// CreateVolumeRequest asks the plugin to provision a volume on a resolved
// instance. Topology has already been resolved to Instance by Bard's dispatcher.
type CreateVolumeRequest struct {
	Name           string            `json:"name"`
	CapacityBytes  int64             `json:"capacityBytes"`
	Instance       string            `json:"instance"`
	FsType         string            `json:"fsType,omitempty"`
	Parameters     map[string]string `json:"parameters,omitempty"`
	MutableParams  map[string]string `json:"mutableParams,omitempty"` // VolumeAttributesClass params
	Secrets        map[string]string `json:"secrets,omitempty"`
	SourceSnapshot *VolumeRef        `json:"sourceSnapshot,omitempty"`
	SourceVolume   *VolumeRef        `json:"sourceVolume,omitempty"`
}

// CreateVolumeResponse returns the plugin-chosen identity and the result.
type CreateVolumeResponse struct {
	Location           string            `json:"location,omitempty"`
	Name               string            `json:"name"`
	CapacityBytes      int64             `json:"capacityBytes"`
	Context            map[string]string `json:"context,omitempty"`
	AccessibleTopology map[string]string `json:"accessibleTopology,omitempty"`
}

type DeleteVolumeRequest struct {
	Volume  VolumeRef         `json:"volume"`
	Secrets map[string]string `json:"secrets,omitempty"`
}

type ExpandVolumeRequest struct {
	Volume       VolumeRef         `json:"volume"`
	NewSizeBytes int64             `json:"newSizeBytes"`
	Secrets      map[string]string `json:"secrets,omitempty"`
}

type ExpandVolumeResponse struct {
	CapacityBytes         int64 `json:"capacityBytes"`
	NodeExpansionRequired bool  `json:"nodeExpansionRequired"`
}

type CreateSnapshotRequest struct {
	Name         string            `json:"name"`
	SourceVolume VolumeRef         `json:"sourceVolume"`
	Parameters   map[string]string `json:"parameters,omitempty"`
	Secrets      map[string]string `json:"secrets,omitempty"`
}

type CreateSnapshotResponse struct {
	Location         string `json:"location,omitempty"`
	Name             string `json:"name"`
	SourceVolumeID   string `json:"sourceVolumeId"`
	SizeBytes        int64  `json:"sizeBytes"`
	CreationTimeUnix int64  `json:"creationTimeUnix"`
	ReadyToUse       bool   `json:"readyToUse"`
}

type DeleteSnapshotRequest struct {
	Snapshot VolumeRef         `json:"snapshot"`
	Secrets  map[string]string `json:"secrets,omitempty"`
}

// ControllerPublishRequest asks the plugin to attach a volume to a node as a
// control-plane operation (CSI ControllerPublishVolume) -- e.g. masking an iSCSI
// LUN to the node's initiator. NodeID is the CSI node id of the node the volume
// is being attached to (a backend may derive the node's initiator identity from
// it). The returned PublishContext is carried by Bard to NodeStage on that node.
type ControllerPublishRequest struct {
	Volume   VolumeRef         `json:"volume"`
	NodeID   string            `json:"nodeId"`
	Readonly bool              `json:"readonly,omitempty"`
	Context  map[string]string `json:"context,omitempty"` // volume context
	Secrets  map[string]string `json:"secrets,omitempty"`
}

// ControllerPublishResponse returns the per-attachment context (e.g. iSCSI
// portal/IQN/LUN) that Bard threads into the matching NodeStage's PublishContext.
type ControllerPublishResponse struct {
	PublishContext map[string]string `json:"publishContext,omitempty"`
}

// ControllerUnpublishRequest asks the plugin to detach a volume from a node (CSI
// ControllerUnpublishVolume) -- e.g. removing the node's iSCSI ACL. Must be
// idempotent: a repeated unpublish for an already-detached node succeeds.
type ControllerUnpublishRequest struct {
	Volume  VolumeRef         `json:"volume"`
	NodeID  string            `json:"nodeId"`
	Secrets map[string]string `json:"secrets,omitempty"`
}

type NodeStageRequest struct {
	Volume      VolumeRef `json:"volume"`
	StagingPath string    `json:"stagingPath"`
	FsType      string    `json:"fsType,omitempty"`
	MountFlags  []string  `json:"mountFlags,omitempty"`
	Readonly    bool      `json:"readonly,omitempty"`
	Block       bool      `json:"block,omitempty"`
	// PublishContext is the result of a prior ControllerPublish for this volume on
	// this node (CSI publish_context). Empty for backends that don't attach.
	PublishContext map[string]string `json:"publishContext,omitempty"`
	// Exclusive is true for single-node-writer access modes (ReadWriteOnce and
	// the single-node RWOP/RWX variants). A backend may use it to fence a stale
	// writer from a previous node before taking the volume over -- the single-
	// writer multi-attach safety guarantee. False for multi-node (shared) modes.
	Exclusive bool              `json:"exclusive,omitempty"`
	Context   map[string]string `json:"context,omitempty"`
	// CrushLocation is the staging node's topology formatted as a Ceph CRUSH
	// location ("region:r1|zone:z1"), derived by core from the node's labels. A
	// backend may use it for read locality (e.g. RBD read-affinity). Empty when
	// core has no crush-location labels configured. Node-level, not per-volume.
	CrushLocation string            `json:"crushLocation,omitempty"`
	Secrets       map[string]string `json:"secrets,omitempty"`
}

type NodeUnstageRequest struct {
	Volume      VolumeRef `json:"volume"`
	StagingPath string    `json:"stagingPath"`
}

type NodePublishRequest struct {
	Volume      VolumeRef         `json:"volume"`
	StagingPath string            `json:"stagingPath"`
	TargetPath  string            `json:"targetPath"`
	FsType      string            `json:"fsType,omitempty"`
	MountFlags  []string          `json:"mountFlags,omitempty"`
	Readonly    bool              `json:"readonly,omitempty"`
	Block       bool              `json:"block,omitempty"`
	Context     map[string]string `json:"context,omitempty"`
}

type NodeUnpublishRequest struct {
	Volume     VolumeRef `json:"volume"`
	TargetPath string    `json:"targetPath"`
}

type NodeExpandRequest struct {
	Volume     VolumeRef `json:"volume"`
	VolumePath string    `json:"volumePath"`
}

type NodeExpandResponse struct {
	CapacityBytes int64 `json:"capacityBytes"`
}

// GetCapacityRequest asks for the available capacity of a backend instance for
// the given StorageClass parameters (e.g. which pool). Instance is the topology-
// resolved instance, or "" when the request carries no topology.
type GetCapacityRequest struct {
	Instance   string            `json:"instance,omitempty"`
	Parameters map[string]string `json:"parameters,omitempty"`
}

// GetCapacityResponse reports the bytes available to provision new volumes.
type GetCapacityResponse struct {
	AvailableBytes int64 `json:"availableBytes"`
}

// GetVolumeHealthRequest asks the plugin to report the condition of an existing
// volume (CSI volume health monitoring). Used by Bard's ControllerGetVolume,
// which the external-health-monitor sidecar polls.
type GetVolumeHealthRequest struct {
	Volume  VolumeRef         `json:"volume"`
	Secrets map[string]string `json:"secrets,omitempty"`
}

// ModifyVolumeRequest asks the plugin to change a volume's mutable parameters
// (CSI ControllerModifyVolume / VolumeAttributesClass). The plugin must reject a
// parameter key it does not support with CodeInvalidArg.
type ModifyVolumeRequest struct {
	Volume        VolumeRef         `json:"volume"`
	MutableParams map[string]string `json:"mutableParams,omitempty"`
	Secrets       map[string]string `json:"secrets,omitempty"`
}

// ModifyVolumeResponse is the (empty) success result of ModifyVolume.
type ModifyVolumeResponse struct{}

// GetVolumeHealthResponse reports a volume's condition. Abnormal is true when
// the backing storage is unhealthy (e.g. the image was deleted out of band);
// Message is a human-readable explanation either way.
type GetVolumeHealthResponse struct {
	Abnormal bool   `json:"abnormal"`
	Message  string `json:"message,omitempty"`
}

// Backend is the interface a Go plugin implements. Pass an implementation to
// Serve. Plugins in other languages implement the equivalent HTTP+JSON routes.
//
// A plugin runs in two contexts: the control-plane methods are invoked in
// Bard's controller pod, the Node* methods in Bard's node DaemonSet. Implement
// only what your Capabilities advertise; for unsupported operations return an
// Error with CodeInvalidArg (or succeed as a no-op where the CSI spec allows).
type Backend interface {
	Info() Info

	CreateVolume(ctx context.Context, req *CreateVolumeRequest) (*CreateVolumeResponse, error)
	DeleteVolume(ctx context.Context, req *DeleteVolumeRequest) error
	ExpandVolume(ctx context.Context, req *ExpandVolumeRequest) (*ExpandVolumeResponse, error)
	CreateSnapshot(ctx context.Context, req *CreateSnapshotRequest) (*CreateSnapshotResponse, error)
	DeleteSnapshot(ctx context.Context, req *DeleteSnapshotRequest) error

	NodeStage(ctx context.Context, req *NodeStageRequest) error
	NodeUnstage(ctx context.Context, req *NodeUnstageRequest) error
	NodePublish(ctx context.Context, req *NodePublishRequest) error
	NodeUnpublish(ctx context.Context, req *NodeUnpublishRequest) error
	NodeExpand(ctx context.Context, req *NodeExpandRequest) (*NodeExpandResponse, error)
}

// CapacityReporter is an OPTIONAL interface a Backend may also implement to
// answer the /capacity route (CSI GetCapacity). Plugins that don't implement it
// simply don't advertise Capabilities.GetCapacity, and Bard won't call it.
type CapacityReporter interface {
	GetCapacity(ctx context.Context, req *GetCapacityRequest) (*GetCapacityResponse, error)
}

// HealthReporter is an OPTIONAL interface a Backend may also implement to answer
// the /volume/health route (CSI volume health monitoring). Plugins that don't
// implement it simply don't advertise Capabilities.VolumeHealth, and Bard won't
// call it.
type HealthReporter interface {
	GetVolumeHealth(ctx context.Context, req *GetVolumeHealthRequest) (*GetVolumeHealthResponse, error)
}

// VolumeModifier is an OPTIONAL interface a Backend may also implement to answer
// the /volume/modify route (CSI ControllerModifyVolume / VolumeAttributesClass).
// A plugin that implements it should also validate the mutable parameter keys it
// receives on CreateVolume (returning CodeInvalidArg for unknown keys), since a
// volume may be created with a VolumeAttributesClass already set.
type VolumeModifier interface {
	ModifyVolume(ctx context.Context, req *ModifyVolumeRequest) (*ModifyVolumeResponse, error)
}

// ControllerPublisher is an OPTIONAL interface a Backend may also implement when
// attaching a volume to a node is a control-plane operation (CSI
// ControllerPublishVolume/Unpublish). Implementing it sets
// Capabilities.RequiresControllerPublish and wires the /controller/publish and
// /controller/unpublish routes; Bard then calls ControllerPublish before NodeStage
// (threading the returned PublishContext into the NodeStage on that node) and
// ControllerUnpublish after NodeUnstage. Both must be idempotent. Node-mapped
// backends (Ceph RBD, LVM) don't implement it -- the volume becomes available
// entirely within NodeStage.
type ControllerPublisher interface {
	ControllerPublish(ctx context.Context, req *ControllerPublishRequest) (*ControllerPublishResponse, error)
	ControllerUnpublish(ctx context.Context, req *ControllerUnpublishRequest) error
}

// ListVolumesRequest carries no parameters: Bard aggregates and paginates across
// all backends, so a plugin returns ALL of its volumes in one response.
type ListVolumesRequest struct{}

// VolumeListEntry is one volume in a ListVolumes response. Location/Name (with the
// instance) are the plugin-owned identity Bard encodes into the CSI volume id.
type VolumeListEntry struct {
	Volume        VolumeRef `json:"volume"`
	CapacityBytes int64     `json:"capacityBytes,omitempty"`
}

type ListVolumesResponse struct {
	Entries []VolumeListEntry `json:"entries"`
}

// ListSnapshotsRequest carries no parameters; Bard does the filtering (by source
// volume / snapshot id) and pagination across backends.
type ListSnapshotsRequest struct{}

// SnapshotListEntry is one snapshot in a ListSnapshots response. SourceVolume is
// the volume it was taken from; Bard encodes both into CSI ids.
type SnapshotListEntry struct {
	Snapshot         VolumeRef `json:"snapshot"`
	SourceVolume     VolumeRef `json:"sourceVolume"`
	SizeBytes        int64     `json:"sizeBytes,omitempty"`
	CreationTimeUnix int64     `json:"creationTimeUnix,omitempty"`
	ReadyToUse       bool      `json:"readyToUse"`
}

type ListSnapshotsResponse struct {
	Entries []SnapshotListEntry `json:"entries"`
}

// VolumeLister is an OPTIONAL interface a Backend may also implement to answer the
// /volume/list route (CSI ListVolumes). It returns ALL of the backend's volumes;
// Bard sorts, paginates (max_entries/starting_token) and aggregates across
// backends. Implementing it sets Capabilities.ListVolumes.
type VolumeLister interface {
	ListVolumes(ctx context.Context, req *ListVolumesRequest) (*ListVolumesResponse, error)
}

// SnapshotLister is an OPTIONAL interface a Backend may also implement to answer
// the /snapshot/list route (CSI ListSnapshots). It returns ALL of the backend's
// snapshots; Bard filters (by source volume / snapshot id), sorts and paginates.
// Implementing it sets Capabilities.ListSnapshots.
type SnapshotLister interface {
	ListSnapshots(ctx context.Context, req *ListSnapshotsRequest) (*ListSnapshotsResponse, error)
}

// ReclaimSpaceRequest asks the plugin to reclaim unused space from a volume's
// backing store (e.g. `rbd sparsify` for Ceph RBD). Maps to the csi-addons
// controller ReclaimSpace operation, which Bard serves for a ReclaimSpaceJob.
type ReclaimSpaceRequest struct {
	Volume  VolumeRef         `json:"volume"`
	Secrets map[string]string `json:"secrets,omitempty"`
}

// ReclaimSpaceResponse optionally reports the volume's backing-store usage in
// bytes before and after the reclaim, for the csi-addons pre/post usage. A
// negative value means "unknown" (Bard then omits that side of the report).
type ReclaimSpaceResponse struct {
	PreUsageBytes  int64 `json:"preUsageBytes,omitempty"`
	PostUsageBytes int64 `json:"postUsageBytes,omitempty"`
}

// SpaceReclaimer is an OPTIONAL interface a Backend may also implement to answer
// the /volume/reclaimspace route. Implementing it makes Bard advertise the
// csi-addons controller (offline) ReclaimSpace capability and dispatch a
// ReclaimSpaceJob's controller phase here. Backends with no notion of space
// reclamation (a plain network filesystem) simply don't implement it.
type SpaceReclaimer interface {
	ReclaimSpace(ctx context.Context, req *ReclaimSpaceRequest) (*ReclaimSpaceResponse, error)
}

// NodeReclaimSpaceRequest asks the plugin to reclaim space from the node side of
// a mounted volume (e.g. `fstrim` on the filesystem, which issues discards down
// to the backing store). Maps to the csi-addons node ReclaimSpace operation.
// VolumePath is the volume's published path; StagingPath is its staging mount (if
// known); Block is true for a raw block volume (no filesystem to trim).
type NodeReclaimSpaceRequest struct {
	Volume      VolumeRef         `json:"volume"`
	VolumePath  string            `json:"volumePath"`
	StagingPath string            `json:"stagingPath,omitempty"`
	Block       bool              `json:"block,omitempty"`
	Secrets     map[string]string `json:"secrets,omitempty"`
}

// NodeSpaceReclaimer is an OPTIONAL interface a Backend may also implement to
// answer the /node/reclaimspace route. Implementing it makes Bard advertise the
// csi-addons node (online) ReclaimSpace capability and dispatch a ReclaimSpaceJob's
// node phase here. It reuses ReclaimSpaceResponse for the optional pre/post usage.
type NodeSpaceReclaimer interface {
	NodeReclaimSpace(ctx context.Context, req *NodeReclaimSpaceRequest) (*ReclaimSpaceResponse, error)
}

// FenceClusterNetworkRequest asks the plugin to fence a set of client network
// ranges at the storage layer (csi-addons NetworkFence) -- e.g. `ceph osd
// blocklist range add` so a partitioned/failed node can no longer reach the
// storage before its volumes are failed over. Unlike volume operations this is
// cluster-scoped: Instance selects the backend cluster (resolved by Bard from the
// NetworkFence CR's parameters), CIDRs are the ranges to fence, and Secrets carry
// the credentials for a user permitted to manage the blocklist (which the per-
// volume provisioning user generally is NOT). Parameters passes the remaining
// NetworkFence CR parameters through unchanged.
type FenceClusterNetworkRequest struct {
	Instance   string            `json:"instance,omitempty"`
	CIDRs      []string          `json:"cidrs"`
	Parameters map[string]string `json:"parameters,omitempty"`
	Secrets    map[string]string `json:"secrets,omitempty"`
}

// UnfenceClusterNetworkRequest asks the plugin to remove the fence on a set of
// client network ranges (csi-addons UnfenceClusterNetwork) -- e.g. `ceph osd
// blocklist range rm` when a previously-fenced node is safe to readmit. Must be
// idempotent: unfencing a range that is not blocklisted succeeds.
type UnfenceClusterNetworkRequest struct {
	Instance   string            `json:"instance,omitempty"`
	CIDRs      []string          `json:"cidrs"`
	Parameters map[string]string `json:"parameters,omitempty"`
	Secrets    map[string]string `json:"secrets,omitempty"`
}

// ListClusterFenceRequest asks the plugin for the currently-fenced network ranges
// (csi-addons ListClusterFence). Instance selects the backend cluster.
type ListClusterFenceRequest struct {
	Instance   string            `json:"instance,omitempty"`
	Parameters map[string]string `json:"parameters,omitempty"`
	Secrets    map[string]string `json:"secrets,omitempty"`
}

// ListClusterFenceResponse reports the currently-fenced CIDRs.
type ListClusterFenceResponse struct {
	CIDRs []string `json:"cidrs"`
}

// GetFenceClientsRequest asks the plugin for the client(s) that a subsequent
// Fence/UnfenceClusterNetwork should target (csi-addons GetFenceClients, the
// GET_CLIENTS_TO_FENCE capability). Cluster-scoped: Instance selects the backend
// cluster. A DR orchestrator (Ramen) calls it to discover what to fence.
type GetFenceClientsRequest struct {
	Instance   string            `json:"instance,omitempty"`
	Parameters map[string]string `json:"parameters,omitempty"`
	Secrets    map[string]string `json:"secrets,omitempty"`
}

// FenceClient is one client to fence: an identity (the Ceph cluster FSID, matching
// ceph-csi) and the client's local address ranges (CIDRs).
type FenceClient struct {
	ID    string   `json:"id"`
	CIDRs []string `json:"cidrs"`
}

// GetFenceClientsResponse lists the clients that need to be fenced.
type GetFenceClientsResponse struct {
	Clients []FenceClient `json:"clients"`
}

// NetworkFencer is an OPTIONAL interface a Backend may also implement to answer
// the /networkfence/* routes (csi-addons NetworkFence). Implementing it sets
// Capabilities.NetworkFence and makes Bard serve the csi-addons FenceController
// service, so a ceph-csi user's existing NetworkFence CRs (node fencing for
// failover/DR) work against Bard. It is a control-plane (controller) operation.
// All four must be idempotent (GetFenceClients is a pure read).
type NetworkFencer interface {
	FenceClusterNetwork(ctx context.Context, req *FenceClusterNetworkRequest) error
	UnfenceClusterNetwork(ctx context.Context, req *UnfenceClusterNetworkRequest) error
	ListClusterFence(ctx context.Context, req *ListClusterFenceRequest) (*ListClusterFenceResponse, error)
	GetFenceClients(ctx context.Context, req *GetFenceClientsRequest) (*GetFenceClientsResponse, error)
}

// Replication requests are volume-scoped (Volume identifies the image to mirror).
// Parameters come from the VolumeReplicationClass; Secrets are its referenced
// credentials (a backend may fall back to its own per-instance credentials).

// EnableReplicationRequest asks the plugin to start mirroring a volume to its
// peer cluster (csi-addons EnableVolumeReplication) -- e.g. `rbd mirror image
// enable <img> snapshot` plus a mirror-snapshot schedule from the class
// parameters (schedulingInterval). Must be idempotent (re-enable is a no-op).
type EnableReplicationRequest struct {
	Volume     VolumeRef         `json:"volume"`
	Parameters map[string]string `json:"parameters,omitempty"`
	Secrets    map[string]string `json:"secrets,omitempty"`
}

// DisableReplicationRequest stops mirroring a volume (csi-addons
// DisableVolumeReplication) -- `rbd mirror image disable`. Idempotent.
type DisableReplicationRequest struct {
	Volume     VolumeRef         `json:"volume"`
	Parameters map[string]string `json:"parameters,omitempty"`
	Secrets    map[string]string `json:"secrets,omitempty"`
}

// PromoteVolumeRequest makes a mirrored volume primary (writable) on this cluster
// (csi-addons PromoteVolume) -- `rbd mirror image promote [--force]`. Force is for
// failover when the peer is unreachable (risks split-brain; resolve with resync).
type PromoteVolumeRequest struct {
	Volume     VolumeRef         `json:"volume"`
	Force      bool              `json:"force,omitempty"`
	Parameters map[string]string `json:"parameters,omitempty"`
	Secrets    map[string]string `json:"secrets,omitempty"`
}

// DemoteVolumeRequest makes a mirrored volume secondary (read-only) on this
// cluster (csi-addons DemoteVolume) -- `rbd mirror image demote`. Graceful failover.
type DemoteVolumeRequest struct {
	Volume     VolumeRef         `json:"volume"`
	Force      bool              `json:"force,omitempty"`
	Parameters map[string]string `json:"parameters,omitempty"`
	Secrets    map[string]string `json:"secrets,omitempty"`
}

// ResyncVolumeRequest re-synchronises a volume from its peer after a split-brain
// (csi-addons ResyncVolume) -- `rbd mirror image resync`. Ready in the response
// reports whether the image is back in sync.
type ResyncVolumeRequest struct {
	Volume     VolumeRef         `json:"volume"`
	Force      bool              `json:"force,omitempty"`
	Parameters map[string]string `json:"parameters,omitempty"`
	Secrets    map[string]string `json:"secrets,omitempty"`
}

// ResyncVolumeResponse reports whether the resynced volume is ready (back in sync
// and replaying).
type ResyncVolumeResponse struct {
	Ready bool `json:"ready"`
}

// ReplicationInfoRequest asks for a volume's replication status (csi-addons
// GetVolumeReplicationInfo), used by the DR orchestrator to read the last
// successful sync time.
type ReplicationInfoRequest struct {
	Volume  VolumeRef         `json:"volume"`
	Secrets map[string]string `json:"secrets,omitempty"`
}

// ReplicationInfoResponse reports the last successful sync as a Unix timestamp
// (0 = unknown / not yet synced).
type ReplicationInfoResponse struct {
	LastSyncTimeUnix int64 `json:"lastSyncTimeUnix,omitempty"`
}

// VolumeReplicator is an OPTIONAL interface a Backend may also implement to answer
// the /replication/* routes (csi-addons VolumeReplication). Implementing it sets
// Capabilities.Replication and makes Bard serve the csi-addons Replication
// Controller service, so a ceph-csi user's existing VolumeReplication resources
// (RBD mirroring for DR) work against Bard. Control-plane (controller) operations;
// Enable/Disable/Promote/Demote must be idempotent.
type VolumeReplicator interface {
	EnableVolumeReplication(ctx context.Context, req *EnableReplicationRequest) error
	DisableVolumeReplication(ctx context.Context, req *DisableReplicationRequest) error
	PromoteVolume(ctx context.Context, req *PromoteVolumeRequest) error
	DemoteVolume(ctx context.Context, req *DemoteVolumeRequest) error
	ResyncVolume(ctx context.Context, req *ResyncVolumeRequest) (*ResyncVolumeResponse, error)
	GetVolumeReplicationInfo(ctx context.Context, req *ReplicationInfoRequest) (*ReplicationInfoResponse, error)
}

// VolumeGroup requests manage a backend consistency group (csi-addons VolumeGroup,
// e.g. an `rbd group`). A group is cluster-scoped: Group identifies it by Instance +
// Location (the group's pool) + Name (the rbd group name, derived by the plugin from
// the CO name). Members are VolumeRefs. A group can only span ONE backend instance
// (an rbd group lives in a single Ceph cluster), which the core enforces.

// CreateVolumeGroupRequest asks the plugin to create a group named for the CO's Name in
// the given Instance/Pool and add the listed Volumes (which may be empty -- an empty
// group). Idempotent: an existing group with the same members is a success.
type CreateVolumeGroupRequest struct {
	Instance   string            `json:"instance"`
	Pool       string            `json:"pool"`
	Name       string            `json:"name"`
	Volumes    []VolumeRef       `json:"volumes,omitempty"`
	Parameters map[string]string `json:"parameters,omitempty"`
	Secrets    map[string]string `json:"secrets,omitempty"`
}

// VolumeGroupResponse describes a group: its own ref (Instance/Location=pool/Name=rbd
// group name) and its current members. Returned by create/modify/get.
type VolumeGroupResponse struct {
	Group   VolumeRef   `json:"group"`
	Volumes []VolumeRef `json:"volumes,omitempty"`
}

// ModifyVolumeGroupRequest sets a group's membership to exactly Volumes (the plugin
// adds the missing and removes the extra). Idempotent.
type ModifyVolumeGroupRequest struct {
	Group      VolumeRef         `json:"group"`
	Volumes    []VolumeRef       `json:"volumes,omitempty"`
	Parameters map[string]string `json:"parameters,omitempty"`
	Secrets    map[string]string `json:"secrets,omitempty"`
}

// DeleteVolumeGroupRequest removes the group itself (NOT its member volumes -- the
// csi-addons DO_NOT_ALLOW_VG_TO_DELETE_VOLUMES contract). Idempotent.
type DeleteVolumeGroupRequest struct {
	Group   VolumeRef         `json:"group"`
	Secrets map[string]string `json:"secrets,omitempty"`
}

// GetVolumeGroupRequest reads a group's current members.
type GetVolumeGroupRequest struct {
	Group   VolumeRef         `json:"group"`
	Secrets map[string]string `json:"secrets,omitempty"`
}

// ListVolumeGroupsRequest enumerates every group across the plugin's instances; core
// paginates the aggregate.
type ListVolumeGroupsRequest struct {
	Secrets map[string]string `json:"secrets,omitempty"`
}

// ListVolumeGroupsResponse lists all groups (each with its members).
type ListVolumeGroupsResponse struct {
	Groups []VolumeGroupResponse `json:"groups"`
}

// VolumeGrouper is an OPTIONAL interface a Backend may implement to answer the
// /volumegroup/* routes (csi-addons VolumeGroup). Implementing it sets
// Capabilities.VolumeGroup and makes Bard serve the VolumeGroup ControllerServer, so a
// ceph-csi user's VolumeGroup(Replication) resources work against Bard. Cluster-scoped
// and controller-side. All must be idempotent; DeleteVolumeGroup must NOT delete member
// volumes.
type VolumeGrouper interface {
	CreateVolumeGroup(ctx context.Context, req *CreateVolumeGroupRequest) (*VolumeGroupResponse, error)
	ModifyVolumeGroup(ctx context.Context, req *ModifyVolumeGroupRequest) (*VolumeGroupResponse, error)
	DeleteVolumeGroup(ctx context.Context, req *DeleteVolumeGroupRequest) error
	GetVolumeGroup(ctx context.Context, req *GetVolumeGroupRequest) (*VolumeGroupResponse, error)
	ListVolumeGroups(ctx context.Context, req *ListVolumeGroupsRequest) (*ListVolumeGroupsResponse, error)
}

// RotateEncryptionKeyRequest asks the plugin to rotate an encrypted volume's key
// (csi-addons EncryptionKeyRotation): change the LUKS passphrase to fresh material
// without re-encrypting the data. NODE-side: the volume must be staged on this node
// (VolumePath is its published path) so the plugin can reach the dm-crypt device.
// The KMS provider + key identity are read from the volume's own metadata.
type RotateEncryptionKeyRequest struct {
	Volume     VolumeRef         `json:"volume"`
	VolumePath string            `json:"volumePath"`
	Parameters map[string]string `json:"parameters,omitempty"`
	Secrets    map[string]string `json:"secrets,omitempty"`
}

// EncryptionKeyRotator is an OPTIONAL interface a Backend may also implement to
// answer the /node/rotate-key route (csi-addons EncryptionKeyRotation). Implementing
// it sets Capabilities.EncryptionKeyRotation and makes Bard serve the csi-addons
// EncryptionKeyRotation operation on the node plane. Must be idempotent enough that a
// retry after a partial rotation converges (the old key still opens until the new one
// is committed). Providers whose key is not rotatable (a deterministic derived key,
// or an explicit passphrase secret) return InvalidArgument.
type EncryptionKeyRotator interface {
	RotateEncryptionKey(ctx context.Context, req *RotateEncryptionKeyRequest) error
}

// StatusError is returned by a Backend method to control the error Code sent to
// Bard. A plain error maps to CodeInternal.
type StatusError struct {
	Code    ErrorCode
	Message string
}

func (e *StatusError) Error() string { return string(e.Code) + ": " + e.Message }

// Errorf builds a StatusError.
func Errorf(code ErrorCode, format string, args ...any) *StatusError {
	return &StatusError{Code: code, Message: fmt.Sprintf(format, args...)}
}
