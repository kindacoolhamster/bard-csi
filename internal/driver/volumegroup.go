package driver

import (
	"context"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/csi-addons/spec/lib/go/volumegroup"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"

	"github.com/kindacoolhamster/bard-csi/internal/backend"
	"github.com/kindacoolhamster/bard-csi/internal/volumeid"
)

// volumeGroupServer implements the csi-addons VolumeGroup ControllerServer by
// dispatching to the backend that can manage consistency groups (ceph-rbd via `rbd
// group`). A group is cluster-scoped and lives in ONE backend instance, so the server
// resolves the instance + pool from the member volumes (or, for an empty group, from
// the request parameters) and serves the operation only when a registered backend
// supports it (Capabilities.VolumeGroup).
type volumeGroupServer struct {
	volumegroup.UnimplementedControllerServer
	driver *Driver
}

// groupBackend returns the registered backend that manages volume groups. backendType
// (when known, e.g. from a member handle) selects it directly; otherwise the single
// VolumeGroup-capable backend is used (ambiguity is an error).
func (s *volumeGroupServer) groupBackend(backendType string) (backend.VolumeGrouper, error) {
	bk := s.driver.snapshot()
	if backendType != "" {
		be, err := bk.registry.Get(backendType)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "%v", err)
		}
		vg, ok := be.(backend.VolumeGrouper)
		if !ok || !be.Capabilities().VolumeGroup {
			return nil, status.Errorf(codes.Unimplemented, "backend %q does not support volume groups", backendType)
		}
		return vg, nil
	}
	var found backend.VolumeGrouper
	for _, t := range bk.registry.Types() {
		be, err := bk.registry.Get(t)
		if err != nil || !be.Capabilities().VolumeGroup {
			continue
		}
		if vg, ok := be.(backend.VolumeGrouper); ok {
			if found != nil {
				return nil, status.Error(codes.InvalidArgument, "multiple backends support volume groups; cannot infer")
			}
			found = vg
		}
	}
	if found == nil {
		return nil, status.Error(codes.Unimplemented, "no registered backend supports volume groups")
	}
	return found, nil
}

// parseMembers parses the member volume ids and verifies they all live in the same
// backend type and instance (an rbd group cannot span clusters). Returns the handles
// plus the shared backend type and instance.
func parseMembers(volumeIDs []string) (handles []volumeid.Handle, backendType, instance string, err error) {
	for _, id := range volumeIDs {
		h, perr := volumeid.Parse(id)
		if perr != nil {
			return nil, "", "", status.Errorf(codes.InvalidArgument, "invalid volume id %q: %v", id, perr)
		}
		if backendType == "" {
			backendType, instance = h.Backend, h.Instance
		} else if h.Backend != backendType || h.Instance != instance {
			return nil, "", "", status.Error(codes.InvalidArgument,
				"all volumes in a group must be in the same backend instance (a group cannot span clusters)")
		}
		handles = append(handles, h)
	}
	return handles, backendType, instance, nil
}

// groupVolumes renders a backend group's members as csi.Volume entries (volume id only).
func groupVolumes(g backend.VolumeGroup) []*csi.Volume {
	out := make([]*csi.Volume, 0, len(g.Members))
	for _, m := range g.Members {
		out = append(out, &csi.Volume{VolumeId: m.String()})
	}
	return out
}

func (s *volumeGroupServer) CreateVolumeGroup(ctx context.Context, req *volumegroup.CreateVolumeGroupRequest) (*volumegroup.CreateVolumeGroupResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume group name is required")
	}
	members, backendType, instance, err := parseMembers(req.GetVolumeIds())
	if err != nil {
		return nil, err
	}
	pool := ""
	if len(members) > 0 {
		pool = members[0].Location // the group lives alongside its members
	}
	if instance == "" { // empty group: take the instance from the parameters
		instance = req.GetParameters()["clusterID"]
		if instance == "" {
			instance = req.GetParameters()["instance"]
		}
		backendType = req.GetParameters()["backend"]
	}
	vg, err := s.groupBackend(backendType)
	if err != nil {
		return nil, err
	}
	g, err := vg.CreateVolumeGroup(ctx, instance, pool, req.GetName(), members, req.GetParameters(), req.GetSecrets())
	if err != nil {
		return nil, toStatus(err, "create volume group")
	}
	klog.V(2).Infof("created volume group %s with %d member(s)", g.Group.String(), len(g.Members))
	return &volumegroup.CreateVolumeGroupResponse{VolumeGroup: &volumegroup.VolumeGroup{
		VolumeGroupId: g.Group.String(),
		Volumes:       groupVolumes(g),
	}}, nil
}

func (s *volumeGroupServer) ModifyVolumeGroupMembership(ctx context.Context, req *volumegroup.ModifyVolumeGroupMembershipRequest) (*volumegroup.ModifyVolumeGroupMembershipResponse, error) {
	gh, err := volumeid.Parse(req.GetVolumeGroupId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "unknown volume group id: %v", err)
	}
	members, _, instance, err := parseMembers(req.GetVolumeIds())
	if err != nil {
		return nil, err
	}
	if instance != "" && instance != gh.Instance {
		return nil, status.Error(codes.InvalidArgument, "group members must be in the same instance as the group")
	}
	vg, err := s.groupBackend(gh.Backend)
	if err != nil {
		return nil, err
	}
	g, err := vg.ModifyVolumeGroup(ctx, gh, members, req.GetParameters(), req.GetSecrets())
	if err != nil {
		return nil, toStatus(err, "modify volume group membership")
	}
	klog.V(2).Infof("modified volume group %s -> %d member(s)", gh.String(), len(g.Members))
	return &volumegroup.ModifyVolumeGroupMembershipResponse{VolumeGroup: &volumegroup.VolumeGroup{
		VolumeGroupId: gh.String(),
		Volumes:       groupVolumes(g),
	}}, nil
}

func (s *volumeGroupServer) DeleteVolumeGroup(ctx context.Context, req *volumegroup.DeleteVolumeGroupRequest) (*volumegroup.DeleteVolumeGroupResponse, error) {
	gh, err := volumeid.Parse(req.GetVolumeGroupId())
	if err != nil {
		// A malformed/already-gone id is treated as deleted (idempotent).
		return &volumegroup.DeleteVolumeGroupResponse{}, nil
	}
	vg, err := s.groupBackend(gh.Backend)
	if err != nil {
		return nil, err
	}
	if err := vg.DeleteVolumeGroup(ctx, gh, req.GetSecrets()); err != nil {
		return nil, toStatus(err, "delete volume group")
	}
	klog.V(2).Infof("deleted volume group %s (members untouched)", gh.String())
	return &volumegroup.DeleteVolumeGroupResponse{}, nil
}

func (s *volumeGroupServer) ControllerGetVolumeGroup(ctx context.Context, req *volumegroup.ControllerGetVolumeGroupRequest) (*volumegroup.ControllerGetVolumeGroupResponse, error) {
	gh, err := volumeid.Parse(req.GetVolumeGroupId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "unknown volume group id: %v", err)
	}
	vg, err := s.groupBackend(gh.Backend)
	if err != nil {
		return nil, err
	}
	g, err := vg.GetVolumeGroup(ctx, gh, req.GetSecrets())
	if err != nil {
		return nil, toStatus(err, "get volume group")
	}
	return &volumegroup.ControllerGetVolumeGroupResponse{VolumeGroup: &volumegroup.VolumeGroup{
		VolumeGroupId: gh.String(),
		Volumes:       groupVolumes(g),
	}}, nil
}

func (s *volumeGroupServer) ListVolumeGroups(ctx context.Context, req *volumegroup.ListVolumeGroupsRequest) (*volumegroup.ListVolumeGroupsResponse, error) {
	vg, err := s.groupBackend("")
	if err != nil {
		return nil, err
	}
	groups, err := vg.ListVolumeGroups(ctx, req.GetSecrets())
	if err != nil {
		return nil, toStatus(err, "list volume groups")
	}
	entries := make([]*volumegroup.ListVolumeGroupsResponse_Entry, 0, len(groups))
	for _, g := range groups {
		entries = append(entries, &volumegroup.ListVolumeGroupsResponse_Entry{
			VolumeGroup: &volumegroup.VolumeGroup{VolumeGroupId: g.Group.String(), Volumes: groupVolumes(g)},
		})
	}
	page, next, err := paginate(entries, req.GetStartingToken(), req.GetMaxEntries())
	if err != nil {
		return nil, err
	}
	return &volumegroup.ListVolumeGroupsResponse{Entries: page, NextToken: next}, nil
}
