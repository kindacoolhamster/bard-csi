// Package plugin adapts an out-of-tree backend (a bardplugin HTTP+JSON server on
// a unix socket) to Bard's internal backend.Backend interface, so the rest of
// the driver treats a plugin exactly like a built-in backend.
package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/kindacoolhamster/bard-csi/internal/backend"
	"github.com/kindacoolhamster/bard-csi/internal/volumeid"
	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// Client is a backend.Backend backed by a plugin process over a unix socket.
type Client struct {
	backendType string
	caps        backend.Capabilities
	http        *http.Client
}

// Dial connects to the plugin at socketPath, fetches its capabilities, and binds
// it to the given backend type (the authoritative type for handle encoding and
// dispatch; the plugin's reported type is advisory). It retries /info for a
// short while so the plugin sidecar has time to come up.
func Dial(ctx context.Context, backendType, socketPath string) (*Client, error) {
	// No flat client timeout: every call carries the inbound gRPC context, so
	// the CO's own deadline (kubelet, the sidecars' --timeout) governs each
	// operation. A fixed cap here silently killed long-but-legitimate work --
	// `rbd sparsify` on a large image (csi-addons ReclaimSpace, whose sidecar
	// deliberately sets a long timeout), a dm-integrity format's device wipe --
	// and killing the plugin's command mid-flight is exactly the failure mode
	// the control plane is written to avoid.
	hc := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
			},
		},
	}
	c := &Client{backendType: backendType, http: hc}

	var info bardplugin.Info
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := c.call(ctx, bardplugin.PathInfo, struct{}{}, &info)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("plugin %q: /info failed: %w", backendType, err)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("plugin %q: /info failed: %w (%w)", backendType, err, ctx.Err())
		case <-time.After(time.Second):
		}
	}
	// Enforce the wire-contract version (empty = pre-versioning 1.0). The gate is
	// asymmetric: an older minor is safe (we understand everything it can say),
	// a NEWER minor is refused -- a minor may add vocabulary to an existing route
	// (1.1 added the Unsupported error code) that we would degrade to a generic
	// Internal error, turning a terminal failure into an indefinitely retried one.
	// An older-minor plugin's missing optionals stay gated by capabilities.
	major, minor, err := bardplugin.ParseContractVersion(info.ContractVersion)
	if err != nil {
		return nil, fmt.Errorf("plugin %q: invalid contractVersion: %w", backendType, err)
	}
	if major != bardplugin.ContractMajor {
		return nil, fmt.Errorf("plugin %q speaks wire-contract %q; this Bard supports v%d.x -- use a matching plugin or Bard release",
			backendType, info.ContractVersion, bardplugin.ContractMajor)
	}
	if minor > bardplugin.ContractMinor {
		return nil, fmt.Errorf("plugin %q speaks wire-contract %q, newer than this Bard understands (v%d.%d) -- upgrade Bard to match the plugin",
			backendType, info.ContractVersion, bardplugin.ContractMajor, bardplugin.ContractMinor)
	}
	c.caps = backend.Capabilities(info.Capabilities)
	return c, nil
}

func (c *Client) Type() string                       { return c.backendType }
func (c *Client) Capabilities() backend.Capabilities { return c.caps }

// call POSTs reqBody as JSON to path and decodes the response, mapping plugin
// error codes to backend sentinel errors.
func (c *Client) call(ctx context.Context, path string, reqBody, respOut any) error {
	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://plugin"+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return mapError(data, resp.StatusCode)
	}
	if respOut != nil {
		if err := json.Unmarshal(data, respOut); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

func mapError(body []byte, status int) error {
	var e bardplugin.Error
	_ = json.Unmarshal(body, &e)
	if e.Message == "" {
		e.Message = fmt.Sprintf("plugin returned HTTP %d", status)
	}
	switch e.Code {
	case bardplugin.CodeNotFound:
		return fmt.Errorf("%w: %s", backend.ErrNotFound, e.Message)
	case bardplugin.CodeAlreadyExists:
		return fmt.Errorf("%w: %s", backend.ErrAlreadyExists, e.Message)
	case bardplugin.CodeInvalidArg:
		return fmt.Errorf("%w: %s", backend.ErrInvalidArgument, e.Message)
	case bardplugin.CodeUnsupported:
		// Same terminal (non-retried) sentinel every optional-capability gap
		// already uses (GetCapacity, ModifyVolume, ...) -- toStatus maps it to
		// codes.Unimplemented, so a plugin-declared permanent "no" here gets
		// the same terminal treatment as a plugin that never implemented the
		// call at all.
		return fmt.Errorf("%w: %s", backend.ErrUnsupported, e.Message)
	default:
		return fmt.Errorf("plugin error: %s", e.Message)
	}
}

func ref(h volumeid.Handle) bardplugin.VolumeRef {
	return bardplugin.VolumeRef{Instance: h.Instance, Location: h.Location, Name: h.Name}
}

func refPtr(h *volumeid.Handle) *bardplugin.VolumeRef {
	if h == nil {
		return nil
	}
	r := ref(*h)
	return &r
}

// ---- control plane -------------------------------------------------------

func (c *Client) CreateVolume(ctx context.Context, req *backend.CreateVolumeRequest) (*backend.Volume, error) {
	var resp bardplugin.CreateVolumeResponse
	err := c.call(ctx, bardplugin.PathCreateVolume, bardplugin.CreateVolumeRequest{
		Name:           req.Name,
		CapacityBytes:  req.CapacityBytes,
		Instance:       req.Instance,
		FsType:         req.FsType,
		Parameters:     req.Parameters,
		MutableParams:  req.MutableParams,
		Secrets:        req.Secrets,
		SourceSnapshot: refPtr(req.SourceSnapshot),
		SourceVolume:   refPtr(req.SourceVolume),
	}, &resp)
	if err != nil {
		return nil, err
	}
	h := volumeid.Handle{Backend: c.backendType, Instance: req.Instance, Location: resp.Location, Name: resp.Name}
	if err := h.Validate(); err != nil {
		return nil, err
	}
	return &backend.Volume{
		Handle:             h,
		CapacityBytes:      resp.CapacityBytes,
		Context:            resp.Context,
		AccessibleTopology: resp.AccessibleTopology,
	}, nil
}

func (c *Client) DeleteVolume(ctx context.Context, h volumeid.Handle, secrets map[string]string) error {
	return c.call(ctx, bardplugin.PathDeleteVolume, bardplugin.DeleteVolumeRequest{Volume: ref(h), Secrets: secrets}, nil)
}

func (c *Client) GetCapacity(ctx context.Context, instance string, params map[string]string) (int64, error) {
	if !c.caps.GetCapacity {
		return 0, backend.ErrUnsupported
	}
	var resp bardplugin.GetCapacityResponse
	if err := c.call(ctx, bardplugin.PathGetCapacity, bardplugin.GetCapacityRequest{Instance: instance, Parameters: params}, &resp); err != nil {
		return 0, err
	}
	return resp.AvailableBytes, nil
}

func (c *Client) GetVolumeHealth(ctx context.Context, h volumeid.Handle, secrets map[string]string) (*backend.VolumeHealth, error) {
	if !c.caps.VolumeHealth {
		return nil, backend.ErrUnsupported
	}
	var resp bardplugin.GetVolumeHealthResponse
	if err := c.call(ctx, bardplugin.PathVolumeHealth, bardplugin.GetVolumeHealthRequest{Volume: ref(h), Secrets: secrets}, &resp); err != nil {
		return nil, err
	}
	return &backend.VolumeHealth{Abnormal: resp.Abnormal, Message: resp.Message}, nil
}

func (c *Client) ModifyVolume(ctx context.Context, h volumeid.Handle, mutableParams, secrets map[string]string) error {
	if !c.caps.ModifyVolume {
		return backend.ErrUnsupported
	}
	return c.call(ctx, bardplugin.PathModifyVolume, bardplugin.ModifyVolumeRequest{Volume: ref(h), MutableParams: mutableParams, Secrets: secrets}, nil)
}

func (c *Client) ReclaimSpace(ctx context.Context, h volumeid.Handle, secrets map[string]string) (*backend.SpaceUsage, error) {
	if !c.caps.ReclaimSpace {
		return nil, backend.ErrUnsupported
	}
	var resp bardplugin.ReclaimSpaceResponse
	if err := c.call(ctx, bardplugin.PathReclaimSpace, bardplugin.ReclaimSpaceRequest{Volume: ref(h), Secrets: secrets}, &resp); err != nil {
		return nil, err
	}
	return &backend.SpaceUsage{PreUsageBytes: resp.PreUsageBytes, PostUsageBytes: resp.PostUsageBytes}, nil
}

func (c *Client) NodeReclaimSpace(ctx context.Context, h volumeid.Handle, volumePath, stagingPath string, block bool, secrets map[string]string) (*backend.SpaceUsage, error) {
	if !c.caps.NodeReclaimSpace {
		return nil, backend.ErrUnsupported
	}
	var resp bardplugin.ReclaimSpaceResponse
	req := bardplugin.NodeReclaimSpaceRequest{Volume: ref(h), VolumePath: volumePath, StagingPath: stagingPath, Block: block, Secrets: secrets}
	if err := c.call(ctx, bardplugin.PathNodeReclaimSpace, req, &resp); err != nil {
		return nil, err
	}
	return &backend.SpaceUsage{PreUsageBytes: resp.PreUsageBytes, PostUsageBytes: resp.PostUsageBytes}, nil
}

func (c *Client) ExpandVolume(ctx context.Context, h volumeid.Handle, newSizeBytes int64, secrets map[string]string) (int64, bool, error) {
	var resp bardplugin.ExpandVolumeResponse
	err := c.call(ctx, bardplugin.PathExpandVolume, bardplugin.ExpandVolumeRequest{Volume: ref(h), NewSizeBytes: newSizeBytes, Secrets: secrets}, &resp)
	if err != nil {
		return 0, false, err
	}
	return resp.CapacityBytes, resp.NodeExpansionRequired, nil
}

func (c *Client) CreateSnapshot(ctx context.Context, req *backend.CreateSnapshotRequest) (*backend.Snapshot, error) {
	var resp bardplugin.CreateSnapshotResponse
	err := c.call(ctx, bardplugin.PathCreateSnapshot, bardplugin.CreateSnapshotRequest{
		Name:         req.Name,
		SourceVolume: ref(req.SourceVolume),
		Parameters:   req.Parameters,
		Secrets:      req.Secrets,
	}, &resp)
	if err != nil {
		return nil, err
	}
	h := volumeid.Handle{Backend: c.backendType, Instance: req.SourceVolume.Instance, Location: resp.Location, Name: resp.Name}
	if err := h.Validate(); err != nil {
		return nil, err
	}
	return &backend.Snapshot{
		Handle: h,
		// Bard core owns volume-id encoding, so it fills the source id itself
		// rather than trusting the plugin to echo it.
		SourceVolumeID: req.SourceVolume.String(),
		SizeBytes:      resp.SizeBytes,
		CreationTime:   time.Unix(resp.CreationTimeUnix, 0),
		ReadyToUse:     resp.ReadyToUse,
	}, nil
}

func (c *Client) DeleteSnapshot(ctx context.Context, h volumeid.Handle, secrets map[string]string) error {
	return c.call(ctx, bardplugin.PathDeleteSnapshot, bardplugin.DeleteSnapshotRequest{Snapshot: ref(h), Secrets: secrets}, nil)
}

// ControllerPublish attaches the volume to a node. Backends that don't attach
// (the common case) leave RequiresControllerPublish false; we then skip the
// round-trip and return an empty publish context, so core can call this
// unconditionally.
func (c *Client) ControllerPublish(ctx context.Context, h volumeid.Handle, nodeID string, readonly bool, volCtx, secrets map[string]string) (map[string]string, error) {
	if !c.caps.RequiresControllerPublish {
		return nil, nil
	}
	var resp bardplugin.ControllerPublishResponse
	req := bardplugin.ControllerPublishRequest{Volume: ref(h), NodeID: nodeID, Readonly: readonly, Context: volCtx, Secrets: secrets}
	if err := c.call(ctx, bardplugin.PathControllerPublish, req, &resp); err != nil {
		return nil, err
	}
	return resp.PublishContext, nil
}

func (c *Client) ControllerUnpublish(ctx context.Context, h volumeid.Handle, nodeID string, secrets map[string]string) error {
	if !c.caps.RequiresControllerPublish {
		return nil
	}
	return c.call(ctx, bardplugin.PathControllerUnpublish, bardplugin.ControllerUnpublishRequest{Volume: ref(h), NodeID: nodeID, Secrets: secrets}, nil)
}

func (c *Client) ListVolumes(ctx context.Context) ([]backend.VolumeListEntry, error) {
	if !c.caps.ListVolumes {
		return nil, backend.ErrUnsupported
	}
	var resp bardplugin.ListVolumesResponse
	if err := c.call(ctx, bardplugin.PathListVolumes, bardplugin.ListVolumesRequest{}, &resp); err != nil {
		return nil, err
	}
	out := make([]backend.VolumeListEntry, 0, len(resp.Entries))
	for _, e := range resp.Entries {
		h := volumeid.Handle{Backend: c.backendType, Instance: e.Volume.Instance, Location: e.Volume.Location, Name: e.Volume.Name}
		if h.Validate() != nil {
			continue // skip anything that wouldn't round-trip as a CSI id
		}
		out = append(out, backend.VolumeListEntry{Handle: h, CapacityBytes: e.CapacityBytes})
	}
	return out, nil
}

func (c *Client) ListSnapshots(ctx context.Context) ([]backend.SnapshotListEntry, error) {
	if !c.caps.ListSnapshots {
		return nil, backend.ErrUnsupported
	}
	var resp bardplugin.ListSnapshotsResponse
	if err := c.call(ctx, bardplugin.PathListSnapshots, bardplugin.ListSnapshotsRequest{}, &resp); err != nil {
		return nil, err
	}
	out := make([]backend.SnapshotListEntry, 0, len(resp.Entries))
	for _, e := range resp.Entries {
		snap := volumeid.Handle{Backend: c.backendType, Instance: e.Snapshot.Instance, Location: e.Snapshot.Location, Name: e.Snapshot.Name}
		src := volumeid.Handle{Backend: c.backendType, Instance: e.SourceVolume.Instance, Location: e.SourceVolume.Location, Name: e.SourceVolume.Name}
		if snap.Validate() != nil || src.Validate() != nil {
			continue
		}
		out = append(out, backend.SnapshotListEntry{
			Handle:       snap,
			SourceVolume: src,
			SizeBytes:    e.SizeBytes,
			CreationTime: time.Unix(e.CreationTimeUnix, 0),
			ReadyToUse:   e.ReadyToUse,
		})
	}
	return out, nil
}

// FenceClusterNetwork fences client network ranges at the backend (csi-addons
// NetworkFence). Cluster-scoped: instance selects the backend cluster.
func (c *Client) FenceClusterNetwork(ctx context.Context, instance string, cidrs []string, params, secrets map[string]string) error {
	if !c.caps.NetworkFence {
		return backend.ErrUnsupported
	}
	return c.call(ctx, bardplugin.PathFenceClusterNetwork, bardplugin.FenceClusterNetworkRequest{
		Instance: instance, CIDRs: cidrs, Parameters: params, Secrets: secrets,
	}, nil)
}

func (c *Client) UnfenceClusterNetwork(ctx context.Context, instance string, cidrs []string, params, secrets map[string]string) error {
	if !c.caps.NetworkFence {
		return backend.ErrUnsupported
	}
	return c.call(ctx, bardplugin.PathUnfenceClusterNetwork, bardplugin.UnfenceClusterNetworkRequest{
		Instance: instance, CIDRs: cidrs, Parameters: params, Secrets: secrets,
	}, nil)
}

func (c *Client) ListClusterFence(ctx context.Context, instance string, params, secrets map[string]string) ([]string, error) {
	if !c.caps.NetworkFence {
		return nil, backend.ErrUnsupported
	}
	var resp bardplugin.ListClusterFenceResponse
	if err := c.call(ctx, bardplugin.PathListClusterFence, bardplugin.ListClusterFenceRequest{
		Instance: instance, Parameters: params, Secrets: secrets,
	}, &resp); err != nil {
		return nil, err
	}
	return resp.CIDRs, nil
}

func (c *Client) GetFenceClients(ctx context.Context, instance string, params, secrets map[string]string) ([]backend.FenceClient, error) {
	if !c.caps.NetworkFence {
		return nil, backend.ErrUnsupported
	}
	var resp bardplugin.GetFenceClientsResponse
	if err := c.call(ctx, bardplugin.PathGetFenceClients, bardplugin.GetFenceClientsRequest{
		Instance: instance, Parameters: params, Secrets: secrets,
	}, &resp); err != nil {
		return nil, err
	}
	out := make([]backend.FenceClient, 0, len(resp.Clients))
	for _, cl := range resp.Clients {
		out = append(out, backend.FenceClient{ID: cl.ID, CIDRs: cl.CIDRs})
	}
	return out, nil
}

// VolumeReplication (csi-addons): mirror a volume to a peer cluster. Volume-scoped.
func (c *Client) EnableVolumeReplication(ctx context.Context, h volumeid.Handle, params, secrets map[string]string) error {
	if !c.caps.Replication {
		return backend.ErrUnsupported
	}
	return c.call(ctx, bardplugin.PathEnableReplication, bardplugin.EnableReplicationRequest{Volume: ref(h), Parameters: params, Secrets: secrets}, nil)
}

func (c *Client) DisableVolumeReplication(ctx context.Context, h volumeid.Handle, params, secrets map[string]string) error {
	if !c.caps.Replication {
		return backend.ErrUnsupported
	}
	return c.call(ctx, bardplugin.PathDisableReplication, bardplugin.DisableReplicationRequest{Volume: ref(h), Parameters: params, Secrets: secrets}, nil)
}

func (c *Client) PromoteVolume(ctx context.Context, h volumeid.Handle, force bool, params, secrets map[string]string) error {
	if !c.caps.Replication {
		return backend.ErrUnsupported
	}
	return c.call(ctx, bardplugin.PathPromoteVolume, bardplugin.PromoteVolumeRequest{Volume: ref(h), Force: force, Parameters: params, Secrets: secrets}, nil)
}

func (c *Client) DemoteVolume(ctx context.Context, h volumeid.Handle, force bool, params, secrets map[string]string) error {
	if !c.caps.Replication {
		return backend.ErrUnsupported
	}
	return c.call(ctx, bardplugin.PathDemoteVolume, bardplugin.DemoteVolumeRequest{Volume: ref(h), Force: force, Parameters: params, Secrets: secrets}, nil)
}

func (c *Client) ResyncVolume(ctx context.Context, h volumeid.Handle, force bool, params, secrets map[string]string) (bool, error) {
	if !c.caps.Replication {
		return false, backend.ErrUnsupported
	}
	var resp bardplugin.ResyncVolumeResponse
	if err := c.call(ctx, bardplugin.PathResyncVolume, bardplugin.ResyncVolumeRequest{Volume: ref(h), Force: force, Parameters: params, Secrets: secrets}, &resp); err != nil {
		return false, err
	}
	return resp.Ready, nil
}

func (c *Client) GetVolumeReplicationInfo(ctx context.Context, h volumeid.Handle, secrets map[string]string) (time.Time, error) {
	if !c.caps.Replication {
		return time.Time{}, backend.ErrUnsupported
	}
	var resp bardplugin.ReplicationInfoResponse
	if err := c.call(ctx, bardplugin.PathReplicationInfo, bardplugin.ReplicationInfoRequest{Volume: ref(h), Secrets: secrets}, &resp); err != nil {
		return time.Time{}, err
	}
	if resp.LastSyncTimeUnix == 0 {
		return time.Time{}, nil
	}
	return time.Unix(resp.LastSyncTimeUnix, 0), nil
}

// RotateEncryptionKey rotates an encrypted volume's key (csi-addons
// EncryptionKeyRotation). Node-scoped: the volume must be staged on this node.
func (c *Client) RotateEncryptionKey(ctx context.Context, h volumeid.Handle, volumePath string, params, secrets map[string]string) error {
	if !c.caps.EncryptionKeyRotation {
		return backend.ErrUnsupported
	}
	return c.call(ctx, bardplugin.PathRotateEncryptionKey, bardplugin.RotateEncryptionKeyRequest{
		Volume: ref(h), VolumePath: volumePath, Parameters: params, Secrets: secrets,
	}, nil)
}

// VolumeGroup (csi-addons): manage backend consistency groups. Cluster-scoped.

// refs maps member handles to wire VolumeRefs.
func refs(hs []volumeid.Handle) []bardplugin.VolumeRef {
	out := make([]bardplugin.VolumeRef, 0, len(hs))
	for _, h := range hs {
		out = append(out, ref(h))
	}
	return out
}

// group decodes a VolumeGroupResponse back into typed handles, tagging each ref with
// the plugin's backend type (the wire form omits it).
func (c *Client) group(r bardplugin.VolumeGroupResponse) backend.VolumeGroup {
	g := backend.VolumeGroup{Group: c.handle(r.Group)}
	for _, m := range r.Volumes {
		g.Members = append(g.Members, c.handle(m))
	}
	return g
}

func (c *Client) handle(r bardplugin.VolumeRef) volumeid.Handle {
	return volumeid.Handle{Backend: c.backendType, Instance: r.Instance, Location: r.Location, Name: r.Name}
}

func (c *Client) CreateVolumeGroup(ctx context.Context, instance, pool, name string, members []volumeid.Handle, params, secrets map[string]string) (backend.VolumeGroup, error) {
	if !c.caps.VolumeGroup {
		return backend.VolumeGroup{}, backend.ErrUnsupported
	}
	var resp bardplugin.VolumeGroupResponse
	if err := c.call(ctx, bardplugin.PathCreateVolumeGroup, bardplugin.CreateVolumeGroupRequest{
		Instance: instance, Pool: pool, Name: name, Volumes: refs(members), Parameters: params, Secrets: secrets,
	}, &resp); err != nil {
		return backend.VolumeGroup{}, err
	}
	return c.group(resp), nil
}

func (c *Client) ModifyVolumeGroup(ctx context.Context, g volumeid.Handle, members []volumeid.Handle, params, secrets map[string]string) (backend.VolumeGroup, error) {
	if !c.caps.VolumeGroup {
		return backend.VolumeGroup{}, backend.ErrUnsupported
	}
	var resp bardplugin.VolumeGroupResponse
	if err := c.call(ctx, bardplugin.PathModifyVolumeGroup, bardplugin.ModifyVolumeGroupRequest{
		Group: ref(g), Volumes: refs(members), Parameters: params, Secrets: secrets,
	}, &resp); err != nil {
		return backend.VolumeGroup{}, err
	}
	return c.group(resp), nil
}

func (c *Client) DeleteVolumeGroup(ctx context.Context, g volumeid.Handle, secrets map[string]string) error {
	if !c.caps.VolumeGroup {
		return backend.ErrUnsupported
	}
	return c.call(ctx, bardplugin.PathDeleteVolumeGroup, bardplugin.DeleteVolumeGroupRequest{Group: ref(g), Secrets: secrets}, nil)
}

func (c *Client) GetVolumeGroup(ctx context.Context, g volumeid.Handle, secrets map[string]string) (backend.VolumeGroup, error) {
	if !c.caps.VolumeGroup {
		return backend.VolumeGroup{}, backend.ErrUnsupported
	}
	var resp bardplugin.VolumeGroupResponse
	if err := c.call(ctx, bardplugin.PathGetVolumeGroup, bardplugin.GetVolumeGroupRequest{Group: ref(g), Secrets: secrets}, &resp); err != nil {
		return backend.VolumeGroup{}, err
	}
	return c.group(resp), nil
}

func (c *Client) ListVolumeGroups(ctx context.Context, secrets map[string]string) ([]backend.VolumeGroup, error) {
	if !c.caps.VolumeGroup {
		return nil, backend.ErrUnsupported
	}
	var resp bardplugin.ListVolumeGroupsResponse
	if err := c.call(ctx, bardplugin.PathListVolumeGroups, bardplugin.ListVolumeGroupsRequest{Secrets: secrets}, &resp); err != nil {
		return nil, err
	}
	out := make([]backend.VolumeGroup, 0, len(resp.Groups))
	for _, g := range resp.Groups {
		out = append(out, c.group(g))
	}
	return out, nil
}

// ---- node plane ----------------------------------------------------------

func (c *Client) NodeStage(ctx context.Context, req *backend.NodeStageRequest) error {
	return c.call(ctx, bardplugin.PathNodeStage, bardplugin.NodeStageRequest{
		Volume:         ref(req.Handle),
		StagingPath:    req.StagingPath,
		FsType:         req.FsType,
		MountFlags:     req.MountFlags,
		Readonly:       req.Readonly,
		Block:          req.Block,
		Exclusive:      req.Exclusive,
		Context:        req.Context,
		PublishContext: req.PublishContext,
		CrushLocation:  req.CrushLocation,
		Secrets:        req.Secrets,
	}, nil)
}

func (c *Client) NodeUnstage(ctx context.Context, h volumeid.Handle, stagingPath string) error {
	return c.call(ctx, bardplugin.PathNodeUnstage, bardplugin.NodeUnstageRequest{Volume: ref(h), StagingPath: stagingPath}, nil)
}

func (c *Client) NodePublish(ctx context.Context, req *backend.NodePublishRequest) error {
	return c.call(ctx, bardplugin.PathNodePublish, bardplugin.NodePublishRequest{
		Volume:      ref(req.Handle),
		StagingPath: req.StagingPath,
		TargetPath:  req.TargetPath,
		FsType:      req.FsType,
		MountFlags:  req.MountFlags,
		Readonly:    req.Readonly,
		Block:       req.Block,
		Context:     req.Context,
	}, nil)
}

func (c *Client) NodeUnpublish(ctx context.Context, h volumeid.Handle, targetPath string) error {
	return c.call(ctx, bardplugin.PathNodeUnpublish, bardplugin.NodeUnpublishRequest{Volume: ref(h), TargetPath: targetPath}, nil)
}

func (c *Client) NodeExpand(ctx context.Context, h volumeid.Handle, volumePath string) (int64, error) {
	var resp bardplugin.NodeExpandResponse
	err := c.call(ctx, bardplugin.PathNodeExpand, bardplugin.NodeExpandRequest{Volume: ref(h), VolumePath: volumePath}, &resp)
	if err != nil {
		return 0, err
	}
	return resp.CapacityBytes, nil
}
