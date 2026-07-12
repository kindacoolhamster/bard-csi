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
// # Snapshots and clone (thin LVs)
//
// With a thin pool configured (per instance or per StorageClass `thinPool`),
// volumes are thin LVs and support cheap copy-on-write snapshots and clones,
// exactly like the LVM plugin: CreateSnapshot makes a read-only thin snapshot
// (never exported through LIO -- it is a control-plane object), and
// restore/clone makes a writable thin snapshot of the source, grows it to the
// requested size, and exports it through its own target like any other volume.
// Snapshot/clone of a thick volume is rejected.
//
// # CHAP
//
// An instance with `chapAuth: true` enforces CHAP on the data path: the target
// requires authentication (`authentication=1` on the TPG), ControllerPublish
// sets the credentials on the node's ACL, and NodeStage sets them on the node
// record before login. Credentials are plugin-resolved per instance from a
// mounted Secret file (--chap-dir/<instance>) -- they never ride the
// StorageClass, the volume context, or the PublishContext (which lands in the
// API-visible VolumeAttachment).
//
// Multipath and remote LIO management (targetd) are deliberately out of scope;
// like the other plugins this depends only on the public bardplugin SDK.
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
// IQN prefix for derived target/initiator names. Access is controlled by per-node
// ACLs, plus CHAP when enabled.
type InstanceConfig struct {
	VG      string `json:"vg"`                // VG to carve LUN backstores from
	Portal  string `json:"portal"`            // iSCSI portal "ip:port" nodes connect to
	IQNBase string `json:"iqnBase,omitempty"` // IQN prefix (default iqn.2025-01.io.bard)
	// ThinPool is the instance default thin pool: when set (and not overridden by
	// the StorageClass thinPool parameter), volumes are thin copy-on-write LVs from
	// that pre-created pool -- what enables snapshots/clone -- instead of thick ones.
	ThinPool string `json:"thinPool,omitempty"`
	// CHAPAuth enforces CHAP on this instance's targets. The credentials are NOT
	// in this config (it ships in a ConfigMap): they are read from the mounted
	// Secret file <chap-dir>/<instance> -- see chapFor for the format.
	CHAPAuth bool `json:"chapAuth,omitempty"`
}

// Backend implements bardplugin.Backend (+ ControllerPublisher) for iSCSI.
type Backend struct {
	instances map[string]InstanceConfig
	nodeID    string // CSI node id (node plane only); source of this node's initiator IQN
	stateDir  string // node-plane: records per-staging-path session state for unstage
	chapDir   string // dir of per-instance CHAP credential files (mounted Secret)
	// iscsiadmChroot, when set (node plane, in-cluster), runs every iscsiadm
	// through `chroot <dir>` so the HOST's own initiator stack is used -- see
	// the iscsiadm method for why that is required.
	iscsiadmChroot string
	run            Runner
}

// New builds the iSCSI plugin backend. nodeID + stateDir are only meaningful on
// the node plane (the controller plane passes ""); chapDir is where per-instance
// CHAP credential files are mounted (both planes need it when chapAuth is on);
// iscsiadmChroot optionally chroots iscsiadm into the host root (node plane,
// in-cluster -- empty runs it directly, e.g. on a host or in the test harness).
func New(instances map[string]InstanceConfig, nodeID, stateDir, chapDir, iscsiadmChroot string, run Runner) *Backend {
	if run == nil {
		run = ExecRunner{}
	}
	return &Backend{instances: instances, nodeID: nodeID, stateDir: stateDir, chapDir: chapDir,
		iscsiadmChroot: iscsiadmChroot, run: run}
}

// iscsiadm runs iscsiadm, chrooted into the host root when configured.
//
// The initiator stack is a MATCHED PAIR: iscsiadm talks a version-sensitive
// binary IPC to iscsid (over an abstract socket, shared with the host under
// hostNetwork), and each distro's build reads its own node/iface DB path
// (Debian /etc/iscsi, RHEL /var/lib/iscsi). A container-shipped iscsiadm
// driving the HOST's iscsid mis-pairs both halves: the CHAP node record lands
// where iscsid never looks and the login dies in negotiation (found live
// in-cluster: LIO logged "iSCSI Login negotiation failed" and the login hung,
// while the host's own iscsiadm logged straight in). Chrooting makes the node
// plane use the host's iscsiadm + DB + iscsid, whatever the host distro --
// the same approach other CSI drivers use for iscsiadm.
func (b *Backend) iscsiadm(ctx context.Context, args ...string) (string, error) {
	if b.iscsiadmChroot != "" {
		return b.run.Run(ctx, "chroot", append([]string{b.iscsiadmChroot, "iscsiadm"}, args...)...)
	}
	return b.run.Run(ctx, "iscsiadm", args...)
}

func (b *Backend) Info() bardplugin.Info {
	return bardplugin.Info{
		Type: "iscsi",
		Capabilities: bardplugin.Capabilities{
			BlockDevice:               true, // a LUN is a block device the node formats + mounts
			RequiresControllerPublish: true, // the headline: attach is a control-plane op
			Expand:                    true, // lvextend + initiator rescan + fs grow
			Snapshots:                 true, // thin instances only; thick ones reject at CreateSnapshot
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

// paramThinPool is the StorageClass parameter naming the thin pool to provision
// from. It overrides the instance's thinPool, so thin-vs-thick can be a per-class
// choice or an instance default.
const paramThinPool = "thinPool"

// thinPoolFor resolves the thin pool for a new volume: the StorageClass parameter
// wins, else the instance default; "" means thick.
func (b *Backend) thinPoolFor(instance string, params map[string]string) string {
	if p := params[paramThinPool]; p != "" {
		return p
	}
	return b.instances[instance].ThinPool
}

// isThinLV reports whether an existing LV is a thin volume, read from its
// attributes (lv_attr starts with 'V') -- the actual truth, independent of
// config, so snapshot/clone of a source behaves correctly.
func (b *Backend) isThinLV(ctx context.Context, vg, lv string) (bool, error) {
	out, err := b.run.Run(ctx, "lvs", "--noheadings", "-o", "lv_attr", vg+"/"+lv)
	if err != nil {
		return false, fmt.Errorf("iscsi: lvs attr %s/%s: %w", vg, lv, err)
	}
	return strings.HasPrefix(strings.TrimSpace(out), "V"), nil
}

// snapName derives a bounded, deterministic snapshot LV name from a CSI name.
func snapName(csiName string) string {
	sum := sha256.Sum256([]byte(csiName))
	return "snap-" + hex.EncodeToString(sum[:8])
}

// lvOrigin returns an LV's origin (the LV it was snapshotted from) and whether
// the LV exists at all.
func (b *Backend) lvOrigin(ctx context.Context, vg, lv string) (string, bool, error) {
	out, err := b.run.Run(ctx, "lvs", "--noheadings", "-o", "origin", vg+"/"+lv)
	if err != nil {
		if isNotFound(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("iscsi: lvs origin %s/%s: %w", vg, lv, err)
	}
	return strings.TrimSpace(out), true, nil
}

// thinClone creates lv as a writable copy-on-write thin snapshot of src and
// activates it (thin snapshots are created with the activation-skip flag set, so
// the backstore device would otherwise not exist). Idempotent on the create.
func (b *Backend) thinClone(ctx context.Context, vg, src, lv string) error {
	if _, err := b.run.Run(ctx, "lvcreate", "-s", "-n", lv, vg+"/"+src); err != nil && !isExists(err) {
		return fmt.Errorf("iscsi: thin clone %s/%s -> %s: %w", vg, src, lv, err)
	}
	if _, err := b.run.Run(ctx, "lvchange", "-ay", "-Ky", vg+"/"+lv); err != nil {
		return fmt.Errorf("iscsi: activate clone %s/%s: %w", vg, lv, err)
	}
	return nil
}

// extendTo grows an LV to size bytes; a no-op when it is already that large.
func (b *Backend) extendTo(ctx context.Context, vg, lv string, size int64) error {
	if size <= 0 {
		return nil
	}
	if _, err := b.run.Run(ctx, "lvextend", "-L", strconv.FormatInt(size, 10)+"b", vg+"/"+lv); err != nil && !isNotLarger(err) {
		return fmt.Errorf("iscsi: lvextend %s/%s: %w", vg, lv, err)
	}
	return nil
}

// ---- CHAP ------------------------------------------------------------------

// chapCreds are one instance's CHAP credentials. Mutual (target->initiator) auth
// is optional and rides the same file.
type chapCreds struct {
	User, Password             string
	MutualUser, MutualPassword string
}

// chapFor loads the CHAP credentials for an instance, or nil when the instance
// does not enforce CHAP. The credential file is <chapDir>/<instance> (a mounted
// Secret key), containing 2 or 4 non-empty lines:
//
//	userid
//	password
//	mutual-userid    (optional pair: target->initiator auth)
//	mutual-password
//
// Line-per-field avoids delimiter ambiguity (an IQN-style userid contains ':').
// CHAP enabled without readable, well-formed credentials is an error, not a
// silent fallback to unauthenticated access.
func (b *Backend) chapFor(instance string) (*chapCreds, error) {
	ic, ok := b.instances[instance]
	if !ok || !ic.CHAPAuth {
		return nil, nil
	}
	path := filepath.Join(b.chapDir, instance)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, bardplugin.Errorf(bardplugin.CodeInternal,
			"iscsi: chapAuth is on for instance %q but credentials at %s are unreadable: %v", instance, path, err)
	}
	var lines []string
	for _, l := range strings.Split(string(data), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	switch len(lines) {
	case 2:
		return &chapCreds{User: lines[0], Password: lines[1]}, nil
	case 4:
		return &chapCreds{User: lines[0], Password: lines[1], MutualUser: lines[2], MutualPassword: lines[3]}, nil
	default:
		return nil, bardplugin.Errorf(bardplugin.CodeInternal,
			"iscsi: chap credentials at %s must be 2 lines (userid, password) or 4 (plus mutual pair), got %d", path, len(lines))
	}
}

// ---- control plane -------------------------------------------------------

func (b *Backend) CreateVolume(ctx context.Context, req *bardplugin.CreateVolumeRequest) (*bardplugin.CreateVolumeResponse, error) {
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
	isClone := req.SourceSnapshot != nil || req.SourceVolume != nil
	switch {
	case found:
		// Idempotent retry: an LV at least as large as requested satisfies the
		// request. A smaller existing LV is a name clash with a different request
		// -- except for a clone, which starts at its SOURCE's size: grow it to the
		// request (resuming a create whose extend never ran).
		if size < req.CapacityBytes {
			if !isClone {
				return nil, bardplugin.Errorf(bardplugin.CodeAlreadyExists,
					"iscsi: volume %q exists at %d bytes, smaller than requested %d", req.Name, size, req.CapacityBytes)
			}
			if err := b.extendTo(ctx, vg, lv, req.CapacityBytes); err != nil {
				return nil, err
			}
		}
	default:
		pool := b.thinPoolFor(req.Instance, req.Parameters)
		switch {
		case isClone:
			// Clone/restore is a writable copy-on-write thin snapshot of the source,
			// which only exists if the source itself is a thin LV.
			src := req.SourceVolume
			if req.SourceSnapshot != nil {
				src = req.SourceSnapshot
			}
			thin, err := b.isThinLV(ctx, vg, src.Name)
			if err != nil {
				return nil, err
			}
			if !thin {
				return nil, bardplugin.Errorf(bardplugin.CodeInvalidArg, "iscsi: clone/restore requires a thin source volume")
			}
			if err := b.thinClone(ctx, vg, src.Name, lv); err != nil {
				return nil, err
			}
			// The clone inherits the SOURCE's virtual size; grow it to the request
			// so the volume matches its PV (the node grows the filesystem at stage).
			if err := b.extendTo(ctx, vg, lv, req.CapacityBytes); err != nil {
				return nil, err
			}
		case pool != "":
			// Thin volume: -V is the virtual (logical) size carved from the pool.
			if _, err := b.run.Run(ctx, "lvcreate", "-T", vg+"/"+pool, "-V", lvBytes(req.CapacityBytes), "-n", lv); err != nil {
				return nil, fmt.Errorf("iscsi: lvcreate thin %s/%s: %w", vg, lv, err)
			}
		default:
			// Thick volume: -L fully allocates the size up front.
			if _, err := b.run.Run(ctx, "lvcreate", "-n", lv, "-L", lvBytes(req.CapacityBytes), vg); err != nil {
				return nil, fmt.Errorf("iscsi: lvcreate %s/%s: %w", vg, lv, err)
			}
		}
	}
	if size, _, err = b.lvSizeBytes(ctx, vg, lv); err != nil {
		return nil, err
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
	// Enforce ACLs: no demo mode (else any initiator could see every LUN). This
	// is what makes per-node masking real. authentication=1 additionally requires
	// CHAP (the credentials go on each node's ACL at ControllerPublish).
	auth := "authentication=0"
	if ic.CHAPAuth {
		auth = "authentication=1"
	}
	if _, err := b.run.Run(ctx, "targetcli", tpgPath(iqn), "set", "attribute",
		"generate_node_acls=0", "demo_mode_write_protect=0", auth); err != nil {
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
	if err := b.extendTo(ctx, vg, lv, req.NewSizeBytes); err != nil {
		return nil, err
	}
	size, _, err := b.lvSizeBytes(ctx, vg, lv)
	if err != nil {
		return nil, err
	}
	// The block backstore follows the LV's size; the initiator must rescan the
	// session and grow the filesystem (done in NodeExpand).
	return &bardplugin.ExpandVolumeResponse{CapacityBytes: size, NodeExpansionRequired: true}, nil
}

// CreateSnapshot makes a read-only copy-on-write thin snapshot of the source LV,
// exactly like the LVM plugin. Thin only: a thick volume has no cheap snapshot,
// so it is rejected. The snapshot is a control-plane object -- it gets NO LIO
// export; a restore clones it into a new volume with its own target.
func (b *Backend) CreateSnapshot(ctx context.Context, req *bardplugin.CreateSnapshotRequest) (*bardplugin.CreateSnapshotResponse, error) {
	src := req.SourceVolume // Location=vg, Name=lv
	thin, err := b.isThinLV(ctx, src.Location, src.Name)
	if err != nil {
		return nil, err
	}
	if !thin {
		return nil, bardplugin.Errorf(bardplugin.CodeInvalidArg, "iscsi: snapshots require a thin volume (provision from a thinPool)")
	}
	vg, snap := src.Location, snapName(req.Name)
	// Snapshot LVs share the VG namespace, so the derived name can already exist
	// -- as an idempotent retry against THIS source (fine), or as the same CSI
	// name against a DIFFERENT source, which must be an AlreadyExists error, not
	// a silent reuse of the other source's snapshot.
	if origin, exists, err := b.lvOrigin(ctx, vg, snap); err != nil {
		return nil, err
	} else if exists && origin != src.Name {
		return nil, bardplugin.Errorf(bardplugin.CodeAlreadyExists,
			"iscsi: snapshot %q already exists for a different source volume (%s)", req.Name, origin)
	}
	if _, err := b.run.Run(ctx, "lvcreate", "-s", "-pr", "-n", snap, vg+"/"+src.Name); err != nil && !isExists(err) {
		return nil, fmt.Errorf("iscsi: snapshot %s/%s: %w", vg, src.Name, err)
	}
	size, _, err := b.lvSizeBytes(ctx, vg, src.Name)
	if err != nil {
		return nil, err
	}
	return &bardplugin.CreateSnapshotResponse{
		Location:         vg,
		Name:             snap,
		SizeBytes:        size,
		CreationTimeUnix: time.Now().Unix(),
		ReadyToUse:       true,
	}, nil
}

func (b *Backend) DeleteSnapshot(ctx context.Context, req *bardplugin.DeleteSnapshotRequest) error {
	vgsnap := req.Snapshot.Location + "/" + req.Snapshot.Name
	if _, err := b.run.Run(ctx, "lvremove", "-f", vgsnap); err != nil && !isNotFound(err) {
		return fmt.Errorf("iscsi: lvremove snapshot %s: %w", vgsnap, err)
	}
	return nil
}

// ---- control-plane attach (ControllerPublisher) --------------------------

// ctxPortal/ctxIQN/ctxLUN are the PublishContext keys carried to NodeStage.
const (
	ctxPortal = "portal"
	ctxIQN    = "targetIqn"
	ctxLUN    = "lun"
)

// ControllerPublish masks this volume's LUN to the node by adding an ACL for the
// node's derived initiator IQN (setting the CHAP credentials on it when the
// instance enforces CHAP), then returns the connection context the node needs to
// log in. The credentials are NOT part of that context -- PublishContext lands in
// the API-visible VolumeAttachment; the node reads its own mounted copy instead.
// Idempotent: an existing ACL is fine, and re-setting auth converges.
func (b *Backend) ControllerPublish(ctx context.Context, req *bardplugin.ControllerPublishRequest) (*bardplugin.ControllerPublishResponse, error) {
	ic, err := b.inst(req.Volume.Instance)
	if err != nil {
		return nil, err
	}
	chap, err := b.chapFor(req.Volume.Instance)
	if err != nil {
		return nil, err
	}
	iqn := targetIQN(ic.IQNBase, req.Volume.Name)
	initIQN := initiatorIQN(ic.IQNBase, req.NodeID)
	// Creating the ACL auto-maps the target's single LUN (LUN 0) to this initiator.
	if _, err := b.run.Run(ctx, "targetcli", tpgPath(iqn)+"/acls", "create", initIQN); err != nil && !isExists(err) {
		return nil, fmt.Errorf("iscsi: create acl %s on %s: %w", initIQN, iqn, err)
	}
	if chap != nil {
		args := []string{tpgPath(iqn) + "/acls/" + initIQN, "set", "auth",
			"userid=" + chap.User, "password=" + chap.Password}
		if chap.MutualUser != "" {
			args = append(args, "mutual_userid="+chap.MutualUser, "mutual_password="+chap.MutualPassword)
		}
		if _, err := b.run.Run(ctx, "targetcli", args...); err != nil {
			return nil, fmt.Errorf("iscsi: set chap auth on acl %s: %w", initIQN, err)
		}
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

// ensureIface makes the dedicated iscsiadm iface carry this node's derived
// initiator IQN, so logins present the IQN the controller put in the ACL without
// touching the host's global initiatorname.
//
// The fast path is a READ: when the iface already carries the IQN, do nothing.
// That is essential for staging a SECOND volume on a node -- iscsiadm refuses to
// create or update an iface that a live session is using (exit 15, "Could not
// create new interface"), so the old create-first flow failed every stage after
// the first until that session logged out (found live by the multi-volume
// harness; single-volume tests never see it). Only a missing iface is created,
// and only a wrong/unset IQN is updated -- and an update refused because of live
// sessions is then a real conflict (the node id changed under an active mount)
// that must surface, not be swallowed.
func (b *Backend) ensureIface(ctx context.Context, initIQN string) error {
	out, err := b.iscsiadm(ctx, "-m", "iface", "-I", iscsiIface)
	if err == nil && strings.Contains(out, "iface.initiatorname = "+initIQN+"\n") {
		return nil
	}
	if err != nil { // no iface record yet
		if _, cerr := b.iscsiadm(ctx, "-m", "iface", "-I", iscsiIface, "--op", "new"); cerr != nil && !isExists(cerr) {
			return fmt.Errorf("iscsi: create iface: %w", cerr)
		}
	}
	if _, err := b.iscsiadm(ctx, "-m", "iface", "-I", iscsiIface, "--op", "update",
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
	// idempotent on a stage retry. When the instance enforces CHAP, the
	// credentials go on the discovered node record before the login (LIO's
	// discovery itself stays unauthenticated; only the login is gated).
	if _, err := b.iscsiadm(ctx, "-m", "discovery", "-t", "sendtargets", "-p", portal, "-I", iscsiIface); err != nil && !isExists(err) {
		return fmt.Errorf("iscsi: discovery on %s: %w", portal, err)
	}
	chap, err := b.chapFor(req.Volume.Instance)
	if err != nil {
		return err
	}
	if chap != nil {
		if err := b.setChapOnNode(ctx, iqn, portal, chap); err != nil {
			return err
		}
	}
	if _, err := b.iscsiadm(ctx, "-m", "node", "-T", iqn, "-p", portal, "-I", iscsiIface, "--login"); err != nil && !isAlreadyLoggedIn(err) {
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
	// Idempotent: skip the mount if the staging path is itself already a mount
	// (retry); the grow below still runs (a clone retry may have mounted but not
	// yet grown).
	if out, _ := b.run.Run(ctx, "findmnt", "-n", "-o", "SOURCE", "--mountpoint", req.StagingPath); strings.TrimSpace(out) == "" {
		mountArgs := []string{"-t", fsType}
		if len(req.MountFlags) > 0 {
			mountArgs = append(mountArgs, "-o", strings.Join(req.MountFlags, ","))
		}
		mountArgs = append(mountArgs, dev, req.StagingPath)
		if _, err := b.run.Run(ctx, "mount", mountArgs...); err != nil {
			return fmt.Errorf("iscsi: mount %s -> %s: %w", dev, req.StagingPath, err)
		}
	}
	// A clone/restore into a LARGER volume carries its SOURCE's filesystem; grow
	// it to the device once mounted (online for every supported fs, a no-op when
	// the sizes already match -- i.e. every non-clone stage).
	return b.growFilesystem(ctx, fsType, dev, req.StagingPath)
}

// setChapOnNode writes the CHAP credentials onto the node record for (iqn,
// portal) under our iface, so the subsequent login authenticates. iscsiadm takes
// one name/value per --op update, so this is a short series of updates.
func (b *Backend) setChapOnNode(ctx context.Context, iqn, portal string, chap *chapCreds) error {
	params := [][2]string{
		{"node.session.auth.authmethod", "CHAP"},
		{"node.session.auth.username", chap.User},
		{"node.session.auth.password", chap.Password},
	}
	if chap.MutualUser != "" {
		params = append(params,
			[2]string{"node.session.auth.username_in", chap.MutualUser},
			[2]string{"node.session.auth.password_in", chap.MutualPassword})
	}
	for _, p := range params {
		if _, err := b.iscsiadm(ctx, "-m", "node", "-T", iqn, "-p", portal, "-I", iscsiIface,
			"--op", "update", "-n", p[0], "-v", p[1]); err != nil {
			return fmt.Errorf("iscsi: set %s on node record %s: %w", p[0], iqn, err)
		}
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
	if _, err := b.iscsiadm(ctx, "-m", "node", "-T", st.IQN, "-p", st.Portal, "--logout"); err != nil && !isNotFound(err) {
		return fmt.Errorf("iscsi: logout %s: %w", st.IQN, err)
	}
	// Confirm the device is actually gone before declaring success.
	if out, _ := b.run.Run(ctx, "blockdev", "--getsize64", st.Device); strings.TrimSpace(out) != "" && strings.TrimSpace(out) != "0" {
		return fmt.Errorf("iscsi: device %s still present after logout", st.Device)
	}
	// Best-effort cleanup of the node record, then drop our state.
	_, _ = b.iscsiadm(ctx, "-m", "node", "-T", st.IQN, "-p", st.Portal, "--op", "delete")
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
	if _, err := b.iscsiadm(ctx, "-m", "session", "--rescan"); err != nil && !isNotFound(err) {
		return nil, fmt.Errorf("iscsi: session rescan: %w", err)
	}
	dev, err := b.run.Run(ctx, "findmnt", "-n", "-o", "SOURCE", "--target", req.VolumePath)
	if err != nil {
		return nil, fmt.Errorf("iscsi: resolve device for %s: %w", req.VolumePath, err)
	}
	dev = strings.TrimSpace(dev)
	fsType, _ := b.run.Run(ctx, "findmnt", "-n", "-o", "FSTYPE", "--target", req.VolumePath)
	if err := b.growFilesystem(ctx, strings.TrimSpace(fsType), dev, req.VolumePath); err != nil {
		return nil, err
	}
	return &bardplugin.NodeExpandResponse{}, nil
}

// growFilesystem grows a mounted filesystem to its backing device's size --
// online for every supported filesystem, and a no-op when they already match.
// Used by NodeExpand (the device grew under a live mount) and NodeStage (a
// clone/restore into a larger volume carries the source's smaller filesystem).
func (b *Backend) growFilesystem(ctx context.Context, fsType, dev, mountPoint string) error {
	var err error
	switch fsType {
	case "xfs":
		// xfs grows by mountpoint, not device.
		_, err = b.run.Run(ctx, "xfs_growfs", mountPoint)
	case "btrfs":
		// btrfs grows online by mountpoint to fill the device.
		_, err = b.run.Run(ctx, "btrfs", "filesystem", "resize", "max", mountPoint)
	default: // ext2/3/4
		_, err = b.run.Run(ctx, "resize2fs", dev)
	}
	if err != nil {
		return fmt.Errorf("iscsi: grow %s filesystem on %s: %w", fsType, dev, err)
	}
	return nil
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

// lvInfo is one row of `lvs` for listing.
type lvInfo struct {
	name, attr, origin string
	size               int64
}

// listLVs returns the LVs in a VG (name, size, attr, origin) via a separator-
// delimited lvs so empty fields (no origin) parse deterministically.
func (b *Backend) listLVs(ctx context.Context, vg string) ([]lvInfo, error) {
	out, err := b.run.Run(ctx, "lvs", "--noheadings", "--units", "b", "--nosuffix",
		"--separator", "|", "-o", "lv_name,lv_size,lv_attr,origin", vg)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("iscsi: lvs %s: %w", vg, err)
	}
	var rows []lvInfo
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(strings.TrimSpace(line), "|")
		if len(f) < 3 || strings.TrimSpace(f[0]) == "" {
			continue
		}
		size, _ := strconv.ParseInt(strings.TrimSpace(f[1]), 10, 64)
		origin := ""
		if len(f) >= 4 {
			origin = strings.TrimSpace(f[3])
		}
		rows = append(rows, lvInfo{name: strings.TrimSpace(f[0]), size: size, attr: strings.TrimSpace(f[2]), origin: origin})
	}
	return rows, nil
}

// isThinPool reports whether an lv_attr marks a thin pool (so it isn't listed as
// a volume). The thin pool itself can share the "bard-" name prefix (bard-thin).
func isThinPool(attr string) bool { return len(attr) > 0 && attr[0] == 't' }

// ListVolumes (bardplugin.VolumeLister) enumerates the Bard volume LVs (the
// "bard-" prefix from lvName) across all instances' VGs, excluding thin pools and
// snapshots. Bard core sorts + paginates.
func (b *Backend) ListVolumes(ctx context.Context, _ *bardplugin.ListVolumesRequest) (*bardplugin.ListVolumesResponse, error) {
	var entries []bardplugin.VolumeListEntry
	for instance, ic := range b.instances {
		if ic.VG == "" {
			continue
		}
		rows, err := b.listLVs(ctx, ic.VG)
		if err != nil {
			return nil, err
		}
		for _, lv := range rows {
			if !strings.HasPrefix(lv.name, "bard-") || isThinPool(lv.attr) {
				continue
			}
			entries = append(entries, bardplugin.VolumeListEntry{
				Volume:        bardplugin.VolumeRef{Instance: instance, Location: ic.VG, Name: lv.name},
				CapacityBytes: lv.size,
			})
		}
	}
	return &bardplugin.ListVolumesResponse{Entries: entries}, nil
}

// ListSnapshots (bardplugin.SnapshotLister) enumerates the Bard snapshot LVs (the
// "snap-" prefix from snapName); each carries its origin LV as the source volume.
func (b *Backend) ListSnapshots(ctx context.Context, _ *bardplugin.ListSnapshotsRequest) (*bardplugin.ListSnapshotsResponse, error) {
	var entries []bardplugin.SnapshotListEntry
	for instance, ic := range b.instances {
		if ic.VG == "" {
			continue
		}
		rows, err := b.listLVs(ctx, ic.VG)
		if err != nil {
			return nil, err
		}
		for _, lv := range rows {
			if !strings.HasPrefix(lv.name, "snap-") || lv.origin == "" {
				continue
			}
			entries = append(entries, bardplugin.SnapshotListEntry{
				Snapshot:     bardplugin.VolumeRef{Instance: instance, Location: ic.VG, Name: lv.name},
				SourceVolume: bardplugin.VolumeRef{Instance: instance, Location: ic.VG, Name: lv.origin},
				SizeBytes:    lv.size,
				ReadyToUse:   true,
			})
		}
	}
	return &bardplugin.ListSnapshotsResponse{Entries: entries}, nil
}
