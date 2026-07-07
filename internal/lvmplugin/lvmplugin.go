// Package lvmplugin is an LVM backend as an out-of-tree Bard plugin. A volume is
// a logical volume (LV) carved from a host volume group (VG); the node formats
// and mounts the LV's block device. Like the other plugins it depends only on
// the public bardplugin SDK.
//
// Thin or thick: with a thin pool (per StorageClass `thinPool` or per instance),
// volumes are copy-on-write thin LVs that over-commit the pool and support cheap
// snapshots and clones (lvcreate -s); without one they are thick, fully-allocated
// LVs with no snapshot support. Snapshot/clone of a thick volume is rejected.
//
// # Locality model (important)
//
// This plugin treats a VG as a *shared* storage instance: the control plane
// (Bard's controller pod) runs lvcreate/lvremove/lvextend against the VG, and
// the node only formats + mounts the resulting device. That is the right model
// when every node can reach the same VG -- e.g. a single-host dev cluster where
// kind "nodes" are containers sharing one host VG, or a VG on shared block
// storage. It is deliberately NOT node-local: Capabilities.NodeLocal is false.
//
// True per-node LVM (an LV lives only on the node whose disks back the VG, so
// provisioning AND deletion must happen on that node) needs a node agent that
// reconciles a per-volume CRD -- the TopoLVM pattern -- because CSI DeleteVolume
// runs on the controller, which cannot reach another node's VG. That is a
// documented follow-up, not something this plugin fakes.
package lvmplugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

const (
	defaultFsType = "ext4"

	// ctxDevPath carries the LV's device path from CreateVolume (controller) to
	// the node, so the node needs no LVM tools to resolve it.
	ctxDevPath = "devPath"
)

// supportedFsTypes are the filesystems the plugin can format and grow (see
// NodeExpand). ext4 is the default; ext2/ext3 ride e2fsprogs; xfs and btrfs need
// their own tools (xfsprogs / btrfs-progs) in the plugin image. Allowlisted so an
// unknown type fails fast instead of a cryptic "mkfs.<x>: not found".
var supportedFsTypes = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true,
}

// InstanceConfig is the per-instance LVM config: which volume group to carve
// logical volumes from. No credentials -- LVM is local to the host.
type InstanceConfig struct {
	VG string `json:"vg"` // volume group name (e.g. "bard-vg")
	// ThinPool is the instance default thin pool: when set (and not overridden by
	// the StorageClass thinPool parameter), volumes are provisioned as thin
	// copy-on-write LVs from that pre-created pool instead of thick (fully
	// allocated) ones. Thin volumes are what support cheap snapshots/clones and
	// over-commit; snapshot/clone of a thick volume is rejected.
	ThinPool string `json:"thinPool,omitempty"`
}

// Backend implements bardplugin.Backend for LVM.
type Backend struct {
	instances map[string]InstanceConfig // instance id -> VG config
	run       Runner
}

// New builds the LVM plugin backend over per-instance VG config.
func New(instances map[string]InstanceConfig, run Runner) *Backend {
	if run == nil {
		run = ExecRunner{}
	}
	return &Backend{instances: instances, run: run}
}

func (b *Backend) Info() bardplugin.Info {
	return bardplugin.Info{
		Type: "lvm",
		Capabilities: bardplugin.Capabilities{
			BlockDevice: true,  // an LV is a block device the node formats + mounts
			NodeLocal:   false, // shared-VG model; see package doc
			Snapshots:   true,  // thin instances only; thick ones reject at CreateSnapshot
			Expand:      true,  // lvextend + node-side fs grow
		},
	}
}

// lvName derives a bounded, deterministic LV name from a CSI name (the volume_id
// has a 128-byte cap), keeping retries idempotent. LV names allow [a-zA-Z0-9+_.-].
func lvName(csiName string) string {
	sum := sha256.Sum256([]byte(csiName))
	return "bard-" + hex.EncodeToString(sum[:8])
}

func (b *Backend) vg(instance string) (string, error) {
	ic, ok := b.instances[instance]
	if !ok || ic.VG == "" {
		return "", bardplugin.Errorf(bardplugin.CodeInvalidArg, "lvm: no volume group configured for instance %q", instance)
	}
	return ic.VG, nil
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
// attributes (lv_attr starts with 'V' for a thin volume) -- the actual truth,
// independent of config, so snapshot/clone of a source behaves correctly.
func (b *Backend) isThinLV(ctx context.Context, vg, lv string) (bool, error) {
	out, err := b.run.Run(ctx, "lvs", "--noheadings", "-o", "lv_attr", vg+"/"+lv)
	if err != nil {
		return false, fmt.Errorf("lvm: lvs attr %s/%s: %w", vg, lv, err)
	}
	attr := strings.TrimSpace(out)
	return strings.HasPrefix(attr, "V"), nil
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
		return "", false, fmt.Errorf("lvm: lvs origin %s/%s: %w", vg, lv, err)
	}
	return strings.TrimSpace(out), true, nil
}

func devPath(vg, lv string) string { return "/dev/" + vg + "/" + lv }

// lvSizeBytes returns the LV's size in bytes and whether it exists.
func (b *Backend) lvSizeBytes(ctx context.Context, vg, lv string) (int64, bool, error) {
	out, err := b.run.Run(ctx, "lvs", "--noheadings", "--units", "b", "--nosuffix", "-o", "lv_size", vg+"/"+lv)
	if err != nil {
		if isNotFound(err) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("lvm: lvs %s/%s: %w", vg, lv, err)
	}
	n, perr := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if perr != nil {
		return 0, false, fmt.Errorf("lvm: parse lv_size %q: %w", strings.TrimSpace(out), perr)
	}
	return n, true, nil
}

// ---- control plane -------------------------------------------------------

func (b *Backend) CreateVolume(ctx context.Context, req *bardplugin.CreateVolumeRequest) (*bardplugin.CreateVolumeResponse, error) {
	vg, err := b.vg(req.Instance)
	if err != nil {
		return nil, err
	}
	lv := lvName(req.Name)

	size, found, err := b.lvSizeBytes(ctx, vg, lv)
	if err != nil {
		return nil, err
	}
	isClone := req.SourceSnapshot != nil || req.SourceVolume != nil
	switch {
	case found:
		// Idempotent retry: an LV at least as large as requested satisfies the
		// request (lvcreate rounds up to the extent size). A smaller existing LV
		// is a name clash with a different request -- except for a clone, which
		// starts at its SOURCE's size: grow it to the request (resuming a create
		// whose extend never ran).
		if size < req.CapacityBytes {
			if !isClone {
				return nil, bardplugin.Errorf(bardplugin.CodeAlreadyExists,
					"lvm: volume %q exists at %d bytes, smaller than requested %d", req.Name, size, req.CapacityBytes)
			}
			if err := b.extendTo(ctx, vg, lv, req.CapacityBytes); err != nil {
				return nil, err
			}
			if size, _, err = b.lvSizeBytes(ctx, vg, lv); err != nil {
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
				return nil, bardplugin.Errorf(bardplugin.CodeInvalidArg, "lvm: clone/restore requires a thin source volume")
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
				return nil, fmt.Errorf("lvm: lvcreate thin %s/%s: %w", vg, lv, err)
			}
		default:
			// Thick volume: -L fully allocates the size up front.
			if _, err := b.run.Run(ctx, "lvcreate", "-n", lv, "-L", lvBytes(req.CapacityBytes), vg); err != nil {
				return nil, fmt.Errorf("lvm: lvcreate %s/%s: %w", vg, lv, err)
			}
		}
		if size, _, err = b.lvSizeBytes(ctx, vg, lv); err != nil {
			return nil, err
		}
	}

	return &bardplugin.CreateVolumeResponse{
		Location:      vg,
		Name:          lv,
		CapacityBytes: size,
		Context:       map[string]string{ctxDevPath: devPath(vg, lv)},
	}, nil
}

// lvBytes formats a byte size for lvcreate -L/-V, defaulting tiny requests up to
// one extent's worth so lvcreate never fails on a zero size.
func lvBytes(b int64) string {
	if b <= 0 {
		b = 1 << 20
	}
	return strconv.FormatInt(b, 10) + "b"
}

// thinClone creates lv as a writable copy-on-write thin snapshot of src and
// activates it (thin snapshots are created with the activation-skip flag set, so
// the node would otherwise not see the device). Idempotent on the create.
func (b *Backend) thinClone(ctx context.Context, vg, src, lv string) error {
	if _, err := b.run.Run(ctx, "lvcreate", "-s", "-n", lv, vg+"/"+src); err != nil && !isAlreadyExists(err) {
		return fmt.Errorf("lvm: thin clone %s/%s -> %s: %w", vg, src, lv, err)
	}
	if _, err := b.run.Run(ctx, "lvchange", "-ay", "-Ky", vg+"/"+lv); err != nil {
		return fmt.Errorf("lvm: activate clone %s/%s: %w", vg, lv, err)
	}
	return nil
}

func (b *Backend) DeleteVolume(ctx context.Context, req *bardplugin.DeleteVolumeRequest) error {
	vglv := req.Volume.Location + "/" + req.Volume.Name
	if _, err := b.run.Run(ctx, "lvremove", "-f", vglv); err != nil && !isNotFound(err) {
		return fmt.Errorf("lvm: lvremove %s: %w", vglv, err)
	}
	return nil
}

func (b *Backend) ExpandVolume(ctx context.Context, req *bardplugin.ExpandVolumeRequest) (*bardplugin.ExpandVolumeResponse, error) {
	if err := b.extendTo(ctx, req.Volume.Location, req.Volume.Name, req.NewSizeBytes); err != nil {
		return nil, err
	}
	size, _, err := b.lvSizeBytes(ctx, req.Volume.Location, req.Volume.Name)
	if err != nil {
		return nil, err
	}
	// Block device grew; the node must grow the filesystem on it.
	return &bardplugin.ExpandVolumeResponse{CapacityBytes: size, NodeExpansionRequired: true}, nil
}

// extendTo grows an LV to size bytes; a no-op when it is already that large.
func (b *Backend) extendTo(ctx context.Context, vg, lv string, size int64) error {
	if size <= 0 {
		return nil
	}
	if _, err := b.run.Run(ctx, "lvextend", "-L", strconv.FormatInt(size, 10)+"b", vg+"/"+lv); err != nil && !isNotLarger(err) {
		return fmt.Errorf("lvm: lvextend %s/%s: %w", vg, lv, err)
	}
	return nil
}

// CreateSnapshot makes a read-only copy-on-write thin snapshot of the source LV.
// Thin only: a thick instance has no cheap snapshot, so it is rejected. The
// snapshot reserves no space up front and grows as the origin diverges.
func (b *Backend) CreateSnapshot(ctx context.Context, req *bardplugin.CreateSnapshotRequest) (*bardplugin.CreateSnapshotResponse, error) {
	src := req.SourceVolume // Location=vg, Name=lv
	thin, err := b.isThinLV(ctx, src.Location, src.Name)
	if err != nil {
		return nil, err
	}
	if !thin {
		return nil, bardplugin.Errorf(bardplugin.CodeInvalidArg, "lvm: snapshots require a thin volume (provision from a thinPool StorageClass)")
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
			"lvm: snapshot %q already exists for a different source volume (%s)", req.Name, origin)
	}
	if _, err := b.run.Run(ctx, "lvcreate", "-s", "-pr", "-n", snap, vg+"/"+src.Name); err != nil && !isAlreadyExists(err) {
		return nil, fmt.Errorf("lvm: snapshot %s/%s: %w", vg, src.Name, err)
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
		return fmt.Errorf("lvm: lvremove snapshot %s: %w", vgsnap, err)
	}
	return nil
}

// ---- node plane ----------------------------------------------------------

func (b *Backend) NodeStage(ctx context.Context, req *bardplugin.NodeStageRequest) error {
	dev := req.Context[ctxDevPath]
	if dev == "" {
		dev = devPath(req.Volume.Location, req.Volume.Name)
	}
	if req.Block {
		// Raw block: the LV device is published directly; nothing to format/mount.
		return nil
	}

	fsType := req.FsType
	if fsType == "" {
		fsType = defaultFsType
	}
	if !supportedFsTypes[fsType] {
		return bardplugin.Errorf(bardplugin.CodeInvalidArg,
			"lvm: unsupported fsType %q (supported: ext4, ext3, ext2, xfs, btrfs)", fsType)
	}
	if err := b.ensureFormatted(ctx, dev, fsType); err != nil {
		return err
	}
	if err := os.MkdirAll(req.StagingPath, 0o750); err != nil {
		return fmt.Errorf("lvm: mkdir staging: %w", err)
	}
	// Idempotent: skip the mount if the staging path is itself already a mount
	// (retry). Use --mountpoint, not --target: --target walks up to the containing
	// filesystem and so matches any path that merely resides on a mount.
	if out, _ := b.run.Run(ctx, "findmnt", "-n", "-o", "SOURCE", "--mountpoint", req.StagingPath); strings.TrimSpace(out) == "" {
		mountArgs := []string{"-t", fsType}
		if len(req.MountFlags) > 0 {
			mountArgs = append(mountArgs, "-o", strings.Join(req.MountFlags, ","))
		}
		mountArgs = append(mountArgs, dev, req.StagingPath)
		if _, err := b.run.Run(ctx, "mount", mountArgs...); err != nil {
			return fmt.Errorf("lvm: mount %s -> %s: %w", dev, req.StagingPath, err)
		}
	}
	// A clone/restore into a LARGER volume carries its SOURCE's filesystem; grow
	// it to the device once mounted (online for every supported fs, a no-op when
	// the sizes already match -- i.e. every non-clone stage).
	return b.growFilesystem(ctx, fsType, dev, req.StagingPath)
}

func (b *Backend) NodeUnstage(ctx context.Context, req *bardplugin.NodeUnstageRequest) error {
	// Just unmount: the LV persists (the controller's DeleteVolume removes it),
	// so a deactivate here would be wrong -- the data must survive a pod restart.
	if _, err := b.run.Run(ctx, "umount", req.StagingPath); err != nil && !isNotMounted(err) {
		return fmt.Errorf("lvm: umount %s: %w", req.StagingPath, err)
	}
	return nil
}

func (b *Backend) NodePublish(ctx context.Context, req *bardplugin.NodePublishRequest) error {
	if err := os.MkdirAll(req.TargetPath, 0o750); err != nil {
		return fmt.Errorf("lvm: mkdir target: %w", err)
	}
	if out, _ := b.run.Run(ctx, "findmnt", "-n", "-o", "SOURCE", "--mountpoint", req.TargetPath); strings.TrimSpace(out) != "" {
		return nil // idempotent: already published on a retry
	}
	if _, err := b.run.Run(ctx, "mount", "--bind", req.StagingPath, req.TargetPath); err != nil {
		return fmt.Errorf("lvm: bind mount %s -> %s: %w", req.StagingPath, req.TargetPath, err)
	}
	if req.Readonly {
		if _, err := b.run.Run(ctx, "mount", "-o", "remount,ro,bind", req.StagingPath, req.TargetPath); err != nil {
			return fmt.Errorf("lvm: remount ro %s: %w", req.TargetPath, err)
		}
	}
	return nil
}

func (b *Backend) NodeUnpublish(ctx context.Context, req *bardplugin.NodeUnpublishRequest) error {
	if _, err := b.run.Run(ctx, "umount", req.TargetPath); err != nil && !isNotMounted(err) {
		return fmt.Errorf("lvm: umount %s: %w", req.TargetPath, err)
	}
	return nil
}

func (b *Backend) NodeExpand(ctx context.Context, req *bardplugin.NodeExpandRequest) (*bardplugin.NodeExpandResponse, error) {
	dev, err := b.run.Run(ctx, "findmnt", "-n", "-o", "SOURCE", "--target", req.VolumePath)
	if err != nil {
		return nil, fmt.Errorf("lvm: resolve device for %s: %w", req.VolumePath, err)
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
		return fmt.Errorf("lvm: grow %s filesystem on %s: %w", fsType, dev, err)
	}
	return nil
}

func (b *Backend) ensureFormatted(ctx context.Context, dev, fsType string) error {
	out, _ := b.run.Run(ctx, "blkid", "-o", "value", "-s", "TYPE", dev)
	if strings.TrimSpace(out) != "" {
		return nil
	}
	if _, err := b.run.Run(ctx, "mkfs."+fsType, dev); err != nil {
		return fmt.Errorf("lvm: mkfs.%s %s: %w", fsType, dev, err)
	}
	return nil
}

// ---- optional capabilities -----------------------------------------------

// GetCapacity (bardplugin.CapacityReporter) reports the VG's physically-free
// space. For thin instances this is conservative -- over-commit beyond the VG's
// extents is not counted -- but it never over-reports, which is the safe bias for
// scheduling.
func (b *Backend) GetCapacity(ctx context.Context, req *bardplugin.GetCapacityRequest) (*bardplugin.GetCapacityResponse, error) {
	vg, err := b.vg(req.Instance)
	if err != nil {
		return nil, err
	}
	out, err := b.run.Run(ctx, "vgs", "--noheadings", "--units", "b", "--nosuffix", "-o", "vg_free", vg)
	if err != nil {
		return nil, fmt.Errorf("lvm: vgs %s: %w", vg, err)
	}
	n, perr := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if perr != nil {
		return nil, fmt.Errorf("lvm: parse vg_free %q: %w", strings.TrimSpace(out), perr)
	}
	return &bardplugin.GetCapacityResponse{AvailableBytes: n}, nil
}

// GetVolumeHealth (bardplugin.HealthReporter) reports whether the LV still exists
// (the meaningful signal in the shared-VG model -- an "active" check would be
// wrong from the controller, where the LV is activated only on the mounting node).
func (b *Backend) GetVolumeHealth(ctx context.Context, req *bardplugin.GetVolumeHealthRequest) (*bardplugin.GetVolumeHealthResponse, error) {
	_, found, err := b.lvSizeBytes(ctx, req.Volume.Location, req.Volume.Name)
	if err != nil {
		return nil, err
	}
	if !found {
		return &bardplugin.GetVolumeHealthResponse{Abnormal: true, Message: "logical volume not found"}, nil
	}
	return &bardplugin.GetVolumeHealthResponse{Abnormal: false, Message: "present"}, nil
}

// NodeReclaimSpace (bardplugin.NodeSpaceReclaimer) fstrims the mounted filesystem;
// on a thin LV the discards free blocks back to the thin pool. A no-op for raw
// block (no filesystem to trim).
func (b *Backend) NodeReclaimSpace(ctx context.Context, req *bardplugin.NodeReclaimSpaceRequest) (*bardplugin.ReclaimSpaceResponse, error) {
	if req.Block {
		return &bardplugin.ReclaimSpaceResponse{PreUsageBytes: -1, PostUsageBytes: -1}, nil
	}
	path := req.VolumePath
	if path == "" {
		path = req.StagingPath
	}
	if _, err := b.run.Run(ctx, "fstrim", path); err != nil {
		return nil, fmt.Errorf("lvm: fstrim %s: %w", path, err)
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
		return nil, fmt.Errorf("lvm: lvs %s: %w", vg, err)
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
