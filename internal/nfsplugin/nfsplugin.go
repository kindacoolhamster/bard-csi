// Package nfsplugin is the NFS backend as an out-of-tree Bard plugin, and a
// worked example of the bardplugin SDK exercising a very different backend shape
// than Ceph RBD: each volume is a subdirectory of an export (created on the
// controller side, mounted on the node side) -- the nfs-subdir pattern. There is
// no block device and no format step (snapshots are file-level tar archives),
// which validates that the plugin contract is not RBD-specific. It depends only
// on pkg/bardplugin.
package nfsplugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// InstanceConfig is one NFS endpoint (server + exported base path).
type InstanceConfig struct {
	Server string `json:"server"` // NFS server host/IP
	Export string `json:"export"` // exported base path, e.g. /srv/nfs/bard
	// OnDelete is the retention policy for this instance's volumes when their PVC
	// is deleted: "delete" (default, remove the data), "retain" (keep it in place),
	// or "archive" (rename to archived-<name>). A delete-time setting, so it lives
	// on the instance config -- CSI DeleteVolume carries no StorageClass params.
	OnDelete string `json:"onDelete,omitempty"`
}

// StorageClass parameters the NFS backend understands at CreateVolume.
const (
	// paramSubDir templates the exported subdirectory name from PVC/PV metadata
	// (passed by the provisioner's --extra-create-metadata), e.g.
	// "${pvc.metadata.namespace}-${pvc.metadata.name}-${pv.metadata.name}". When
	// unset, the directory is an opaque hash of the CSI name.
	paramSubDir = "subDir"
	// paramMountPermissions sets the created directory's octal mode (e.g. "0770").
	paramMountPermissions = "mountPermissions"

	onDeleteRetain  = "retain"
	onDeleteArchive = "archive"
)

// Backend implements bardplugin.Backend for NFS.
type Backend struct {
	instances map[string]InstanceConfig
	run       Runner
}

// New builds the NFS plugin backend over per-instance endpoint config. A nil
// run uses the real ExecRunner.
func New(instances map[string]InstanceConfig, run Runner) *Backend {
	if run == nil {
		run = ExecRunner{}
	}
	return &Backend{instances: instances, run: run}
}

func (b *Backend) Info() bardplugin.Info {
	return bardplugin.Info{
		Type: "nfs",
		Capabilities: bardplugin.Capabilities{
			// NFS is a plain network filesystem: no block device, no format, no
			// node-local pinning. Snapshots are a tar archive of the subdirectory
			// (crash-consistent only, see CreateSnapshot); no online expand.
			BlockDevice: false,
			NodeLocal:   false,
			Snapshots:   true,
			Expand:      false,
		},
	}
}

// snapDir is the subdirectory on each export that holds snapshot archives.
const snapDir = ".snapshots"

// snapName derives a bounded snapshot id from the CSI snapshot name.
func snapName(csiName string) string {
	sum := sha256.Sum256([]byte(csiName))
	return "snap-" + hex.EncodeToString(sum[:8])
}

// snapArchive is the export-relative path of a snapshot's tarball.
func snapArchive(snapID string) string {
	return filepath.Join(snapDir, snapID+".tar.gz")
}

// snapSrcFile is the export-relative path of a snapshot's source-provenance
// sidecar (the source subdirectory name). It records what the opaque tarball was
// taken from, so ListSnapshots can report a source volume. Written best-effort at
// snapshot time; a snapshot with no sidecar simply isn't enumerated.
func snapSrcFile(snapID string) string {
	return filepath.Join(snapDir, snapID+".src")
}

func (b *Backend) instance(id string) (InstanceConfig, error) {
	ic, ok := b.instances[id]
	if !ok {
		return InstanceConfig{}, bardplugin.Errorf(bardplugin.CodeInvalidArg, "nfs: no instance %q configured", id)
	}
	return ic, nil
}

// volName derives a bounded, deterministic directory name from the CSI name so
// the volume handle stays within the CSI 128-byte limit and retries are
// idempotent.
func volName(csiName string) string {
	sum := sha256.Sum256([]byte(csiName))
	return "bard-" + hex.EncodeToString(sum[:8])
}

// volDir is the exported subdirectory for a volume: a subDir template expanded
// from PVC/PV metadata when set (and fully substituted), else the opaque hash.
func volDir(req *bardplugin.CreateVolumeRequest) string {
	if t := req.Parameters[paramSubDir]; t != "" {
		if d := expandSubDir(t, req.Parameters); d != "" && !strings.Contains(d, "${") {
			return strings.Trim(d, "/")
		}
	}
	return volName(req.Name)
}

// expandSubDir substitutes the pv/pvc metadata tokens the external-provisioner
// supplies with --extra-create-metadata. Unknown tokens are left intact so volDir
// can fall back to the hash rather than create a path with a literal "${...}".
func expandSubDir(tmpl string, params map[string]string) string {
	return strings.NewReplacer(
		"${pvc.metadata.name}", params["csi.storage.k8s.io/pvc/name"],
		"${pvc.metadata.namespace}", params["csi.storage.k8s.io/pvc/namespace"],
		"${pv.metadata.name}", params["csi.storage.k8s.io/pv/name"],
	).Replace(tmpl)
}

// mounted reports whether path is itself a mountpoint. --mountpoint (not --target)
// matches only an exact mountpoint, answering the node plane's idempotency check.
func (b *Backend) mounted(ctx context.Context, path string) bool {
	out, _ := b.run.Run(ctx, "findmnt", "-n", "-o", "SOURCE", "--mountpoint", path)
	return strings.TrimSpace(out) != ""
}

// withExportMounted mounts the instance's export at a temp dir, runs fn against
// it, and unmounts -- used by the controller to create/delete subdirectories.
func (b *Backend) withExportMounted(ctx context.Context, ic InstanceConfig, fn func(mnt string) error) error {
	tmp, err := os.MkdirTemp("", "bard-nfs-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if _, err := b.run.Run(ctx, "mount", "-t", "nfs", "-o", "vers=4.1", ic.Server+":"+ic.Export, tmp); err != nil {
		return bardplugin.Errorf(bardplugin.CodeInternal, "nfs: mount export: %v", err)
	}
	defer func() { _, _ = b.run.Run(ctx, "umount", tmp) }()
	return fn(tmp)
}

// ---- control plane -------------------------------------------------------

func (b *Backend) CreateVolume(ctx context.Context, req *bardplugin.CreateVolumeRequest) (*bardplugin.CreateVolumeResponse, error) {
	ic, err := b.instance(req.Instance)
	if err != nil {
		return nil, err
	}
	name := volDir(req)
	// Reserved names: the snapshot store and archive leftovers live beside the
	// volume subdirectories on the export, and a subDir template must not be able
	// to address them (or climb out of the export). The hash fallback never
	// collides; only a template can.
	if name == snapDir || strings.HasPrefix(name, "archived-") || strings.Contains(name, "..") {
		return nil, bardplugin.Errorf(bardplugin.CodeInvalidArg, "nfs: reserved volume directory name %q", name)
	}
	err = b.withExportMounted(ctx, ic, func(mnt string) error {
		dir := filepath.Join(mnt, name)
		if err := os.MkdirAll(dir, 0o777); err != nil {
			return err
		}
		// Populate from a source, if any: restore extracts the snapshot tarball;
		// clone copies the source directory. Both source and target live on this
		// export (same-instance restore/clone).
		switch {
		case req.SourceSnapshot != nil:
			archive := filepath.Join(mnt, snapArchive(req.SourceSnapshot.Name))
			if _, err := b.run.Run(ctx, "tar", "xzf", archive, "-C", dir); err != nil {
				return fmt.Errorf("nfs: restore snapshot %s: %w", req.SourceSnapshot.Name, err)
			}
		case req.SourceVolume != nil:
			// cp -a <src>/. <dir> copies contents (including dotfiles) preserving attrs.
			if _, err := b.run.Run(ctx, "cp", "-a", filepath.Join(mnt, req.SourceVolume.Name)+"/.", dir); err != nil {
				return fmt.Errorf("nfs: clone volume %s: %w", req.SourceVolume.Name, err)
			}
		}
		if perm := req.Parameters[paramMountPermissions]; perm != "" {
			mode, err := strconv.ParseUint(perm, 8, 32)
			if err != nil {
				return bardplugin.Errorf(bardplugin.CodeInvalidArg, "nfs: invalid mountPermissions %q: %v", perm, err)
			}
			if err := os.Chmod(dir, os.FileMode(mode)); err != nil {
				return fmt.Errorf("nfs: chmod %s: %w", dir, err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &bardplugin.CreateVolumeResponse{
		Location:      ic.Export,
		Name:          name,
		CapacityBytes: req.CapacityBytes, // NFS shares the backing fs; size is advisory
		Context:       map[string]string{"server": ic.Server},
	}, nil
}

func (b *Backend) DeleteVolume(ctx context.Context, req *bardplugin.DeleteVolumeRequest) error {
	ic, err := b.instance(req.Volume.Instance)
	if err != nil {
		return err
	}
	// retain: leave the data untouched (operator reclaims it out of band).
	if strings.EqualFold(ic.OnDelete, onDeleteRetain) {
		return nil
	}
	return b.withExportMounted(ctx, ic, func(mnt string) error {
		dir := filepath.Join(mnt, req.Volume.Name)
		if strings.EqualFold(ic.OnDelete, onDeleteArchive) {
			// Rename rather than remove, so the data survives an accidental delete.
			// Idempotent: a missing source means a prior call already archived it.
			archived := filepath.Join(filepath.Dir(dir), "archived-"+filepath.Base(dir))
			if err := os.Rename(dir, archived); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("nfs: archive %s: %w", dir, err)
			}
			return nil
		}
		return os.RemoveAll(dir)
	})
}

func (b *Backend) ExpandVolume(context.Context, *bardplugin.ExpandVolumeRequest) (*bardplugin.ExpandVolumeResponse, error) {
	return nil, bardplugin.Errorf(bardplugin.CodeInvalidArg, "nfs: expand not supported")
}

// CreateSnapshot archives the source volume's directory into a gzip tarball under
// the export's .snapshots/. NFS has no native snapshot, so this is a file-level
// copy: it is crash-consistent (a point-in-time tar of a live directory), not
// atomic -- quiesce the workload first if you need application consistency. Cost
// and time scale with the data. Idempotent: re-archiving overwrites in place.
func (b *Backend) CreateSnapshot(ctx context.Context, req *bardplugin.CreateSnapshotRequest) (*bardplugin.CreateSnapshotResponse, error) {
	src := req.SourceVolume // Location=export, Name=subdir
	ic, err := b.instance(src.Instance)
	if err != nil {
		return nil, err
	}
	snapID := snapName(req.Name)
	var size int64
	err = b.withExportMounted(ctx, ic, func(mnt string) error {
		if err := os.MkdirAll(filepath.Join(mnt, snapDir), 0o700); err != nil {
			return err
		}
		// The archive name is derived from the CSI snapshot name, so it can
		// already exist: an idempotent retry against THIS source re-archives in
		// place (fine), but the same CSI name against a DIFFERENT source must be
		// an AlreadyExists error -- overwriting would silently destroy the other
		// source's snapshot. The provenance sidecar is the arbiter; a snapshot
		// without one (pre-provenance) is accepted as before.
		if data, rerr := os.ReadFile(filepath.Join(mnt, snapSrcFile(snapID))); rerr == nil {
			if prev := strings.TrimSpace(string(data)); prev != "" && prev != src.Name {
				return bardplugin.Errorf(bardplugin.CodeAlreadyExists,
					"nfs: snapshot %q already exists for a different source volume (%s)", req.Name, prev)
			}
		}
		archive := filepath.Join(mnt, snapArchive(snapID))
		if _, err := b.run.Run(ctx, "tar", "czf", archive, "-C", filepath.Join(mnt, src.Name), "."); err != nil {
			return fmt.Errorf("nfs: snapshot %s: %w", src.Name, err)
		}
		// Record what this tarball was taken from, so ListSnapshots can report a
		// source (the tarball name alone doesn't carry it). Best-effort.
		_ = os.WriteFile(filepath.Join(mnt, snapSrcFile(snapID)), []byte(src.Name), 0o600)
		if fi, serr := os.Stat(archive); serr == nil {
			size = fi.Size()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &bardplugin.CreateSnapshotResponse{
		Location:         ic.Export,
		Name:             snapID,
		SizeBytes:        size,
		CreationTimeUnix: time.Now().Unix(),
		ReadyToUse:       true,
	}, nil
}

func (b *Backend) DeleteSnapshot(ctx context.Context, req *bardplugin.DeleteSnapshotRequest) error {
	ic, err := b.instance(req.Snapshot.Instance)
	if err != nil {
		return err
	}
	return b.withExportMounted(ctx, ic, func(mnt string) error {
		_ = os.RemoveAll(filepath.Join(mnt, snapSrcFile(req.Snapshot.Name)))
		return os.RemoveAll(filepath.Join(mnt, snapArchive(req.Snapshot.Name)))
	})
}

// GetCapacity implements bardplugin.CapacityReporter: the bytes available on the
// instance's export, from statfs of a transient mount of it (NFS volumes are
// subdirectories sharing the export's backing filesystem). Implementing this
// interface makes Bard advertise CSI GetCapacity for nfs.
func (b *Backend) GetCapacity(ctx context.Context, req *bardplugin.GetCapacityRequest) (*bardplugin.GetCapacityResponse, error) {
	ic, err := b.instance(req.Instance)
	if err != nil {
		return nil, err
	}
	var avail int64
	err = b.withExportMounted(ctx, ic, func(mnt string) error {
		var st syscall.Statfs_t
		if err := syscall.Statfs(mnt, &st); err != nil {
			return bardplugin.Errorf(bardplugin.CodeInternal, "nfs: statfs %s: %v", mnt, err)
		}
		avail = int64(st.Bavail) * int64(st.Bsize)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &bardplugin.GetCapacityResponse{AvailableBytes: avail}, nil
}

// GetVolumeHealth implements bardplugin.HealthReporter: it reports a volume as
// abnormal when its subdirectory no longer exists on the export (deleted out of
// band). Implementing this interface makes Bard advertise CSI volume health.
func (b *Backend) GetVolumeHealth(ctx context.Context, req *bardplugin.GetVolumeHealthRequest) (*bardplugin.GetVolumeHealthResponse, error) {
	ic, err := b.instance(req.Volume.Instance)
	if err != nil {
		return nil, err
	}
	dir := req.Volume.Name
	var resp *bardplugin.GetVolumeHealthResponse
	err = b.withExportMounted(ctx, ic, func(mnt string) error {
		switch _, statErr := os.Stat(filepath.Join(mnt, dir)); {
		case statErr == nil:
			resp = &bardplugin.GetVolumeHealthResponse{Abnormal: false, Message: "nfs subdirectory " + dir + " is accessible"}
		case os.IsNotExist(statErr):
			resp = &bardplugin.GetVolumeHealthResponse{Abnormal: true, Message: "nfs subdirectory " + dir + " no longer exists"}
		default:
			return bardplugin.Errorf(bardplugin.CodeInternal, "nfs: stat %s: %v", dir, statErr)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// ---- listing (VolumeLister + SnapshotLister) -----------------------------

// ListVolumes (bardplugin.VolumeLister) enumerates the volume subdirectories on
// each instance's export (the nfs-subdir model assumes the export base is Bard's
// volume root). Excludes the .snapshots dir and onDelete=archive leftovers.
// CapacityBytes is 0 -- NFS subdirs share the backing fs and carry no quota.
func (b *Backend) ListVolumes(ctx context.Context, _ *bardplugin.ListVolumesRequest) (*bardplugin.ListVolumesResponse, error) {
	var entries []bardplugin.VolumeListEntry
	for instance, ic := range b.instances {
		err := b.withExportMounted(ctx, ic, func(mnt string) error {
			ents, err := os.ReadDir(mnt)
			if err != nil {
				return fmt.Errorf("nfs: readdir export %s: %w", ic.Export, err)
			}
			for _, e := range ents {
				n := e.Name()
				if !e.IsDir() || n == snapDir || strings.HasPrefix(n, "archived-") {
					continue
				}
				entries = append(entries, bardplugin.VolumeListEntry{
					Volume: bardplugin.VolumeRef{Instance: instance, Location: ic.Export, Name: n},
				})
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return &bardplugin.ListVolumesResponse{Entries: entries}, nil
}

// ListSnapshots (bardplugin.SnapshotLister) enumerates the snapshot tarballs under
// each export's .snapshots/, reading each one's source-provenance sidecar.
// Snapshots without a sidecar (pre-provenance, or a failed sidecar write) carry no
// source and are dropped by core's handle validation.
func (b *Backend) ListSnapshots(ctx context.Context, _ *bardplugin.ListSnapshotsRequest) (*bardplugin.ListSnapshotsResponse, error) {
	var entries []bardplugin.SnapshotListEntry
	for instance, ic := range b.instances {
		err := b.withExportMounted(ctx, ic, func(mnt string) error {
			ents, err := os.ReadDir(filepath.Join(mnt, snapDir))
			if err != nil {
				if os.IsNotExist(err) {
					return nil // no snapshots yet
				}
				return fmt.Errorf("nfs: readdir snapshots %s: %w", ic.Export, err)
			}
			for _, e := range ents {
				n := e.Name()
				if !strings.HasPrefix(n, "snap-") || !strings.HasSuffix(n, ".tar.gz") {
					continue
				}
				snapID := strings.TrimSuffix(n, ".tar.gz")
				entry := bardplugin.SnapshotListEntry{
					Snapshot:   bardplugin.VolumeRef{Instance: instance, Location: ic.Export, Name: snapID},
					ReadyToUse: true,
				}
				if fi, ierr := e.Info(); ierr == nil {
					entry.SizeBytes = fi.Size()
				}
				if data, rerr := os.ReadFile(filepath.Join(mnt, snapSrcFile(snapID))); rerr == nil {
					entry.SourceVolume = bardplugin.VolumeRef{Instance: instance, Location: ic.Export, Name: strings.TrimSpace(string(data))}
				}
				entries = append(entries, entry)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return &bardplugin.ListSnapshotsResponse{Entries: entries}, nil
}

// ---- node plane ----------------------------------------------------------

func (b *Backend) NodeStage(ctx context.Context, req *bardplugin.NodeStageRequest) error {
	ic, err := b.instance(req.Volume.Instance)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(req.StagingPath, 0o750); err != nil {
		return err
	}
	if b.mounted(ctx, req.StagingPath) { // idempotent: already staged on a retry
		return nil
	}
	opts := "vers=4.1"
	if len(req.MountFlags) > 0 {
		opts += "," + strings.Join(req.MountFlags, ",")
	}
	if req.Readonly {
		opts += ",ro"
	}
	src := ic.Server + ":" + filepath.Join(req.Volume.Location, req.Volume.Name)
	_, err = b.run.Run(ctx, "mount", "-t", "nfs", "-o", opts, src, req.StagingPath)
	return err
}

func (b *Backend) NodeUnstage(ctx context.Context, req *bardplugin.NodeUnstageRequest) error {
	if _, err := b.run.Run(ctx, "umount", req.StagingPath); err != nil && !isNotMounted(err) {
		return err
	}
	return nil
}

func (b *Backend) NodePublish(ctx context.Context, req *bardplugin.NodePublishRequest) error {
	if err := os.MkdirAll(req.TargetPath, 0o750); err != nil {
		return err
	}
	if b.mounted(ctx, req.TargetPath) { // idempotent: already published on a retry
		return nil
	}
	if _, err := b.run.Run(ctx, "mount", "--bind", req.StagingPath, req.TargetPath); err != nil {
		return err
	}
	if req.Readonly {
		_, err := b.run.Run(ctx, "mount", "-o", "remount,ro,bind", req.StagingPath, req.TargetPath)
		return err
	}
	return nil
}

func (b *Backend) NodeUnpublish(ctx context.Context, req *bardplugin.NodeUnpublishRequest) error {
	if _, err := b.run.Run(ctx, "umount", req.TargetPath); err != nil && !isNotMounted(err) {
		return err
	}
	return nil
}

func (b *Backend) NodeExpand(context.Context, *bardplugin.NodeExpandRequest) (*bardplugin.NodeExpandResponse, error) {
	return nil, bardplugin.Errorf(bardplugin.CodeInvalidArg, "nfs: expand not supported")
}
