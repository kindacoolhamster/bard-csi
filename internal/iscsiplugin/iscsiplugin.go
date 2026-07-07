// Package iscsiplugin is an iSCSI backend as an out-of-tree Bard plugin, and the
// reference ATTACH-style backend: unlike Ceph RBD/LVM (which map on the node),
// making an iSCSI volume reachable is a control-plane operation. The controller
// masks the volume's LUN to the staging node's initiator (ControllerPublish ->
// targetcli ACL); only then can that node log in and mount it. This exercises
// the CSI ControllerPublishVolume/Unpublish path end to end.
//
// # Model
//
// A volume is an LVM logical volume (carved from a host VG, like the LVM plugin)
// exported through an LIO target. Each volume gets its OWN target (one LUN, LUN
// 0), so login/logout is per-volume clean -- no session reference-counting. Access
// is masked PER NODE: ControllerPublish adds an ACL for the node's initiator IQN,
// ControllerUnpublish removes it. Without the ACL the node's login is rejected, so
// the single-writer guarantee holds at the iSCSI transport, not just in Kubernetes.
//
// The node's initiator IQN is derived deterministically from the CSI node id, so
// the controller (which sets the ACL) and the node (which logs in) agree with no
// lookup. The node logs in under a dedicated iscsiadm iface carrying that IQN, so
// it never touches the host's global initiatorname.
//
// Snapshots/clone, CHAP and multipath are deliberately out of scope here (the LVM
// plugin already demonstrates thin snapshots); this plugin's purpose is the attach
// path. Like the other plugins it depends only on the public bardplugin SDK.
package iscsiplugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

const (
	defaultFsType  = "ext4"
	defaultIQNBase = "iqn.2025-01.io.bard"
	// iscsiIface is the dedicated iscsiadm iface the node logs in under, so the
	// per-node initiator IQN is set without touching the host initiatorname.
	iscsiIface = "bard"
)

// supportedFsTypes mirrors the other plugins' allowlist so an unknown type fails
// fast rather than as "mkfs.<x>: not found".
var supportedFsTypes = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true,
}

// InstanceConfig is the per-instance iSCSI config. The VG backs LUN block devices
// (like the LVM plugin); Portal is the address nodes connect to; IQNBase is the
// IQN prefix for derived target/initiator names. No credentials (CHAP is a
// follow-up); access is controlled by per-node ACLs.
type InstanceConfig struct {
	VG      string `json:"vg"`                // VG to carve LUN backstores from
	Portal  string `json:"portal"`            // iSCSI portal "ip:port" nodes connect to
	IQNBase string `json:"iqnBase,omitempty"` // IQN prefix (default iqn.2025-01.io.bard)
}

// Backend implements bardplugin.Backend (+ ControllerPublisher) for iSCSI.
type Backend struct {
	instances map[string]InstanceConfig
	nodeID    string // CSI node id (node plane only); source of this node's initiator IQN
	stateDir  string // node-plane: records per-staging-path session state for unstage
	run       Runner
}

// New builds the iSCSI plugin backend. nodeID + stateDir are only meaningful on
// the node plane (the controller plane passes "").
func New(instances map[string]InstanceConfig, nodeID, stateDir string, run Runner) *Backend {
	if run == nil {
		run = ExecRunner{}
	}
	return &Backend{instances: instances, nodeID: nodeID, stateDir: stateDir, run: run}
}

func (b *Backend) Info() bardplugin.Info {
	return bardplugin.Info{
		Type: "iscsi",
		Capabilities: bardplugin.Capabilities{
			BlockDevice:               true, // a LUN is a block device the node formats + mounts
			RequiresControllerPublish: true, // the headline: attach is a control-plane op
			Expand:                    true, // lvextend + initiator rescan + fs grow
			Snapshots:                 false,
		},
	}
}

// ---- identity helpers ----------------------------------------------------

// lvName derives a bounded, deterministic LV/backstore name from the CSI name.
func lvName(csiName string) string {
	sum := sha256.Sum256([]byte(csiName))
	return "bard-" + hex.EncodeToString(sum[:8])
}

func (b *Backend) inst(instance string) (InstanceConfig, error) {
	ic, ok := b.instances[instance]
	if !ok || ic.VG == "" || ic.Portal == "" {
		return InstanceConfig{}, bardplugin.Errorf(bardplugin.CodeInvalidArg, "iscsi: instance %q not configured (need vg + portal)", instance)
	}
	if ic.IQNBase == "" {
		ic.IQNBase = defaultIQNBase
	}
	return ic, nil
}

// targetIQN is the per-volume target name; initiatorIQN is the per-node client
// name. Distinct sub-labels keep them from colliding under a shared base.
func targetIQN(base, lv string) string { return base + ":tgt-" + lv }

func initiatorIQN(base, nodeID string) string {
	return base + ":init-" + sanitizeIQN(nodeID)
}

// sanitizeIQN maps an arbitrary node id into the IQN charset ([a-z0-9.:-]).
func sanitizeIQN(s string) string {
	s = strings.ToLower(s)
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-':
			sb.WriteRune(r)
		default:
			sb.WriteByte('-')
		}
	}
	return sb.String()
}

func devPath(vg, lv string) string { return "/dev/" + vg + "/" + lv }
func backstore(lv string) string   { return "/backstores/block/" + lv }
func tpgPath(iqn string) string    { return "/iscsi/" + iqn + "/tpg1" }

// lvSizeBytes returns the LV size and whether it exists.
func (b *Backend) lvSizeBytes(ctx context.Context, vg, lv string) (int64, bool, error) {
	out, err := b.run.Run(ctx, "lvs", "--noheadings", "--units", "b", "--nosuffix", "-o", "lv_size", vg+"/"+lv)
	if err != nil {
		if isNotFound(err) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("iscsi: lvs %s/%s: %w", vg, lv, err)
	}
	n, perr := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if perr != nil {
		return 0, false, fmt.Errorf("iscsi: parse lv_size %q: %w", strings.TrimSpace(out), perr)
	}
	return n, true, nil
}

// ---- control plane -------------------------------------------------------

func (b *Backend) CreateVolume(ctx context.Context, req *bardplugin.CreateVolumeRequest) (*bardplugin.CreateVolumeResponse, error) {
	if req.SourceSnapshot != nil || req.SourceVolume != nil {
		return nil, bardplugin.Errorf(bardplugin.CodeInvalidArg, "iscsi: clone/restore not supported (snapshots are a follow-up)")
	}
	ic, err := b.inst(req.Instance)
	if err != nil {
		return nil, err
	}
	vg, lv := ic.VG, lvName(req.Name)

	// 1. The backing LV (idempotent via size check, like the LVM plugin).
	size, found, err := b.lvSizeBytes(ctx, vg, lv)
	if err != nil {
		return nil, err
	}
	if found {
		if size < req.CapacityBytes {
			return nil, bardplugin.Errorf(bardplugin.CodeAlreadyExists,
				"iscsi: volume %q exists at %d bytes, smaller than requested %d", req.Name, size, req.CapacityBytes)
		}
	} else {
		if _, err := b.run.Run(ctx, "lvcreate", "-n", lv, "-L", lvBytes(req.CapacityBytes), vg); err != nil {
			return nil, fmt.Errorf("iscsi: lvcreate %s/%s: %w", vg, lv, err)
		}
		if size, _, err = b.lvSizeBytes(ctx, vg, lv); err != nil {
			return nil, err
		}
	}

	// 2. The LIO export: backstore + per-volume target + LUN 0. Each step swallows
	//    "already exists" so a retried CreateVolume converges.
	iqn := targetIQN(ic.IQNBase, lv)
	// create takes the PARENT path + name=; the full backstore() path is what the
	// LUN below references (and what delete addresses).
	if _, err := b.run.Run(ctx, "targetcli", "/backstores/block", "create", "name="+lv, "dev="+devPath(vg, lv)); err != nil && !isExists(err) {
		return nil, fmt.Errorf("iscsi: create backstore %s: %w", lv, err)
	}
	if _, err := b.run.Run(ctx, "targetcli", "/iscsi", "create", iqn); err != nil && !isExists(err) {
		return nil, fmt.Errorf("iscsi: create target %s: %w", iqn, err)
	}
	if _, err := b.run.Run(ctx, "targetcli", tpgPath(iqn)+"/luns", "create", backstore(lv)); err != nil && !isExists(err) {
		return nil, fmt.Errorf("iscsi: map lun for %s: %w", iqn, err)
	}
	// Enforce ACLs: no demo mode (else any initiator could see every LUN), no
	// auth (CHAP is a follow-up). This is what makes per-node masking real.
	if _, err := b.run.Run(ctx, "targetcli", tpgPath(iqn), "set", "attribute",
		"generate_node_acls=0", "demo_mode_write_protect=0", "authentication=0"); err != nil {
		return nil, fmt.Errorf("iscsi: set tpg attributes for %s: %w", iqn, err)
	}

	return &bardplugin.CreateVolumeResponse{
		Location:      vg,
		Name:          lv,
		CapacityBytes: size,
	}, nil
}

func lvBytes(b int64) string {
	if b <= 0 {
		b = 1 << 20
	}
	return strconv.FormatInt(b, 10) + "b"
}

// DeleteVolume tears the export down then removes the LV, in dependency order
// (target -> backstore -> LV) so nothing references a removed object. Every step
// is idempotent and a non-not-found error is surfaced (never reports success while
// the volume's data could still exist -- no silent orphan).
func (b *Backend) DeleteVolume(ctx context.Context, req *bardplugin.DeleteVolumeRequest) error {
	vg, lv := req.Volume.Location, req.Volume.Name
	ic := b.instances[req.Volume.Instance]
	base := ic.IQNBase
	if base == "" {
		base = defaultIQNBase
	}
	iqn := targetIQN(base, lv)

	if _, err := b.run.Run(ctx, "targetcli", "/iscsi", "delete", iqn); err != nil && !isNotFound(err) {
		return fmt.Errorf("iscsi: delete target %s: %w", iqn, err)
	}
	if _, err := b.run.Run(ctx, "targetcli", "/backstores/block", "delete", lv); err != nil && !isNotFound(err) {
		return fmt.Errorf("iscsi: delete backstore %s: %w", lv, err)
	}
	if _, err := b.run.Run(ctx, "lvremove", "-f", vg+"/"+lv); err != nil && !isNotFound(err) {
		return fmt.Errorf("iscsi: lvremove %s/%s: %w", vg, lv, err)
	}
	return nil
}

func (b *Backend) ExpandVolume(ctx context.Context, req *bardplugin.ExpandVolumeRequest) (*bardplugin.ExpandVolumeResponse, error) {
	vg, lv := req.Volume.Location, req.Volume.Name
	if _, err := b.run.Run(ctx, "lvextend", "-L", strconv.FormatInt(req.NewSizeBytes, 10)+"b", vg+"/"+lv); err != nil && !isNotLarger(err) {
		return nil, fmt.Errorf("iscsi: lvextend %s/%s: %w", vg, lv, err)
	}
	size, _, err := b.lvSizeBytes(ctx, vg, lv)
	if err != nil {
		return nil, err
	}
	// The block backstore follows the LV's size; the initiator must rescan the
	// session and grow the filesystem (done in NodeExpand).
	return &bardplugin.ExpandVolumeResponse{CapacityBytes: size, NodeExpansionRequired: true}, nil
}

// Snapshots are a follow-up; reject clearly rather than pretend.
func (b *Backend) CreateSnapshot(_ context.Context, _ *bardplugin.CreateSnapshotRequest) (*bardplugin.CreateSnapshotResponse, error) {
	return nil, bardplugin.Errorf(bardplugin.CodeInvalidArg, "iscsi: snapshots not supported")
}

func (b *Backend) DeleteSnapshot(_ context.Context, _ *bardplugin.DeleteSnapshotRequest) error {
	return bardplugin.Errorf(bardplugin.CodeInvalidArg, "iscsi: snapshots not supported")
}

// ---- control-plane attach (ControllerPublisher) --------------------------

// ctxPortal/ctxIQN/ctxLUN are the PublishContext keys carried to NodeStage.
const (
	ctxPortal = "portal"
	ctxIQN    = "targetIqn"
	ctxLUN    = "lun"
)

// ControllerPublish masks this volume's LUN to the node by adding an ACL for the
// node's derived initiator IQN, then returns the connection context the node
// needs to log in. Idempotent: an existing ACL is fine.
func (b *Backend) ControllerPublish(ctx context.Context, req *bardplugin.ControllerPublishRequest) (*bardplugin.ControllerPublishResponse, error) {
	ic, err := b.inst(req.Volume.Instance)
	if err != nil {
		return nil, err
	}
	iqn := targetIQN(ic.IQNBase, req.Volume.Name)
	initIQN := initiatorIQN(ic.IQNBase, req.NodeID)
	// Creating the ACL auto-maps the target's single LUN (LUN 0) to this initiator.
	if _, err := b.run.Run(ctx, "targetcli", tpgPath(iqn)+"/acls", "create", initIQN); err != nil && !isExists(err) {
		return nil, fmt.Errorf("iscsi: create acl %s on %s: %w", initIQN, iqn, err)
	}
	return &bardplugin.ControllerPublishResponse{PublishContext: map[string]string{
		ctxPortal: ic.Portal,
		ctxIQN:    iqn,
		ctxLUN:    "0",
	}}, nil
}

// ControllerUnpublish removes the node's ACL, revoking its access. Idempotent: a
// missing ACL (already detached) succeeds.
func (b *Backend) ControllerUnpublish(ctx context.Context, req *bardplugin.ControllerUnpublishRequest) error {
	ic, err := b.inst(req.Volume.Instance)
	if err != nil {
		// Unknown instance: nothing we manage is attached. Treat as detached.
		return nil
	}
	iqn := targetIQN(ic.IQNBase, req.Volume.Name)
	initIQN := initiatorIQN(ic.IQNBase, req.NodeID)
	if _, err := b.run.Run(ctx, "targetcli", tpgPath(iqn)+"/acls", "delete", initIQN); err != nil && !isNotFound(err) {
		return fmt.Errorf("iscsi: delete acl %s on %s: %w", initIQN, iqn, err)
	}
	return nil
}

// ---- node plane ----------------------------------------------------------

// stagedState records per-staging-path session info so NodeUnstage can log out
// and verify detachment with no PublishContext (CSI doesn't carry it on unstage).
type stagedState struct {
	Device string `json:"device"`
	IQN    string `json:"iqn"`
	Portal string `json:"portal"`
}

func (b *Backend) statePath(stagingPath string) string {
	sum := sha256.Sum256([]byte(stagingPath))
	return filepath.Join(b.stateDir, hex.EncodeToString(sum[:16]))
}

func (b *Backend) recordState(stagingPath string, st stagedState) error {
	if b.stateDir == "" {
		return nil
	}
	if err := os.MkdirAll(b.stateDir, 0o750); err != nil {
		return fmt.Errorf("iscsi: state dir: %w", err)
	}
	data, _ := json.Marshal(st)
	if err := os.WriteFile(b.statePath(stagingPath), data, 0o600); err != nil {
		return fmt.Errorf("iscsi: record state: %w", err)
	}
	return nil
}

func (b *Backend) loadState(stagingPath string) (stagedState, bool) {
	if b.stateDir == "" {
		return stagedState{}, false
	}
	data, err := os.ReadFile(b.statePath(stagingPath))
	if err != nil {
		return stagedState{}, false
	}
	var st stagedState
	if json.Unmarshal(data, &st) != nil {
		return stagedState{}, false
	}
	return st, true
}

func (b *Backend) clearState(stagingPath string) {
	if b.stateDir != "" {
		_ = os.Remove(b.statePath(stagingPath))
	}
}

// byPath is the stable device symlink the kernel creates for a logged-in LUN.
func byPath(portal, iqn, lun string) string {
	return "/dev/disk/by-path/ip-" + portal + "-iscsi-" + iqn + "-lun-" + lun
}

// ensureIface creates the dedicated iscsiadm iface carrying this node's derived
// initiator IQN, so logins present the IQN the controller put in the ACL without
// touching the host's global initiatorname. Idempotent.
func (b *Backend) ensureIface(ctx context.Context, initIQN string) error {
	if _, err := b.run.Run(ctx, "iscsiadm", "-m", "iface", "-I", iscsiIface, "--op", "new"); err != nil && !isExists(err) {
		return fmt.Errorf("iscsi: create iface: %w", err)
	}
	if _, err := b.run.Run(ctx, "iscsiadm", "-m", "iface", "-I", iscsiIface, "--op", "update",
		"-n", "iface.initiatorname", "-v", initIQN); err != nil {
		return fmt.Errorf("iscsi: set iface initiatorname: %w", err)
	}
	return nil
}

// waitForDevice polls until the device reports a non-zero size (the LUN may
// appear before it is fully sized after login/rescan).
func (b *Backend) waitForDevice(ctx context.Context, dev string) error {
	deadline := time.Now().Add(20 * time.Second)
	for {
		out, _ := b.run.Run(ctx, "blockdev", "--getsize64", dev)
		if n, _ := strconv.ParseInt(strings.TrimSpace(out), 10, 64); n > 0 {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("iscsi: wait for device %s: %w", dev, ctx.Err())
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("iscsi: device %s not ready (size 0) after timeout", dev)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func (b *Backend) NodeStage(ctx context.Context, req *bardplugin.NodeStageRequest) error {
	ic, err := b.inst(req.Volume.Instance)
	if err != nil {
		return err
	}
	// Connection details come from ControllerPublish via PublishContext; fall back
	// to deriving them (so a manual/no-attach run still works).
	portal := req.PublishContext[ctxPortal]
	iqn := req.PublishContext[ctxIQN]
	lun := req.PublishContext[ctxLUN]
	if portal == "" {
		portal = ic.Portal
	}
	if iqn == "" {
		iqn = targetIQN(ic.IQNBase, req.Volume.Name)
	}
	if lun == "" {
		lun = "0"
	}
	initIQN := initiatorIQN(ic.IQNBase, b.nodeID)

	if err := b.ensureIface(ctx, initIQN); err != nil {
		return err
	}
	// Discover the target on the portal, then log in under our iface. Both are
	// idempotent on a stage retry.
	if _, err := b.run.Run(ctx, "iscsiadm", "-m", "discovery", "-t", "sendtargets", "-p", portal, "-I", iscsiIface); err != nil && !isExists(err) {
		return fmt.Errorf("iscsi: discovery on %s: %w", portal, err)
	}
	if _, err := b.run.Run(ctx, "iscsiadm", "-m", "node", "-T", iqn, "-p", portal, "-I", iscsiIface, "--login"); err != nil && !isAlreadyLoggedIn(err) {
		return fmt.Errorf("iscsi: login to %s: %w", iqn, err)
	}

	dev := byPath(portal, iqn, lun)
	if err := b.waitForDevice(ctx, dev); err != nil {
		return err
	}
	if err := b.recordState(req.StagingPath, stagedState{Device: dev, IQN: iqn, Portal: portal}); err != nil {
		return err
	}

	if req.Block {
		return nil // raw block: device published directly, nothing to format/mount
	}
	fsType := req.FsType
	if fsType == "" {
		fsType = defaultFsType
	}
	if !supportedFsTypes[fsType] {
		return bardplugin.Errorf(bardplugin.CodeInvalidArg,
			"iscsi: unsupported fsType %q (supported: ext4, ext3, ext2, xfs, btrfs)", fsType)
	}
	if err := b.ensureFormatted(ctx, dev, fsType); err != nil {
		return err
	}
	if err := os.MkdirAll(req.StagingPath, 0o750); err != nil {
		return fmt.Errorf("iscsi: mkdir staging: %w", err)
	}
	if out, _ := b.run.Run(ctx, "findmnt", "-n", "-o", "SOURCE", "--mountpoint", req.StagingPath); strings.TrimSpace(out) != "" {
		return nil // idempotent: already mounted on a retry
	}
	mountArgs := []string{"-t", fsType}
	if len(req.MountFlags) > 0 {
		mountArgs = append(mountArgs, "-o", strings.Join(req.MountFlags, ","))
	}
	mountArgs = append(mountArgs, dev, req.StagingPath)
	if _, err := b.run.Run(ctx, "mount", mountArgs...); err != nil {
		return fmt.Errorf("iscsi: mount %s -> %s: %w", dev, req.StagingPath, err)
	}
	return nil
}

// NodeUnstage unmounts, logs out the session, and verifies the device is gone --
// returning an error if it is still present so kubelet retries (never reports
// success while the LUN is still attached, mirroring the rbd-nbd unmap rule).
func (b *Backend) NodeUnstage(ctx context.Context, req *bardplugin.NodeUnstageRequest) error {
	if _, err := b.run.Run(ctx, "umount", req.StagingPath); err != nil && !isNotMounted(err) {
		return fmt.Errorf("iscsi: umount %s: %w", req.StagingPath, err)
	}
	st, ok := b.loadState(req.StagingPath)
	if !ok {
		return nil // never staged (or already cleaned): idempotent success
	}
	if _, err := b.run.Run(ctx, "iscsiadm", "-m", "node", "-T", st.IQN, "-p", st.Portal, "--logout"); err != nil && !isNotFound(err) {
		return fmt.Errorf("iscsi: logout %s: %w", st.IQN, err)
	}
	// Confirm the device is actually gone before declaring success.
	if out, _ := b.run.Run(ctx, "blockdev", "--getsize64", st.Device); strings.TrimSpace(out) != "" && strings.TrimSpace(out) != "0" {
		return fmt.Errorf("iscsi: device %s still present after logout", st.Device)
	}
	// Best-effort cleanup of the node record, then drop our state.
	_, _ = b.run.Run(ctx, "iscsiadm", "-m", "node", "-T", st.IQN, "-p", st.Portal, "--op", "delete")
	b.clearState(req.StagingPath)
	return nil
}

func (b *Backend) NodePublish(ctx context.Context, req *bardplugin.NodePublishRequest) error {
	if req.Block {
		// Raw block: bind-mount the staged device node to the target path.
		st, ok := b.loadState(req.StagingPath)
		dev := st.Device
		if !ok || dev == "" {
			return bardplugin.Errorf(bardplugin.CodeInvalidArg, "iscsi: no staged device for %s", req.StagingPath)
		}
		if err := os.MkdirAll(filepath.Dir(req.TargetPath), 0o750); err != nil {
			return fmt.Errorf("iscsi: mkdir target parent: %w", err)
		}
		if f, err := os.OpenFile(req.TargetPath, os.O_CREATE, 0o600); err == nil {
			_ = f.Close()
		}
		if out, _ := b.run.Run(ctx, "findmnt", "-n", "-o", "SOURCE", "--mountpoint", req.TargetPath); strings.TrimSpace(out) != "" {
			return nil
		}
		if _, err := b.run.Run(ctx, "mount", "--bind", dev, req.TargetPath); err != nil {
			return fmt.Errorf("iscsi: bind block %s -> %s: %w", dev, req.TargetPath, err)
		}
		return nil
	}
	if err := os.MkdirAll(req.TargetPath, 0o750); err != nil {
		return fmt.Errorf("iscsi: mkdir target: %w", err)
	}
	if out, _ := b.run.Run(ctx, "findmnt", "-n", "-o", "SOURCE", "--mountpoint", req.TargetPath); strings.TrimSpace(out) != "" {
		return nil // idempotent
	}
	if _, err := b.run.Run(ctx, "mount", "--bind", req.StagingPath, req.TargetPath); err != nil {
		return fmt.Errorf("iscsi: bind mount %s -> %s: %w", req.StagingPath, req.TargetPath, err)
	}
	if req.Readonly {
		if _, err := b.run.Run(ctx, "mount", "-o", "remount,ro,bind", req.StagingPath, req.TargetPath); err != nil {
			return fmt.Errorf("iscsi: remount ro %s: %w", req.TargetPath, err)
		}
	}
	return nil
}

func (b *Backend) NodeUnpublish(ctx context.Context, req *bardplugin.NodeUnpublishRequest) error {
	if _, err := b.run.Run(ctx, "umount", req.TargetPath); err != nil && !isNotMounted(err) {
		return fmt.Errorf("iscsi: umount %s: %w", req.TargetPath, err)
	}
	return nil
}

// NodeExpand rescans the iSCSI session so the device picks up the LV's new size,
// then grows the filesystem.
func (b *Backend) NodeExpand(ctx context.Context, req *bardplugin.NodeExpandRequest) (*bardplugin.NodeExpandResponse, error) {
	// Rescan all sessions: cheap, and avoids needing the per-volume target here.
	if _, err := b.run.Run(ctx, "iscsiadm", "-m", "session", "--rescan"); err != nil && !isNotFound(err) {
		return nil, fmt.Errorf("iscsi: session rescan: %w", err)
	}
	dev, err := b.run.Run(ctx, "findmnt", "-n", "-o", "SOURCE", "--target", req.VolumePath)
	if err != nil {
		return nil, fmt.Errorf("iscsi: resolve device for %s: %w", req.VolumePath, err)
	}
	dev = strings.TrimSpace(dev)
	fsType, _ := b.run.Run(ctx, "findmnt", "-n", "-o", "FSTYPE", "--target", req.VolumePath)
	switch strings.TrimSpace(fsType) {
	case "xfs":
		_, err = b.run.Run(ctx, "xfs_growfs", req.VolumePath)
	case "btrfs":
		_, err = b.run.Run(ctx, "btrfs", "filesystem", "resize", "max", req.VolumePath)
	default: // ext2/3/4
		_, err = b.run.Run(ctx, "resize2fs", dev)
	}
	if err != nil {
		return nil, fmt.Errorf("iscsi: grow filesystem on %s: %w", dev, err)
	}
	return &bardplugin.NodeExpandResponse{}, nil
}

func (b *Backend) ensureFormatted(ctx context.Context, dev, fsType string) error {
	out, _ := b.run.Run(ctx, "blkid", "-o", "value", "-s", "TYPE", dev)
	if strings.TrimSpace(out) != "" {
		return nil
	}
	if _, err := b.run.Run(ctx, "mkfs."+fsType, dev); err != nil {
		return fmt.Errorf("iscsi: mkfs.%s %s: %w", fsType, dev, err)
	}
	return nil
}

// ---- optional capabilities -----------------------------------------------

// GetCapacity (bardplugin.CapacityReporter) reports the backing VG's free space.
func (b *Backend) GetCapacity(ctx context.Context, req *bardplugin.GetCapacityRequest) (*bardplugin.GetCapacityResponse, error) {
	ic, err := b.inst(req.Instance)
	if err != nil {
		return nil, err
	}
	out, err := b.run.Run(ctx, "vgs", "--noheadings", "--units", "b", "--nosuffix", "-o", "vg_free", ic.VG)
	if err != nil {
		return nil, fmt.Errorf("iscsi: vgs %s: %w", ic.VG, err)
	}
	n, perr := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if perr != nil {
		return nil, fmt.Errorf("iscsi: parse vg_free %q: %w", strings.TrimSpace(out), perr)
	}
	return &bardplugin.GetCapacityResponse{AvailableBytes: n}, nil
}

// GetVolumeHealth (bardplugin.HealthReporter) reports whether the backing LV
// still exists (deleted out of band => abnormal).
func (b *Backend) GetVolumeHealth(ctx context.Context, req *bardplugin.GetVolumeHealthRequest) (*bardplugin.GetVolumeHealthResponse, error) {
	_, found, err := b.lvSizeBytes(ctx, req.Volume.Location, req.Volume.Name)
	if err != nil {
		return nil, err
	}
	if !found {
		return &bardplugin.GetVolumeHealthResponse{Abnormal: true, Message: "backing logical volume not found"}, nil
	}
	return &bardplugin.GetVolumeHealthResponse{Abnormal: false, Message: "present"}, nil
}

// NodeReclaimSpace (bardplugin.NodeSpaceReclaimer) fstrims the mounted filesystem;
// the discards travel over iSCSI down to the LUN (and, on a thin backstore, free
// the thin pool). A no-op for raw block.
func (b *Backend) NodeReclaimSpace(ctx context.Context, req *bardplugin.NodeReclaimSpaceRequest) (*bardplugin.ReclaimSpaceResponse, error) {
	if req.Block {
		return &bardplugin.ReclaimSpaceResponse{PreUsageBytes: -1, PostUsageBytes: -1}, nil
	}
	path := req.VolumePath
	if path == "" {
		path = req.StagingPath
	}
	if _, err := b.run.Run(ctx, "fstrim", path); err != nil {
		return nil, fmt.Errorf("iscsi: fstrim %s: %w", path, err)
	}
	return &bardplugin.ReclaimSpaceResponse{PreUsageBytes: -1, PostUsageBytes: -1}, nil
}

// ListVolumes (bardplugin.VolumeLister) enumerates the Bard volume LVs (the
// "bard-" prefix from lvName) across all instances' VGs. iSCSI has no snapshots,
// so it implements no SnapshotLister.
func (b *Backend) ListVolumes(ctx context.Context, _ *bardplugin.ListVolumesRequest) (*bardplugin.ListVolumesResponse, error) {
	var entries []bardplugin.VolumeListEntry
	for instance, ic := range b.instances {
		if ic.VG == "" {
			continue
		}
		out, err := b.run.Run(ctx, "lvs", "--noheadings", "--units", "b", "--nosuffix",
			"--separator", "|", "-o", "lv_name,lv_size,lv_attr", ic.VG)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("iscsi: lvs %s: %w", ic.VG, err)
		}
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			f := strings.Split(strings.TrimSpace(line), "|")
			if len(f) < 3 || !strings.HasPrefix(strings.TrimSpace(f[0]), "bard-") {
				continue
			}
			if attr := strings.TrimSpace(f[2]); len(attr) > 0 && attr[0] == 't' {
				continue // a thin pool, not a volume
			}
			size, _ := strconv.ParseInt(strings.TrimSpace(f[1]), 10, 64)
			entries = append(entries, bardplugin.VolumeListEntry{
				Volume:        bardplugin.VolumeRef{Instance: instance, Location: ic.VG, Name: strings.TrimSpace(f[0])},
				CapacityBytes: size,
			})
		}
	}
	return &bardplugin.ListVolumesResponse{Entries: entries}, nil
}
