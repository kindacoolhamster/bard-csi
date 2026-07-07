// Package cephfsplugin is the CephFS backend as an out-of-tree Bard plugin. Like
// the Ceph RBD plugin it depends only on the public bardplugin SDK, but it is a
// filesystem backend: a volume is a CephFS *subvolume* (a quota'd directory
// tree), provisioned with `ceph fs subvolume` and mounted directly -- no block
// device, no format, no device mapping. CephFS supports ReadWriteMany.
package cephfsplugin

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
	"sync"
	"time"

	"github.com/kindacoolhamster/bard-csi/internal/cephenc"
	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

const (
	defaultUserID = "admin"
	secretUserID  = "userID"
	secretUserKey = "userKey"

	// Mounters: "kernel" (default, `mount -t ceph`) is preferred in production;
	// "fuse" (ceph-fuse, userspace) works where the ceph kernel client is
	// unavailable (e.g. nested-container nodes); "nfs" exports the subvolume
	// through a Ceph-managed NFS-Ganesha gateway and mounts it over NFS, so nodes
	// need no ceph client at all. The volume is still a CephFS subvolume, so
	// snapshot/clone/expand/health are identical -- this is the ceph-csi NFS shape
	// (a CephFS subvolume behind Ganesha), which is why those features carry over.
	mounterFuse = "fuse"
	mounterNFS  = "nfs"

	// ctxPath is the volume-context key carrying the subvolume's mount path from
	// CreateVolume (controller) to NodeStage (node), so the node needs no mgr.
	ctxPath = "path"

	// paramBackingSnapshot, when "true" on a snapshot-restore PVC, makes a shallow
	// read-only volume: instead of cloning the snapshot into a new subvolume, the
	// node mounts the snapshot's `.snap/<snap>` directory directly (read-only,
	// instant, zero-copy). Intended for ReadOnlyMany. Mirrors ceph-csi.
	paramBackingSnapshot = "backingSnapshot"
	// paramSubvolumeGroup overrides, per StorageClass, the CephFS subvolume group new
	// volumes are created in (default "csi"). ceph-csi parity.
	paramSubvolumeGroup = "subvolumeGroup"
	// paramVolumeNamePrefix / paramSnapshotNamePrefix override the default "bard-" /
	// "snap-" subvolume/snapshot name prefixes (StorageClass / VolumeSnapshotClass
	// parameters). The name is recorded in the handle, so the whole lifecycle
	// (delete, clone, restore) follows a custom prefix. ceph-csi parity (its cephfs
	// defaults are csi-vol-/csi-snap-).
	paramVolumeNamePrefix   = "volumeNamePrefix"
	paramSnapshotNamePrefix = "snapshotNamePrefix"
	// subvolNamePrefix / snapNamePrefix are the defaults for those parameters;
	// listings recognise Bard objects by the shortName hash shape (any prefix +
	// a 16-hex suffix), so custom prefixes stay listable.
	subvolNamePrefix = "bard-"
	snapNamePrefix   = "snap-"
	// tmpClonePrefix names the transient source snapshot a subvolume-to-subvolume
	// clone rides on (see cloneFromVolume). Listings skip it.
	tmpClonePrefix = "clonetmp-"
	// shallowPrefix marks a shallow volume's handle Name so DeleteVolume knows it
	// owns no subvolume (the snapshot belongs to its VolumeSnapshot, not the PVC).
	shallowPrefix = "shallow-"
	// ctxNFSServer / ctxNFSPseudo carry the Ganesha endpoint and the export's
	// pseudo path to the node for a mounter:nfs volume.
	ctxNFSServer = "nfsServer"
	ctxNFSPseudo = "nfsPseudo"
)

// ClusterConfig is the per-instance CephFS connection config. The cephx key is
// resolved per-request (keyDir or CSI secret), never stored here.
type ClusterConfig struct {
	Monitors []string `json:"monitors"` // mon endpoints host:port
	FSName   string   `json:"fsName"`   // CephFS filesystem name
	UserID   string   `json:"userID"`   // cephx user id
	Mounter  string   `json:"mounter"`  // "kernel" (default), "fuse", or "nfs"
	// NFSCluster / NFSServer are required only for mounter "nfs": the id of the
	// Ceph-managed NFS cluster (Ganesha) to export through, and the gateway
	// endpoint (host or host:port) that nodes mount over NFS.
	NFSCluster string `json:"nfsCluster,omitempty"`
	NFSServer  string `json:"nfsServer,omitempty"`
	// SubvolumeGroup is the CephFS subvolume group new volumes are created in. Empty
	// means the ceph-csi-compatible default "csi" (a StorageClass subvolumeGroup
	// param overrides per-volume). The group is encoded in the volume Location so the
	// whole lifecycle addresses the subvolume in its group.
	SubvolumeGroup string `json:"subvolumeGroup,omitempty"`
}

// defaultSubvolumeGroup matches ceph-csi: new subvolumes go in group "csi" (not the
// cluster default _nogroup), which is also where existing ceph-csi subvolumes live --
// so a migrated StorageClass adopts them.
const defaultSubvolumeGroup = "csi"

// subvolumeGroupOf returns the subvolume group encoded in a volume Location
// ("<fs>/<group>"), or "" (the cluster _nogroup default) for a bare "<fs>" Location.
// Handles written before group support carry no group and so resolve to _nogroup.
func subvolumeGroupOf(location string) string {
	if i := strings.IndexByte(location, '/'); i >= 0 {
		return location[i+1:]
	}
	return ""
}

// cephfsLocation encodes a filesystem + subvolume group into a volume Location.
func cephfsLocation(fs, group string) string {
	if group == "" {
		return fs
	}
	return fs + "/" + group
}

// withGroup appends --group-name to `ceph fs subvolume` args when a group is set
// (empty means the cluster default _nogroup, which takes no flag).
func withGroup(args []string, group string) []string {
	if group == "" {
		return args
	}
	return append(args, "--group-name", group)
}

// resolveSubvolumeGroup picks the group for a NEW volume: the StorageClass param,
// else the instance config, else the ceph-csi-compatible default.
func resolveSubvolumeGroup(params map[string]string, cc ClusterConfig) string {
	if g := params[paramSubvolumeGroup]; g != "" {
		return g
	}
	if cc.SubvolumeGroup != "" {
		return cc.SubvolumeGroup
	}
	return defaultSubvolumeGroup
}

// ensureSubvolumeGroup idempotently creates a subvolume group (no-op for the default
// _nogroup). ceph-csi does the same before creating a subvolume in a named group.
func (b *Backend) ensureSubvolumeGroup(ctx context.Context, conn []string, fs, group string) error {
	if group == "" {
		return nil
	}
	if _, err := b.run.Run(ctx, "ceph", append(append([]string{}, conn...), "fs", "subvolumegroup", "create", fs, group)...); err != nil {
		return fmt.Errorf("cephfs: subvolumegroup create %s/%s: %w", fs, group, err)
	}
	return nil
}

// metaSpec encodes fs + subvolume group + subvolume into the spec the cephenc KMS
// Host (MetaGet/MetaSet) parses, so metadata commands address the subvolume in its
// group: "<fs>/<group>/<sub>", or "<fs>/<sub>" for the default _nogroup.
func metaSpec(fs, group, sub string) string {
	if group == "" {
		return fs + "/" + sub
	}
	return fs + "/" + group + "/" + sub
}

// Backend implements bardplugin.Backend for CephFS.
type Backend struct {
	clusters  map[string]ClusterConfig
	keyDir    string
	encKeyDir string            // dir of per-instance master keys ("" disables encryption)
	kms       *cephenc.Registry // KMS providers, bound to this backend as cephenc.Host
	run       Runner

	mu        sync.Mutex
	snapIndex map[string]string // CSI snapshot name -> source volume key
	// snapNames reverses snapIndex by snapshot handle, so DeleteSnapshot -- which
	// sees only the handle -- can release the CSI name for reuse (see ceph-rbd).
	snapNames map[string]string // snapshot handle key -> CSI snapshot name
}

// New builds the CephFS plugin backend. keyDir holds per-instance cephx key
// files ("" => rely on CSI secrets, tests).
func New(clusters map[string]ClusterConfig, keyDir string, run Runner) *Backend {
	if run == nil {
		run = ExecRunner{}
	}
	b := &Backend{clusters: clusters, keyDir: keyDir, run: run, snapIndex: map[string]string{}, snapNames: map[string]string{}}
	// The KMS registry resolves an encrypted volume's fscrypt passphrase through
	// pluggable providers; it reads the master key dir, a Ceph connection, and
	// subvolume metadata back from this backend (which implements cephenc.Host).
	b.kms = cephenc.NewRegistry(b, nil)
	return b
}

func (b *Backend) Info() bardplugin.Info {
	return bardplugin.Info{
		Type: "cephfs",
		Capabilities: bardplugin.Capabilities{
			BlockDevice: false, // a filesystem mount, not a block device
			Snapshots:   true,  // subvolume snapshots + clone-from-snapshot
			Expand:      true,  // online quota resize, no node action
		},
	}
}

func subvolName(prefix, csiName string) string {
	sum := sha256.Sum256([]byte(csiName))
	return prefix + hex.EncodeToString(sum[:8])
}

func snapName(prefix, csiName string) string {
	sum := sha256.Sum256([]byte(csiName))
	return prefix + hex.EncodeToString(sum[:8])
}

// namePrefix resolves a name-prefix parameter (volumeNamePrefix /
// snapshotNamePrefix), defaulting when unset. The prefix becomes part of a
// subvolume/snapshot name (and thus a handle), so it must be a valid name
// fragment: no '/' (path separator), no '@' (the subvolume@snapshot separator
// handles encode), no whitespace.
func namePrefix(param, value, def string) (string, error) {
	if value == "" {
		return def, nil
	}
	if strings.ContainsAny(value, "/@ \t\n") {
		return "", bardplugin.Errorf(bardplugin.CodeInvalidArg, "cephfs: invalid %s %q (no '/', '@', or whitespace)", param, value)
	}
	return value, nil
}

// nameHashLen is the hex length of the shortName hash (8 bytes -> 16 chars).
const nameHashLen = 16

// isBardObjectName reports whether a subvolume/snapshot name was minted by
// subvolName/snapName (any prefix + a 16-hex-char hash), so listings recognise
// custom-prefix objects as Bard-managed, not just the default prefixes.
func isBardObjectName(name string) bool {
	if len(name) <= nameHashLen {
		return false
	}
	for _, c := range name[len(name)-nameHashLen:] {
		if c < '0' || (c > '9' && c < 'a') || c > 'f' {
			return false
		}
	}
	return true
}

// nfsPseudoPath is the deterministic NFS-Ganesha pseudo path for a subvolume, so
// DeleteVolume can reconstruct it (the CSI DeleteVolume carries no context) and
// the node can mount it. The subvolume name is already unique per instance.
func nfsPseudoPath(sub string) string { return "/" + sub }

// createNFSExport exports a subvolume's path through the instance's Ganesha
// cluster so it can be mounted over NFS. Idempotent: re-exporting the same path
// is tolerated on a retried CreateVolume.
func (b *Backend) createNFSExport(ctx context.Context, conn []string, cc ClusterConfig, subPath, pseudo string) error {
	if cc.NFSCluster == "" || cc.NFSServer == "" {
		return bardplugin.Errorf(bardplugin.CodeInvalidArg, "cephfs: mounter %q requires nfsCluster and nfsServer in instance config", mounterNFS)
	}
	args := append(append([]string{}, conn...), "nfs", "export", "create", "cephfs",
		"--cluster-id", cc.NFSCluster, "--pseudo-path", pseudo, "--fsname", cc.FSName, "--path", subPath)
	if _, err := b.run.Run(ctx, "ceph", args...); err != nil && !isAlreadyExists(err) {
		return fmt.Errorf("cephfs: nfs export create %s %s: %w", cc.NFSCluster, pseudo, err)
	}
	return nil
}

// removeNFSExport deletes a subvolume's Ganesha export. Idempotent: a missing
// export is fine (the export may already be gone on a retried delete).
func (b *Backend) removeNFSExport(ctx context.Context, conn []string, cc ClusterConfig, pseudo string) error {
	args := append(append([]string{}, conn...), "nfs", "export", "rm", cc.NFSCluster, pseudo)
	if _, err := b.run.Run(ctx, "ceph", args...); err != nil && !isNotFound(err) {
		return fmt.Errorf("cephfs: nfs export rm %s %s: %w", cc.NFSCluster, pseudo, err)
	}
	return nil
}

// ownerMeta maps the provisioner's --extra-create-metadata parameter keys to the
// subvolume-metadata keys recording the owning PVC, for operability. Keys are
// lowercase because Ceph lowercases subvolume-metadata keys -- so what we set
// matches what `ceph fs subvolume metadata get/ls` returns (and matches ceph-rbd).
var ownerMeta = map[string]string{
	"csi.storage.k8s.io/pvc/name":      "bard.pvcname",
	"csi.storage.k8s.io/pvc/namespace": "bard.pvcnamespace",
	"csi.storage.k8s.io/pv/name":       "bard.pvname",
}

// setOwnerMetadata records the owning PVC/PV on the subvolume (best-effort: a
// cosmetic label must not fail the provision; subvolume metadata needs a recent Ceph).
func (b *Backend) setOwnerMetadata(ctx context.Context, conn []string, fs, group, sub string, params map[string]string) {
	for src, key := range ownerMeta {
		if v := params[src]; v != "" {
			_, _ = b.run.Run(ctx, "ceph", withGroup(append(append([]string{}, conn...), "fs", "subvolume", "metadata", "set", fs, sub, key, v), group)...)
		}
	}
}

func refKey(r bardplugin.VolumeRef) string {
	return r.Instance + "|" + r.Location + "|" + r.Name
}

// A snapshot handle's Name encodes "subvolume@snapshot"; split reverses it.
func splitSnap(name string) (subvol, snap string, ok bool) {
	i := strings.LastIndex(name, "@")
	if i < 0 {
		return "", "", false
	}
	return name[:i], name[i+1:], true
}

func (b *Backend) cluster(instance string) (ClusterConfig, error) {
	cc, ok := b.clusters[instance]
	if !ok {
		return ClusterConfig{}, bardplugin.Errorf(bardplugin.CodeInvalidArg, "cephfs: no cluster configured for instance %q", instance)
	}
	return cc, nil
}

func (b *Backend) keyFor(instance string, secrets map[string]string) (string, error) {
	if b.keyDir != "" {
		data, err := os.ReadFile(filepath.Join(b.keyDir, instance))
		switch {
		case err == nil:
			return strings.TrimSpace(string(data)), nil
		case !os.IsNotExist(err):
			return "", fmt.Errorf("cephfs: read key for instance %q: %w", instance, err)
		}
	}
	if k := secrets[secretUserKey]; k != "" {
		return k, nil
	}
	if b.keyDir != "" {
		return "", fmt.Errorf("cephfs: no cephx key for instance %q", instance)
	}
	return "", nil
}

func (b *Backend) userID(cc ClusterConfig, secrets map[string]string) string {
	if u := secrets[secretUserID]; u != "" {
		return u
	}
	if cc.UserID != "" {
		return cc.UserID
	}
	return defaultUserID
}

// keyFile writes the cephx key to a temp file, returning its path and cleanup.
func (b *Backend) keyFile(instance string, secrets map[string]string) (string, func(), error) {
	key, err := b.keyFor(instance, secrets)
	if err != nil {
		return "", func() {}, err
	}
	if key == "" {
		return "", func() {}, nil
	}
	f, err := cephenc.SecretTemp("csi-cephfs-key-")
	if err != nil {
		return "", func() {}, fmt.Errorf("cephfs: keyfile: %w", err)
	}
	if _, err := f.WriteString(key); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", func() {}, fmt.Errorf("cephfs: keyfile: %w", err)
	}
	f.Close()
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}

// cephConn returns the `ceph` CLI connection flags for control-plane ops, plus a
// cleanup. The Python ceph CLI needs a conf file to initialise librados (unlike
// the C++ rbd/ceph-fuse tools, which accept -m alone), so we write a minimal
// conf with mon_host.
func (b *Backend) cephConn(cc ClusterConfig, instance string, secrets map[string]string) ([]string, func(), error) {
	var files []string
	cleanup := func() {
		for _, f := range files {
			os.Remove(f)
		}
	}
	conf, err := os.CreateTemp("", "csi-cephfs-conf-")
	if err != nil {
		return nil, cleanup, fmt.Errorf("cephfs: conf: %w", err)
	}
	fmt.Fprintf(conf, "[global]\nmon_host = %s\n", strings.Join(cc.Monitors, ","))
	conf.Close()
	files = append(files, conf.Name())

	args := []string{"--conf", conf.Name(), "--id", b.userID(cc, secrets)}

	key, err := b.keyFor(instance, secrets)
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	if key != "" {
		kf, err := cephenc.SecretTemp("csi-cephfs-key-")
		if err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("cephfs: keyfile: %w", err)
		}
		kf.WriteString(key)
		kf.Close()
		files = append(files, kf.Name())
		args = append(args, "--keyfile", kf.Name())
	}
	return args, cleanup, nil
}

// ---- control plane -------------------------------------------------------

func (b *Backend) CreateVolume(ctx context.Context, req *bardplugin.CreateVolumeRequest) (*bardplugin.CreateVolumeResponse, error) {
	cc, err := b.cluster(req.Instance)
	if err != nil {
		return nil, err
	}
	conn, cleanup, err := b.cephConn(cc, req.Instance, req.Secrets)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	// Reject encryption configurations CephFS cannot honour (block mode, fuse/nfs
	// mounter, shallow volumes, restore-from-snapshot/clone) before doing any work.
	isClone := req.SourceSnapshot != nil || req.SourceVolume != nil
	if err := b.validateEncryptionParams(cc, req.Parameters, isClone); err != nil {
		return nil, err
	}
	// A volume may be created with a VolumeAttributesClass already set (MDS
	// pinning); reject an unsupported mutable parameter up front, as CSI requires.
	if err := validateMutableParams(req.MutableParams); err != nil {
		return nil, err
	}

	// Shallow read-only volume: mount the snapshot directory directly instead of
	// cloning it. Only valid when restoring from a snapshot.
	if req.SourceSnapshot != nil && req.Parameters[paramBackingSnapshot] == "true" {
		return b.createShallowVolume(ctx, conn, cc, req)
	}

	prefix, err := namePrefix(paramVolumeNamePrefix, req.Parameters[paramVolumeNamePrefix], subvolNamePrefix)
	if err != nil {
		return nil, err
	}
	sub := subvolName(prefix, req.Name)
	// The subvolume group (default "csi") rides in the volume Location, so the whole
	// lifecycle addresses the subvolume in its group. Ensure it exists first.
	group := resolveSubvolumeGroup(req.Parameters, cc)
	if err := b.ensureSubvolumeGroup(ctx, conn, cc.FSName, group); err != nil {
		return nil, err
	}
	switch {
	case req.SourceSnapshot != nil:
		// Restore: clone the snapshot into the new subvolume (creates it).
		if err := b.cloneFromSnapshot(ctx, conn, cc.FSName, group, req.SourceSnapshot, sub); err != nil {
			return nil, err
		}
	case req.SourceVolume != nil:
		// Volume clone (PVC dataSource): CephFS has no direct subvolume-to-
		// subvolume copy, so snapshot the source, clone that, then drop the snap.
		if err := b.cloneFromVolume(ctx, conn, cc.FSName, group, req.SourceVolume, sub); err != nil {
			return nil, err
		}
	default:
		// `ceph fs subvolume create` is idempotent; --size sets the quota.
		args := append(append([]string{}, conn...), "fs", "subvolume", "create", cc.FSName, sub)
		if req.CapacityBytes > 0 {
			args = append(args, "--size", strconv.FormatInt(req.CapacityBytes, 10))
		}
		if _, err := b.run.Run(ctx, "ceph", withGroup(args, group)...); err != nil {
			return nil, fmt.Errorf("cephfs: subvolume create %s/%s: %w", cc.FSName, sub, err)
		}
	}
	// A cloned subvolume inherits its SOURCE's quota; grow it to the request so
	// the pod gets the space the PV claims. --no_shrink: a restore into a request
	// smaller than the source keeps the larger quota (never shrink below used
	// bytes). Idempotent; a no-op when they already match.
	if isClone && req.CapacityBytes > 0 {
		args := append(append([]string{}, conn...), "fs", "subvolume", "resize", cc.FSName, sub, strconv.FormatInt(req.CapacityBytes, 10), "--no_shrink")
		if _, err := b.run.Run(ctx, "ceph", withGroup(args, group)...); err != nil {
			return nil, fmt.Errorf("cephfs: resize cloned subvolume %s/%s: %w", cc.FSName, sub, err)
		}
	}
	path, err := b.run.Run(ctx, "ceph", withGroup(append(append([]string{}, conn...), "fs", "subvolume", "getpath", cc.FSName, sub), group)...)
	if err != nil {
		return nil, fmt.Errorf("cephfs: getpath %s/%s: %w", cc.FSName, sub, err)
	}
	subPath := strings.TrimSpace(path)
	// Label the subvolume with its owning PVC for operability (ceph fs subvolume
	// metadata ls), from the provisioner's --extra-create-metadata. Best-effort.
	b.setOwnerMetadata(ctx, conn, cc.FSName, group, sub, req.Parameters)

	// Apply a create-time VolumeAttributesClass (MDS pinning), validated up front.
	if err := b.applyPin(ctx, conn, cc, group, sub, req.MutableParams); err != nil {
		return nil, err
	}

	// Encryption: record the KMS descriptor on the subvolume and carry the decision to
	// the node, which applies fscrypt after mounting. (Encrypted clones are rejected
	// above, so this is always a fresh volume.) The spec carries the group so the
	// metadata commands address the subvolume in its group.
	if err := b.recordEncryption(ctx, conn, req, metaSpec(cc.FSName, group, sub)); err != nil {
		return nil, err
	}

	volCtx := map[string]string{ctxPath: subPath}
	for k, v := range encryptionVolumeContext(req.Parameters) {
		volCtx[k] = v
	}

	// mounter:nfs -- additionally export the subvolume through Ganesha and carry
	// the gateway endpoint + pseudo path to the node, which mounts it over NFS.
	if cc.Mounter == mounterNFS {
		pseudo := nfsPseudoPath(sub)
		if err := b.createNFSExport(ctx, conn, cc, subPath, pseudo); err != nil {
			return nil, err
		}
		volCtx[ctxNFSServer] = cc.NFSServer
		volCtx[ctxNFSPseudo] = pseudo
	}
	return &bardplugin.CreateVolumeResponse{
		Location:      cephfsLocation(cc.FSName, group),
		Name:          sub,
		CapacityBytes: req.CapacityBytes,
		Context:       volCtx,
	}, nil
}

func (b *Backend) DeleteVolume(ctx context.Context, req *bardplugin.DeleteVolumeRequest) error {
	// A shallow (snapshot-backed) volume owns no subvolume: it only referenced a
	// snapshot, which belongs to its VolumeSnapshot. Deleting it is a no-op --
	// removing the source subvolume or snapshot here would corrupt other users.
	if strings.HasPrefix(req.Volume.Name, shallowPrefix) {
		return nil
	}
	cc, err := b.cluster(req.Volume.Instance)
	if err != nil {
		return err
	}
	conn, cleanup, err := b.cephConn(cc, req.Volume.Instance, req.Secrets)
	if err != nil {
		return err
	}
	defer cleanup()
	group := subvolumeGroupOf(req.Volume.Location)
	// For an NFS-exported instance, drop the Ganesha export first (idempotent, and
	// derived from the subvolume name since DeleteVolume carries no context), then
	// remove the subvolume. Order matters: a leftover export with no subvolume is
	// worse than a retry of the export rm.
	if cc.Mounter == mounterNFS {
		if err := b.removeNFSExport(ctx, conn, cc, nfsPseudoPath(req.Volume.Name)); err != nil {
			return err
		}
	}
	// Clean up any KMS-stored key material before removing the subvolume. Done first so
	// a failure leaves the subvolume + its recorded KMS id for a retry (both idempotent).
	if err := b.deleteEncryptionKey(ctx, conn, req.Volume.Instance, metaSpec(cc.FSName, group, req.Volume.Name)); err != nil {
		return err
	}
	// Reap any clonetmp- snapshots left on this subvolume: the transient vehicle
	// of a PVC-PVC clone whose create was abandoned mid-recipe (never a
	// VolumeSnapshot's -- those use the snapshotNamePrefix). A subvolume with
	// snapshots cannot be removed, so a leftover would wedge this delete forever.
	b.reapTmpCloneSnaps(ctx, conn, cc.FSName, group, req.Volume.Name)
	args := append(append([]string{}, conn...), "fs", "subvolume", "rm", cc.FSName, req.Volume.Name)
	if _, err := b.run.Run(ctx, "ceph", withGroup(args, group)...); err != nil && !isNotFound(err) {
		return fmt.Errorf("cephfs: subvolume rm %s/%s: %w", cc.FSName, req.Volume.Name, err)
	}
	return nil
}

func (b *Backend) ExpandVolume(ctx context.Context, req *bardplugin.ExpandVolumeRequest) (*bardplugin.ExpandVolumeResponse, error) {
	if strings.HasPrefix(req.Volume.Name, shallowPrefix) {
		return nil, bardplugin.Errorf(bardplugin.CodeInvalidArg, "cephfs: a shallow read-only volume cannot be expanded")
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
	args := append(append([]string{}, conn...), "fs", "subvolume", "resize", cc.FSName, req.Volume.Name, strconv.FormatInt(req.NewSizeBytes, 10))
	if _, err := b.run.Run(ctx, "ceph", withGroup(args, subvolumeGroupOf(req.Volume.Location))...); err != nil {
		return nil, fmt.Errorf("cephfs: subvolume resize %s/%s: %w", cc.FSName, req.Volume.Name, err)
	}
	// Quota change is online; no node-side expansion needed.
	return &bardplugin.ExpandVolumeResponse{CapacityBytes: req.NewSizeBytes, NodeExpansionRequired: false}, nil
}

func (b *Backend) CreateSnapshot(ctx context.Context, req *bardplugin.CreateSnapshotRequest) (*bardplugin.CreateSnapshotResponse, error) {
	src := req.SourceVolume // Location=FSName, Name=subvolume
	cc, err := b.cluster(src.Instance)
	if err != nil {
		return nil, err
	}
	// Validate the name prefix before any side effect (index registration, ceph
	// calls), so a bad VolumeSnapshotClass fails fast with nothing to clean up.
	snapPrefix, err := namePrefix(paramSnapshotNamePrefix, req.Parameters[paramSnapshotNamePrefix], snapNamePrefix)
	if err != nil {
		return nil, err
	}

	snap := snapName(snapPrefix, req.Name)
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

	conn, cleanup, err := b.cephConn(cc, src.Instance, req.Secrets)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	group := subvolumeGroupOf(src.Location)
	args := append(append([]string{}, conn...), "fs", "subvolume", "snapshot", "create", cc.FSName, src.Name, snap)
	if _, err := b.run.Run(ctx, "ceph", withGroup(args, group)...); err != nil && !isAlreadyExists(err) {
		return nil, fmt.Errorf("cephfs: snapshot create %s/%s@%s: %w", cc.FSName, src.Name, snap, err)
	}

	// Name encodes subvolume@snapshot so DeleteSnapshot/clone can reconstruct it.
	// Bard core fills the CSI SourceVolumeID from the request.
	return &bardplugin.CreateSnapshotResponse{
		Location:         src.Location,
		Name:             src.Name + "@" + snap,
		SizeBytes:        b.subvolBytes(ctx, conn, cc.FSName, group, src.Name),
		CreationTimeUnix: time.Now().Unix(),
		ReadyToUse:       true,
	}, nil
}

func (b *Backend) DeleteSnapshot(ctx context.Context, req *bardplugin.DeleteSnapshotRequest) error {
	cc, err := b.cluster(req.Snapshot.Instance)
	if err != nil {
		return err
	}
	subvol, snap, ok := splitSnap(req.Snapshot.Name)
	if !ok {
		return nil // malformed/unknown handle => nothing to delete
	}
	conn, cleanup, err := b.cephConn(cc, req.Snapshot.Instance, req.Secrets)
	if err != nil {
		return err
	}
	defer cleanup()
	args := append(append([]string{}, conn...), "fs", "subvolume", "snapshot", "rm", cc.FSName, subvol, snap, "--force")
	if _, err := b.run.Run(ctx, "ceph", withGroup(args, subvolumeGroupOf(req.Snapshot.Location))...); err != nil && !isNotFound(err) {
		return fmt.Errorf("cephfs: snapshot rm %s/%s@%s: %w", cc.FSName, subvol, snap, err)
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

// cloneFromSnapshot clones a snapshot into a new subvolume and waits for the
// (asynchronous) clone to complete. Issuing the clone is idempotent on retry --
// an already-started clone is detected by AlreadyExists, then re-polled.
func (b *Backend) cloneFromSnapshot(ctx context.Context, conn []string, fs, targetGroup string, snapRef *bardplugin.VolumeRef, target string) error {
	srcSub, snap, ok := splitSnap(snapRef.Name)
	if !ok {
		return bardplugin.Errorf(bardplugin.CodeInvalidArg, "cephfs: malformed source snapshot %q", snapRef.Name)
	}
	srcGroup := subvolumeGroupOf(snapRef.Location)
	args := append(append([]string{}, conn...), "fs", "subvolume", "snapshot", "clone", fs, srcSub, snap, target)
	if _, err := b.run.Run(ctx, "ceph", cloneGroupArgs(args, srcGroup, targetGroup)...); err != nil && !isAlreadyExists(err) {
		return fmt.Errorf("cephfs: snapshot clone %s/%s@%s -> %s: %w", fs, srcSub, snap, target, err)
	}
	return b.waitClone(ctx, conn, fs, targetGroup, target)
}

// cloneFromVolume clones one subvolume into another (PVC dataSource). CephFS has
// no direct copy, so it takes a temporary snapshot of the source, clones that into
// the target, waits for completion, then removes the temp snapshot -- safe once the
// clone is complete, as CephFS clones are full copies, not CoW. Each step is
// idempotent so a retried CreateVolume converges.
func (b *Backend) cloneFromVolume(ctx context.Context, conn []string, fs, targetGroup string, srcRef *bardplugin.VolumeRef, target string) error {
	srcSub := srcRef.Name
	srcGroup := subvolumeGroupOf(srcRef.Location)
	tmpSnap := tmpClonePrefix + target
	if _, err := b.run.Run(ctx, "ceph", withGroup(append(append([]string{}, conn...), "fs", "subvolume", "snapshot", "create", fs, srcSub, tmpSnap), srcGroup)...); err != nil && !isAlreadyExists(err) {
		return fmt.Errorf("cephfs: temp snapshot %s/%s@%s: %w", fs, srcSub, tmpSnap, err)
	}
	cloneArgs := append(append([]string{}, conn...), "fs", "subvolume", "snapshot", "clone", fs, srcSub, tmpSnap, target)
	if _, err := b.run.Run(ctx, "ceph", cloneGroupArgs(cloneArgs, srcGroup, targetGroup)...); err != nil && !isAlreadyExists(err) {
		return fmt.Errorf("cephfs: clone %s/%s@%s -> %s: %w", fs, srcSub, tmpSnap, target, err)
	}
	if err := b.waitClone(ctx, conn, fs, targetGroup, target); err != nil {
		return err
	}
	// Clone is complete and independent; drop the temp snapshot (best-effort).
	_, _ = b.run.Run(ctx, "ceph", withGroup(append(append([]string{}, conn...), "fs", "subvolume", "snapshot", "rm", fs, srcSub, tmpSnap, "--force"), srcGroup)...)
	return nil
}

// reapTmpCloneSnaps removes any clonetmp- snapshots on a subvolume before it is
// deleted. Best-effort: if the listing fails, `subvolume rm` surfaces the real
// state and the delete retries.
func (b *Backend) reapTmpCloneSnaps(ctx context.Context, conn []string, fs, group, sub string) {
	out, err := b.run.Run(ctx, "ceph", withGroup(append(append([]string{}, conn...), "fs", "subvolume", "snapshot", "ls", fs, sub, "--format", "json"), group)...)
	if err != nil {
		return
	}
	var rows []cephNamed
	if json.Unmarshal([]byte(out), &rows) != nil {
		return
	}
	for _, r := range rows {
		if !strings.HasPrefix(r.Name, tmpClonePrefix) {
			continue
		}
		_, _ = b.run.Run(ctx, "ceph", withGroup(append(append([]string{}, conn...), "fs", "subvolume", "snapshot", "rm", fs, sub, r.Name, "--force"), group)...)
	}
}

// cloneGroupArgs adds the source group (--group-name) and target group
// (--target-group-name) to a `subvolume snapshot clone` command, each omitted when
// empty (the _nogroup default).
func cloneGroupArgs(args []string, srcGroup, targetGroup string) []string {
	args = withGroup(args, srcGroup)
	if targetGroup != "" {
		args = append(args, "--target-group-name", targetGroup)
	}
	return args
}

// createShallowVolume provisions a read-only snapshot-backed volume without
// copying any data: it resolves the source snapshot's `.snap/<snap>` directory
// and hands that path to the node to mount read-only. CephFS snapshots are
// immutable, so the mount is inherently read-only. The handle Name is marked with
// shallowPrefix so DeleteVolume knows it owns no subvolume and must not touch the
// source subvolume or the snapshot (which belongs to its VolumeSnapshot).
func (b *Backend) createShallowVolume(ctx context.Context, conn []string, cc ClusterConfig, req *bardplugin.CreateVolumeRequest) (*bardplugin.CreateVolumeResponse, error) {
	if cc.Mounter == mounterNFS {
		return nil, bardplugin.Errorf(bardplugin.CodeInvalidArg, "cephfs: %s is not supported with the nfs mounter", paramBackingSnapshot)
	}
	srcSub, snap, ok := splitSnap(req.SourceSnapshot.Name)
	if !ok {
		return nil, bardplugin.Errorf(bardplugin.CodeInvalidArg, "cephfs: malformed source snapshot %q", req.SourceSnapshot.Name)
	}
	path, err := b.run.Run(ctx, "ceph", withGroup(append(append([]string{}, conn...), "fs", "subvolume", "getpath", cc.FSName, srcSub), subvolumeGroupOf(req.SourceSnapshot.Location))...)
	if err != nil {
		return nil, fmt.Errorf("cephfs: getpath %s/%s: %w", cc.FSName, srcSub, err)
	}
	// The snapshot is accessible under the subvolume's virtual `.snap` directory.
	snapPath := strings.TrimSpace(path) + "/.snap/" + snap
	return &bardplugin.CreateVolumeResponse{
		Location:      cc.FSName,
		Name:          shallowPrefix + srcSub + "@" + snap,
		CapacityBytes: req.CapacityBytes,
		Context:       map[string]string{ctxPath: snapPath},
	}, nil
}

// waitClone polls a subvolume clone until it reports complete. CephFS clones are
// asynchronous; the provisioner blocks on CreateVolume, so we poll here.
func (b *Backend) waitClone(ctx context.Context, conn []string, fs, targetGroup, target string) error {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		out, err := b.run.Run(ctx, "ceph", withGroup(append(append([]string{}, conn...), "fs", "clone", "status", fs, target, "--format", "json"), targetGroup)...)
		if err != nil {
			return fmt.Errorf("cephfs: clone status %s/%s: %w", fs, target, err)
		}
		var st struct {
			Status struct {
				State string `json:"state"`
			} `json:"status"`
		}
		if err := json.Unmarshal([]byte(out), &st); err != nil {
			return fmt.Errorf("cephfs: parse clone status: %w", err)
		}
		switch st.Status.State {
		case "complete":
			return nil
		case "failed", "canceled":
			// Reap the dead clone target before erroring: it would otherwise wedge
			// every retry (the re-issued clone hits AlreadyExists against a target
			// that will never complete). Best-effort -- the retry re-clones either way.
			rmArgs := append(append([]string{}, conn...), "fs", "subvolume", "rm", fs, target, "--force")
			_, _ = b.run.Run(ctx, "ceph", withGroup(rmArgs, targetGroup)...)
			return fmt.Errorf("cephfs: clone of %s/%s %s", fs, target, st.Status.State)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("cephfs: clone of %s/%s not complete after timeout", fs, target)
		}
		time.Sleep(2 * time.Second)
	}
}

// subvolBytes returns a subvolume's quota in bytes, or 0 when unset/unknown
// (CephFS quotas are advisory; a 0 restoreSize lets the restore PVC set its own).
func (b *Backend) subvolBytes(ctx context.Context, conn []string, fs, group, sub string) int64 {
	out, err := b.run.Run(ctx, "ceph", withGroup(append(append([]string{}, conn...), "fs", "subvolume", "info", fs, sub, "--format", "json"), group)...)
	if err != nil {
		return 0
	}
	var info struct {
		BytesQuota json.RawMessage `json:"bytes_quota"` // a number, or the string "infinite"
	}
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		return 0
	}
	var n int64
	if json.Unmarshal(info.BytesQuota, &n) == nil {
		return n
	}
	return 0
}

// GetCapacity implements bardplugin.CapacityReporter: the bytes available to
// this filesystem's DATA pools, from `ceph df` max_avail (which accounts for
// replication/EC overhead), scoped by the pool ids `ceph fs get` reports.
// Cluster-wide total_avail_bytes would overstate capacity on a multi-pool
// cluster (it counts pools this filesystem cannot write to); it remains the
// fallback when the user's caps don't allow `fs get`. Implementing this
// interface makes Bard advertise CSI GetCapacity for cephfs.
func (b *Backend) GetCapacity(ctx context.Context, req *bardplugin.GetCapacityRequest) (*bardplugin.GetCapacityResponse, error) {
	cc, err := b.cluster(req.Instance)
	if err != nil {
		return nil, err
	}
	conn, cleanup, err := b.cephConn(cc, req.Instance, nil)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	out, err := b.run.Run(ctx, "ceph", append(append([]string{}, conn...), "df", "--format", "json")...)
	if err != nil {
		return nil, fmt.Errorf("cephfs: ceph df: %w", err)
	}
	var df struct {
		Stats struct {
			TotalAvailBytes int64 `json:"total_avail_bytes"`
		} `json:"stats"`
		Pools []dfPool `json:"pools"`
	}
	if err := json.Unmarshal([]byte(out), &df); err != nil {
		return nil, fmt.Errorf("cephfs: parse ceph df: %w", err)
	}
	if avail, ok := b.dataPoolAvail(ctx, conn, cc.FSName, df.Pools); ok {
		return &bardplugin.GetCapacityResponse{AvailableBytes: avail}, nil
	}
	return &bardplugin.GetCapacityResponse{AvailableBytes: df.Stats.TotalAvailBytes}, nil
}

// dfPool is one pool row of `ceph df --format json`.
type dfPool struct {
	ID    int64 `json:"id"`
	Stats struct {
		MaxAvail int64 `json:"max_avail"`
	} `json:"stats"`
}

// dataPoolAvail sums max_avail over the filesystem's data pools (ids from
// `ceph fs get`). ok=false -- caller falls back to the cluster-wide number --
// when the command fails (caps) or nothing matches.
func (b *Backend) dataPoolAvail(ctx context.Context, conn []string, fs string, pools []dfPool) (int64, bool) {
	out, err := b.run.Run(ctx, "ceph", append(append([]string{}, conn...), "fs", "get", fs, "--format", "json")...)
	if err != nil {
		return 0, false
	}
	var fsInfo struct {
		MDSMap struct {
			DataPools []int64 `json:"data_pools"`
		} `json:"mdsmap"`
	}
	if json.Unmarshal([]byte(out), &fsInfo) != nil || len(fsInfo.MDSMap.DataPools) == 0 {
		return 0, false
	}
	dataPool := map[int64]bool{}
	for _, id := range fsInfo.MDSMap.DataPools {
		dataPool[id] = true
	}
	var avail int64
	matched := false
	for _, p := range pools {
		if dataPool[p.ID] {
			avail += p.Stats.MaxAvail
			matched = true
		}
	}
	return avail, matched
}

// GetVolumeHealth implements bardplugin.HealthReporter: it reports a volume as
// abnormal when its backing subvolume no longer exists (deleted out of band).
// Implementing this interface makes Bard advertise CSI volume health monitoring.
func (b *Backend) GetVolumeHealth(ctx context.Context, req *bardplugin.GetVolumeHealthRequest) (*bardplugin.GetVolumeHealthResponse, error) {
	cc, err := b.cluster(req.Volume.Instance)
	if err != nil {
		return nil, err
	}
	conn, cleanup, err := b.cephConn(cc, req.Volume.Instance, req.Secrets)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	spec := cc.FSName + "/" + req.Volume.Name
	_, infoErr := b.run.Run(ctx, "ceph", withGroup(append(append([]string{}, conn...), "fs", "subvolume", "info", cc.FSName, req.Volume.Name, "--format", "json"), subvolumeGroupOf(req.Volume.Location))...)
	switch {
	case infoErr == nil:
		return &bardplugin.GetVolumeHealthResponse{Abnormal: false, Message: "cephfs subvolume " + spec + " is accessible"}, nil
	case isNotFound(infoErr):
		return &bardplugin.GetVolumeHealthResponse{Abnormal: true, Message: "cephfs subvolume " + spec + " no longer exists"}, nil
	default:
		// A transient query failure (mon unreachable, auth) is not a verdict on
		// the volume's health -- surface it so the monitor retries.
		return nil, infoErr
	}
}

// ---- listing (VolumeLister + SnapshotLister) -----------------------------

// cephNamed parses the `{"name": ...}` rows that `ceph fs subvolume ls` and
// `... snapshot ls` return.
type cephNamed struct {
	Name string `json:"name"`
}

// listGroups returns the subvolume groups to enumerate for an instance: the default
// _nogroup (the empty string) plus every named subvolume group, so listing finds
// Bard volumes wherever they were placed.
func (b *Backend) listGroups(ctx context.Context, conn []string, fs string) []string {
	groups := []string{""} // _nogroup is implicit (no `subvolumegroup ls` row)
	out, err := b.run.Run(ctx, "ceph", append(append([]string{}, conn...), "fs", "subvolumegroup", "ls", fs, "--format", "json")...)
	if err != nil {
		return groups
	}
	var rows []cephNamed
	if json.Unmarshal([]byte(out), &rows) != nil {
		return groups
	}
	for _, r := range rows {
		if r.Name != "" {
			groups = append(groups, r.Name)
		}
	}
	return groups
}

// listSubvolumes returns the Bard-managed subvolume names (shortName shape, any
// prefix) in a subvolume group (empty group = the _nogroup default).
func (b *Backend) listSubvolumes(ctx context.Context, conn []string, fs, group string) ([]string, error) {
	out, err := b.run.Run(ctx, "ceph", withGroup(append(append([]string{}, conn...), "fs", "subvolume", "ls", fs, "--format", "json"), group)...)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("cephfs: subvolume ls %s: %w", fs, err)
	}
	var rows []cephNamed
	if e := json.Unmarshal([]byte(out), &rows); e != nil {
		return nil, fmt.Errorf("cephfs: parse subvolume ls %s: %w", fs, e)
	}
	var subs []string
	for _, r := range rows {
		// shortName shape (any prefix + 16-hex hash) = Bard-managed, so
		// custom-volumeNamePrefix subvolumes are listed too.
		if isBardObjectName(r.Name) {
			subs = append(subs, r.Name)
		}
	}
	return subs, nil
}

// ListVolumes (bardplugin.VolumeLister) enumerates the Bard subvolumes across all
// configured instances and their subvolume groups; Bard core sorts + paginates.
func (b *Backend) ListVolumes(ctx context.Context, _ *bardplugin.ListVolumesRequest) (*bardplugin.ListVolumesResponse, error) {
	var entries []bardplugin.VolumeListEntry
	for instance, cc := range b.clusters {
		conn, cleanup, err := b.cephConn(cc, instance, nil)
		if err != nil {
			return nil, err
		}
		for _, group := range b.listGroups(ctx, conn, cc.FSName) {
			subs, err := b.listSubvolumes(ctx, conn, cc.FSName, group)
			if err != nil {
				cleanup()
				return nil, err
			}
			loc := cephfsLocation(cc.FSName, group)
			for _, sub := range subs {
				entries = append(entries, bardplugin.VolumeListEntry{
					Volume:        bardplugin.VolumeRef{Instance: instance, Location: loc, Name: sub},
					CapacityBytes: b.subvolBytes(ctx, conn, cc.FSName, group, sub),
				})
			}
		}
		cleanup()
	}
	return &bardplugin.ListVolumesResponse{Entries: entries}, nil
}

// ListSnapshots (bardplugin.SnapshotLister) enumerates each subvolume's Bard
// snapshots (shortName shape, any prefix), encoding "subvolume@snapshot" to match
// DeleteSnapshot.
func (b *Backend) ListSnapshots(ctx context.Context, _ *bardplugin.ListSnapshotsRequest) (*bardplugin.ListSnapshotsResponse, error) {
	var entries []bardplugin.SnapshotListEntry
	for instance, cc := range b.clusters {
		conn, cleanup, err := b.cephConn(cc, instance, nil)
		if err != nil {
			return nil, err
		}
		for _, group := range b.listGroups(ctx, conn, cc.FSName) {
			subs, err := b.listSubvolumes(ctx, conn, cc.FSName, group)
			if err != nil {
				cleanup()
				return nil, err
			}
			loc := cephfsLocation(cc.FSName, group)
			for _, sub := range subs {
				out, err := b.run.Run(ctx, "ceph", withGroup(append(append([]string{}, conn...), "fs", "subvolume", "snapshot", "ls", cc.FSName, sub, "--format", "json"), group)...)
				if err != nil {
					if isNotFound(err) {
						continue
					}
					cleanup()
					return nil, fmt.Errorf("cephfs: snapshot ls %s/%s: %w", cc.FSName, sub, err)
				}
				var rows []cephNamed
				if e := json.Unmarshal([]byte(out), &rows); e != nil {
					cleanup()
					return nil, fmt.Errorf("cephfs: parse snapshot ls %s/%s: %w", cc.FSName, sub, e)
				}
				size := b.subvolBytes(ctx, conn, cc.FSName, group, sub)
				for _, r := range rows {
					// Any shortName-shaped snapshot (custom snapshotNamePrefix
					// included) is Bard-managed; foreign snapshots are skipped, as
					// is the transient clonetmp- vehicle of an in-flight clone.
					if !isBardObjectName(r.Name) || strings.HasPrefix(r.Name, tmpClonePrefix) {
						continue
					}
					entries = append(entries, bardplugin.SnapshotListEntry{
						Snapshot:     bardplugin.VolumeRef{Instance: instance, Location: loc, Name: sub + "@" + r.Name},
						SourceVolume: bardplugin.VolumeRef{Instance: instance, Location: loc, Name: sub},
						SizeBytes:    size,
						ReadyToUse:   true,
					})
				}
			}
		}
		cleanup()
	}
	return &bardplugin.ListSnapshotsResponse{Entries: entries}, nil
}

// ---- node plane ----------------------------------------------------------

func (b *Backend) NodeStage(ctx context.Context, req *bardplugin.NodeStageRequest) error {
	cc, err := b.cluster(req.Volume.Instance)
	if err != nil {
		return err
	}
	path := req.Context[ctxPath]
	if path == "" {
		return bardplugin.Errorf(bardplugin.CodeInvalidArg, "cephfs: missing subvolume path in volume context")
	}
	if err := os.MkdirAll(req.StagingPath, 0o750); err != nil {
		return fmt.Errorf("cephfs: mkdir staging: %w", err)
	}
	// Idempotent: skip the mount if the staging path is already a mountpoint (CSI
	// may retry NodeStage). Use --mountpoint, not --target: --target walks up to
	// the containing filesystem and so matches any path that merely resides on a mount.
	// For an encrypted volume we still fall through to re-add the fscrypt key (the key
	// lives in the mount's keyring and SetupFscrypt is idempotent), so only short-
	// circuit when the volume is unencrypted.
	out, _ := b.run.Run(ctx, "findmnt", "-n", "-o", "SOURCE", "--mountpoint", req.StagingPath)
	mounted := strings.TrimSpace(out) != ""
	if mounted && !cephenc.IsEncrypted(req.Context) {
		return nil
	}

	// mounter:nfs mounts the Ganesha export over NFS -- no ceph client, no cephx
	// key on the node. The subvolume's data path is irrelevant here; the pseudo
	// path the gateway publishes is what clients mount.
	if cc.Mounter == mounterNFS {
		server := req.Context[ctxNFSServer]
		if server == "" {
			server = cc.NFSServer
		}
		pseudo := req.Context[ctxNFSPseudo]
		if pseudo == "" {
			pseudo = nfsPseudoPath(req.Volume.Name)
		}
		if server == "" || pseudo == "" {
			return bardplugin.Errorf(bardplugin.CodeInvalidArg, "cephfs: nfs mount missing server/pseudo path")
		}
		opts := "vers=4.1"
		if req.Readonly {
			opts += ",ro"
		}
		if len(req.MountFlags) > 0 {
			opts += "," + strings.Join(req.MountFlags, ",")
		}
		if _, err := b.run.Run(ctx, "mount", "-t", "nfs", server+":"+pseudo, req.StagingPath, "-o", opts); err != nil {
			return fmt.Errorf("cephfs: nfs mount %s:%s: %w", server, pseudo, err)
		}
		return nil
	}

	keyfile, cleanup, err := b.keyFile(req.Volume.Instance, req.Secrets)
	if err != nil {
		return err
	}
	defer cleanup()
	user := b.userID(cc, req.Secrets)

	if cc.Mounter == mounterFuse {
		// ceph-fuse <staging> -m <mons> --id <user> --keyfile <kf>
		//           --client_fs <fs> -r <subvolume-path>
		args := []string{
			req.StagingPath, "-m", strings.Join(cc.Monitors, ","),
			"--id", user, "--client_fs", cc.FSName, "-r", path,
		}
		if keyfile != "" {
			args = append(args, "--keyfile", keyfile)
		}
		// StorageClass mountOptions, same as the kernel mounter honours them.
		if len(req.MountFlags) > 0 {
			args = append(args, "-o", strings.Join(req.MountFlags, ","))
		}
		if _, err := b.run.Run(ctx, "ceph-fuse", args...); err != nil {
			return fmt.Errorf("cephfs: ceph-fuse mount: %w", err)
		}
		return nil
	}

	// kernel: mount -t ceph <mons>:<path> <staging> -o name=,secretfile=,mds_namespace=
	// (skipped when already mounted -- an encrypted restage only needs the fscrypt key
	// re-added below).
	if !mounted {
		src := strings.Join(cc.Monitors, ",") + ":" + path
		opts := "name=" + user + ",mds_namespace=" + cc.FSName
		if keyfile != "" {
			opts += ",secretfile=" + keyfile
		}
		if len(req.MountFlags) > 0 {
			opts += "," + strings.Join(req.MountFlags, ",")
		}
		if _, err := b.run.Run(ctx, "mount", "-t", "ceph", src, req.StagingPath, "-o", opts); err != nil {
			return fmt.Errorf("cephfs: kernel mount: %w", err)
		}
	}

	// fscrypt: with the subvolume mounted (kernel client only), add the master key to
	// its keyring and ensure the encrypted data directory + policy exist. Idempotent,
	// so a restage (mount skipped above) re-adds the key -- it lives in the mount
	// keyring, dropped on unmount, so every stage re-adds it. NodePublish bind-mounts
	// the encrypted subdir as the pod's volume root.
	if cephenc.IsEncrypted(req.Context) {
		// nil conn: a KMS provider that needs Ceph (secrets-metadata/aws/kmip reading
		// subvolume metadata) opens its own connection via ConnFor; derived/vault/azure
		// need none.
		pass, err := b.volumePassphrase(ctx, nil, req)
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
	if _, err := b.run.Run(ctx, "umount", req.StagingPath); err != nil && !isNotMounted(err) {
		return fmt.Errorf("cephfs: umount %s: %w", req.StagingPath, err)
	}
	return nil
}

func (b *Backend) NodePublish(ctx context.Context, req *bardplugin.NodePublishRequest) error {
	if err := os.MkdirAll(req.TargetPath, 0o750); err != nil {
		return fmt.Errorf("cephfs: mkdir target: %w", err)
	}
	if out, _ := b.run.Run(ctx, "findmnt", "-n", "-o", "SOURCE", "--mountpoint", req.TargetPath); strings.TrimSpace(out) != "" {
		return nil // idempotent: already published on a retry
	}
	// For an encrypted volume, publish the fscrypt data subdir (set up at NodeStage),
	// so the pod's volume root is the encrypted directory and the unencrypted mount
	// root stays hidden.
	source := req.StagingPath
	if cephenc.IsEncrypted(req.Context) {
		source = cephenc.FscryptDataDir(req.StagingPath)
	}
	if _, err := b.run.Run(ctx, "mount", "--bind", source, req.TargetPath); err != nil {
		return fmt.Errorf("cephfs: bind mount %s -> %s: %w", source, req.TargetPath, err)
	}
	if req.Readonly {
		if _, err := b.run.Run(ctx, "mount", "-o", "remount,ro,bind", source, req.TargetPath); err != nil {
			return fmt.Errorf("cephfs: remount ro %s: %w", req.TargetPath, err)
		}
	}
	return nil
}

func (b *Backend) NodeUnpublish(ctx context.Context, req *bardplugin.NodeUnpublishRequest) error {
	if _, err := b.run.Run(ctx, "umount", req.TargetPath); err != nil && !isNotMounted(err) {
		return fmt.Errorf("cephfs: umount %s: %w", req.TargetPath, err)
	}
	return nil
}

func (b *Backend) NodeExpand(context.Context, *bardplugin.NodeExpandRequest) (*bardplugin.NodeExpandResponse, error) {
	// CephFS quota resize is online; nothing to do on the node.
	return &bardplugin.NodeExpandResponse{}, nil
}
