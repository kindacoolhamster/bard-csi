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
// dm-multipath (instance portals list) and remote LIO management (per-instance
// management: targetd) are supported; like the other plugins this depends only
// on the public bardplugin SDK.
package iscsiplugin

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

const (
	defaultFsType  = "ext4"
	defaultIQNBase = "iqn.2025-01.io.bard"
	// iscsiIface is the dedicated iscsiadm iface the node logs in under, so the
	// per-node initiator IQN is set without touching the host initiatorname.
	iscsiIface = "bard"
	// mgmtLocal/mgmtTargetd are the recognized InstanceConfig.Management values;
	// mgmtLocal is also the effective default when Management is "".
	mgmtLocal   = "local"
	mgmtTargetd = "targetd"
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
	VG     string `json:"vg"`     // VG to carve LUN backstores from
	Portal string `json:"portal"` // iSCSI portal "ip:port" nodes connect to
	// Portals is an optional list of "ip:port" portals for dm-multipath (one LIO
	// network portal per address, so a node logs in over every path). When set it
	// WINS over the single Portal (even if both are present); Portal alone still
	// works unchanged for every existing single-path instance. Use portalList() to
	// read either field, never Portal/Portals directly.
	Portals []string `json:"portals,omitempty"`
	IQNBase string   `json:"iqnBase,omitempty"` // IQN prefix (default iqn.2025-01.io.bard)
	// ThinPool is the instance default thin pool: when set (and not overridden by
	// the StorageClass thinPool parameter), volumes are thin copy-on-write LVs from
	// that pre-created pool -- what enables snapshots/clone -- instead of thick ones.
	ThinPool string `json:"thinPool,omitempty"`
	// CHAPAuth enforces CHAP on this instance's targets. The credentials are NOT
	// in this config (it ships in a ConfigMap): they are read from the mounted
	// Secret file <chap-dir>/<instance> -- see chapFor for the format.
	CHAPAuth bool `json:"chapAuth,omitempty"`

	// Management selects how this instance's LIO export(s) are administered.
	// "" or "local" (the default) drives targetcli directly against the
	// plugin's own host/configfs, as documented above -- unchanged. "targetd"
	// instead manages a REMOTE LIO host over targetd's JSON-RPC API, for a
	// control plane that does not run on the target node itself. inst()
	// rejects any other value.
	Management string `json:"management,omitempty"`
	// TargetdEndpoint is the targetd JSON-RPC endpoint (e.g.
	// "http://host:18700/targetrpc"). Required when Management is "targetd".
	TargetdEndpoint string `json:"targetdEndpoint,omitempty"`
	// TargetdPool is the targetd-side storage pool volumes are carved from
	// (targetd owns this pool remotely -- a targetd instance has no local VG).
	// Required when Management is "targetd".
	TargetdPool string `json:"targetdPool,omitempty"`
	// TargetIQN is the IQN of the single target targetd manages: unlike the
	// local one-target-per-volume model, targetd exposes every volume as a
	// LUN under ONE fixed target, matching targetd's API shape. Required when
	// Management is "targetd".
	TargetIQN string `json:"targetIqn,omitempty"`
}

// isTargetd reports whether this instance's LIO export is administered
// remotely via targetd rather than local targetcli.
func (ic InstanceConfig) isTargetd() bool { return ic.Management == mgmtTargetd }

// portalList is the single read path for an instance's portal(s): Portals if
// non-empty, else the single Portal wrapped in a one-element list, else nil.
// Every direct Portal/Portals read in this file goes through this so the
// single-vs-multi-portal decision lives in exactly one place.
func (ic InstanceConfig) portalList() []string {
	if len(ic.Portals) > 0 {
		return ic.Portals
	}
	if ic.Portal != "" {
		return []string{ic.Portal}
	}
	return nil
}

// Backend implements bardplugin.Backend (+ ControllerPublisher) for iSCSI.
type Backend struct {
	instances map[string]InstanceConfig
	nodeID    string // CSI node id (node plane only); source of this node's initiator IQN
	stateDir  string // node-plane: records per-staging-path session state for unstage
	chapDir   string // dir of per-instance CHAP credential files (mounted Secret)
	// targetdDir is the dir of per-instance targetd JSON-RPC credential files
	// (mounted Secret), read only for instances with management: targetd --
	// see targetdCredsFor for the format. Mirrors chapDir.
	targetdDir string
	// iscsiadmChroot, when set (node plane, in-cluster), runs every iscsiadm
	// through `chroot <dir>` so the HOST's own initiator stack is used -- see
	// the iscsiadm method for why that is required.
	iscsiadmChroot string
	run            Runner
	// sysfsRoot/devRoot let dm-multipath resolution (WWID reads, by-path/by-id
	// device paths) be pointed at a fake tree in tests -- those are direct file
	// reads/symlinks the fakeRunner cannot intercept. Production defaults (set in
	// New) are the real /sys and /dev; kept unexported since New's signature is
	// the plugin's stable public entry point (used by cmd/bard-plugin-iscsi).
	sysfsRoot string
	devRoot   string
	// tdAccessMu serializes every targetd grantAccess/revokeAccess call across
	// ALL targetd instances. tdManager is built fresh per RPC call (see
	// newTdManager in targetd.go), so a mutex FIELD ON tdManager would guard
	// nothing across calls -- this one is owned by the long-lived Backend and
	// handed to each tdManager by pointer. Needed because core's inflight
	// guard keys per VOLUME, not per initiator: ControllerPublish for two
	// DIFFERENT volumes to the SAME initiator can run fully concurrently, and
	// without this lock both could read the same "used LUNs" snapshot,
	// compute the same lowest-unused LUN, and create two exports with a
	// colliding LUN. One global lock rather than per-instance: contention is
	// negligible at CSI scale, and it keeps the fix trivially correct.
	tdAccessMu sync.Mutex
}

// New builds the iSCSI plugin backend. nodeID + stateDir are only meaningful on
// the node plane (the controller plane passes ""); chapDir is where per-instance
// CHAP credential files are mounted (both planes need it when chapAuth is on);
// targetdDir is where per-instance targetd JSON-RPC credential files are mounted
// (both planes need it for a management: targetd instance -- see
// targetdCredsFor); iscsiadmChroot optionally chroots iscsiadm into the host
// root (node plane, in-cluster -- empty runs it directly, e.g. on a host or in
// the test harness).
func New(instances map[string]InstanceConfig, nodeID, stateDir, chapDir, targetdDir, iscsiadmChroot string, run Runner) *Backend {
	if run == nil {
		run = ExecRunner{}
	}
	return &Backend{instances: instances, nodeID: nodeID, stateDir: stateDir, chapDir: chapDir,
		targetdDir: targetdDir, iscsiadmChroot: iscsiadmChroot, run: run, sysfsRoot: "/sys", devRoot: "/dev"}
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
	portals := ic.portalList()
	switch ic.Management {
	case "", mgmtLocal:
		// Local (targetcli-driven) instance -- unchanged validation, byte-for-byte:
		// an unconfigured instance and a configured-but-underspecified one share
		// this one error.
		if !ok || ic.VG == "" || len(portals) == 0 {
			return InstanceConfig{}, bardplugin.Errorf(bardplugin.CodeInvalidArg, "iscsi: instance %q not configured (need vg + portal)", instance)
		}
	case mgmtTargetd:
		// targetd manages its own remote storage pool -- no local VG here.
		if !ok || ic.TargetdEndpoint == "" || ic.TargetdPool == "" || ic.TargetIQN == "" || len(portals) == 0 {
			return InstanceConfig{}, bardplugin.Errorf(bardplugin.CodeInvalidArg,
				"iscsi: instance %q management=targetd not configured (need targetdEndpoint + targetdPool + targetIqn + portal)", instance)
		}
		// targetd's own export_create hardcodes the shared target's TPG
		// `authentication` attribute to "0" on EVERY export -- unconditionally,
		// with no API to override it -- even though initiator_set_auth happily
		// stores CHAP credentials on the per-initiator ACL. Live-verified
		// (targetd 0.10.4, upstream git main, 2026-07-20): the resulting LIO
		// config is internally inconsistent -- the login response still
		// advertises AuthMethod=CHAP, but the kernel initiator aborts the
		// connection immediately after receiving it (never reaches the actual
		// CHAP challenge/response), for BOTH a correct password and no
		// credentials at all. So chapAuth: true on a targetd instance would
		// silently protect nothing while claiming to -- reject at config load
		// (the "Honest MVP" precedent CreateVolume already applies to
		// snapshots/clones on targetd) rather than ship a StorageClass flag
		// that lies about the data path's security.
		if ic.CHAPAuth {
			return InstanceConfig{}, bardplugin.Errorf(bardplugin.CodeInvalidArg,
				"iscsi: instance %q management=targetd cannot set chapAuth: true -- targetd's export_create "+
					"unconditionally disables TPG-level authentication on every export (upstream limitation, no API "+
					"to override), so CHAP credentials set via initiator_set_auth are never actually enforced; "+
					"access control on a targetd instance is IQN-based ACLs only", instance)
		}
	default:
		return InstanceConfig{}, bardplugin.Errorf(bardplugin.CodeInvalidArg,
			"iscsi: instance %q has unknown management %q (want \"local\" or \"targetd\")", instance, ic.Management)
	}
	for _, p := range portals {
		// Bracketed IPv6 portals ("[::1]:3260") are out of scope this round: the
		// explicit multi-portal split (splitPortal) is only proven against plain
		// "ip:port". Reject clearly here rather than risk a silent mis-split later.
		if strings.Contains(p, "[") {
			return InstanceConfig{}, bardplugin.Errorf(bardplugin.CodeInvalidArg,
				"iscsi: instance %q portal %q: bracketed IPv6 portals are not supported", instance, p)
		}
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

// mpathID derives the dm-uuid "by-id" suffix from a SCSI WWID, matching the
// scsi_id/multipath convention used to build /dev/disk/by-id/dm-uuid-mpath-<id>:
// a "naa." WWID becomes "3<hex>", "eui." becomes "2<hex>", "t10." becomes
// "1<string>" (kept as-is -- t10 vendor IDs are not hex). An unrecognized prefix
// is an error: guessing would risk polling a mapper link that will never appear.
func mpathID(wwid string) (string, error) {
	switch {
	case strings.HasPrefix(wwid, "naa."):
		return "3" + strings.TrimPrefix(wwid, "naa."), nil
	case strings.HasPrefix(wwid, "eui."):
		return "2" + strings.TrimPrefix(wwid, "eui."), nil
	case strings.HasPrefix(wwid, "t10."):
		return "1" + strings.TrimPrefix(wwid, "t10."), nil
	default:
		return "", fmt.Errorf("iscsi: unrecognized wwid %q (want a naa./eui./t10. prefix)", wwid)
	}
}

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

// isThinLV reports whether an LV exists and is a thin volume, read from its
// attributes (lv_attr starts with 'V') -- the actual truth, independent of
// config, so snapshot/clone of a source behaves correctly. A missing LV is
// (false, false, nil), so callers can answer with NotFound instead of a
// generic lvs failure.
func (b *Backend) isThinLV(ctx context.Context, vg, lv string) (thin, exists bool, err error) {
	out, err := b.run.Run(ctx, "lvs", "--noheadings", "-o", "lv_attr", vg+"/"+lv)
	if err != nil {
		if isNotFound(err) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("iscsi: lvs attr %s/%s: %w", vg, lv, err)
	}
	return strings.HasPrefix(strings.TrimSpace(out), "V"), true, nil
}

// snapName derives a bounded, deterministic snapshot LV name from a CSI name.
func snapName(csiName string) string {
	sum := sha256.Sum256([]byte(csiName))
	return "snap-" + hex.EncodeToString(sum[:8])
}

// srcTagPrefix prefixes the LV tag recording a snapshot's source LV name at
// create time. `lvs -o origin` goes EMPTY once the source is deleted (a thin
// snapshot outlives its origin), which silently dropped such snapshots from
// ListSnapshots -- the same provenance problem the NFS plugin solves with its
// .snapshots/<id>.src sidecar. The tag survives with the snapshot; origin
// stays the primary source (covers pre-tag snapshots). Tag charset is
// [a-zA-Z0-9_+.-], which covers the "bard-<hex>" names.
const srcTagPrefix = "bardsrc."

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
	if _, err := b.lvm(ctx, "lvcreate", "-s", "-n", lv, vg+"/"+src); err != nil && !isExists(err) {
		return fmt.Errorf("iscsi: thin clone %s/%s -> %s: %w", vg, src, lv, err)
	}
	if _, err := b.lvm(ctx, "lvchange", "-ay", "-Ky", vg+"/"+lv); err != nil {
		return fmt.Errorf("iscsi: activate clone %s/%s: %w", vg, lv, err)
	}
	return nil
}

// extendTo grows an LV to size bytes; a no-op when it is already that large.
func (b *Backend) extendTo(ctx context.Context, vg, lv string, size int64) error {
	if size <= 0 {
		return nil
	}
	if _, err := b.lvm(ctx, "lvextend", "-L", strconv.FormatInt(size, 10)+"b", vg+"/"+lv); err != nil && !isNotLarger(err) {
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
	for _, l := range lines {
		// targetcli re-joins and re-parses its argv through configshell, so
		// whitespace or quotes inside a credential would split the `set auth`
		// command at publish time; reject at load with a clear message instead.
		if strings.ContainsAny(l, " \t'\"") {
			return nil, bardplugin.Errorf(bardplugin.CodeInternal,
				"iscsi: chap credentials at %s must not contain whitespace or quotes", path)
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

// targetdCreds is the HTTP Basic Auth identity a targetd-managed instance's
// JSON-RPC client authenticates with (see the targetd JSON-RPC client, added
// in a follow-up task).
type targetdCreds struct {
	User, Password string
}

// targetdCredsFor loads the targetd JSON-RPC credentials for a
// management: targetd instance, or nil when the instance is not
// targetd-managed. The credential file is <targetdDir>/<instance> (a mounted
// Secret key), containing exactly 2 non-empty lines:
//
//	username
//	password
//
// Reuses chapFor's parsing/whitespace discipline line-for-line (trim blank
// lines, reject embedded whitespace/quotes): a targetd instance is just as
// unforgiving about credential hygiene as a CHAP one, and there is no reason
// to invent a second convention. A targetd instance with unreadable or
// malformed credentials is an error, not a silent fallback -- and the error
// names only the file PATH, never its contents.
func (b *Backend) targetdCredsFor(instance string) (*targetdCreds, error) {
	ic, ok := b.instances[instance]
	if !ok || !ic.isTargetd() {
		return nil, nil
	}
	path := filepath.Join(b.targetdDir, instance)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, bardplugin.Errorf(bardplugin.CodeInternal,
			"iscsi: instance %q is management=targetd but credentials at %s are unreadable: %v", instance, path, err)
	}
	var lines []string
	for _, l := range strings.Split(string(data), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	for _, l := range lines {
		// The credentials become an HTTP Basic Auth header; whitespace/quotes
		// would at best be silently mangled and at worst inject stray header
		// content -- reject at load instead, matching chapFor's argv-safety
		// rationale for the same class of mistake.
		if strings.ContainsAny(l, " \t'\"") {
			return nil, bardplugin.Errorf(bardplugin.CodeInternal,
				"iscsi: targetd credentials at %s must not contain whitespace or quotes", path)
		}
	}
	if len(lines) != 2 {
		return nil, bardplugin.Errorf(bardplugin.CodeInternal,
			"iscsi: targetd credentials at %s must be 2 lines (username, password), got %d", path, len(lines))
	}
	return &targetdCreds{User: lines[0], Password: lines[1]}, nil
}

// lvmUdevConfig makes lvm create and remove /dev nodes itself instead of
// deferring to udev. In a container there is no udev to serve an activation
// (the host udevd's completion handshake lives in the host IPC namespace), so
// activating an INACTIVE thin pool -- the first volume after a node reboot, or
// after the last thin LV was removed -- died with "open failed: No such file
// or directory" on the pool's tmeta node (found live in-cluster on the second
// round; the first round masked it because a host-side command had left the
// pool active). Self-managed nodes are also correct on a host WITH udev: the
// dm uevent flags tell udev's own rules to skip node management.
const lvmUdevConfig = "activation{udev_sync=0 udev_rules=0}"

// lvm runs a state-changing lvm command (lvcreate/lvchange/lvextend/lvremove)
// with lvmUdevConfig. Plain reads (lvs, vgs) don't touch device nodes and run
// directly.
func (b *Backend) lvm(ctx context.Context, cmd string, args ...string) (string, error) {
	return b.run.Run(ctx, cmd, append([]string{"--config", lvmUdevConfig}, args...)...)
}

// redactSecrets replaces every occurrence of the given secret values in err's
// text with "***". Command errors embed the full argv (diagnostic gold
// everywhere else), but the two CHAP call sites pass credentials ON that argv,
// and a plugin error becomes a CSI error -- which lands in sidecar logs,
// VolumeAttachment status, and kubelet pod events. Redacting at the wrap keeps
// the failure diagnosable without ever putting a password in the cluster.
func redactSecrets(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	for _, s := range secrets {
		if s != "" {
			msg = strings.ReplaceAll(msg, s, "***")
		}
	}
	return errors.New(msg)
}

// ---- control plane -------------------------------------------------------

func (b *Backend) CreateVolume(ctx context.Context, req *bardplugin.CreateVolumeRequest) (*bardplugin.CreateVolumeResponse, error) {
	ic, err := b.inst(req.Instance)
	if err != nil {
		return nil, err
	}
	isClone := req.SourceSnapshot != nil || req.SourceVolume != nil
	if ic.isTargetd() && isClone {
		// targetd's vol_copy is a synchronous full copy: unsafe to drive under
		// CSI provisioner retries (a retried CreateVolume could pile up
		// concurrent full copies, or double-bill the copy time on every retry).
		// Reject fail-fast rather than silently hang a PVC restore/clone.
		//
		// InvalidArgument, NOT Unsupported: CSI mandates INVALID_ARGUMENT when a
		// plugin cannot create a volume from the requested source ("Source
		// incompatible or not supported", CreateVolume Errors) -- an RPC-specific
		// MUST that overrides the general "disabled in the plugin's current mode
		// of operation -> UNIMPLEMENTED" rule CreateSnapshot rides on. It also
		// tells the CO the right recovery: use a different source or none, rather
		// than "this RPC does not exist". Bard's own conformance runner enforces
		// this (internal/conformance: an unsupported clone must be InvalidArgument).
		return nil, bardplugin.Errorf(bardplugin.CodeInvalidArg,
			"iscsi: creating a volume from a snapshot or another volume is not supported on targetd-managed instance %q "+
				"(targetd's vol_copy is a synchronous full copy, unsafe under provisioner retries); local-management instances support snapshots and clones", req.Instance)
	}
	if ic.isTargetd() {
		return b.createVolumeTargetd(ctx, req.Instance, ic, req)
	}
	vg, lv := ic.VG, lvName(req.Name)

	// 1. The backing LV (idempotent via size check, like the LVM plugin).
	size, found, err := b.lvSizeBytes(ctx, vg, lv)
	if err != nil {
		return nil, err
	}
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
			// Thin snapshots cannot cross VGs, so the source must live in this
			// instance's VG; a mismatched handle is a routing error, not a
			// missing LV.
			if src.Location != "" && src.Location != vg {
				return nil, bardplugin.Errorf(bardplugin.CodeInvalidArg,
					"iscsi: clone/restore source %s/%s is not in this instance's VG %s", src.Location, src.Name, vg)
			}
			thin, exists, err := b.isThinLV(ctx, vg, src.Name)
			if err != nil {
				return nil, err
			}
			if !exists {
				return nil, bardplugin.Errorf(bardplugin.CodeNotFound,
					"iscsi: clone/restore source %s/%s not found", vg, src.Name)
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
			if _, err := b.lvm(ctx, "lvcreate", "-T", vg+"/"+pool, "-V", lvBytes(req.CapacityBytes), "-n", lv); err != nil {
				return nil, fmt.Errorf("iscsi: lvcreate thin %s/%s: %w", vg, lv, err)
			}
		default:
			// Thick volume: -L fully allocates the size up front.
			if _, err := b.lvm(ctx, "lvcreate", "-n", lv, "-L", lvBytes(req.CapacityBytes), vg); err != nil {
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
	// Advertise thin-provisioning UNMAP so a node-side fstrim travels down to
	// the LV (and, on a thin LV, frees the pool) -- without it the initiator
	// sees "discard not supported" and NodeReclaimSpace is hollow. Best-effort:
	// a backing device with no discard support just leaves it off.
	_, _ = b.run.Run(ctx, "targetcli", backstore(lv), "set", "attribute", "emulate_tpu=1")
	if _, err := b.run.Run(ctx, "targetcli", "/iscsi", "create", iqn); err != nil && !isExists(err) {
		return nil, fmt.Errorf("iscsi: create target %s: %w", iqn, err)
	}
	// dm-multipath: a 2+-portal instance gets one explicit LIO network portal per
	// configured address, replacing targetcli's auto-created default.
	if portals := ic.portalList(); len(portals) >= 2 {
		if err := b.setExplicitPortals(ctx, tpgPath(iqn), portals); err != nil {
			return nil, err
		}
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

// setExplicitPortals replaces a tpg's auto-created default network portal with
// one explicit portal per configured address -- what makes dm-multipath login
// possible (a node needs a distinct path per portal to build multiple sessions
// to the same LUN). Substrate-verified 2026-07-19: modern targetcli auto-creates
// the default as dual-stack "::0:3260" (IPv6-any, which also holds port 3260 for
// v4), NOT "0.0.0.0:3260" as older targetcli/kernels did -- so both forms are
// attempted for delete, in that order, each tolerating not-found (observed live
// phrasing: "No such NetworkPortal in configfs: ..."). Each create tolerates
// already-exists so a retried CreateVolume converges.
func (b *Backend) setExplicitPortals(ctx context.Context, tpg string, portals []string) error {
	if _, err := b.run.Run(ctx, "targetcli", tpg+"/portals", "delete", "0.0.0.0", "3260"); err != nil && !isNotFound(err) {
		return fmt.Errorf("iscsi: delete default portal 0.0.0.0:3260 on %s: %w", tpg, err)
	}
	if _, err := b.run.Run(ctx, "targetcli", tpg+"/portals", "delete", "::0", "3260"); err != nil && !isNotFound(err) {
		return fmt.Errorf("iscsi: delete default portal ::0:3260 on %s: %w", tpg, err)
	}
	for _, p := range portals {
		ip, port, err := splitPortal(p)
		if err != nil {
			return err
		}
		if _, err := b.run.Run(ctx, "targetcli", tpg+"/portals", "create", ip, port); err != nil && !isExists(err) {
			return fmt.Errorf("iscsi: create portal %s on %s: %w", p, tpg, err)
		}
	}
	return nil
}

// splitPortal splits an "ip:port" portal string on the LAST colon. Bracketed
// IPv6 portals are rejected earlier at inst() validation, so any colon here is
// the ip:port separator, not part of an IPv6 address.
func splitPortal(portal string) (ip, port string, err error) {
	i := strings.LastIndex(portal, ":")
	if i < 0 {
		return "", "", bardplugin.Errorf(bardplugin.CodeInvalidArg, "iscsi: malformed portal %q (want ip:port)", portal)
	}
	return portal[:i], portal[i+1:], nil
}

// DeleteVolume tears the export down then removes the LV, in dependency order
// (target -> backstore -> LV) so nothing references a removed object. Every step
// is idempotent and a non-not-found error is surfaced (never reports success while
// the volume's data could still exist -- no silent orphan).
func (b *Backend) DeleteVolume(ctx context.Context, req *bardplugin.DeleteVolumeRequest) error {
	ic, ok := b.instances[req.Volume.Instance]
	if !ok {
		if isTdLocation(req.Volume.Location) {
			// The volume's own recorded Location marks it as targetd-managed
			// (see tdLocationPrefix) -- reaching it needs the instance's
			// endpoint+creds, which only live in CURRENT config, so there is
			// nothing to derive. CodeInternal is retriable (unlike NotFound,
			// which core/CSI treats as terminal success -- that would silently
			// orphan the remote volume/export) and not terminal (unlike
			// InvalidArgument, which would kill an
			// operator-restores-the-instance-then-retries recovery path).
			return bardplugin.Errorf(bardplugin.CodeInternal,
				"iscsi: instance %q not configured, cannot reach remote targetd volume %s -- restore the instance or clean up manually",
				req.Volume.Instance, req.Volume.Location)
		}
		// Unmarked (local, or a pre-targetd handle): ic stays the zero value,
		// so IQNBase falls back to defaultIQNBase below exactly as it always
		// has -- this is the PRE-EXISTING, deliberate derived-cleanup design
		// (the handle's own Location IS the VG, so lvremove genuinely deletes
		// it with no instance config at all; mirrors NodeUnstage's documented
		// derived-logout fallback). Falls through to the shared teardown below.
	} else if ic.isTargetd() {
		return b.deleteVolumeTargetd(ctx, req.Volume.Instance, ic, req.Volume.Name)
	}
	vg, lv := req.Volume.Location, req.Volume.Name
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
	if _, err := b.lvm(ctx, "lvremove", "-f", vg+"/"+lv); err != nil && !isNotFound(err) {
		return fmt.Errorf("iscsi: lvremove %s/%s: %w", vg, lv, err)
	}
	return nil
}

func (b *Backend) ExpandVolume(ctx context.Context, req *bardplugin.ExpandVolumeRequest) (*bardplugin.ExpandVolumeResponse, error) {
	if ic := b.instances[req.Volume.Instance]; ic.isTargetd() {
		return b.expandVolumeTargetd(ctx, req.Volume.Instance, ic, req.Volume.Name, req.NewSizeBytes)
	}
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
	// Unlike the rest of this method (historically instance-agnostic: it works
	// purely off src.Location/src.Name), this needs the INSTANCE to know
	// whether it's targetd-managed -- targetd's vol_copy is a synchronous full
	// copy, unsafe under provisioner retries.
	ic, err := b.inst(req.SourceVolume.Instance)
	if err != nil {
		return nil, err
	}
	if ic.isTargetd() {
		return nil, bardplugin.Errorf(bardplugin.CodeUnsupported,
			"iscsi: snapshots are not supported on targetd-managed instance %q "+
				"(targetd's vol_copy is a synchronous full copy, unsafe under provisioner retries); local-management instances support snapshots", req.SourceVolume.Instance)
	}
	src := req.SourceVolume // Location=vg, Name=lv
	thin, exists, err := b.isThinLV(ctx, src.Location, src.Name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, bardplugin.Errorf(bardplugin.CodeNotFound,
			"iscsi: snapshot source %s/%s not found", src.Location, src.Name)
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
	// --addtag records the source (see srcTagPrefix): provenance that survives
	// the source volume's deletion, unlike the origin attribute.
	if _, err := b.lvm(ctx, "lvcreate", "-s", "-pr", "--addtag", srcTagPrefix+src.Name, "-n", snap, vg+"/"+src.Name); err != nil && !isExists(err) {
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
	if _, err := b.lvm(ctx, "lvremove", "-f", vgsnap); err != nil && !isNotFound(err) {
		return fmt.Errorf("iscsi: lvremove snapshot %s: %w", vgsnap, err)
	}
	return nil
}

// ---- control-plane attach (ControllerPublisher) --------------------------

// ctxPortal/ctxIQN/ctxLUN are the PublishContext keys carried to NodeStage.
// ctxPortals is additive: only present for a 2+-portal instance (dm-multipath),
// so a single-portal PublishContext is byte-identical to before this field
// existed.
const (
	ctxPortal  = "portal"
	ctxIQN     = "targetIqn"
	ctxLUN     = "lun"
	ctxPortals = "portals"
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
	if ic.isTargetd() {
		return b.controllerPublishTargetd(ctx, req.Volume.Instance, ic, req)
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
			return nil, fmt.Errorf("iscsi: set chap auth on acl %s: %w", initIQN,
				redactSecrets(err, chap.Password, chap.MutualPassword))
		}
	}
	portals := ic.portalList()
	pc := map[string]string{
		ctxPortal: portals[0],
		ctxIQN:    iqn,
		ctxLUN:    "0",
	}
	if len(portals) >= 2 {
		pc[ctxPortals] = strings.Join(portals, ",")
	}
	return &bardplugin.ControllerPublishResponse{PublishContext: pc}, nil
}

// ControllerUnpublish removes the node's ACL, revoking its access. Idempotent: a
// missing ACL (already detached) succeeds. Like DeleteVolume it does NOT require
// the instance to still be configured: both IQNs derive from the volume name +
// node id, and reporting success while leaving the ACL in place would let the
// node keep reaching the LUN whenever the config entry is missing or broken.
func (b *Backend) ControllerUnpublish(ctx context.Context, req *bardplugin.ControllerUnpublishRequest) error {
	ic, ok := b.instances[req.Volume.Instance]
	if !ok {
		if isTdLocation(req.Volume.Location) {
			// Same reasoning as DeleteVolume's identical guard: a MARKED
			// Location means reaching this volume needs the instance's
			// endpoint+creds, which only live in current config.
			return bardplugin.Errorf(bardplugin.CodeInternal,
				"iscsi: instance %q not configured, cannot reach remote targetd volume %s -- restore the instance or clean up manually",
				req.Volume.Instance, req.Volume.Location)
		}
		// Unmarked: same derived-ACL-cleanup fallback as DeleteVolume's
		// identical case -- falls through to the shared teardown below with
		// ic at its zero value (IQNBase defaults to defaultIQNBase, as always).
	} else if ic.isTargetd() {
		return b.controllerUnpublishTargetd(ctx, req.Volume.Instance, ic, req)
	}
	base := ic.IQNBase
	if base == "" {
		base = defaultIQNBase
	}
	iqn := targetIQN(base, req.Volume.Name)
	initIQN := initiatorIQN(base, req.NodeID)
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
	// Portals/Devices/Mapper are additive: set only for a dm-multipath (2+
	// portal) stage. An old single-path record (or a fresh single-path stage)
	// loads with them empty, which keeps NodeUnstage/NodePublish on the
	// single-path branch.
	Portals []string `json:"portals,omitempty"`
	Devices []string `json:"devices,omitempty"`
	Mapper  string   `json:"mapper,omitempty"`
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

// withTargetLock serializes, per target IQN, NodeStage's login-or-rescan
// decision and NodeUnstage's refcount-scan-then-act -- both read "do any
// OTHER staged volumes share this target?" and then act on the answer, and
// without a lock two concurrent unstages of the last two volumes sharing a
// target can both observe "another record exists" before either clears its
// own, and both skip the final logout (a leaked session). The lock is a flock
// on a per-IQN file under stateDir, held for the whole read-then-act; when
// stateDir is empty (no persistent state configured -- shouldn't happen on
// the node plane in practice) fn runs unlocked, mirroring recordState/
// loadState's no-op-when-empty convention.
func (b *Backend) withTargetLock(iqn string, fn func() error) error {
	if b.stateDir == "" {
		return fn()
	}
	if err := os.MkdirAll(b.stateDir, 0o750); err != nil {
		return fmt.Errorf("iscsi: state dir: %w", err)
	}
	sum := sha1.Sum([]byte(iqn))
	lockPath := filepath.Join(b.stateDir, "lock-"+hex.EncodeToString(sum[:]))
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("iscsi: open target lock: %w", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("iscsi: lock target %s: %w", iqn, err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

// otherRecordsForIQN counts staged-state records -- other than the one at
// excludeStagingPath, if any -- that share iqn. This is the target-IQN
// refcount NodeStage/NodeUnstage use to decide, for a shared target (targetd
// management: every volume is a LUN under ONE fixed target IQN), whether a
// session is already up (skip discovery/login, rescan instead) and whether
// this is the last volume on that target (only then log out). For a
// per-volume target (local management: each volume's target IQN is unique to
// it) this always comes out to zero -- so local-mode behavior is unchanged BY
// CONSTRUCTION. A missing state dir counts as zero, not an error; a lock file
// (skipped by its "lock-" prefix) or a corrupt/mid-write state file (skipped
// on read/parse error) is tolerated exactly like loadState.
func (b *Backend) otherRecordsForIQN(iqn, excludeStagingPath string) (int, error) {
	if b.stateDir == "" {
		return 0, nil
	}
	entries, err := os.ReadDir(b.stateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("iscsi: scan state dir: %w", err)
	}
	var exclude string
	if excludeStagingPath != "" {
		exclude = b.statePath(excludeStagingPath)
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), "lock-") {
			continue
		}
		full := filepath.Join(b.stateDir, entry.Name())
		if exclude != "" && full == exclude {
			continue
		}
		data, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		var st stagedState
		if json.Unmarshal(data, &st) != nil {
			continue
		}
		if st.IQN == iqn {
			count++
		}
	}
	return count, nil
}

// sessionActiveForIQN reports whether `iscsiadm -m session` still lists a
// session for iqn. Used to verify the final logout on a shared target
// (targetd management) with no state record and so no known device/LUN to
// check a blockdev size against -- the check NodeUnstage's state-based/
// locally-derived branches use instead. iscsiadm errors (classified
// isNotFound, e.g. "No active sessions") when there are no sessions at all,
// which itself means "not active", not a failure to check.
func (b *Backend) sessionActiveForIQN(ctx context.Context, iqn string) (bool, error) {
	out, err := b.iscsiadm(ctx, "-m", "session")
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return strings.Contains(out, iqn), nil
}

// byPath is the stable device symlink the kernel creates for a logged-in LUN,
// under devRoot (defaults to /dev; tests point it at a fake tree).
func (b *Backend) byPath(portal, iqn, lun string) string {
	return filepath.Join(b.devRoot, "disk", "by-path", "ip-"+portal+"-iscsi-"+iqn+"-lun-"+lun)
}

// multipathPortals resolves the portal list for a NodeStage request: the
// PublishContext's explicit "portals" key (set by ControllerPublish for a
// 2+-portal instance) takes precedence over the instance config, mirroring how
// portal/targetIqn/lun already fall back to instance config when the context
// doesn't carry them. NodeUnstage has no PublishContext at all (CSI doesn't
// carry one on unstage), so its callers go straight to ic.portalList().
func multipathPortals(pc map[string]string, ic InstanceConfig) []string {
	if v := pc[ctxPortals]; v != "" {
		return strings.Split(v, ",")
	}
	return ic.portalList()
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

// waitForMapper resolves the assembled dm-multipath device from ONE already-
// logged-in path device: EvalSymlinks the by-path link to the kernel's sd node,
// read its WWID from sysfs, and poll the dm-uuid "by-id" link that
// multipathd/udev assemble once every path is up (observed live: ~4s after the
// 2nd login; same poll cadence as waitForDevice). This NEVER returns a
// /dev/mapper/<name> path -- host multipath.conf may set user_friendly_names,
// so only the dm-uuid link (stable, naming-independent) is safe to reference.
func (b *Backend) waitForMapper(ctx context.Context, pathDev string) (string, error) {
	sdPath, err := filepath.EvalSymlinks(pathDev)
	if err != nil {
		return "", fmt.Errorf("iscsi: resolve path device %s: %w", pathDev, err)
	}
	sd := filepath.Base(sdPath)
	wwidPath := filepath.Join(b.sysfsRoot, "class", "block", sd, "device", "wwid")
	wwidRaw, err := os.ReadFile(wwidPath)
	if err != nil {
		return "", fmt.Errorf("iscsi: read wwid %s: %w", wwidPath, err)
	}
	id, err := mpathID(strings.TrimSpace(string(wwidRaw)))
	if err != nil {
		return "", err
	}
	mapper := filepath.Join(b.devRoot, "disk", "by-id", "dm-uuid-mpath-"+id)
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Lstat(mapper); err == nil {
			return mapper, nil
		}
		if ctx.Err() != nil {
			return "", fmt.Errorf("iscsi: wait for mapper %s: %w", mapper, ctx.Err())
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("iscsi: mapper %s not ready after timeout", mapper)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func (b *Backend) NodeStage(ctx context.Context, req *bardplugin.NodeStageRequest) error {
	ic, err := b.inst(req.Volume.Instance)
	if err != nil {
		return err
	}
	// dm-multipath: PublishContext's explicit portals list takes precedence over
	// instance config (mirrors how portal/targetIqn/lun already fall back to
	// instance config below); 2+ portals hands the ENTIRE stage to the multipath
	// branch below, which never touches the single-portal code beneath it.
	if portals := multipathPortals(req.PublishContext, ic); len(portals) >= 2 {
		return b.nodeStageMultipath(ctx, req, ic, portals)
	}
	// Connection details come from ControllerPublish via PublishContext; fall back
	// to deriving them (so a manual/no-attach run still works).
	portal := req.PublishContext[ctxPortal]
	iqn := req.PublishContext[ctxIQN]
	lun := req.PublishContext[ctxLUN]
	if portal == "" {
		portal = ic.portalList()[0]
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
	// The login-or-rescan decision and the wait for OUR device are made under
	// the per-target flock, together with the refcount read that decides
	// between them -- see withTargetLock.
	dev := b.byPath(portal, iqn, lun)
	if err := b.withTargetLock(iqn, func() error {
		shared, err := b.otherRecordsForIQN(iqn, "")
		if err != nil {
			return err
		}
		if shared > 0 {
			// Another staged volume already has a live session for this target
			// (a shared-target instance, e.g. targetd) -- logging in again is
			// wrong (or at best redundant); rescan so the kernel picks up THIS
			// volume's own LUN instead, the same idiom NodeExpand already uses
			// for a resize.
			if _, err := b.iscsiadm(ctx, "-m", "session", "--rescan"); err != nil && !isNotFound(err) {
				return fmt.Errorf("iscsi: session rescan: %w", err)
			}
		} else {
			// Discover the target on the portal, then log in under our iface. Both
			// are idempotent on a stage retry. When the instance enforces CHAP,
			// the credentials go on the discovered node record before the login
			// (LIO's discovery itself stays unauthenticated; only the login is
			// gated).
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
		}
		return b.waitForDevice(ctx, dev)
	}); err != nil {
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

// nodeStageMultipath is NodeStage's dm-multipath branch (2+ portals): it logs in
// under EVERY portal (ensureIface once, then per portal: discovery, CHAP, login
// -- the SAME per-portal order the single-path code above already uses),
// resolves the kernel-assembled multipath device from one path's WWID, and
// formats/mounts/grows against the MAPPER, never a single leg -- so
// multipathd, not the filesystem, owns path failover.
func (b *Backend) nodeStageMultipath(ctx context.Context, req *bardplugin.NodeStageRequest, ic InstanceConfig, portals []string) error {
	iqn := req.PublishContext[ctxIQN]
	if iqn == "" {
		iqn = targetIQN(ic.IQNBase, req.Volume.Name)
	}
	lun := req.PublishContext[ctxLUN]
	if lun == "" {
		lun = "0"
	}
	initIQN := initiatorIQN(ic.IQNBase, b.nodeID)

	if err := b.ensureIface(ctx, initIQN); err != nil {
		return err
	}
	chap, err := b.chapFor(req.Volume.Instance)
	if err != nil {
		return err
	}

	devs := make([]string, 0, len(portals))
	for _, portal := range portals {
		if _, err := b.iscsiadm(ctx, "-m", "discovery", "-t", "sendtargets", "-p", portal, "-I", iscsiIface); err != nil && !isExists(err) {
			return fmt.Errorf("iscsi: discovery on %s: %w", portal, err)
		}
		if chap != nil {
			if err := b.setChapOnNode(ctx, iqn, portal, chap); err != nil {
				return err
			}
		}
		if _, err := b.iscsiadm(ctx, "-m", "node", "-T", iqn, "-p", portal, "-I", iscsiIface, "--login"); err != nil && !isAlreadyLoggedIn(err) {
			return fmt.Errorf("iscsi: login to %s: %w", iqn, err)
		}
		dev := b.byPath(portal, iqn, lun)
		if err := b.waitForDevice(ctx, dev); err != nil {
			return err
		}
		devs = append(devs, dev)
	}

	// Every path shares the same WWID (same LUN); resolve the assembled mapper
	// from whichever one -- the first, since all are now logged in.
	mapper, err := b.waitForMapper(ctx, devs[0])
	if err != nil {
		return err
	}
	if err := b.recordState(req.StagingPath, stagedState{
		Device: devs[0], IQN: iqn, Portal: portals[0],
		Portals: portals, Devices: devs, Mapper: mapper,
	}); err != nil {
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
	if err := b.ensureFormatted(ctx, mapper, fsType); err != nil {
		return err
	}
	if err := os.MkdirAll(req.StagingPath, 0o750); err != nil {
		return fmt.Errorf("iscsi: mkdir staging: %w", err)
	}
	if out, _ := b.run.Run(ctx, "findmnt", "-n", "-o", "SOURCE", "--mountpoint", req.StagingPath); strings.TrimSpace(out) == "" {
		mountArgs := []string{"-t", fsType}
		if len(req.MountFlags) > 0 {
			mountArgs = append(mountArgs, "-o", strings.Join(req.MountFlags, ","))
		}
		mountArgs = append(mountArgs, mapper, req.StagingPath)
		if _, err := b.run.Run(ctx, "mount", mountArgs...); err != nil {
			return fmt.Errorf("iscsi: mount %s -> %s: %w", mapper, req.StagingPath, err)
		}
	}
	return b.growFilesystem(ctx, fsType, mapper, req.StagingPath)
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
			return fmt.Errorf("iscsi: set %s on node record %s: %w", p[0], iqn,
				redactSecrets(err, chap.Password, chap.MutualPassword))
		}
	}
	return nil
}

// NodeUnstage unmounts, then hands off to unstageSingleTarget (or, for a
// 2+-portal instance, the multipath teardown) to log out and verify
// detachment -- returning an error if a session/device that must be gone
// isn't, so kubelet retries (never reports success while the LUN is still
// attached, mirroring the rbd-nbd unmap rule).
func (b *Backend) NodeUnstage(ctx context.Context, req *bardplugin.NodeUnstageRequest) error {
	if _, err := b.run.Run(ctx, "umount", req.StagingPath); err != nil && !isNotMounted(err) {
		return fmt.Errorf("iscsi: umount %s: %w", req.StagingPath, err)
	}
	st, ok := b.loadState(req.StagingPath)
	if !ok {
		// No session record. That does NOT mean nothing is staged: the record is
		// lost whenever the plugin container restarts with an unpersisted state
		// dir (found live in-cluster -- a mid-lifetime pod restart turned every
		// later unstage into a silent no-op, leaking the session past volume
		// deletion). Everything needed is derivable for a per-volume target
		// (local management): the target IQN from the volume name, the portal
		// from the instance, LUN is always 0 -- so reconstruct and log out
		// anyway; on a node that truly never staged the volume the logout is a
		// clean isNotFound no-op.
		ic := b.instances[req.Volume.Instance]
		base := ic.IQNBase
		if base == "" {
			base = defaultIQNBase
		}
		pl := ic.portalList()
		if len(pl) == 0 {
			return nil // instance unknown: nothing derivable, nothing to log out of
		}
		iqn := targetIQN(base, req.Volume.Name)
		if ic.isTargetd() {
			// A shared-target instance (targetd) exposes EVERY volume as a LUN
			// under ONE fixed target IQN -- the per-volume derived IQN above is
			// the wrong target here.
			iqn = ic.TargetIQN
		}
		if len(pl) >= 2 {
			return b.nodeUnstageDerivedMultipath(ctx, req, iqn, pl)
		}
		portal := pl[0]
		if ic.isTargetd() {
			// With no state record, this volume's LUN on the shared target isn't
			// known (targetd allocates it per-initiator at ControllerPublish
			// time) -- unlike local mode there is no LUN 0 to guess, so hand off
			// with an empty device: unstageSingleTarget skips the sysfs
			// device-delete step and verifies the final logout via the session
			// list instead of a device size.
			return b.unstageSingleTarget(ctx, req, iqn, portal, "")
		}
		st = stagedState{IQN: iqn, Portal: portal, Device: b.byPath(portal, iqn, "0")}
	}
	if len(st.Portals) >= 2 {
		return b.nodeUnstageMultipath(ctx, req, st)
	}
	return b.unstageSingleTarget(ctx, req, st.IQN, st.Portal, st.Device)
}

// unstageSingleTarget is the single-portal NodeUnstage teardown core, shared
// by the state-found branch and the locally-derived (non-targetd) fallback --
// both know a device (the recorded one, or LUN 0 for a derived per-volume
// target), so refcounting them here provably behaves as "always last" for a
// per-volume target (its IQN is unique, so no other record can ever share
// it), leaving pre-this-task local-mode behavior unchanged BY CONSTRUCTION --
// and by the targetd-derived fallback with no known device (dev == ""),
// which cannot sysfs-delete a specific LUN and instead verifies the final
// logout via the session list rather than a device size.
//
// The refcount read and the resulting not-last/last decision run under the
// per-target flock (withTargetLock), closing the TOCTOU where two concurrent
// unstages of the last two volumes sharing a target both see "another record
// exists" and both skip the final logout.
func (b *Backend) unstageSingleTarget(ctx context.Context, req *bardplugin.NodeUnstageRequest, iqn, portal, dev string) error {
	return b.withTargetLock(iqn, func() error {
		shared, err := b.otherRecordsForIQN(iqn, req.StagingPath)
		if err != nil {
			return err
		}
		if shared > 0 {
			// Not last: another staged volume still needs this target's shared
			// session -- do NOT log out. Detach only OUR LUN mapping (when it is
			// known) via a raw sysfs delete, which drops the one SCSI device
			// without touching the session other volumes still use.
			if dev != "" {
				sdPath, err := filepath.EvalSymlinks(dev)
				if err != nil {
					return fmt.Errorf("iscsi: resolve device %s: %w", dev, err)
				}
				deletePath := filepath.Join(b.sysfsRoot, "class", "block", filepath.Base(sdPath), "device", "delete")
				// Mode only matters if this path doesn't already exist (a real
				// sysfs attribute file always does, so the kernel's own
				// permissions win there); 0o600 just lets a test fixture read
				// back what was written.
				if err := os.WriteFile(deletePath, []byte("1"), 0o600); err != nil {
					return fmt.Errorf("iscsi: detach %s: %w", deletePath, err)
				}
			}
			b.clearState(req.StagingPath)
			return nil
		}
		// Last (or a per-volume target, which is always "last"): today's full
		// teardown -- logout, tolerate no-session, verify detachment, best-effort
		// drop the node record, then clear state.
		if _, err := b.iscsiadm(ctx, "-m", "node", "-T", iqn, "-p", portal, "--logout"); err != nil && !isNotFound(err) {
			return fmt.Errorf("iscsi: logout %s: %w", iqn, err)
		}
		if dev != "" {
			// Confirm the device is actually gone before declaring success.
			if out, _ := b.run.Run(ctx, "blockdev", "--getsize64", dev); strings.TrimSpace(out) != "" && strings.TrimSpace(out) != "0" {
				return fmt.Errorf("iscsi: device %s still present after logout", dev)
			}
		} else {
			// No known LUN/device to check a size for (targetd, record lost) --
			// verify via the session list instead.
			active, err := b.sessionActiveForIQN(ctx, iqn)
			if err != nil {
				return fmt.Errorf("iscsi: verify session for %s: %w", iqn, err)
			}
			if active {
				return fmt.Errorf("iscsi: session for %s still present after logout", iqn)
			}
		}
		// Best-effort cleanup of the node record, then drop our state.
		_, _ = b.iscsiadm(ctx, "-m", "node", "-T", iqn, "-p", portal, "--op", "delete")
		b.clearState(req.StagingPath)
		return nil
	})
}

// flushMultipath removes a dm-multipath map so its underlying paths can be
// logged out cleanly. Retries once (multipathd can be mid-reconfigure right
// after a path drops), then confirms the map is actually gone via `dmsetup
// info` (which errors once the device node no longer exists) rather than
// trusting `multipath -f`'s exit code alone. A no-op when there is no known
// mapper (nothing was ever assembled).
func (b *Backend) flushMultipath(ctx context.Context, mapper string) error {
	if mapper == "" {
		return nil
	}
	// multipath -f does NOT resolve the dm-uuid by-id symlink we track ("device
	// not found", live-verified); it needs the real dm node. dmsetup below DOES
	// accept the symlink, so only the flush argument is resolved.
	if resolved, rerr := filepath.EvalSymlinks(mapper); rerr == nil {
		mapper = resolved
	}
	_, err := b.run.Run(ctx, "multipath", "-f", mapper)
	if err != nil {
		_, err = b.run.Run(ctx, "multipath", "-f", mapper) // one retry
	}
	if _, derr := b.run.Run(ctx, "dmsetup", "info", "-c", "--noheadings", "-o", "name", mapper); derr == nil {
		// dmsetup still resolves the map: the flush did not actually take.
		if err != nil {
			return fmt.Errorf("iscsi: flush multipath map %s: %w", mapper, err)
		}
		return fmt.Errorf("iscsi: multipath map %s still present after flush", mapper)
	}
	return nil
}

// unstageMultipath is the shared multipath teardown core for both the
// state-based and derived NodeUnstage branches: flush the map (if known)
// BEFORE logging any path out -- logging out from under a live map leaves it
// stuck holding a dead path -- then log out every portal (tolerating
// no-session), verify every known path device is actually gone (never report
// success while a leg is still attached), then best-effort drop each portal's
// node record.
func (b *Backend) unstageMultipath(ctx context.Context, iqn, mapper string, portals, devices []string) error {
	// Pre-logout flush is BEST-EFFORT only: while the sessions (and so the sd
	// paths) are still alive, multipathd can re-assemble the map the moment
	// `multipath -f` removes it (find_multipaths + a known wwid). Live-found
	// in-cluster: the flush "succeeded" but the map was back before the check,
	// wedging unstage forever. The authoritative map-gone check moves to AFTER
	// the logouts destroy the paths (multipathd reaps a pathless map).
	_ = b.flushMultipath(ctx, mapper)
	for _, portal := range portals {
		if _, err := b.iscsiadm(ctx, "-m", "node", "-T", iqn, "-p", portal, "--logout"); err != nil && !isNotFound(err) {
			return fmt.Errorf("iscsi: logout %s (%s): %w", iqn, portal, err)
		}
	}
	for _, dev := range devices {
		if out, _ := b.run.Run(ctx, "blockdev", "--getsize64", dev); strings.TrimSpace(out) != "" && strings.TrimSpace(out) != "0" {
			return fmt.Errorf("iscsi: device %s still present after logout", dev)
		}
	}
	// Ground truth, post-logout: with every path gone the map must not survive.
	// One more flush covers a multipathd that keeps pathless maps (queueing).
	if err := b.flushMultipath(ctx, mapper); err != nil {
		return err
	}
	for _, portal := range portals {
		_, _ = b.iscsiadm(ctx, "-m", "node", "-T", iqn, "-p", portal, "--op", "delete")
	}
	return nil
}

// nodeUnstageMultipath is NodeUnstage's dm-multipath branch when a state record
// is present (st.Portals has 2+ entries): tear down using the RECORDED mapper
// and device list, then clear the state.
func (b *Backend) nodeUnstageMultipath(ctx context.Context, req *bardplugin.NodeUnstageRequest, st stagedState) error {
	if err := b.unstageMultipath(ctx, st.IQN, st.Mapper, st.Portals, st.Devices); err != nil {
		return err
	}
	b.clearState(req.StagingPath)
	return nil
}

// derivedMapper resolves a 2+-portal instance's assembled multipath device from
// whichever configured portal still has a live path device -- used by both
// NodeUnstage's and NodePublish's fallbacks when the state record is lost.
// Returns ("", nil) when no configured portal has a live device (nothing to
// resolve, nothing to flush); a live device whose wwid/mapper cannot be
// resolved is a real error, not a silent skip.
func (b *Backend) derivedMapper(ctx context.Context, iqn string, portals []string) (string, error) {
	for _, portal := range portals {
		dev := b.byPath(portal, iqn, "0")
		if out, _ := b.run.Run(ctx, "blockdev", "--getsize64", dev); strings.TrimSpace(out) != "" && strings.TrimSpace(out) != "0" {
			return b.waitForMapper(ctx, dev)
		}
	}
	return "", nil
}

// nodeUnstageDerivedMultipath is NodeUnstage's derived-identity fallback for a
// LOST state record on a 2+-portal instance: resolve whichever path device (if
// any) is still live to its mapper -- so a live map is flushed before any
// logout, exactly the state-based order, just without a recorded device list to
// rely on -- then run the same teardown core.
func (b *Backend) nodeUnstageDerivedMultipath(ctx context.Context, req *bardplugin.NodeUnstageRequest, iqn string, portals []string) error {
	devices := make([]string, len(portals))
	for i, portal := range portals {
		devices[i] = b.byPath(portal, iqn, "0")
	}
	mapper, err := b.derivedMapper(ctx, iqn, portals)
	if err != nil {
		return err
	}
	if err := b.unstageMultipath(ctx, iqn, mapper, portals, devices); err != nil {
		return err
	}
	b.clearState(req.StagingPath)
	return nil
}

func (b *Backend) NodePublish(ctx context.Context, req *bardplugin.NodePublishRequest) error {
	if req.Block {
		// Raw block: bind-mount the staged device node to the target path. The
		// stage record names the device; with the record lost (a restart with
		// an unpersisted state dir -- the same failure NodeUnstage derives its
		// way out of) the device is equally derivable, so publish must not
		// wedge on a missing record while the LUN is attached.
		st, ok := b.loadState(req.StagingPath)
		dev := st.Device
		if ok && st.Mapper != "" {
			// A recorded multipath stage must bind-mount the MAPPER, never one leg
			// -- that would defeat the entire point of multipath.
			dev = st.Mapper
		}
		if !ok || dev == "" {
			ic := b.instances[req.Volume.Instance]
			pl := ic.portalList()
			if len(pl) == 0 {
				return bardplugin.Errorf(bardplugin.CodeInvalidArg, "iscsi: no staged device for %s", req.StagingPath)
			}
			base := ic.IQNBase
			if base == "" {
				base = defaultIQNBase
			}
			iqn := targetIQN(base, req.Volume.Name)
			if len(pl) >= 2 {
				mapper, err := b.derivedMapper(ctx, iqn, pl)
				if err != nil {
					return err
				}
				if mapper == "" {
					return bardplugin.Errorf(bardplugin.CodeInternal,
						"iscsi: no live path device found to derive the multipath device for %s; refusing to bind-mount a single leg", req.StagingPath)
				}
				dev = mapper
			} else {
				dev = b.byPath(pl[0], iqn, "0")
			}
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
	if isMultipathDevice(dev) {
		if err := b.resizeMultipath(ctx, dev); err != nil {
			return nil, err
		}
	}
	fsType, _ := b.run.Run(ctx, "findmnt", "-n", "-o", "FSTYPE", "--target", req.VolumePath)
	if err := b.growFilesystem(ctx, strings.TrimSpace(fsType), dev, req.VolumePath); err != nil {
		return nil, err
	}
	return &bardplugin.NodeExpandResponse{}, nil
}

// isMultipathDevice reports whether a mount SOURCE is a dm-multipath device --
// either the raw /dev/dm-N node or a /dev/mapper/<name> path (host
// multipath.conf may set user_friendly_names, which resolve through
// /dev/mapper, not /dev/dm-N -- findmnt reports whichever path the mount table
// actually recorded). Mount sources come from findmnt output, not devRoot, so
// this checks the literal /dev prefixes regardless of devRoot.
func isMultipathDevice(src string) bool {
	return strings.HasPrefix(src, "/dev/dm-") || strings.HasPrefix(src, "/dev/mapper/")
}

// resizeMultipath makes multipathd pick up a grown map's new size: resolve the
// map NAME from the mount source (dmsetup accepts a device path directly), then
// `multipathd resize map <name>` -- growFilesystem then runs against the SAME
// mount source, so the fs grow sees the resized map.
func (b *Backend) resizeMultipath(ctx context.Context, dev string) error {
	name, err := b.run.Run(ctx, "dmsetup", "info", "-c", "--noheadings", "-o", "name", dev)
	if err != nil {
		return fmt.Errorf("iscsi: resolve multipath map name for %s: %w", dev, err)
	}
	name = strings.TrimSpace(name)
	if _, err := b.run.Run(ctx, "multipathd", "resize", "map", name); err != nil {
		return fmt.Errorf("iscsi: multipathd resize map %s: %w", name, err)
	}
	return nil
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
	if ic.isTargetd() {
		return b.getCapacityTargetd(ctx, req.Instance, ic)
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
		// A stack that cannot discard (no UNMAP end to end) has nothing to
		// reclaim: a clean no-op, not a permanently failing ReclaimSpaceJob.
		if strings.Contains(strings.ToLower(err.Error()), "not supported") {
			return &bardplugin.ReclaimSpaceResponse{PreUsageBytes: -1, PostUsageBytes: -1}, nil
		}
		return nil, fmt.Errorf("iscsi: fstrim %s: %w", path, err)
	}
	return &bardplugin.ReclaimSpaceResponse{PreUsageBytes: -1, PostUsageBytes: -1}, nil
}

// lvInfo is one row of `lvs` for listing. srcTag is the source LV recorded at
// snapshot create (srcTagPrefix), which outlives the origin attribute.
type lvInfo struct {
	name, attr, origin, srcTag string
	size                       int64
}

// listLVs returns the LVs in a VG (name, size, attr, origin, source tag) via a
// separator-delimited lvs so empty fields (no origin) parse deterministically.
func (b *Backend) listLVs(ctx context.Context, vg string) ([]lvInfo, error) {
	out, err := b.run.Run(ctx, "lvs", "--noheadings", "--units", "b", "--nosuffix",
		"--separator", "|", "-o", "lv_name,lv_size,lv_attr,origin,lv_tags", vg)
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
		row := lvInfo{name: strings.TrimSpace(f[0]), size: size, attr: strings.TrimSpace(f[2])}
		if len(f) >= 4 {
			row.origin = strings.TrimSpace(f[3])
		}
		if len(f) >= 5 { // lv_tags is comma-separated
			for _, t := range strings.Split(strings.TrimSpace(f[4]), ",") {
				if rest, ok := strings.CutPrefix(t, srcTagPrefix); ok {
					row.srcTag = rest
				}
			}
		}
		rows = append(rows, row)
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
		if ic.isTargetd() {
			tdEntries, err := b.listVolumesTargetd(ctx, instance, ic)
			if err != nil {
				return nil, err
			}
			entries = append(entries, tdEntries...)
			continue
		}
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
			if !strings.HasPrefix(lv.name, "snap-") {
				continue
			}
			// origin is authoritative while the source LV lives; the create-time
			// tag keeps the snapshot listed after the source is deleted (core
			// drops entries with no source). A pre-tag snapshot whose source is
			// gone has no provenance left and stays dropped.
			src := lv.origin
			if src == "" {
				src = lv.srcTag
			}
			if src == "" {
				continue
			}
			entries = append(entries, bardplugin.SnapshotListEntry{
				Snapshot:     bardplugin.VolumeRef{Instance: instance, Location: ic.VG, Name: lv.name},
				SourceVolume: bardplugin.VolumeRef{Instance: instance, Location: ic.VG, Name: src},
				SizeBytes:    lv.size,
				ReadyToUse:   true,
			})
		}
	}
	return &bardplugin.ListSnapshotsResponse{Entries: entries}, nil
}
