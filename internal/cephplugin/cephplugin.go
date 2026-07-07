// Package cephplugin is the Ceph RBD backend as an out-of-tree Bard plugin. It
// depends only on the public bardplugin SDK -- exactly as a third-party plugin
// would -- proving that Bard core needs no built-in knowledge of Ceph. The rbd
// logic is the same that used to live inside Bard core; only the request/response
// plumbing is the plugin contract instead of core's internal interface.
//
// Control-plane ops (create/delete/resize/snapshot) shell out to `rbd` against
// the cluster; node-plane maps the image to a block device, formats, and mounts.
package cephplugin

import (
	"context"
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
	"time"

	"github.com/kindacoolhamster/bard-csi/internal/cephenc"
	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// KMSConfig is the per-provider KMS config (re-exported from the shared cephenc
// package so the plugin's config loading and WithKMS keep a stable type).
type KMSConfig = cephenc.KMSConfig

const (
	mib           = 1 << 20
	defaultFsType = "ext4"
	paramPool     = "pool"
	// paramRadosNamespace isolates a volume's image inside a rados namespace within
	// the pool (multi-tenancy: many tenants share one pool, separated by namespace,
	// and a cephx user can be scoped `profile rbd pool=P namespace=N`). Overrides
	// the instance's radosNamespace. ceph-csi parity.
	paramRadosNamespace = "radosNamespace"
	// paramVolumeNamePrefix overrides the default "csi-vol-" prefix of the backend
	// rbd image name, so operators can recognise a team's/cluster's images in the
	// pool (e.g. "pvc-"). The full name is prefix+hash(pvc-name); the prefix is
	// recorded in the volume handle, so it threads the whole lifecycle. ceph-csi
	// parity (its StorageClass `volumeNamePrefix`).
	paramVolumeNamePrefix = "volumeNamePrefix"
	secretUserID          = "userID"
	secretUserKey         = "userKey"
	defaultUserID         = "admin"

	// StorageClass parameters passed through to `rbd create`. imageFeatures is a
	// comma-separated list (e.g. "layering,exclusive-lock,object-map,fast-diff");
	// stripeUnit/stripeCount configure striping (both required together, and need
	// the image's striping-v2 feature).
	paramImageFeatures = "imageFeatures"
	paramStripeUnit    = "stripeUnit"
	paramStripeCount   = "stripeCount"
	paramObjectSize    = "objectSize"
	// paramDataPool puts an image's DATA in a separate pool (`rbd create/clone/cp
	// --data-pool`) while its metadata stays in `pool` -- the way to back RBD with an
	// erasure-coded pool (the replicated `pool` holds the small metadata, the EC pool
	// the bulk data). The data pool is recorded in the image by rbd, so it needs no
	// handle threading. ceph-csi parity.
	paramDataPool = "dataPool"

	// mapOptions/unmapOptions are passed through to `rbd map`/`rbd unmap` (or the
	// rbd-nbd equivalents) at the node. A value may be mounter-scoped with
	// `krbd:`/`nbd:` segment prefixes separated by ';' (e.g.
	// "krbd:notrim;nbd:try-netlink"); an unprefixed segment applies to whichever
	// mounter is in use. They ride the volume context to the node (mapOptions) and
	// are persisted with the device record (unmapOptions, since NodeUnstage carries
	// no context). ceph-csi parity.
	paramMapOptions   = "mapOptions"
	paramUnmapOptions = "unmapOptions"

	// Mounters select how the node maps an image to a block device.
	//   krbd     - kernel rbd via `rbd map` (writes /sys/bus/rbd/add). Fast, but
	//              needs a writable rbd sysfs, unavailable in nested containers.
	//   rbd-nbd  - userspace map via `rbd-nbd map` over the NBD module (/dev/nbdN).
	mounterNBD  = "rbd-nbd"
	mounterKRBD = "krbd"

	// paramTryOtherMounters: when "true" on the StorageClass and the instance's
	// mounter is krbd, a failed krbd `rbd map` (e.g. the node's krbd driver lacks an
	// image feature) falls back to rbd-nbd, instead of failing the stage. ceph-csi
	// parity. Carried to the node in the volume context.
	paramTryOtherMounters = "tryOtherMounters"
	// paramMkfsOptions overrides the mkfs arguments used to format a fresh volume
	// (space-separated). When unset, the tuned per-filesystem defaults below are used.
	// ceph-csi parity. Carried to the node in the volume context.
	paramMkfsOptions = "mkfsOptions"
	// paramSnapshotNamePrefix overrides the default "csi-snap-" prefix of the backend
	// rbd snapshot name (a VolumeSnapshotClass parameter; also honoured on a
	// VolumeGroupSnapshotClass for group-snapshot members). The snapshot name is
	// recorded in the snapshot handle, so it threads delete/restore. ceph-csi parity.
	paramSnapshotNamePrefix = "snapshotNamePrefix"
	// paramCephLogDir / paramCephLogStrategy control the rbd-nbd client log file
	// (ceph-csi parity; rbd-nbd maps only -- krbd has no client log). cephLogDir
	// places a per-volume log file there at map time; cephLogStrategy is what
	// NodeUnstage does with that file after a clean unmap: "remove" (default),
	// "compress" (gzip in place), or "preserve". Both ride the volume context; the
	// log path + strategy are persisted with the device record (NodeUnstage carries
	// no volume context).
	paramCephLogDir      = "cephLogDir"
	paramCephLogStrategy = "cephLogStrategy"

	// tmpClonePrefix names the transient source snapshot a PVC-PVC clone rides on
	// (snap + COW clone + out-of-band flatten; see provision). Listings skip it.
	tmpClonePrefix = "clonetmp-"
)

// errImageNotFound is an internal sentinel for imageInfo.
var errImageNotFound = errors.New("ceph-rbd: image not found")

// supportedFsTypes are the filesystems the plugin can format (mkfs.<type>) and
// grow (see NodeExpand). ext4 is the default; ext2/ext3 ride e2fsprogs; xfs and
// btrfs need their own tools (xfsprogs / btrfs-progs) shipped in the plugin image.
// A raw block volume uses none of these. Kept as an allowlist so an unknown type
// fails fast with a clear error instead of a cryptic "mkfs.<x>: not found".
var supportedFsTypes = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true,
}

// mountFlagsForFs augments the StorageClass mount flags with filesystem-specific
// defaults. For xfs it adds `nouuid`: an rbd clone (snapshot restore / PVC clone)
// copies the source's xfs superblock, so the clone's filesystem has the SAME UUID
// as the source, and xfs refuses to mount a duplicate UUID without nouuid. (ceph-csi
// does the same.) Idempotent -- nouuid is not duplicated if already present.
func mountFlagsForFs(fsType string, flags []string) []string {
	if fsType != "xfs" {
		return flags
	}
	for _, f := range flags {
		if strings.TrimSpace(f) == "nouuid" {
			return flags
		}
	}
	return append(append([]string{}, flags...), "nouuid")
}

// mkfsArgsForFs returns the mkfs arguments (excluding the device) for formatting a
// fresh volume. An explicit mkfsOptions overrides the tuned defaults entirely (matching
// ceph-csi); otherwise per-filesystem defaults tuned for thin-provisioned rbd images are
// used: `-m0` drops the 5% root-reserved blocks (an rbd image is not a root fs) and
// `nodiscard` skips a pointless TRIM (the image returns zeros for unwritten areas),
// while `lazy_*_init=1` defers inode-table/journal zeroing -- safe here because we only
// mkfs a *fresh* image (ensureFormatted skips an already-formatted device, so a clone or
// restore is never reformatted). The fscrypt encrypt feature is appended last so it
// survives an mkfsOptions override (ext4-only; validated by the caller).
func mkfsArgsForFs(fsType, mkfsOptions string, fsCrypt bool) []string {
	var args []string
	if strings.TrimSpace(mkfsOptions) != "" {
		args = strings.Fields(mkfsOptions)
	} else {
		switch fsType {
		case "ext4", "ext3":
			args = []string{"-m0", "-Enodiscard,lazy_itable_init=1,lazy_journal_init=1"}
		case "ext2": // no journal
			args = []string{"-m0", "-Enodiscard,lazy_itable_init=1"}
		case "xfs":
			args = []string{"-K"} // do not discard blocks at mkfs time
			// btrfs: leave defaults; mkfsOptions still applies if set.
		}
	}
	if fsCrypt {
		args = append(args, "-O", "encrypt")
	}
	return args
}

// ClusterConfig is the connection config for one Ceph cluster instance. The
// cephx key is not stored here; it is read per-request from keyDir (or a CSI
// secret), so it never lives in the plugin's plain config.
type ClusterConfig struct {
	Monitors []string `json:"monitors"` // mon endpoints host:port
	Pool     string   `json:"pool"`     // default rbd pool
	UserID   string   `json:"userID"`   // default cephx user (e.g. "admin")
	Mounter  string   `json:"mounter"`  // "krbd" (default) or "rbd-nbd"
	// RadosNamespace optionally scopes this instance's images to a rados namespace
	// inside Pool (multi-tenancy). A StorageClass radosNamespace param overrides it.
	RadosNamespace string `json:"radosNamespace"`
	// ReadAffinity opts this cluster into RBD read-affinity: when the node's CRUSH
	// location is known (core threads it into NodeStage), krbd maps add
	// read_from_replica=localize so reads prefer a local OSD replica. krbd only.
	ReadAffinity bool `json:"readAffinity"`
	// ClusterName optionally names the deployment that owns this instance's
	// volumes; when set, every created image is stamped with it in image-meta
	// (key csi.ceph.com/cluster/name -- ceph-csi's, so existing tooling reads
	// Bard's images too). Pure operability: identifies which Kubernetes cluster
	// / Bard install minted an image when many share a pool.
	ClusterName string `json:"clusterName,omitempty"`
}

// Backend implements bardplugin.Backend for Ceph RBD.
type Backend struct {
	clusters  map[string]ClusterConfig // instance id -> cluster
	keyDir    string                   // dir of per-instance cephx key files
	encKeyDir string                   // dir of per-instance LUKS master keys ("" disables encryption)
	kms       *cephenc.Registry        // KMS providers (encryptionKMSID -> provider), bound to this backend as Host
	run       Runner

	// stateDir records the block device mapped for each staging path, so
	// NodeUnstage can unmap it even when the staging mount is already gone (CSI
	// requires NodeUnstage to be idempotent, so a retried call after the unmount
	// already happened must still unmap -- otherwise the device leaks and its
	// rbd watcher blocks DeleteVolume). Must be on node-persistent storage so it
	// survives a plugin restart. "" disables it (tests fall back to findmnt).
	stateDir string

	// cloneDepthLimit bounds the rbd COW clone chain: when a freshly cloned image's
	// parent-chain depth reaches this, the clone is flattened (its parent's data is
	// copied in and the link severed) so iterative snapshot->restore can't grow the
	// chain past rbd's hard limit (~14) or degrade reads. 0 disables flattening.
	cloneDepthLimit int
	// flattenAsync runs the post-clone flatten in a background goroutine (true in
	// production so it never blocks/expires the CSI call); tests clear it to flatten
	// synchronously for determinism.
	flattenAsync bool

	mu        sync.Mutex
	snapIndex map[string]string // CSI snapshot name -> source volume key
	// snapNames reverses snapIndex by snapshot handle (refKey of the snapshot's
	// VolumeRef), so DeleteSnapshot -- which sees only the handle, not the CSI
	// name -- can clear the index entry. Without it a deleted snapshot's CSI name
	// stays claimed and a later reuse against a different source is falsely
	// rejected AlreadyExists (until a plugin restart).
	snapNames map[string]string // snapshot handle key -> CSI snapshot name

	// flattenInFlight dedups the out-of-band flattens (one per image spec):
	// csi-addons retries EnableVolumeReplication on a backoff, and each retry of a
	// still-flattening clone must not stack another `rbd flatten`.
	flattenMu       sync.Mutex
	flattenInFlight map[string]bool
}

// defaultCloneDepthLimit matches ceph-csi's soft clone-depth limit (rbd flattens
// the chain before it nears the kernel/rbd hard cap).
const defaultCloneDepthLimit = 4

// New builds the Ceph RBD plugin backend over per-instance cluster config.
// keyDir holds per-instance cephx key files; stateDir records staging->device
// mappings for reliable unmap. "" disables either (tests).
func New(clusters map[string]ClusterConfig, keyDir, stateDir string, run Runner) *Backend {
	if run == nil {
		run = ExecRunner{}
	}
	b := &Backend{clusters: clusters, keyDir: keyDir, stateDir: stateDir, run: run, snapIndex: map[string]string{}, snapNames: map[string]string{}, cloneDepthLimit: defaultCloneDepthLimit, flattenAsync: true}
	// The KMS registry resolves a volume's passphrase through pluggable providers; it
	// reads the master key dir, a Ceph connection, and image metadata back from this
	// backend (which implements cephenc.Host). Lazily uses b.encKeyDir, so WithEncryption
	// may be chained after New.
	b.kms = cephenc.NewRegistry(b, nil)
	return b
}

// WithCloneDepthLimit sets the rbd clone-chain depth at which a freshly cloned image
// is flattened (0 disables flattening). Returns the backend for chaining.
func (b *Backend) WithCloneDepthLimit(n int) *Backend {
	b.cloneDepthLimit = n
	return b
}

// cephenc.Host: the KMS providers reach the master key dir, a reusable Ceph
// connection, and the per-volume metadata store (rbd image-meta) through these.
func (b *Backend) MasterKeyDir() string { return b.encKeyDir }

// ConnFor reuses the caller's Ceph connection (conn non-nil) rather than open a second
// one -- with a second temp keyfile -- per operation. A nil conn opens a fresh
// connection whose returned cleanup closes it; a borrowed conn gets a no-op cleanup.
func (b *Backend) ConnFor(conn []string, instance string, secrets map[string]string) ([]string, func(), error) {
	if conn != nil {
		return conn, func() {}, nil
	}
	cc, err := b.cluster(instance)
	if err != nil {
		return nil, nil, err
	}
	return b.connArgs(cc, instance, secrets)
}

func (b *Backend) MetaGet(ctx context.Context, conn []string, spec, key string) string {
	return b.imageMetaGet(ctx, conn, spec, key)
}

func (b *Backend) MetaSet(ctx context.Context, conn []string, spec, key, value string) error {
	return b.imageMetaSet(ctx, conn, spec, key, value)
}

func (b *Backend) Info() bardplugin.Info {
	return bardplugin.Info{
		Type: "ceph-rbd",
		Capabilities: bardplugin.Capabilities{
			BlockDevice: true, // rbd exposes a block device the node formats
			Snapshots:   true,
			Expand:      true,
		},
	}
}

// shortName derives a bounded, deterministic backend object name from a CSI name
// (the volume_id has a 128-byte cap), keeping retries idempotent.
func shortName(prefix, csiName string) string {
	sum := sha256.Sum256([]byte(csiName))
	return prefix + hex.EncodeToString(sum[:8])
}

// namePrefix resolves an object-name prefix parameter (volumeNamePrefix,
// snapshotNamePrefix, volumeGroupNamePrefix), defaulting when unset. The prefix
// becomes part of an rbd object name (and thus a volume/snapshot handle), so it
// must be a valid name fragment: no '/' (the pool/namespace/image separator),
// no '@' (the image@snap separator handles encode), no whitespace.
func namePrefix(param, value, def string) (string, error) {
	if value == "" {
		return def, nil
	}
	if strings.ContainsAny(value, "/@ \t\n") {
		return "", bardplugin.Errorf(bardplugin.CodeInvalidArg, "ceph-rbd: invalid %s %q (no '/', '@', or whitespace)", param, value)
	}
	return value, nil
}

// volumeImagePrefix resolves the rbd image-name prefix from the StorageClass
// `volumeNamePrefix` param, defaulting to "csi-vol-".
func volumeImagePrefix(p string) (string, error) {
	return namePrefix(paramVolumeNamePrefix, p, volNamePrefix)
}

// isBardImageName reports whether an rbd image name was minted by shortName (any
// prefix + a 16-hex-char hash). Used by ListVolumes so that custom-volumeNamePrefix
// images are still recognised as Bard-managed, not just the default "csi-vol-".
func isBardImageName(name string) bool {
	if len(name) <= shortNameHashLen {
		return false
	}
	for _, c := range name[len(name)-shortNameHashLen:] {
		if c < '0' || (c > '9' && c < 'a') || c > 'f' {
			return false
		}
	}
	return true
}

func refKey(r bardplugin.VolumeRef) string {
	return r.Instance + "|" + r.Location + "|" + r.Name
}

func (b *Backend) cluster(instance string) (ClusterConfig, error) {
	cc, ok := b.clusters[instance]
	if !ok {
		return ClusterConfig{}, bardplugin.Errorf(bardplugin.CodeInvalidArg, "ceph-rbd: no cluster configured for instance %q", instance)
	}
	return cc, nil
}

// keyFor resolves the cephx key for an instance: the per-instance key file wins
// (this is what lets one StorageClass address many clusters), then a CSI secret.
func (b *Backend) keyFor(instance string, secrets map[string]string) (string, error) {
	if b.keyDir != "" {
		data, err := os.ReadFile(filepath.Join(b.keyDir, instance))
		switch {
		case err == nil:
			return strings.TrimSpace(string(data)), nil
		case !os.IsNotExist(err):
			return "", fmt.Errorf("ceph-rbd: read key for instance %q: %w", instance, err)
		}
	}
	if k := secrets[secretUserKey]; k != "" {
		return k, nil
	}
	if b.keyDir != "" {
		return "", fmt.Errorf("ceph-rbd: no cephx key for instance %q (no %s and no CSI secret)", instance, filepath.Join(b.keyDir, instance))
	}
	return "", nil
}

// connArgs builds the common rbd CLI connection flags and writes the cephx key
// to a temp keyfile; cleanup removes it.
func (b *Backend) connArgs(cc ClusterConfig, instance string, secrets map[string]string) (args []string, cleanup func(), err error) {
	cleanup = func() {}
	user := secrets[secretUserID]
	if user == "" {
		user = cc.UserID
	}
	if user == "" {
		user = defaultUserID
	}
	// -c /dev/null: read an (empty) config instead of /etc/ceph/ceph.conf, which the
	// plugin image does not ship. Without this, rbd/ceph print "can't open ceph.conf:
	// (2) No such file or directory" to stderr -- benign, but its "No such file"
	// substring otherwise poisons the not-found error classifier (see errString).
	args = []string{"-c", "/dev/null", "-m", strings.Join(cc.Monitors, ","), "--id", user}

	key, err := b.keyFor(instance, secrets)
	if err != nil {
		return nil, cleanup, err
	}
	if key != "" {
		f, ferr := cephenc.SecretTemp("csi-ceph-key-")
		if ferr != nil {
			return nil, cleanup, fmt.Errorf("ceph-rbd: keyfile: %w", ferr)
		}
		if _, werr := f.WriteString(key); werr != nil {
			f.Close()
			os.Remove(f.Name())
			return nil, cleanup, fmt.Errorf("ceph-rbd: keyfile: %w", werr)
		}
		f.Close()
		args = append(args, "--keyfile", f.Name())
		cleanup = func() { os.Remove(f.Name()) }
	}
	return args, cleanup, nil
}

func bytesToMiB(n int64) int64 {
	if n <= 0 {
		return 1
	}
	return (n + mib - 1) / mib
}

// imgMetaKMSID is the rbd image-meta key recording which KMS provider holds an
// encrypted volume's passphrase. DeleteVolume reads it (the CSI DeleteVolume RPC
// carries no volume context) to clean up the key when the image is removed.
const imgMetaKMSID = cephenc.MetaKMSID

// imgMetaKeyID is the rbd image-meta key recording an encrypted image's key
// identity -- the value the derived KMS provider keys off. A freshly created
// volume's identity is its own pool/image, so it is not recorded (the node
// defaults to that). A clone copies its source's LUKS header, so it inherits the
// source's key id here at create time and advertises it to the node, making the
// derived provider re-derive the source's passphrase. Copied down a clone chain.
const imgMetaKeyID = cephenc.MetaKeyID

// imgMetaWrappedDEK holds a secrets-metadata/aws-kms volume's wrapped data key; it is
// copied onto an encrypted clone (inheritEncryption) so the clone unwraps the same key
// from its own image metadata. The KMS layer owns the value; aliased for the copy.
const imgMetaWrappedDEK = cephenc.MetaWrappedDEK

// imgMetaStatic marks an image as statically provisioned (pre-existing, brought
// under management by an admin rather than created by CSI). DeleteVolume reads it
// -- the only way to know at delete time, since the RPC carries no volume context
// -- and no-ops when it is "true", so a pre-existing image is never reaped. Set it
// with `rbd image-meta set <pool>/<image> bard.static true`.
const imgMetaStatic = "bard.static"

// imgMetaClusterName records which deployment created an image (the instance's
// clusterName config). The key is ceph-csi's, so tooling that inventories
// ceph-csi images by cluster reads Bard's the same way. ceph-csi parity.
const imgMetaClusterName = "csi.ceph.com/cluster/name"

// imgMetaSource records the content source a clone/restore was created from
// (sourceDesc form), stamped at provision time. The CO retries CreateVolume by
// name, and CSI requires an existing volume to be an AlreadyExists ERROR -- not
// an idempotent hit -- when the retry names a different content source; without
// this stamp the retry cannot tell. Plain creates stamp nothing.
const imgMetaSource = "bard.source"

// sourceDesc canonically describes a create's content source for the
// bard.source stamp ("" for a plain create).
func sourceDesc(req *bardplugin.CreateVolumeRequest) string {
	switch {
	case req.SourceSnapshot != nil:
		return "snap:" + req.SourceSnapshot.Location + "/" + req.SourceSnapshot.Name
	case req.SourceVolume != nil:
		return "vol:" + req.SourceVolume.Location + "/" + req.SourceVolume.Name
	}
	return ""
}

// ownerMeta maps the provisioner's --extra-create-metadata parameter keys to the
// metadata keys that record the owning PVC, for operability. Keys are lowercase
// to match CephFS, which lowercases subvolume-metadata keys (so a documented
// `metadata get bard.pvcname` works the same across both Ceph backends).
var ownerMeta = map[string]string{
	"csi.storage.k8s.io/pvc/name":      "bard.pvcname",
	"csi.storage.k8s.io/pvc/namespace": "bard.pvcnamespace",
	"csi.storage.k8s.io/pv/name":       "bard.pvname",
}

// setOwnerMetadata records the owning PVC/PV on the image (best-effort: labels are
// operability, not correctness, so a failure here must not fail the provision).
func (b *Backend) setOwnerMetadata(ctx context.Context, conn []string, spec string, params map[string]string) {
	for src, key := range ownerMeta {
		if v := params[src]; v != "" {
			_ = b.imageMetaSet(ctx, conn, spec, key, v)
		}
	}
}

// imageMetaSet records a key/value on an rbd image.
func (b *Backend) imageMetaSet(ctx context.Context, conn []string, spec, key, value string) error {
	if _, err := b.run.Run(ctx, "rbd", appendArgs(conn, "image-meta", "set", spec, key, value)...); err != nil {
		return fmt.Errorf("ceph-rbd: image-meta set %s %s: %w", spec, key, err)
	}
	return nil
}

// imageMetaGet reads an image-meta value, returning "" when the key is absent.
func (b *Backend) imageMetaGet(ctx context.Context, conn []string, spec, key string) string {
	v, _ := b.imageMetaGetChecked(ctx, conn, spec, key)
	return v
}

// imageMetaGetChecked reads an image-meta value, distinguishing "absent" (the key
// or the image does not exist -> "", nil) from a read that FAILED (mon down,
// auth). Decisions that must not proceed on a misread -- DeleteVolume's static
// guard and KMS-id lookup -- use this form and surface the error for a retry.
func (b *Backend) imageMetaGetChecked(ctx context.Context, conn []string, spec, key string) (string, error) {
	out, err := b.run.Run(ctx, "rbd", appendArgs(conn, "image-meta", "get", spec, key)...)
	if err != nil {
		if isNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("ceph-rbd: image-meta get %s %s: %w", spec, key, err)
	}
	return strings.TrimSpace(out), nil
}

// imageInfo queries an rbd image's size, returning errImageNotFound if absent.
func (b *Backend) imageInfo(ctx context.Context, conn []string, spec string) (int64, error) {
	out, err := b.run.Run(ctx, "rbd", appendArgs(conn, "info", spec, "--format", "json")...)
	if err != nil {
		if isNotFound(err) {
			return 0, errImageNotFound
		}
		return 0, fmt.Errorf("ceph-rbd: info %s: %w", spec, err)
	}
	var raw struct {
		Size int64 `json:"size"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return 0, fmt.Errorf("ceph-rbd: parse info %s: %w", spec, err)
	}
	return raw.Size / mib, nil
}

// imageParent returns the spec (location/image) of an rbd image's clone parent, or
// "" if it has none. Parses the `parent` object of `rbd info --format json`.
// imageParent returns spec's COW parent ("" if none) and whether that parent is
// in the rbd trash. A trashed parent (clone-v2 lets an admin `rbd trash mv` a
// parent while children exist -- rook does this) is still a real chain layer, but
// it cannot be opened by name, so the chain above it is unwalkable; the child's
// own info reports it via the parent `trash` flag (verified live on Ceph 20.2).
func (b *Backend) imageParent(ctx context.Context, conn []string, spec string) (parent string, trashed bool, err error) {
	out, err := b.run.Run(ctx, "rbd", appendArgs(conn, "info", spec, "--format", "json")...)
	if err != nil {
		return "", false, fmt.Errorf("ceph-rbd: info %s: %w", spec, err)
	}
	var raw struct {
		Parent *struct {
			Pool          string `json:"pool"`
			PoolNamespace string `json:"pool_namespace"`
			Image         string `json:"image"`
			Trash         bool   `json:"trash"`
		} `json:"parent"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return "", false, fmt.Errorf("ceph-rbd: parse info %s: %w", spec, err)
	}
	if raw.Parent == nil || raw.Parent.Image == "" {
		return "", false, nil
	}
	return locator(raw.Parent.Pool, raw.Parent.PoolNamespace) + "/" + raw.Parent.Image, raw.Parent.Trash, nil
}

// cloneDepth counts the parent images above an rbd image (0 = it is not a clone).
// exact=false means the chain has a trashed ancestor: that ancestor is counted (it
// is a real COW layer) but cannot be opened by name, so the depth above it is
// unknowable -- ceph-csi's open #4013, where the same blindness left chains
// unbounded and (via rook#12312) clones unmountable.
func (b *Backend) cloneDepth(ctx context.Context, conn []string, spec string) (depth int, exact bool, err error) {
	cur := spec
	for depth <= 64 { // safety bound; rbd's own hard limit is ~14
		parent, trashed, err := b.imageParent(ctx, conn, cur)
		if err != nil {
			return depth, false, err
		}
		if parent == "" {
			return depth, true, nil
		}
		depth++
		if trashed {
			return depth, false, nil
		}
		cur = parent
	}
	return depth, true, nil
}

// flattenIfDeep flattens a freshly cloned image once its clone-chain depth reaches
// the configured limit, so iterative snapshot->restore can't grow the COW parent
// chain past rbd's hard limit (~14) or degrade read performance. `rbd flatten` copies
// the parent's data into the image and severs the parent link (depth -> 0), making the
// image self-contained. ceph-csi does the same (its soft/hard limits); Bard flattens
// synchronously at the limit.
func (b *Backend) flattenIfDeep(ctx context.Context, conn []string, spec string) error {
	if b.cloneDepthLimit <= 0 {
		return nil
	}
	depth, exact, derr := b.cloneDepth(ctx, conn, spec)
	// Flatten conservatively when the true depth is unknowable -- a trashed
	// ancestor (exact=false) or a walk error (e.g. a foreign ancestor rbd cannot
	// resolve). The property that matters is a BOUNDED chain: assuming "deep" and
	// flattening is always safe, while assuming "shallow" is how chains creep to
	// rbd's hard cap. Flattening also releases a trashed ancestor for reclaim
	// (trash auto-purges once no children reference it).
	if derr == nil && exact && depth < b.cloneDepthLimit {
		return nil
	}
	if derr == nil && depth == 0 {
		return nil // not a clone; nothing to flatten
	}
	if _, err := b.run.Run(ctx, "rbd", appendArgs(conn, "flatten", spec)...); err != nil {
		if derr != nil {
			return fmt.Errorf("ceph-rbd: flatten %s (depth walk failed: %v): %w", spec, derr, err)
		}
		return fmt.Errorf("ceph-rbd: flatten %s (clone depth %d, exact=%v): %w", spec, depth, exact, err)
	}
	return nil
}

// scheduleFlatten runs flattenIfDeep for a freshly cloned image OUT of band: the
// depth walk shells out once per ancestor and `rbd flatten` copies the parent's data
// (which can be gigabytes), so doing it inline would risk the CSI gRPC deadline (a
// slow cluster killed the chained `rbd info` walk mid-flight). It is best-effort --
// the clone is already usable; flattening only keeps the COW chain short -- so a
// failure is logged, not fatal. It opens its OWN Ceph connection (the caller's is torn
// down when CreateVolume returns) and its own long timeout. In tests flattenAsync is
// cleared to run synchronously for determinism.
func (b *Backend) scheduleFlatten(instance string, secrets map[string]string, spec string) {
	if b.cloneDepthLimit <= 0 {
		return
	}
	b.backgroundFlatten(instance, secrets, spec, true)
}

// flattenForMirror severs spec's parent link out of band, UNCONDITIONALLY (no
// depth gate): snapshot-based mirroring refuses an image whose parent is not
// mirrored, so EnableVolumeReplication on a snapshot-restored clone flattens it
// first. Runs even when the depth limit is 0 (flattening-for-depth disabled must
// not break mirroring).
func (b *Backend) flattenForMirror(instance string, secrets map[string]string, spec string) {
	b.backgroundFlatten(instance, secrets, spec, false)
}

// backgroundFlatten is the shared out-of-band flatten worker. Dedup'd per image
// spec: the csi-addons controller retries EnableVolumeReplication on a backoff,
// and stacking a second `rbd flatten` on an image already flattening would only
// add load. depthGated selects flattenIfDeep (post-clone bounding) vs a plain
// flatten (mirroring). In tests flattenAsync is cleared to run synchronously.
func (b *Backend) backgroundFlatten(instance string, secrets map[string]string, spec string, depthGated bool) {
	b.flattenMu.Lock()
	if b.flattenInFlight == nil {
		b.flattenInFlight = map[string]bool{}
	}
	if b.flattenInFlight[spec] {
		b.flattenMu.Unlock()
		return
	}
	b.flattenInFlight[spec] = true
	b.flattenMu.Unlock()
	run := func() {
		defer func() {
			b.flattenMu.Lock()
			delete(b.flattenInFlight, spec)
			b.flattenMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		cc, err := b.cluster(instance)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ceph-rbd: background flatten %s: %v\n", spec, err)
			return
		}
		conn, cleanup, err := b.connArgs(cc, instance, secrets)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ceph-rbd: background flatten %s: %v\n", spec, err)
			return
		}
		defer cleanup()
		if depthGated {
			err = b.flattenIfDeep(ctx, conn, spec)
		} else {
			// Unconditional flatten (PVC-PVC clone / mirroring): a no-op when the
			// parent link is already severed, so retries and resumed creates don't
			// error on an image with no parent.
			parent, _, perr := b.imageParent(ctx, conn, spec)
			if perr == nil && parent == "" {
				return
			}
			if _, ferr := b.run.Run(ctx, "rbd", appendArgs(conn, "flatten", spec)...); ferr != nil {
				err = fmt.Errorf("ceph-rbd: flatten %s: %w", spec, ferr)
			}
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "ceph-rbd: background flatten %s: %v\n", spec, err)
		}
	}
	if b.flattenAsync {
		go run()
	} else {
		run()
	}
}

// ---- control plane -------------------------------------------------------

func (b *Backend) CreateVolume(ctx context.Context, req *bardplugin.CreateVolumeRequest) (*bardplugin.CreateVolumeResponse, error) {
	cc, err := b.cluster(req.Instance)
	if err != nil {
		return nil, err
	}
	// A volume may be created with a VolumeAttributesClass already set; reject an
	// unsupported mutable parameter up front, as CSI requires.
	if err := validateMutableParams(req.MutableParams); err != nil {
		return nil, err
	}
	// Validate encryption tuning BEFORE provisioning the image: an InvalidArgument that
	// returns no volume id is never retried or deleted by the provisioner, so failing
	// after `rbd create` would leak an orphan image. (Caught by the live negative test.)
	if err := validateLuksTuning(req.Parameters); err != nil {
		return nil, err
	}
	if err := validateCephLogStrategy(req.Parameters); err != nil {
		return nil, err
	}
	pool := req.Parameters[paramPool]
	if pool == "" {
		pool = cc.Pool
	}
	if pool == "" {
		return nil, fmt.Errorf("ceph-rbd: no pool given in parameters or cluster config")
	}
	// Optional rados namespace (multi-tenancy): from the StorageClass or the
	// instance config. The locator (pool or pool/namespace) becomes the volume's
	// backend Location, so every later op that sees only the volume handle
	// (DeleteVolume, NodeStage, snapshot) addresses the image in its namespace.
	namespace := req.Parameters[paramRadosNamespace]
	if namespace == "" {
		namespace = cc.RadosNamespace
	}
	location := locator(pool, namespace)
	prefix, err := volumeImagePrefix(req.Parameters[paramVolumeNamePrefix])
	if err != nil {
		return nil, err
	}
	image := shortName(prefix, req.Name)
	sizeMiB := bytesToMiB(req.CapacityBytes)

	conn, cleanup, err := b.connArgs(cc, req.Instance, req.Secrets)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	if namespace != "" {
		if err := b.ensureNamespace(ctx, conn, pool, namespace); err != nil {
			return nil, err
		}
	}

	spec := location + "/" + image
	isClone := req.SourceSnapshot != nil || req.SourceVolume != nil
	created := false
	finalMiB := sizeMiB
	existingMiB, infoErr := b.imageInfo(ctx, conn, spec)
	switch {
	case infoErr == nil:
		// An existing image is an idempotent hit only if it was created from the
		// SAME content source (stamped at provision); a retry naming a different
		// snapshot/volume -- or none -- is an incompatible AlreadyExists per CSI.
		// An unstamped image (pre-upgrade, or a create that died before the
		// stamp) is accepted as before.
		if recorded := b.imageMetaGet(ctx, conn, spec, imgMetaSource); recorded != "" && recorded != sourceDesc(req) {
			return nil, bardplugin.Errorf(bardplugin.CodeAlreadyExists,
				"volume %q exists with a different content source (%s)", req.Name, recorded)
		}
		if existingMiB != sizeMiB {
			// A clone/restore starts at the SOURCE's size (rbd clone cannot size the
			// child), so a mismatch here is expected: grow a smaller image to the
			// request (resuming an interrupted create whose resize never ran), and
			// accept a larger one (clone of a bigger source), reporting the real
			// size. Only a plain create is strict -- same name, different size is a
			// genuinely incompatible AlreadyExists.
			if !isClone {
				return nil, bardplugin.Errorf(bardplugin.CodeAlreadyExists, "volume %q exists at %dMiB, requested %dMiB", req.Name, existingMiB, sizeMiB)
			}
			if existingMiB < sizeMiB {
				if err := b.resizeImage(ctx, conn, spec, sizeMiB); err != nil {
					return nil, err
				}
				existingMiB = sizeMiB
			}
		}
		finalMiB = existingMiB // idempotent hit
	case errors.Is(infoErr, errImageNotFound):
		if err := b.provision(ctx, conn, spec, sizeMiB, req); err != nil {
			return nil, err
		}
		created = true
		if isClone {
			// The clone inherited the source's size; reconcile with the request.
			cloneMiB, err := b.imageInfo(ctx, conn, spec)
			if err != nil {
				return nil, err
			}
			if cloneMiB < sizeMiB {
				if err := b.resizeImage(ctx, conn, spec, sizeMiB); err != nil {
					return nil, err
				}
				cloneMiB = sizeMiB
			}
			finalMiB = cloneMiB
		}
	default:
		return nil, infoErr
	}

	if req.SourceVolume != nil {
		// Tail of the PVC-PVC clone recipe, on BOTH the fresh and the resumed
		// (idempotent-hit) path -- each step idempotent. Drop the temp source
		// snapshot: with a linked clone, clone-v2 moves it to the trash, where the
		// flatten releases it. Then sever the parent link out of band,
		// unconditionally (no depth gate): until the flatten lands, deleting the
		// SOURCE fails-and-retries like any image with clone children (and
		// DeleteVolume kicks the children's flatten itself, so that converges even
		// if this one is lost to a plugin restart); after it, source and clone are
		// fully independent -- `rbd cp` semantics without the inline copy. The
		// flatten no-ops on an already-severed clone and dedups against mirroring
		// flattens of the same image.
		tmpSnap := req.SourceVolume.Location + "/" + req.SourceVolume.Name + "@" + shortName(tmpClonePrefix, req.Name)
		if _, err := b.run.Run(ctx, "rbd", appendArgs(conn, "snap", "rm", tmpSnap)...); err != nil && !isNotFound(err) {
			return nil, fmt.Errorf("ceph-rbd: temp snapshot rm %s: %w", tmpSnap, err)
		}
		b.backgroundFlatten(req.Instance, req.Secrets, spec, false)
	}

	// Apply any VolumeAttributesClass parameters to the (now existing) image.
	if err := b.applyMutableParams(ctx, conn, spec, req.MutableParams); err != nil {
		return nil, err
	}

	// Encryption metadata. A clone (from a snapshot or another volume) copies its
	// source's LUKS header byte-for-byte, so it must open with the source's key;
	// inheritEncryption copies the source's encryption descriptor onto the clone
	// and returns the key identity to advertise to the node. A freshly created
	// encrypted volume just records its KMS id so DeleteVolume can clean up the
	// stored key (the CSI DeleteVolume RPC carries no volume context); the derived
	// provider stores nothing, so an empty id is not recorded.
	encKeyID := ""
	if req.Parameters[paramEncrypted] == "true" {
		switch {
		case created && (req.SourceSnapshot != nil || req.SourceVolume != nil):
			encKeyID, err = b.inheritEncryption(ctx, conn, cloneSourceImageSpec(req), spec, req.Instance, req.Secrets)
			if err != nil {
				return nil, err
			}
		default:
			if id := req.Parameters[paramEncryptionKMSID]; id != "" {
				if err := b.imageMetaSet(ctx, conn, spec, imgMetaKMSID, id); err != nil {
					return nil, err
				}
			}
		}
	}

	// Label the image with its owning PVC for operability (rbd image-meta list),
	// from the provisioner's --extra-create-metadata. Best-effort: a cosmetic
	// label must never fail a provision.
	b.setOwnerMetadata(ctx, conn, spec, req.Parameters)
	// Stamp the owning deployment's name when the instance configures one
	// (best-effort, like the owner labels: operability, not correctness).
	if cc.ClusterName != "" {
		_ = b.imageMetaSet(ctx, conn, spec, imgMetaClusterName, cc.ClusterName)
	}

	volCtx := map[string]string{"pool": pool, "imageName": image}
	// Carry node map/unmap tuning to the node. mapOptions is applied at NodeStage;
	// unmapOptions is persisted with the device record there (NodeUnstage has no
	// volume context). Both may be mounter-scoped; resolution happens node-side.
	if v := req.Parameters[paramMapOptions]; v != "" {
		volCtx[paramMapOptions] = v
	}
	if v := req.Parameters[paramUnmapOptions]; v != "" {
		volCtx[paramUnmapOptions] = v
	}
	// tryOtherMounters lets a krbd map failure fall back to rbd-nbd at the node.
	if req.Parameters[paramTryOtherMounters] == "true" {
		volCtx[paramTryOtherMounters] = "true"
	}
	// mkfsOptions overrides the tuned mkfs defaults when the node formats the volume.
	if v := req.Parameters[paramMkfsOptions]; v != "" {
		volCtx[paramMkfsOptions] = v
	}
	// rbd-nbd client log placement/retention (validated up front; applied at the
	// node, and only for rbd-nbd maps -- krbd has no client log).
	if v := req.Parameters[paramCephLogDir]; v != "" {
		volCtx[paramCephLogDir] = v
	}
	if v := req.Parameters[paramCephLogStrategy]; v != "" {
		volCtx[paramCephLogStrategy] = v
	}
	// Carry the encryption decision to the node in the volume context (which CSI
	// echoes back at NodeStage); the LUKS layer is applied node-side. The KMS id
	// (if any) rides along so the node resolves the passphrase from the same
	// provider that would be used at create time.
	if req.Parameters[paramEncrypted] == "true" {
		volCtx[paramEncrypted] = "true"
		if id := req.Parameters[paramEncryptionKMSID]; id != "" {
			volCtx[paramEncryptionKMSID] = id
		}
		if req.Parameters[paramEncryptedDiscards] == "true" {
			volCtx[paramEncryptedDiscards] = "true"
		}
		// Encryption mode (block/LUKS default, or file/fscrypt) -- the node applies it.
		if t := req.Parameters[paramEncryptionType]; t != "" {
			volCtx[paramEncryptionType] = t
		}
		// LUKS format tuning (cipher/key-size/sector-size/integrity): already validated up
		// front; carry to the node, which runs luksFormat. Block-mode only (fscrypt has no
		// LUKS header to tune), which the up-front validation enforces.
		for _, k := range []string{paramEncryptionCipher, paramEncryptionKeySize, paramEncryptionSectorSize, paramEncryptionIntegrity} {
			if v := req.Parameters[k]; v != "" {
				volCtx[k] = v
			}
		}
		// For a clone, tell the node which key identity to resolve so the derived
		// provider re-derives the source's passphrase. A fresh volume omits this and
		// the node falls back to its own pool/image (the same value it always used).
		if encKeyID != "" {
			volCtx[ctxEncryptionKeyID] = encKeyID
		}
	}
	return &bardplugin.CreateVolumeResponse{
		Location:      location,
		Name:          image,
		CapacityBytes: finalMiB * mib,
		Context:       volCtx,
	}, nil
}

// resizeImage grows an image to sizeMiB (used to reconcile a clone, which starts
// at its source's size, with the requested capacity).
func (b *Backend) resizeImage(ctx context.Context, conn []string, spec string, sizeMiB int64) error {
	if _, err := b.run.Run(ctx, "rbd", appendArgs(conn, "resize", spec, "--size", strconv.FormatInt(sizeMiB, 10))...); err != nil {
		return fmt.Errorf("ceph-rbd: resize %s: %w", spec, err)
	}
	return nil
}

// cloneSourceImageSpec is the pool/image of a clone's source -- the head image
// that carries the encryption descriptor. For a snapshot source it strips the
// "@snap" suffix (image metadata lives on the head image, not the snapshot).
func cloneSourceImageSpec(req *bardplugin.CreateVolumeRequest) string {
	if req.SourceSnapshot != nil {
		name := req.SourceSnapshot.Name
		if i := strings.IndexByte(name, '@'); i >= 0 {
			name = name[:i]
		}
		return req.SourceSnapshot.Location + "/" + name
	}
	return req.SourceVolume.Location + "/" + req.SourceVolume.Name
}

// inheritEncryption makes an encrypted clone open with its source's key. rbd
// clone/cp copies the source's LUKS header byte-for-byte but copies no image
// metadata, so without this the clone would try to open the copied header with a
// key derived from (or stored under) its own new identity and fail. It copies the
// source's encryption descriptor -- KMS id, key identity, and any wrapped DEK
// (secrets-metadata) -- onto the clone, and for a provider that keeps key
// material outside the image (Vault) duplicates it into the clone's own slot, so
// the clone is self-contained: it opens with the source's passphrase yet
// DeleteVolume removes only its own key (no shared-key ref-count hazard). Returns
// the key identity to advertise to the node (used by the derived provider).
func (b *Backend) inheritEncryption(ctx context.Context, conn []string, sourceSpec, cloneSpec, instance string, secrets map[string]string) (string, error) {
	// The source's key identity: its recorded id, else its own pool/image (covers a
	// source provisioned before this descriptor existed, whose derived passphrase
	// used its pool/image). Persisted on the clone so a clone chain keeps deriving
	// the one root identity.
	keyID := b.imageMetaGet(ctx, conn, sourceSpec, imgMetaKeyID)
	if keyID == "" {
		keyID = sourceSpec
	}
	if err := b.imageMetaSet(ctx, conn, cloneSpec, imgMetaKeyID, keyID); err != nil {
		return "", err
	}
	// Carry the provider selection (so DeleteVolume cleans up the right KMS) and any
	// wrapped DEK (so a secrets-metadata clone unwraps the same key from its own
	// metadata) down to the clone.
	kmsID := b.imageMetaGet(ctx, conn, sourceSpec, imgMetaKMSID)
	if kmsID != "" {
		if err := b.imageMetaSet(ctx, conn, cloneSpec, imgMetaKMSID, kmsID); err != nil {
			return "", err
		}
	}
	if dek := b.imageMetaGet(ctx, conn, sourceSpec, imgMetaWrappedDEK); dek != "" {
		if err := b.imageMetaSet(ctx, conn, cloneSpec, imgMetaWrappedDEK, dek); err != nil {
			return "", err
		}
	}
	// A provider with external key material (Vault, Azure, KMIP) copies the source's
	// entry into the clone's own slot. Providers that store in image metadata need no
	// hook (CloneKey is a no-op for them).
	if kmsID != "" {
		if err := b.kms.CloneKey(ctx, conn, instance, kmsID, sourceSpec, cloneSpec, secrets); err != nil {
			return "", err
		}
	}
	return keyID, nil
}

func (b *Backend) provision(ctx context.Context, conn []string, spec string, sizeMiB int64, req *bardplugin.CreateVolumeRequest) error {
	switch {
	case req.SourceSnapshot != nil:
		parent := req.SourceSnapshot.Location + "/" + req.SourceSnapshot.Name // "image@snap"
		// Clone v2: unlike v1, it does not require the parent snapshot to be
		// protected, and it lets the parent snapshot be deleted while clones still
		// exist (Ceph tracks the dependency). Without this, recent Ceph rejects the
		// clone with "parent snapshot must be protected".
		args := appendArgs(conn, "clone", parent, spec, "--rbd-default-clone-format", "2")
		args = append(args, dataPoolArgs(req.Parameters)...)
		if _, err := b.run.Run(ctx, "rbd", args...); err != nil {
			return fmt.Errorf("ceph-rbd: clone: %w", err)
		}
		// Record the content source so an idempotent retry can verify it matches.
		if err := b.imageMetaSet(ctx, conn, spec, imgMetaSource, sourceDesc(req)); err != nil {
			return err
		}
		// Bound the COW chain: a clone of a clone of ... eventually hits rbd's hard
		// parent-depth limit and slows reads, so flatten once the chain is deep enough.
		// Out of band (best-effort): the flatten copies data and the depth walk shells
		// out per ancestor, which must not block/expire the CSI provisioning call.
		b.scheduleFlatten(req.Instance, req.Secrets, spec)
	case req.SourceVolume != nil:
		// PVC-PVC clone. Deliberately NOT `rbd cp`: a full copy takes minutes to
		// hours on a large image (racing every CSI deadline), and a copy killed
		// mid-flight leaves a full-size, partially-copied destination that a retry
		// would accept as an idempotent hit -- silent corruption. Instead take a
		// temporary snapshot of the source and COW-clone it (both instant, both
		// idempotent), then flatten OUT OF BAND -- the same end state as a full
		// copy, without ever racing a deadline.
		parent := req.SourceVolume.Location + "/" + req.SourceVolume.Name
		tmpSnap := parent + "@" + shortName(tmpClonePrefix, req.Name)
		if _, err := b.run.Run(ctx, "rbd", appendArgs(conn, "snap", "create", tmpSnap)...); err != nil && !isAlreadyExists(err) {
			return fmt.Errorf("ceph-rbd: temp snapshot %s: %w", tmpSnap, err)
		}
		args := appendArgs(conn, "clone", tmpSnap, spec, "--rbd-default-clone-format", "2")
		args = append(args, dataPoolArgs(req.Parameters)...)
		if _, err := b.run.Run(ctx, "rbd", args...); err != nil && !isAlreadyExists(err) {
			return fmt.Errorf("ceph-rbd: clone: %w", err)
		}
		// Record the content source so an idempotent retry can verify it matches.
		if err := b.imageMetaSet(ctx, conn, spec, imgMetaSource, sourceDesc(req)); err != nil {
			return err
		}
		// The recipe's tail -- dropping the temp snapshot and flattening the clone
		// -- runs in CreateVolume after the switch, so a create resumed on the
		// idempotent-hit path (crashed between clone and snap rm) converges too.
	default:
		args := appendArgs(conn, "create", spec, "--size", strconv.FormatInt(sizeMiB, 10))
		args = append(args, createParams(req.Parameters)...)
		if _, err := b.run.Run(ctx, "rbd", args...); err != nil && !isAlreadyExists(err) {
			return fmt.Errorf("ceph-rbd: create: %w", err)
		}
	}
	return nil
}

// createParams turns StorageClass parameters into extra `rbd create` flags.
// Unset params are simply omitted, so the cluster's rbd defaults apply.
func createParams(p map[string]string) []string {
	var a []string
	if v := p[paramImageFeatures]; v != "" {
		a = append(a, "--image-feature", v)
	}
	if v := p[paramStripeUnit]; v != "" {
		a = append(a, "--stripe-unit", v)
	}
	if v := p[paramStripeCount]; v != "" {
		a = append(a, "--stripe-count", v)
	}
	if v := p[paramObjectSize]; v != "" {
		a = append(a, "--object-size", v)
	}
	a = append(a, dataPoolArgs(p)...)
	return a
}

// dataPoolArgs returns the `--data-pool` flag for an erasure-coded backing pool, or
// nil. Applies to create, clone, and cp (each writes a new image's data).
func dataPoolArgs(p map[string]string) []string {
	if v := p[paramDataPool]; v != "" {
		return []string{"--data-pool", v}
	}
	return nil
}

// rbdMutableParams maps the VolumeAttributesClass parameter keys this backend
// accepts (CSI mutable parameters) to their underlying rbd image config keys.
// These are runtime QoS throttles, changeable on a live volume via
// `rbd config image set` -- the mutable analogue of the create-time params.
var rbdMutableParams = map[string]string{
	"qosIopsLimit":      "rbd_qos_iops_limit",
	"qosBpsLimit":       "rbd_qos_bps_limit",
	"qosReadIopsLimit":  "rbd_qos_read_iops_limit",
	"qosWriteIopsLimit": "rbd_qos_write_iops_limit",
	"qosReadBpsLimit":   "rbd_qos_read_bps_limit",
	"qosWriteBpsLimit":  "rbd_qos_write_bps_limit",
}

// validateMutableParams rejects any mutable parameter key this backend does not
// support, as CSI requires (an unknown VolumeAttributesClass parameter must fail
// with InvalidArgument rather than be silently ignored). Empty/nil is valid.
func validateMutableParams(p map[string]string) error {
	for k := range p {
		if _, ok := rbdMutableParams[k]; !ok {
			return bardplugin.Errorf(bardplugin.CodeInvalidArg, "ceph-rbd: unsupported mutable parameter %q", k)
		}
	}
	return nil
}

// applyMutableParams sets each supported mutable parameter on the image's rbd
// config. An empty value removes the override (reverts to the pool/global
// default). Callers must validate first.
func (b *Backend) applyMutableParams(ctx context.Context, conn []string, spec string, p map[string]string) error {
	for k, v := range p {
		rbdKey := rbdMutableParams[k]
		var err error
		if v == "" {
			_, err = b.run.Run(ctx, "rbd", appendArgs(conn, "config", "image", "remove", spec, rbdKey)...)
			if err != nil && isNotFound(err) {
				err = nil // already at default
			}
		} else {
			_, err = b.run.Run(ctx, "rbd", appendArgs(conn, "config", "image", "set", spec, rbdKey, v)...)
		}
		if err != nil {
			return fmt.Errorf("ceph-rbd: set %s=%q on %s: %w", rbdKey, v, spec, err)
		}
	}
	return nil
}

// ModifyVolume implements bardplugin.VolumeModifier: it changes a live volume's
// mutable QoS parameters (CSI ControllerModifyVolume / VolumeAttributesClass).
func (b *Backend) ModifyVolume(ctx context.Context, req *bardplugin.ModifyVolumeRequest) (*bardplugin.ModifyVolumeResponse, error) {
	if err := validateMutableParams(req.MutableParams); err != nil {
		return nil, err
	}
	cc, err := b.cluster(req.Volume.Instance)
	if err != nil {
		return nil, err
	}
	conn, cleanup, err := b.connArgs(cc, req.Volume.Instance, req.Secrets)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	spec := req.Volume.Location + "/" + req.Volume.Name
	// The volume must exist; a modify on a missing image is NotFound.
	if _, err := b.imageInfo(ctx, conn, spec); err != nil {
		if errors.Is(err, errImageNotFound) {
			return nil, bardplugin.Errorf(bardplugin.CodeNotFound, "ceph-rbd: image %s not found", spec)
		}
		return nil, err
	}
	if err := b.applyMutableParams(ctx, conn, spec, req.MutableParams); err != nil {
		return nil, err
	}
	return &bardplugin.ModifyVolumeResponse{}, nil
}

// ReclaimSpace implements bardplugin.SpaceReclaimer: it runs `rbd sparsify` to
// deallocate runs of zeroed blocks in the image back to the pool (the csi-addons
// controller ReclaimSpace operation). It reports used bytes before and after so
// the csi-addons job can show how much was reclaimed; usage is best-effort
// (a -1 means unknown and Bard omits that side).
func (b *Backend) ReclaimSpace(ctx context.Context, req *bardplugin.ReclaimSpaceRequest) (*bardplugin.ReclaimSpaceResponse, error) {
	cc, err := b.cluster(req.Volume.Instance)
	if err != nil {
		return nil, err
	}
	conn, cleanup, err := b.connArgs(cc, req.Volume.Instance, req.Secrets)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	spec := req.Volume.Location + "/" + req.Volume.Name
	if _, err := b.imageInfo(ctx, conn, spec); err != nil {
		if errors.Is(err, errImageNotFound) {
			return nil, bardplugin.Errorf(bardplugin.CodeNotFound, "ceph-rbd: image %s not found", spec)
		}
		return nil, err
	}
	pre := b.usedBytes(ctx, conn, spec)
	if _, err := b.run.Run(ctx, "rbd", appendArgs(conn, "sparsify", spec)...); err != nil {
		return nil, fmt.Errorf("ceph-rbd: sparsify %s: %w", spec, err)
	}
	post := b.usedBytes(ctx, conn, spec)
	return &bardplugin.ReclaimSpaceResponse{PreUsageBytes: pre, PostUsageBytes: post}, nil
}

// usedBytes returns an image's actually-consumed bytes from `rbd du`, or -1 when
// it can't be determined (the reclaim still succeeds; only the report is omitted).
func (b *Backend) usedBytes(ctx context.Context, conn []string, spec string) int64 {
	out, err := b.run.Run(ctx, "rbd", appendArgs(conn, "du", spec, "--format", "json")...)
	if err != nil {
		return -1
	}
	var du struct {
		TotalUsedSize *int64 `json:"total_used_size"`
		Images        []struct {
			UsedSize *int64 `json:"used_size"`
		} `json:"images"`
	}
	if err := json.Unmarshal([]byte(out), &du); err != nil {
		return -1
	}
	if du.TotalUsedSize != nil {
		return *du.TotalUsedSize
	}
	if len(du.Images) == 1 && du.Images[0].UsedSize != nil {
		return *du.Images[0].UsedSize
	}
	return -1
}

func (b *Backend) DeleteVolume(ctx context.Context, req *bardplugin.DeleteVolumeRequest) error {
	cc, err := b.cluster(req.Volume.Instance)
	if err != nil {
		return err
	}
	conn, cleanup, err := b.connArgs(cc, req.Volume.Instance, req.Secrets)
	if err != nil {
		return err
	}
	defer cleanup()
	spec := req.Volume.Location + "/" + req.Volume.Name

	// A statically provisioned image is owned by the admin, not by CSI: never reap
	// it. Marked with `rbd image-meta set <img> bard.static true`, so DeleteVolume
	// no-ops (success) and even a reclaimPolicy:Delete PV cannot destroy the image.
	// KMS cleanup is skipped too -- the admin owns the key material. The checked
	// read matters: a transient meta-read failure must fail the delete (retry),
	// not be misread as "not static" and reap an admin-owned image.
	static, err := b.imageMetaGetChecked(ctx, conn, spec, imgMetaStatic)
	if err != nil {
		return err
	}
	if static == "true" {
		return nil
	}

	// Clean up KMS-stored key material before removing the image. Done first so
	// that if it fails, the image (and its recorded KMS id) still exist for CSI to
	// retry; both deleteKey and rm are idempotent, so retries converge. An empty
	// id (derived provider or unencrypted) routes to a no-op deleteKey. Checked
	// read: a misread "" here would leak the external key (Vault/KMIP/Azure).
	kmsID, err := b.imageMetaGetChecked(ctx, conn, spec, imgMetaKMSID)
	if err != nil {
		return err
	}
	if kmsID != "" {
		if err := b.kms.DeleteKey(ctx, conn, req.Volume.Instance, kmsID, req.Volume.Location+"/"+req.Volume.Name); err != nil {
			return err
		}
	}

	if _, err := b.run.Run(ctx, "rbd", appendArgs(conn, "rm", spec)...); err != nil && !isNotFound(err) {
		// Blocked by TRASHED snapshots with linked clones -- a PVC-PVC clone whose
		// out-of-band flatten hasn't landed (e.g. lost to a plugin restart), or a
		// snapshot deleted while its restores still reference it: flatten the
		// children out of band so the retry converges on its own. Live snapshots
		// (owned by VolumeSnapshot objects) are NOT touched -- failing on those is
		// correct and final until the snapshots are deleted.
		if errContains(err, "linked", "clones") {
			b.flattenChildren(ctx, conn, req.Volume.Instance, req.Secrets, spec)
		}
		return fmt.Errorf("ceph-rbd: rm %s: %w", spec, err)
	}
	return nil
}

// flattenChildren kicks a background flatten for every clone child of spec
// (including children of its trashed snapshots), so a DeleteVolume blocked by
// clone-linked snapshots converges without operator action. Best-effort: the
// delete is failing anyway and will be retried.
func (b *Backend) flattenChildren(ctx context.Context, conn []string, instance string, secrets map[string]string, spec string) {
	out, err := b.run.Run(ctx, "rbd", appendArgs(conn, "children", "--all", spec, "--format", "json")...)
	if err != nil {
		return
	}
	var kids []struct {
		Pool          string `json:"pool"`
		PoolNamespace string `json:"pool_namespace"`
		Image         string `json:"image"`
		Trash         bool   `json:"trash"`
	}
	if json.Unmarshal([]byte(out), &kids) != nil {
		return
	}
	for _, k := range kids {
		if k.Image == "" || k.Trash {
			continue // a trashed child reaps on its own trash purge
		}
		b.backgroundFlatten(instance, secrets, locator(k.Pool, k.PoolNamespace)+"/"+k.Image, false)
	}
}

func (b *Backend) ExpandVolume(ctx context.Context, req *bardplugin.ExpandVolumeRequest) (*bardplugin.ExpandVolumeResponse, error) {
	cc, err := b.cluster(req.Volume.Instance)
	if err != nil {
		return nil, err
	}
	conn, cleanup, err := b.connArgs(cc, req.Volume.Instance, req.Secrets)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	sizeMiB := bytesToMiB(req.NewSizeBytes)
	spec := req.Volume.Location + "/" + req.Volume.Name
	if err := b.resizeImage(ctx, conn, spec, sizeMiB); err != nil {
		return nil, err
	}
	return &bardplugin.ExpandVolumeResponse{CapacityBytes: sizeMiB * mib, NodeExpansionRequired: true}, nil
}

func (b *Backend) CreateSnapshot(ctx context.Context, req *bardplugin.CreateSnapshotRequest) (*bardplugin.CreateSnapshotResponse, error) {
	src := req.SourceVolume
	cc, err := b.cluster(src.Instance)
	if err != nil {
		return nil, err
	}
	// Validate the name prefix before any side effect (index registration, rbd
	// calls), so a bad VolumeSnapshotClass fails fast with nothing to clean up.
	snapPrefix, err := namePrefix(paramSnapshotNamePrefix, req.Parameters[paramSnapshotNamePrefix], snapNamePrefix)
	if err != nil {
		return nil, err
	}

	snap := shortName(snapPrefix, req.Name)
	// A CSI snapshot name is cluster-unique: reject reuse against a different
	// source volume; allow an idempotent retry against the same one. The reverse
	// entry (by the handle DeleteSnapshot will see) lets the delete release the name.
	b.mu.Lock()
	if prev, ok := b.snapIndex[req.Name]; ok && prev != refKey(src) {
		b.mu.Unlock()
		return nil, bardplugin.Errorf(bardplugin.CodeAlreadyExists, "snapshot %q already exists for a different source volume", req.Name)
	}
	b.snapIndex[req.Name] = refKey(src)
	b.snapNames[refKey(bardplugin.VolumeRef{Instance: src.Instance, Location: src.Location, Name: src.Name + "@" + snap})] = req.Name
	b.mu.Unlock()

	conn, cleanup, err := b.connArgs(cc, src.Instance, req.Secrets)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	snapSpec := fmt.Sprintf("%s/%s@%s", src.Location, src.Name, snap)
	if _, err := b.run.Run(ctx, "rbd", appendArgs(conn, "snap", "create", snapSpec)...); err != nil && !isAlreadyExists(err) {
		return nil, fmt.Errorf("ceph-rbd: snap create %s: %w", snapSpec, err)
	}
	// The restore size the CO records comes from here; a transiently failed read
	// must fail the (idempotent) create for a retry, not report size 0.
	sizeMiB, err := b.imageInfo(ctx, conn, src.Location+"/"+src.Name)
	if err != nil {
		return nil, err
	}

	// Location/Name encode image@snap so DeleteSnapshot can reconstruct it. Bard
	// core fills the CSI SourceVolumeID from the request.
	return &bardplugin.CreateSnapshotResponse{
		Location:         src.Location,
		Name:             src.Name + "@" + snap,
		SizeBytes:        sizeMiB * mib,
		CreationTimeUnix: time.Now().Unix(),
		ReadyToUse:       true,
	}, nil
}

func (b *Backend) DeleteSnapshot(ctx context.Context, req *bardplugin.DeleteSnapshotRequest) error {
	cc, err := b.cluster(req.Snapshot.Instance)
	if err != nil {
		return err
	}
	conn, cleanup, err := b.connArgs(cc, req.Snapshot.Instance, req.Secrets)
	if err != nil {
		return err
	}
	defer cleanup()
	spec := req.Snapshot.Location + "/" + req.Snapshot.Name // Name encodes "image@snap"
	if _, err := b.run.Run(ctx, "rbd", appendArgs(conn, "snap", "rm", spec)...); err != nil && !isNotFound(err) {
		return fmt.Errorf("ceph-rbd: snap rm %s: %w", spec, err)
	}
	// Release the CSI snapshot name so it can be reused against another source.
	b.mu.Lock()
	if csiName, ok := b.snapNames[refKey(req.Snapshot)]; ok {
		delete(b.snapIndex, csiName)
		delete(b.snapNames, refKey(req.Snapshot))
	}
	b.mu.Unlock()
	return nil
}

// volNamePrefix / snapNamePrefix are the default shortName() prefixes; listing
// filters to Bard-managed objects so unrelated images/snapshots in the pool aren't
// enumerated. Both prefixes are overridable per class (volumeNamePrefix /
// snapshotNamePrefix), so the listings recognise objects by the shortName hash
// shape, not just these prefixes; shortNameHashLen is the hex length of that
// hash (hex of 8 bytes).
const (
	volNamePrefix    = "csi-vol-"
	snapNamePrefix   = "csi-snap-"
	shortNameHashLen = 16
)

// ListVolumes implements bardplugin.VolumeLister: every Bard rbd image across all
// configured instances/pools. Bard core sorts + paginates across backends.
func (b *Backend) ListVolumes(ctx context.Context, _ *bardplugin.ListVolumesRequest) (*bardplugin.ListVolumesResponse, error) {
	var entries []bardplugin.VolumeListEntry
	for instance, cc := range b.clusters {
		if cc.Pool == "" {
			continue
		}
		conn, cleanup, err := b.connArgs(cc, instance, nil)
		if err != nil {
			return nil, err
		}
		loc := locator(cc.Pool, cc.RadosNamespace)
		images, err := b.listImages(ctx, conn, cc.Pool, cc.RadosNamespace)
		if err != nil {
			cleanup()
			return nil, err
		}
		for _, img := range images {
			if !isBardImageName(img) {
				continue
			}
			sizeMiB, _ := b.imageInfo(ctx, conn, loc+"/"+img)
			entries = append(entries, bardplugin.VolumeListEntry{
				Volume:        bardplugin.VolumeRef{Instance: instance, Location: loc, Name: img},
				CapacityBytes: sizeMiB * mib,
			})
		}
		cleanup()
	}
	return &bardplugin.ListVolumesResponse{Entries: entries}, nil
}

// ListSnapshots implements bardplugin.SnapshotLister: every Bard rbd snapshot
// (image@snap) across all configured instances/pools. Bard core filters + paginates.
func (b *Backend) ListSnapshots(ctx context.Context, _ *bardplugin.ListSnapshotsRequest) (*bardplugin.ListSnapshotsResponse, error) {
	var entries []bardplugin.SnapshotListEntry
	for instance, cc := range b.clusters {
		if cc.Pool == "" {
			continue
		}
		conn, cleanup, err := b.connArgs(cc, instance, nil)
		if err != nil {
			return nil, err
		}
		loc := locator(cc.Pool, cc.RadosNamespace)
		images, err := b.listImages(ctx, conn, cc.Pool, cc.RadosNamespace)
		if err != nil {
			cleanup()
			return nil, err
		}
		for _, img := range images {
			if !isBardImageName(img) {
				continue
			}
			snaps, err := b.listSnaps(ctx, conn, loc+"/"+img)
			if err != nil {
				cleanup()
				return nil, err
			}
			for _, sn := range snaps {
				// Any shortName-shaped snapshot (custom snapshotNamePrefix included)
				// is Bard-managed; mirror/group snapshots ('.mirror.*', '.group.*')
				// and foreign snaps fail the 16-hex-suffix check and are skipped.
				// A clonetmp- snapshot is the transient vehicle of an in-flight
				// PVC-PVC clone, not a CSI snapshot.
				if !isBardImageName(sn.Name) || strings.HasPrefix(sn.Name, tmpClonePrefix) {
					continue
				}
				entries = append(entries, bardplugin.SnapshotListEntry{
					Snapshot:     bardplugin.VolumeRef{Instance: instance, Location: loc, Name: img + "@" + sn.Name},
					SourceVolume: bardplugin.VolumeRef{Instance: instance, Location: loc, Name: img},
					SizeBytes:    sn.Size,
					ReadyToUse:   true,
				})
			}
		}
		cleanup()
	}
	return &bardplugin.ListSnapshotsResponse{Entries: entries}, nil
}

// locator is the backend Location of a volume: the pool, or "pool/namespace" when a
// rados namespace is in use. Every rbd object spec is Location+"/"+name, so encoding
// the namespace here threads it through the whole lifecycle -- the volume handle
// carries Location, so DeleteVolume / NodeStage (which get no parameters) still
// address the image in its namespace.
func locator(pool, namespace string) string {
	if namespace == "" {
		return pool
	}
	return pool + "/" + namespace
}

// ensureNamespace makes sure a rados namespace exists before an image is created in
// it (rbd create into a missing namespace fails). Idempotent: it lists first and
// creates only when absent, so re-provisioning is a no-op.
func (b *Backend) ensureNamespace(ctx context.Context, conn []string, pool, namespace string) error {
	out, err := b.run.Run(ctx, "rbd", appendArgs(conn, "namespace", "ls", pool, "--format", "json")...)
	if err != nil {
		return fmt.Errorf("ceph-rbd: rbd namespace ls %s: %w", pool, err)
	}
	var existing []struct {
		Name string `json:"name"`
	}
	if e := json.Unmarshal([]byte(out), &existing); e != nil {
		return fmt.Errorf("ceph-rbd: parse rbd namespace ls %s: %w", pool, e)
	}
	for _, n := range existing {
		if n.Name == namespace {
			return nil
		}
	}
	if _, err := b.run.Run(ctx, "rbd", appendArgs(conn, "namespace", "create", pool+"/"+namespace)...); err != nil {
		return fmt.Errorf("ceph-rbd: rbd namespace create %s/%s: %w", pool, namespace, err)
	}
	return nil
}

// listImages returns the rbd image names in a pool (rbd ls --format json), scoped to
// a rados namespace when one is given.
func (b *Backend) listImages(ctx context.Context, conn []string, pool, namespace string) ([]string, error) {
	args := []string{"ls", pool, "--format", "json"}
	if namespace != "" {
		args = append(args, "--namespace", namespace)
	}
	out, err := b.run.Run(ctx, "rbd", appendArgs(conn, args...)...)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("ceph-rbd: rbd ls %s: %w", pool, err)
	}
	var names []string
	if e := json.Unmarshal([]byte(out), &names); e != nil {
		return nil, fmt.Errorf("ceph-rbd: parse rbd ls %s: %w", pool, e)
	}
	return names, nil
}

type rbdSnap struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// listSnaps returns an rbd image's snapshots (rbd snap ls --format json).
func (b *Backend) listSnaps(ctx context.Context, conn []string, spec string) ([]rbdSnap, error) {
	out, err := b.run.Run(ctx, "rbd", appendArgs(conn, "snap", "ls", spec, "--format", "json")...)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("ceph-rbd: rbd snap ls %s: %w", spec, err)
	}
	var snaps []rbdSnap
	if e := json.Unmarshal([]byte(out), &snaps); e != nil {
		return nil, fmt.Errorf("ceph-rbd: parse rbd snap ls %s: %w", spec, e)
	}
	return snaps, nil
}

// GetCapacity implements bardplugin.CapacityReporter: the bytes available to
// provision in the instance's pool, from `ceph df` (max_avail accounts for
// replication). Implementing this interface makes Bard advertise CSI GetCapacity.
func (b *Backend) GetCapacity(ctx context.Context, req *bardplugin.GetCapacityRequest) (*bardplugin.GetCapacityResponse, error) {
	cc, err := b.cluster(req.Instance)
	if err != nil {
		return nil, err
	}
	pool := req.Parameters[paramPool]
	if pool == "" {
		pool = cc.Pool
	}
	if pool == "" {
		return nil, fmt.Errorf("ceph-rbd: no pool for capacity")
	}
	conn, cleanup, err := b.cephCLIConn(cc, req.Instance, nil)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	out, err := b.run.Run(ctx, "ceph", appendArgs(conn, "df", "--format", "json")...)
	if err != nil {
		return nil, fmt.Errorf("ceph-rbd: ceph df: %w", err)
	}
	var df struct {
		Pools []struct {
			Name  string `json:"name"`
			Stats struct {
				MaxAvail int64 `json:"max_avail"`
			} `json:"stats"`
		} `json:"pools"`
	}
	if err := json.Unmarshal([]byte(out), &df); err != nil {
		return nil, fmt.Errorf("ceph-rbd: parse ceph df: %w", err)
	}
	for _, p := range df.Pools {
		if p.Name == pool {
			return &bardplugin.GetCapacityResponse{AvailableBytes: p.Stats.MaxAvail}, nil
		}
	}
	return nil, fmt.Errorf("ceph-rbd: pool %q not found in ceph df", pool)
}

// GetVolumeHealth implements bardplugin.HealthReporter: it reports a volume as
// abnormal when its backing rbd image no longer exists (deleted out of band) or
// cannot be queried. Implementing this interface makes Bard advertise CSI volume
// health monitoring (ControllerGetVolume with a VolumeCondition).
func (b *Backend) GetVolumeHealth(ctx context.Context, req *bardplugin.GetVolumeHealthRequest) (*bardplugin.GetVolumeHealthResponse, error) {
	cc, err := b.cluster(req.Volume.Instance)
	if err != nil {
		return nil, err
	}
	conn, cleanup, err := b.connArgs(cc, req.Volume.Instance, req.Secrets)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	spec := req.Volume.Location + "/" + req.Volume.Name
	switch _, infoErr := b.imageInfo(ctx, conn, spec); {
	case infoErr == nil:
		return &bardplugin.GetVolumeHealthResponse{Abnormal: false, Message: "rbd image " + spec + " is accessible"}, nil
	case errors.Is(infoErr, errImageNotFound):
		return &bardplugin.GetVolumeHealthResponse{Abnormal: true, Message: "rbd image " + spec + " no longer exists"}, nil
	default:
		// A transient query failure (mon unreachable, auth) is not a verdict on
		// the volume's health -- surface it as an error so the monitor retries.
		return nil, infoErr
	}
}

// cephCLIConn builds connection flags for the Python `ceph` CLI, which (unlike
// rbd) needs a conf file to initialise librados -- so it writes a minimal one
// with mon_host. Mirrors the CephFS plugin's helper. secrets (optional) lets a
// CSI-secret-only deployment -- no mounted key dir -- reach the ceph CLI too
// (the single-writer fence at NodeStage); the mounted per-instance key still
// wins, matching connArgs.
func (b *Backend) cephCLIConn(cc ClusterConfig, instance string, secrets map[string]string) ([]string, func(), error) {
	conf, err := os.CreateTemp("", "csi-ceph-conf-")
	if err != nil {
		return nil, func() {}, fmt.Errorf("ceph-rbd: conf: %w", err)
	}
	fmt.Fprintf(conf, "[global]\nmon_host = %s\n", strings.Join(cc.Monitors, ","))
	conf.Close()
	files := []string{conf.Name()}
	cleanup := func() {
		for _, f := range files {
			os.Remove(f)
		}
	}

	user := secrets[secretUserID]
	if user == "" {
		user = cc.UserID
	}
	if user == "" {
		user = defaultUserID
	}
	args := []string{"--conf", conf.Name(), "--id", user}

	key, err := b.keyFor(instance, secrets)
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	if key != "" {
		kf, err := cephenc.SecretTemp("csi-ceph-key-")
		if err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("ceph-rbd: keyfile: %w", err)
		}
		kf.WriteString(key)
		kf.Close()
		files = append(files, kf.Name())
		args = append(args, "--keyfile", kf.Name())
	}
	return args, cleanup, nil
}

// ---- node plane ----------------------------------------------------------

func (b *Backend) mapImage(ctx context.Context, mounter string, conn []string, spec, mapOptions, cookie, logFile string, readOnly bool) (string, error) {
	bin := "rbd"
	args := appendArgs(conn, "map", spec)
	if mounter == mounterNBD {
		bin = "rbd-nbd"
		// Map over the nbd netlink interface with a stable per-volume cookie: that
		// is what lets the healer `rbd-nbd attach` the device back after a
		// node-plugin restart (a legacy ioctl map cannot be reattached). Harmless
		// for krbd, which is not involved here.
		args = append(args, "--try-netlink", "--cookie", cookie)
		// Per-volume client log under cephLogDir (unset: rbd-nbd's own default).
		if logFile != "" {
			args = append(args, "--log-file", logFile)
		}
	}
	if readOnly {
		// Map the image read-only at the Ceph client (rbd/rbd-nbd both honor
		// --read-only): writes are rejected before they reach the OSDs, so a
		// ReadOnlyMany volume's shared image cannot be mutated by any consumer on
		// any node. Driven by the access mode (see core's readOnlyAccess), not a
		// host-local blockdev flag -- the same approach ceph-csi uses.
		args = append(args, "--read-only")
	}
	if mapOptions != "" {
		args = append(args, "--options", mapOptions)
	}
	return b.run.Run(ctx, bin, args...)
}

// mapWithFallback maps the image with the instance's mounter, falling back from krbd
// to rbd-nbd when the krbd map fails and the volume opted into `tryOtherMounters`
// (the ceph-csi knob -- typically because the node's krbd driver lacks an image
// feature). It returns the mapped device and the mounter that actually mapped it, so
// NodeStage records the right mounter for NodeUnstage. Map options are resolved for
// whichever mounter is used (read-affinity is krbd-only, so it is dropped on the
// rbd-nbd fallback).
func (b *Backend) mapWithFallback(ctx context.Context, cc ClusterConfig, conn []string, req *bardplugin.NodeStageRequest, spec, cookie string) (string, string, error) {
	primary := cc.Mounter
	if primary == "" {
		primary = mounterKRBD
	}
	mapOpts := combineOptions(
		readAffinityOptions(cc, req.CrushLocation),
		resolveMounterOptions(req.Context[paramMapOptions], primary),
	)
	logFile := nbdLogFile(req.Context[paramCephLogDir], req.Volume)
	if logFile != "" {
		// rbd-nbd does not create the log directory, and a missing one silently
		// loses the log (the map itself still succeeds). Best-effort like the
		// logging itself.
		_ = os.MkdirAll(filepath.Dir(logFile), 0o755)
	}
	dev, err := b.mapImage(ctx, primary, conn, spec, mapOpts, cookie, logFile, req.Readonly)
	if err == nil {
		return strings.TrimSpace(dev), primary, nil
	}
	// Only krbd has an "other" mounter to try, and only when the volume opted in.
	if primary == mounterNBD || req.Context[paramTryOtherMounters] != "true" {
		return "", "", fmt.Errorf("ceph-rbd: map %s: %w", spec, err)
	}
	nbdOpts := resolveMounterOptions(req.Context[paramMapOptions], mounterNBD)
	dev, nbdErr := b.mapImage(ctx, mounterNBD, conn, spec, nbdOpts, cookie, logFile, req.Readonly)
	if nbdErr != nil {
		return "", "", fmt.Errorf("ceph-rbd: map %s: krbd failed (%v) and rbd-nbd fallback failed: %w", spec, err, nbdErr)
	}
	return strings.TrimSpace(dev), mounterNBD, nil
}

// nbdCookie derives a stable per-volume rbd-nbd cookie from the staging path, so
// the `rbd-nbd map --cookie` at NodeStage and the healer's later `attach --cookie`
// agree on the same value without the healer needing the staging path.
func nbdCookie(stagingPath string) string {
	sum := sha256.Sum256([]byte("bard-nbd:" + stagingPath))
	return hex.EncodeToString(sum[:8])
}

// validateCephLogStrategy rejects an unknown cephLogStrategy at CreateVolume,
// before any image exists (a bad class must not leak an orphan).
func validateCephLogStrategy(p map[string]string) error {
	switch p[paramCephLogStrategy] {
	case "", "remove", "compress", "preserve":
		return nil
	}
	return bardplugin.Errorf(bardplugin.CodeInvalidArg,
		"ceph-rbd: invalid cephLogStrategy %q (remove|compress|preserve)", p[paramCephLogStrategy])
}

// nbdLogFile is the per-volume rbd-nbd client log path under cephLogDir. Empty
// when the volume did not set cephLogDir: rbd-nbd then logs to its own default
// and NodeUnstage manages nothing.
func nbdLogFile(dir string, vol bardplugin.VolumeRef) string {
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "rbd-nbd-"+strings.ReplaceAll(vol.Location, "/", "-")+"-"+vol.Name+".log")
}

// disposeNbdLog applies the recorded cephLogStrategy to the volume's rbd-nbd log
// after a clean unmap: remove (the default), compress (gzip in place), preserve.
// Best-effort -- log housekeeping must never fail an unstage.
func (b *Backend) disposeNbdLog(ctx context.Context, rec deviceRecord) {
	if rec.LogFile == "" {
		return
	}
	switch rec.LogStrategy {
	case "preserve":
	case "compress":
		_, _ = b.run.Run(ctx, "gzip", "-f", rec.LogFile)
	default: // "remove" (and "", the default)
		_, _ = b.run.Run(ctx, "rm", "-f", rec.LogFile)
	}
}

// unmapBinary picks the unmap tool from the mounter that actually mapped the device
// (recorded at stage time), so a volume that fell back from krbd to rbd-nbd is
// unmapped with rbd-nbd. Defaults to krbd (rbd) for an empty/legacy record.
func unmapBinary(mounter string) string {
	if mounter == mounterNBD {
		return "rbd-nbd"
	}
	return "rbd"
}

// deviceRecordPath maps a staging path to its record file under stateDir.
func (b *Backend) deviceRecordPath(stagingPath string) string {
	sum := sha256.Sum256([]byte(stagingPath))
	return filepath.Join(b.stateDir, hex.EncodeToString(sum[:16]))
}

// deviceRecord is what NodeStage persists per staging path. NodeUnstage uses it
// to unmap reliably without a volume context (Device + UnmapOptions), and the
// rbd-nbd healer uses Instance/Pool/Image/Mounter to reattach a userspace map
// that died with a previous node-plugin process (see Heal).
type deviceRecord struct {
	Device       string `json:"device"`
	UnmapOptions string `json:"unmapOptions,omitempty"`
	Instance     string `json:"instance,omitempty"`
	Pool         string `json:"pool,omitempty"`
	Image        string `json:"image,omitempty"`
	Mounter      string `json:"mounter,omitempty"`
	Cookie       string `json:"cookie,omitempty"` // rbd-nbd --cookie, for reattach
	// LogFile/LogStrategy carry the volume's cephLogDir/cephLogStrategy to
	// NodeUnstage (which has no volume context): the rbd-nbd client log to act on
	// after a clean unmap, and how (remove default / compress / preserve).
	LogFile     string `json:"logFile,omitempty"`
	LogStrategy string `json:"logStrategy,omitempty"`
}

// recordDevice persists a device record for a staging path.
func (b *Backend) recordDevice(stagingPath string, rec deviceRecord) error {
	if b.stateDir == "" {
		return nil
	}
	if err := os.MkdirAll(b.stateDir, 0o750); err != nil {
		return fmt.Errorf("ceph-rbd: state dir: %w", err)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("ceph-rbd: marshal device record: %w", err)
	}
	if err := os.WriteFile(b.deviceRecordPath(stagingPath), data, 0o600); err != nil {
		return fmt.Errorf("ceph-rbd: record device: %w", err)
	}
	return nil
}

// readDeviceRecord returns the record for a staging path (zero value if none).
// Records written before this format was JSON were the bare device path, so a
// non-JSON file is read back as a device with no unmap options.
func (b *Backend) readDeviceRecord(stagingPath string) deviceRecord {
	if b.stateDir == "" {
		return deviceRecord{}
	}
	data, err := os.ReadFile(b.deviceRecordPath(stagingPath))
	if err != nil {
		return deviceRecord{}
	}
	var rec deviceRecord
	if json.Unmarshal(data, &rec) == nil && rec.Device != "" {
		return rec
	}
	return deviceRecord{Device: strings.TrimSpace(string(data))}
}

// lookupDevice returns the device recorded for a staging path, or "".
func (b *Backend) lookupDevice(stagingPath string) string {
	return b.readDeviceRecord(stagingPath).Device
}

// clearDevice removes the record for a staging path (idempotent).
func (b *Backend) clearDevice(stagingPath string) {
	if b.stateDir != "" {
		_ = os.Remove(b.deviceRecordPath(stagingPath))
	}
}

// waitForDevice polls until the block device reports a non-zero size (rbd-nbd
// returns the path before the device is fully sized).
func (b *Backend) waitForDevice(ctx context.Context, dev string) error {
	deadline := time.Now().Add(20 * time.Second)
	for {
		out, _ := b.run.Run(ctx, "blockdev", "--getsize64", dev)
		if n, _ := strconv.ParseInt(strings.TrimSpace(out), 10, 64); n > 0 {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("ceph-rbd: wait for device %s: %w", dev, ctx.Err())
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ceph-rbd: device %s not ready (size 0) after timeout", dev)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func (b *Backend) NodeStage(ctx context.Context, req *bardplugin.NodeStageRequest) error {
	cc, err := b.cluster(req.Volume.Instance)
	if err != nil {
		return err
	}
	conn, cleanup, err := b.connArgs(cc, req.Volume.Instance, req.Secrets)
	if err != nil {
		return err
	}
	defer cleanup()

	spec := req.Volume.Location + "/" + req.Volume.Name
	// Idempotent: reuse the device already mapped for this staging path. A
	// retried NodeStage that maps again would orphan a second device whose rbd
	// watcher then blocks DeleteVolume.
	dev := b.lookupDevice(req.StagingPath)
	if dev == "" || !b.deviceMapped(ctx, dev) {
		// Single-writer multi-attach safety: this is a fresh map on this node (an
		// idempotent re-stage on the same node reused its recorded device above and
		// never reaches here). For an exclusive volume, any client still watching
		// the image is therefore a stale writer on a *previous* node -- e.g. a
		// partitioned/crashed node that never cleanly detached. Fence it at the OSD
		// level (blocklist) before we take the image over, so it cannot corrupt the
		// volume after the failover. This is what makes attachRequired=false safe
		// for ReadWriteOnce.
		if req.Exclusive {
			if err := b.fenceStaleWatchers(ctx, cc, conn, req.Volume.Instance, spec, req.Secrets); err != nil {
				return err
			}
		}
		cookie := nbdCookie(req.StagingPath)
		// Map the image, resolving read-affinity + user mapOptions for the mounter.
		// req.Readonly (set by core for read-only access modes) maps the image
		// read-only at the Ceph client. `mounter` is the one that actually mapped --
		// krbd, or rbd-nbd if krbd failed and tryOtherMounters opted into the fallback.
		mapped, mounter, mErr := b.mapWithFallback(ctx, cc, conn, req, spec, cookie)
		if mErr != nil {
			return mErr
		}
		dev = mapped
		if err := b.waitForDevice(ctx, dev); err != nil {
			return err
		}
		// Record it (with any unmap options) so NodeUnstage can unmap reliably even
		// if the staging mount -- and the volume context -- are gone by the time it
		// runs. The recorded mounter drives the unmap tool.
		unmapOpts := resolveMounterOptions(req.Context[paramUnmapOptions], mounter)
		rec := deviceRecord{
			Device:       dev,
			UnmapOptions: unmapOpts,
			Instance:     req.Volume.Instance,
			Pool:         req.Volume.Location,
			Image:        req.Volume.Name,
			Mounter:      mounter,
			Cookie:       cookie,
		}
		// An rbd-nbd map with cephLogDir set has a per-volume client log to manage;
		// persist its path + strategy so NodeUnstage can act without volume context.
		if mounter == mounterNBD {
			if lf := nbdLogFile(req.Context[paramCephLogDir], req.Volume); lf != "" {
				rec.LogFile = lf
				rec.LogStrategy = req.Context[paramCephLogStrategy]
			}
		}
		if err := b.recordDevice(req.StagingPath, rec); err != nil {
			return err
		}
	}

	// Layer LUKS over the mapped device for an encrypted volume (no-op
	// otherwise). The decrypted /dev/mapper device is what we format/mount for a
	// filesystem volume, or hand to NodePublish for a block volume.
	mountDev, err := b.stageDevice(ctx, conn, req, dev)
	if err != nil {
		return err
	}
	if req.Block {
		if cephenc.IsFsCrypt(req.Context) {
			// fscrypt is filesystem-level: there is no filesystem on a raw block volume
			// to host the encryption policy. Use block-mode (LUKS) encryption instead.
			return bardplugin.Errorf(bardplugin.CodeInvalidArg,
				"ceph-rbd: encryptionType=file (fscrypt) is not supported for raw block volumes; use the default block (LUKS) encryption")
		}
		return nil
	}

	fsType := req.FsType
	if fsType == "" {
		fsType = defaultFsType
	}
	if !supportedFsTypes[fsType] {
		return bardplugin.Errorf(bardplugin.CodeInvalidArg,
			"ceph-rbd: unsupported fsType %q (supported: ext4, ext3, ext2, xfs, btrfs)", fsType)
	}
	// fscrypt: format ext4 with the encrypt feature; the policy is applied to a
	// directory after the mount (setupFscrypt below). Only ext4 is supported.
	fsCrypt := cephenc.IsFsCrypt(req.Context)
	if fsCrypt && fsType != "ext4" {
		return bardplugin.Errorf(bardplugin.CodeInvalidArg,
			"ceph-rbd: encryptionType=file (fscrypt) requires fsType ext4, got %q", fsType)
	}
	mkfsArgs := mkfsArgsForFs(fsType, req.Context[paramMkfsOptions], fsCrypt)
	if err := b.ensureFormatted(ctx, mountDev, fsType, mkfsArgs...); err != nil {
		return err
	}
	if err := os.MkdirAll(req.StagingPath, 0o750); err != nil {
		return fmt.Errorf("ceph-rbd: mkdir staging: %w", err)
	}
	// Idempotent: skip the mount if the staging path is itself already a mount
	// (retry). Use --mountpoint, not --target: --target walks up to the containing
	// filesystem and so matches any path that merely resides on a mount.
	out, _ := b.run.Run(ctx, "findmnt", "-n", "-o", "SOURCE", "--mountpoint", req.StagingPath)
	if strings.TrimSpace(out) == "" {
		mountArgs := []string{"-t", fsType}
		flags := mountFlagsForFs(fsType, req.MountFlags)
		if len(flags) > 0 {
			mountArgs = append(mountArgs, "-o", strings.Join(flags, ","))
		}
		mountArgs = append(mountArgs, mountDev, req.StagingPath)
		if _, err := b.run.Run(ctx, "mount", mountArgs...); err != nil {
			return fmt.Errorf("ceph-rbd: mount %s -> %s: %w", mountDev, req.StagingPath, err)
		}
	}
	// A clone/restore into a LARGER volume carries its SOURCE's filesystem: the
	// image was grown to the request at create, but the filesystem inside still
	// has the source's size, so the pod would see less space than the PV claims.
	// Grow it to the device now that it is mounted (online for every supported
	// fs; a no-op when the sizes already match, i.e. every non-clone stage).
	if err := b.growFilesystem(ctx, fsType, mountDev, req.StagingPath); err != nil {
		return err
	}
	// fscrypt: with the filesystem mounted, add the master key to its keyring and
	// ensure the encrypted data directory + policy exist. Idempotent, so it also
	// re-adds the key on a restage (where the mount above was skipped) -- the key
	// lives in the filesystem keyring, dropped on unmount, so every stage re-adds it.
	if cephenc.IsFsCrypt(req.Context) {
		pass, err := b.volumePassphrase(ctx, conn, req)
		if err != nil {
			return err
		}
		if err := cephenc.SetupFscrypt(req.StagingPath, pass); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) NodeUnstage(ctx context.Context, req *bardplugin.NodeUnstageRequest) error {
	// Validate the instance, but unmap is driven by the recorded device + mounter
	// (independent of the live cluster config), so cc itself is not needed here.
	if _, err := b.cluster(req.Volume.Instance); err != nil {
		return err
	}
	// Prefer the device recorded at stage time: it is independent of the staging
	// mount, so unmap still works on a retry where the unmount already happened.
	// Fall back to findmnt for volumes staged before this record existed.
	rec := b.readDeviceRecord(req.StagingPath)
	dev := rec.Device
	if dev == "" {
		out, _ := b.run.Run(ctx, "findmnt", "-n", "-o", "SOURCE", "--target", req.StagingPath)
		dev = strings.TrimSpace(out)
	}
	if _, err := b.run.Run(ctx, "umount", req.StagingPath); err != nil && !isNotMounted(err) {
		return fmt.Errorf("ceph-rbd: umount %s: %w", req.StagingPath, err)
	}
	// Close the LUKS mapper before unmapping the underlying device. Deterministic
	// name from the staging path; a no-op when the volume was not encrypted (the
	// mapper was never opened), so NodeUnstage needs no encryption flag.
	if err := b.closeLuks(ctx, luksMapperName(req.StagingPath)); err != nil {
		return err
	}
	if dev != "" {
		// Trust the device state, not the exit code: some rbd-nbd builds return
		// non-zero even after freeing the device. If it is still mapped after the
		// unmap, return an error (keep the record) so kubelet retries -- never
		// report success while the device (and its rbd watcher) leaks.
		unmapArgs := []string{"unmap", dev}
		if rec.UnmapOptions != "" {
			unmapArgs = append(unmapArgs, "--options", rec.UnmapOptions)
		}
		_, _ = b.run.Run(ctx, unmapBinary(rec.Mounter), unmapArgs...)
		if b.deviceMapped(ctx, dev) {
			return fmt.Errorf("ceph-rbd: device %s still mapped after unmap", dev)
		}
	}
	// Device confirmed free: apply the volume's log strategy to its rbd-nbd log.
	b.disposeNbdLog(ctx, rec)
	b.clearDevice(req.StagingPath)
	return nil
}

// deviceMapped reports whether dev still backs a block device (non-zero size).
// A freed rbd/rbd-nbd device reports size 0 or no longer exists.
func (b *Backend) deviceMapped(ctx context.Context, dev string) bool {
	out, err := b.run.Run(ctx, "blockdev", "--getsize64", dev)
	if err != nil {
		return false
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	return n > 0
}

// fenceStaleWatchers blocklists every client currently watching spec, so a stale
// writer on a previous node cannot reach the OSDs after this node takes the
// exclusive volume over. `osd blocklist add` fences at the OSD level, which (un-
// like the cooperative rbd exclusive-lock feature) a partitioned node cannot
// ignore -- that is the property single-writer safety needs. Entries auto-expire
// (Ceph default 1h), so no un-blocklist is required when the old node is gone;
// the scoped cephx user's `profile rbd` grants blocklist-add but not -rm anyway.
//
// Only called on a fresh map of an exclusive volume, so any watcher seen is
// foreign by construction (the same node's idempotent re-stage reuses its
// recorded device and never reaches this path).
func (b *Backend) fenceStaleWatchers(ctx context.Context, cc ClusterConfig, conn []string, instance, spec string, secrets map[string]string) error {
	out, err := b.run.Run(ctx, "rbd", appendArgs(conn, "status", spec, "--format", "json")...)
	if err != nil {
		return fmt.Errorf("ceph-rbd: rbd status %s: %w", spec, err)
	}
	var st struct {
		Watchers []struct {
			Address string `json:"address"`
		} `json:"watchers"`
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		return fmt.Errorf("ceph-rbd: parse rbd status %s: %w", spec, err)
	}
	if len(st.Watchers) == 0 {
		return nil
	}
	// Blocklisting goes through the Python `ceph` CLI (needs a conf), unlike rbd.
	// The request secrets ride along so a CSI-secret-only deployment can fence.
	cephConn, cleanup, err := b.cephCLIConn(cc, instance, secrets)
	if err != nil {
		return err
	}
	defer cleanup()
	for _, w := range st.Watchers {
		if w.Address == "" {
			continue
		}
		// Fail the stage if a watcher cannot be fenced: better to retry than to
		// take the volume over while a previous writer is still un-fenced.
		if _, err := b.run.Run(ctx, "ceph", appendArgs(cephConn, "osd", "blocklist", "add", w.Address)...); err != nil {
			return fmt.Errorf("ceph-rbd: blocklist stale watcher %s on %s: %w", w.Address, spec, err)
		}
	}
	return nil
}

func (b *Backend) NodePublish(ctx context.Context, req *bardplugin.NodePublishRequest) error {
	if req.Block {
		return b.publishBlock(ctx, req)
	}
	if err := os.MkdirAll(req.TargetPath, 0o750); err != nil {
		return fmt.Errorf("ceph-rbd: mkdir target: %w", err)
	}
	if out, _ := b.run.Run(ctx, "findmnt", "-n", "-o", "SOURCE", "--mountpoint", req.TargetPath); strings.TrimSpace(out) != "" {
		return nil // idempotent: already published on a retry
	}
	// For fscrypt the pod's volume root is the encrypted data directory on the
	// staging mount, not the mount itself (whose root holds lost+found and the
	// unencrypted filesystem metadata).
	source := req.StagingPath
	if cephenc.IsFsCrypt(req.Context) {
		source = cephenc.FscryptDataDir(req.StagingPath)
	}
	if _, err := b.run.Run(ctx, "mount", "--bind", source, req.TargetPath); err != nil {
		return fmt.Errorf("ceph-rbd: bind mount %s -> %s: %w", source, req.TargetPath, err)
	}
	if req.Readonly {
		if _, err := b.run.Run(ctx, "mount", "-o", "remount,ro,bind", source, req.TargetPath); err != nil {
			return fmt.Errorf("ceph-rbd: remount ro %s: %w", req.TargetPath, err)
		}
	}
	return nil
}

// publishBlock exposes a raw block volume by bind-mounting the mapped device
// (recorded at NodeStage) onto the target, which for block is a device file the
// node must create first.
func (b *Backend) publishBlock(ctx context.Context, req *bardplugin.NodePublishRequest) error {
	dev := b.lookupDevice(req.StagingPath)
	if dev == "" {
		return fmt.Errorf("ceph-rbd: no mapped device recorded for staging %q", req.StagingPath)
	}
	// For an encrypted block volume the pod must see the decrypted mapper (opened
	// at NodeStage), not the ciphertext device.
	if isEncrypted(req.Context) {
		dev = "/dev/mapper/" + luksMapperName(req.StagingPath)
	}
	if err := os.MkdirAll(filepath.Dir(req.TargetPath), 0o750); err != nil {
		return fmt.Errorf("ceph-rbd: mkdir target dir: %w", err)
	}
	f, err := os.OpenFile(req.TargetPath, os.O_CREATE, 0o660)
	if err != nil {
		return fmt.Errorf("ceph-rbd: create block target: %w", err)
	}
	f.Close()
	// No read-only handling here: for a ReadOnlyMany volume the image is mapped
	// read-only at NodeStage (see mapImage), which protects it at the Ceph client.
	// A read-only bind mount is a no-op for a block device anyway (it only
	// constrains filesystem mounts), and the consumer-level readOnly of a writable
	// block volume is enforced by the kubelet device cgroup.
	if _, err := b.run.Run(ctx, "mount", "--bind", dev, req.TargetPath); err != nil {
		return fmt.Errorf("ceph-rbd: bind device %s -> %s: %w", dev, req.TargetPath, err)
	}
	return nil
}

func (b *Backend) NodeUnpublish(ctx context.Context, req *bardplugin.NodeUnpublishRequest) error {
	if _, err := b.run.Run(ctx, "umount", req.TargetPath); err != nil && !isNotMounted(err) {
		return fmt.Errorf("ceph-rbd: umount %s: %w", req.TargetPath, err)
	}
	return nil
}

func (b *Backend) NodeExpand(ctx context.Context, req *bardplugin.NodeExpandRequest) (*bardplugin.NodeExpandResponse, error) {
	src, err := b.run.Run(ctx, "findmnt", "-n", "-o", "SOURCE", "--target", req.VolumePath)
	if err != nil {
		return nil, fmt.Errorf("ceph-rbd: resolve device for %s: %w", req.VolumePath, err)
	}
	// findmnt reports a bind-mounted subdirectory as `<device>[/subpath]` -- which an
	// fscrypt volume always is, since the pod is published the encrypted subdir.
	// Strip the subpath to the bare block device for the filesystem grow.
	dev := strings.TrimSpace(src)
	if i := strings.IndexByte(dev, '['); i >= 0 {
		dev = dev[:i]
	}
	// LUKS (block encryption): the underlying rbd device has already grown, but the
	// dm-crypt mapping on top has not -- grow it first or resize2fs only sees the old
	// size. cryptsetup needs the volume key to resize; it cannot be relied on to find
	// it in the kernel keyring across plugin invocations in a container, so we supply
	// the passphrase via a key file, re-resolved through the KMS the same way
	// DeleteVolume does (the KMS id + key id are recorded on the image). fscrypt and
	// plaintext volumes have no mapper, so this is skipped.
	if name := strings.TrimPrefix(dev, "/dev/mapper/"); name != dev && strings.HasPrefix(name, luksMapperPrefix) {
		if err := b.resizeLuks(ctx, req.Volume, name); err != nil {
			return nil, err
		}
	}
	fsType, _ := b.run.Run(ctx, "findmnt", "-n", "-o", "FSTYPE", "--target", req.VolumePath)
	if err := b.growFilesystem(ctx, strings.TrimSpace(fsType), dev, req.VolumePath); err != nil {
		return nil, err
	}
	return &bardplugin.NodeExpandResponse{}, nil
}

// growFilesystem grows a mounted filesystem to its backing device's size --
// online for every supported filesystem, and a no-op when they already match.
// Two callers: NodeExpand (the device grew under a live mount) and NodeStage (a
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
	default: // ext2/3/4 (incl. fscrypt, which is ext4) -- online resize the device.
		_, err = b.run.Run(ctx, "resize2fs", dev)
	}
	if err != nil {
		return fmt.Errorf("ceph-rbd: grow %s filesystem on %s: %w", fsType, dev, err)
	}
	return nil
}

// NodeReclaimSpace implements bardplugin.NodeSpaceReclaimer: it runs `fstrim` on
// the mounted filesystem so discards reach the rbd image and Ceph reclaims the
// freed blocks (the csi-addons node, "online", ReclaimSpace operation). This is
// the node-side complement to ReclaimSpace's controller-side `rbd sparsify`. A
// raw block volume has no filesystem to trim, so it is a no-op. fstrim reports
// only the trimmed amount, not absolute usage, so the pre/post usage is left
// unknown (-1) and Bard omits it.
func (b *Backend) NodeReclaimSpace(ctx context.Context, req *bardplugin.NodeReclaimSpaceRequest) (*bardplugin.ReclaimSpaceResponse, error) {
	unknown := &bardplugin.ReclaimSpaceResponse{PreUsageBytes: -1, PostUsageBytes: -1}
	if req.Block {
		return unknown, nil // no filesystem on a raw block device
	}
	path := req.VolumePath
	if path == "" {
		path = req.StagingPath
	}
	if path == "" {
		return nil, bardplugin.Errorf(bardplugin.CodeInvalidArg, "ceph-rbd: node reclaim space requires a volume path")
	}
	if _, err := b.run.Run(ctx, "fstrim", path); err != nil {
		return nil, fmt.Errorf("ceph-rbd: fstrim %s: %w", path, err)
	}
	return unknown, nil
}

func (b *Backend) ensureFormatted(ctx context.Context, dev, fsType string, mkfsArgs ...string) error {
	out, _ := b.run.Run(ctx, "blkid", "-o", "value", "-s", "TYPE", dev)
	if strings.TrimSpace(out) != "" {
		return nil
	}
	args := append(append([]string{}, mkfsArgs...), dev)
	if _, err := b.run.Run(ctx, "mkfs."+fsType, args...); err != nil {
		return fmt.Errorf("ceph-rbd: mkfs.%s %s: %w", fsType, dev, err)
	}
	return nil
}

func appendArgs(base []string, extra ...string) []string {
	out := make([]string, 0, len(base)+len(extra))
	out = append(out, base...)
	out = append(out, extra...)
	return out
}
