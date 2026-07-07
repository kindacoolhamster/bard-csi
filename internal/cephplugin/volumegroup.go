package cephplugin

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// VolumeGroup (csi-addons) manages an `rbd group` -- a Ceph consistency group -- as the
// backing for the VolumeGroup service. A group lives in ONE Ceph cluster (one instance),
// so all members share the group's instance (core enforces this). Deleting a group
// disbands it but never deletes the member images (the DO_NOT_ALLOW_VG_TO_DELETE_VOLUMES
// contract). An rbd image may belong to at most one group (LIMIT_VOLUME_TO_ONE_VOLUME_GROUP).
//
// The group name is derived from the CO-supplied name with shortName (prefix below), so
// it is deterministic + idempotent and carries no user input into the rbd CLI args.

const groupNamePrefix = "csi-group-"

// paramVolumeGroupNamePrefix overrides the default group-name prefix (a
// class/CR parameter, like volumeNamePrefix on a StorageClass). ceph-csi parity.
const paramVolumeGroupNamePrefix = "volumeGroupNamePrefix"

// CreateVolumeGroup creates the rbd group and adds the requested members. Idempotent:
// an existing group / already-added member is a success.
func (b *Backend) CreateVolumeGroup(ctx context.Context, req *bardplugin.CreateVolumeGroupRequest) (*bardplugin.VolumeGroupResponse, error) {
	cc, err := b.cluster(req.Instance)
	if err != nil {
		return nil, err
	}
	pool := req.Pool
	if pool == "" {
		pool = cc.Pool
	}
	if pool == "" {
		return nil, bardplugin.Errorf(bardplugin.CodeInvalidArg, "ceph-rbd: no pool for volume group")
	}
	conn, cleanup, err := b.connArgs(cc, req.Instance, req.Secrets)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	prefix, err := namePrefix(paramVolumeGroupNamePrefix, req.Parameters[paramVolumeGroupNamePrefix], groupNamePrefix)
	if err != nil {
		return nil, err
	}
	group := shortName(prefix, req.Name)
	groupSpec := pool + "/" + group
	if _, err := b.run.Run(ctx, "rbd", appendArgs(conn, "group", "create", groupSpec)...); err != nil && !isAlreadyExists(err) {
		return nil, fmt.Errorf("ceph-rbd: rbd group create %s: %w", groupSpec, err)
	}
	for _, m := range req.Volumes {
		if err := b.groupImageAdd(ctx, conn, groupSpec, m); err != nil {
			return nil, err
		}
	}
	members, err := b.groupImageList(ctx, conn, req.Instance, groupSpec)
	if err != nil {
		return nil, err
	}
	return &bardplugin.VolumeGroupResponse{
		Group:   bardplugin.VolumeRef{Instance: req.Instance, Location: pool, Name: group},
		Volumes: members,
	}, nil
}

// ModifyVolumeGroup sets the group's membership to exactly req.Volumes: it adds the ones
// not yet in the group and removes the ones no longer wanted. Idempotent.
func (b *Backend) ModifyVolumeGroup(ctx context.Context, req *bardplugin.ModifyVolumeGroupRequest) (*bardplugin.VolumeGroupResponse, error) {
	cc, err := b.cluster(req.Group.Instance)
	if err != nil {
		return nil, err
	}
	conn, cleanup, err := b.connArgs(cc, req.Group.Instance, req.Secrets)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	groupSpec := req.Group.Location + "/" + req.Group.Name

	current, err := b.groupImageList(ctx, conn, req.Group.Instance, groupSpec)
	if err != nil {
		return nil, err
	}
	want := map[string]bardplugin.VolumeRef{}
	for _, m := range req.Volumes {
		want[m.Location+"/"+m.Name] = m
	}
	have := map[string]bool{}
	for _, m := range current {
		have[m.Location+"/"+m.Name] = true
	}
	for key, m := range want {
		if !have[key] {
			if err := b.groupImageAdd(ctx, conn, groupSpec, m); err != nil {
				return nil, err
			}
		}
	}
	for _, m := range current {
		if _, ok := want[m.Location+"/"+m.Name]; !ok {
			if err := b.groupImageRemove(ctx, conn, groupSpec, m); err != nil {
				return nil, err
			}
		}
	}
	members, err := b.groupImageList(ctx, conn, req.Group.Instance, groupSpec)
	if err != nil {
		return nil, err
	}
	return &bardplugin.VolumeGroupResponse{Group: req.Group, Volumes: members}, nil
}

// DeleteVolumeGroup removes the rbd group (disbanding it). The member images are NOT
// deleted -- rbd group remove leaves them intact. Idempotent: a missing group succeeds.
func (b *Backend) DeleteVolumeGroup(ctx context.Context, req *bardplugin.DeleteVolumeGroupRequest) error {
	cc, err := b.cluster(req.Group.Instance)
	if err != nil {
		return err
	}
	conn, cleanup, err := b.connArgs(cc, req.Group.Instance, req.Secrets)
	if err != nil {
		return err
	}
	defer cleanup()
	groupSpec := req.Group.Location + "/" + req.Group.Name
	if _, err := b.run.Run(ctx, "rbd", appendArgs(conn, "group", "remove", groupSpec)...); err != nil && !isNotFound(err) {
		return fmt.Errorf("ceph-rbd: rbd group remove %s: %w", groupSpec, err)
	}
	return nil
}

// GetVolumeGroup returns the group's current members.
func (b *Backend) GetVolumeGroup(ctx context.Context, req *bardplugin.GetVolumeGroupRequest) (*bardplugin.VolumeGroupResponse, error) {
	cc, err := b.cluster(req.Group.Instance)
	if err != nil {
		return nil, err
	}
	conn, cleanup, err := b.connArgs(cc, req.Group.Instance, req.Secrets)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	groupSpec := req.Group.Location + "/" + req.Group.Name
	members, err := b.groupImageList(ctx, conn, req.Group.Instance, groupSpec)
	if err != nil {
		return nil, err
	}
	return &bardplugin.VolumeGroupResponse{Group: req.Group, Volumes: members}, nil
}

// ListVolumeGroups enumerates every Bard-managed group across the plugin's instances.
func (b *Backend) ListVolumeGroups(ctx context.Context, req *bardplugin.ListVolumeGroupsRequest) (*bardplugin.ListVolumeGroupsResponse, error) {
	resp := &bardplugin.ListVolumeGroupsResponse{}
	for instance, cc := range b.clusters {
		conn, cleanup, err := b.connArgs(cc, instance, req.Secrets)
		if err != nil {
			return nil, err
		}
		out, err := b.run.Run(ctx, "rbd", appendArgs(conn, "group", "list", cc.Pool, "--format", "json")...)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("ceph-rbd: rbd group list %s: %w", cc.Pool, err)
		}
		var names []string
		_ = json.Unmarshal([]byte(out), &names)
		for _, name := range names {
			// Bard-managed groups have the shortName shape (any prefix + 16-hex
			// hash), so custom volumeGroupNamePrefix groups stay listable while
			// foreign groups in the pool are skipped.
			if !isBardImageName(name) {
				continue
			}
			groupSpec := cc.Pool + "/" + name
			members, err := b.groupImageList(ctx, conn, instance, groupSpec)
			if err != nil {
				cleanup()
				return nil, err
			}
			resp.Groups = append(resp.Groups, bardplugin.VolumeGroupResponse{
				Group:   bardplugin.VolumeRef{Instance: instance, Location: cc.Pool, Name: name},
				Volumes: members,
			})
		}
		cleanup()
	}
	return resp, nil
}

// groupImageAdd adds one image to the group; an already-member image is a success.
func (b *Backend) groupImageAdd(ctx context.Context, conn []string, groupSpec string, m bardplugin.VolumeRef) error {
	imgSpec := m.Location + "/" + m.Name
	if _, err := b.run.Run(ctx, "rbd", appendArgs(conn, "group", "image", "add", groupSpec, imgSpec)...); err != nil && !isAlreadyExists(err) {
		return fmt.Errorf("ceph-rbd: rbd group image add %s %s: %w", groupSpec, imgSpec, err)
	}
	return nil
}

// groupImageRemove removes one image from the group; an absent image is a success.
func (b *Backend) groupImageRemove(ctx context.Context, conn []string, groupSpec string, m bardplugin.VolumeRef) error {
	imgSpec := m.Location + "/" + m.Name
	if _, err := b.run.Run(ctx, "rbd", appendArgs(conn, "group", "image", "rm", groupSpec, imgSpec)...); err != nil && !isNotFound(err) {
		return fmt.Errorf("ceph-rbd: rbd group image rm %s %s: %w", groupSpec, imgSpec, err)
	}
	return nil
}

// groupImageList returns the group's member images as VolumeRefs (Location = pool, or
// pool/namespace). Each member's Instance is the group's instance (rbd groups are
// single-cluster), so the resulting refs form valid volume handles. Parses `rbd group
// image list --format json`, an array of {pool, namespace, image}.
func (b *Backend) groupImageList(ctx context.Context, conn []string, instance, groupSpec string) ([]bardplugin.VolumeRef, error) {
	out, err := b.run.Run(ctx, "rbd", appendArgs(conn, "group", "image", "list", groupSpec, "--format", "json")...)
	if err != nil {
		return nil, fmt.Errorf("ceph-rbd: rbd group image list %s: %w", groupSpec, err)
	}
	var raw []struct {
		Pool      string `json:"pool"`
		Namespace string `json:"namespace"`
		Image     string `json:"image"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("ceph-rbd: parse group image list: %w", err)
	}
	members := make([]bardplugin.VolumeRef, 0, len(raw))
	for _, r := range raw {
		members = append(members, bardplugin.VolumeRef{Instance: instance, Location: locator(r.Pool, r.Namespace), Name: r.Image})
	}
	return members, nil
}
