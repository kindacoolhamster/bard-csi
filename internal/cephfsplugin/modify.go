package cephfsplugin

import (
	"context"
	"fmt"
	"strconv"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// MDS pinning via VolumeAttributesClass (CSI ControllerModifyVolume). A CephFS
// subvolume's directory tree is served by one MDS rank by default; `ceph fs
// subvolume pin` overrides that placement -- pin a hot subvolume to a dedicated
// rank (`export`), distribute its children across ranks (`distributed`), or
// spread them probabilistically (`random`). Exposing it as VAC mutable
// parameters lets an admin re-balance MDS load per volume at runtime with a
// `kubectl patch pvc` -- an open ceph-csi ask (its driver has no ModifyVolume
// at all). The pin applies to a live volume with no remount.
const (
	// mutablePinType selects the pin policy: "export" (pin the subtree to an MDS
	// rank), "distributed" (spread child dirs across ranks), or "random"
	// (probabilistically export-pin descendants).
	mutablePinType = "pinType"
	// mutablePinSetting is the policy's argument: export -> an MDS rank ("-1"
	// unpins), distributed -> "0"/"1", random -> a probability in [0.0, 1.0].
	mutablePinSetting = "pinSetting"
)

// validateMutableParams rejects any mutable parameter this backend does not
// support, as CSI requires (an unknown VolumeAttributesClass parameter must fail
// with InvalidArgument rather than be silently ignored). Empty/nil is valid;
// pinType and pinSetting are only valid together.
func validateMutableParams(p map[string]string) error {
	if len(p) == 0 {
		return nil
	}
	for k := range p {
		if k != mutablePinType && k != mutablePinSetting {
			return bardplugin.Errorf(bardplugin.CodeInvalidArg, "cephfs: unsupported mutable parameter %q", k)
		}
	}
	pt, ps := p[mutablePinType], p[mutablePinSetting]
	if pt == "" || ps == "" {
		return bardplugin.Errorf(bardplugin.CodeInvalidArg, "cephfs: pinType and pinSetting are required together")
	}
	switch pt {
	case "export":
		if _, err := strconv.Atoi(ps); err != nil {
			return bardplugin.Errorf(bardplugin.CodeInvalidArg, "cephfs: pinType export needs an MDS rank (or -1 to unpin), got %q", ps)
		}
	case "distributed":
		if ps != "0" && ps != "1" {
			return bardplugin.Errorf(bardplugin.CodeInvalidArg, "cephfs: pinType distributed needs pinSetting 0 or 1, got %q", ps)
		}
	case "random":
		f, err := strconv.ParseFloat(ps, 64)
		if err != nil || f < 0 || f > 1 {
			return bardplugin.Errorf(bardplugin.CodeInvalidArg, "cephfs: pinType random needs a probability in [0.0, 1.0], got %q", ps)
		}
	default:
		return bardplugin.Errorf(bardplugin.CodeInvalidArg, "cephfs: invalid pinType %q (export|distributed|random)", pt)
	}
	return nil
}

// applyPin runs `ceph fs subvolume pin` for a validated pinType/pinSetting pair.
// A no-op when the params are empty. Callers must validate first.
func (b *Backend) applyPin(ctx context.Context, conn []string, cc ClusterConfig, group, subvol string, p map[string]string) error {
	if len(p) == 0 {
		return nil
	}
	args := append(append([]string{}, conn...),
		"fs", "subvolume", "pin", cc.FSName, subvol, p[mutablePinType], p[mutablePinSetting])
	if _, err := b.run.Run(ctx, "ceph", withGroup(args, group)...); err != nil {
		return fmt.Errorf("cephfs: subvolume pin %s/%s %s=%s: %w", cc.FSName, subvol, p[mutablePinType], p[mutablePinSetting], err)
	}
	return nil
}

// ModifyVolume implements bardplugin.VolumeModifier: it re-pins a live subvolume's
// MDS placement (CSI ControllerModifyVolume / VolumeAttributesClass).
func (b *Backend) ModifyVolume(ctx context.Context, req *bardplugin.ModifyVolumeRequest) (*bardplugin.ModifyVolumeResponse, error) {
	if err := validateMutableParams(req.MutableParams); err != nil {
		return nil, err
	}
	if len(req.MutableParams) == 0 {
		return &bardplugin.ModifyVolumeResponse{}, nil
	}
	cc, err := b.cluster(req.Volume.Instance)
	if err != nil {
		return nil, err
	}
	conn, cleanup, err := b.cephConn(cc, req.Volume.Instance, req.Secrets)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	group := subvolumeGroupOf(req.Volume.Location)
	// The volume must exist; a modify on a missing subvolume is NotFound.
	infoArgs := append(append([]string{}, conn...), "fs", "subvolume", "info", cc.FSName, req.Volume.Name, "--format", "json")
	if _, err := b.run.Run(ctx, "ceph", withGroup(infoArgs, group)...); err != nil {
		if isNotFound(err) {
			return nil, bardplugin.Errorf(bardplugin.CodeNotFound, "cephfs: subvolume %s/%s not found", cc.FSName, req.Volume.Name)
		}
		return nil, err
	}
	if err := b.applyPin(ctx, conn, cc, group, req.Volume.Name, req.MutableParams); err != nil {
		return nil, err
	}
	return &bardplugin.ModifyVolumeResponse{}, nil
}
