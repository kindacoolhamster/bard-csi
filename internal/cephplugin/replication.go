package cephplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// VolumeReplication (csi-addons) mirrors an RBD image to a peer cluster for DR,
// using snapshot-based image-mode mirroring (`rbd mirror image ...`). Bard serves
// the real csi-addons Replication API, so a ceph-csi user's VolumeReplication +
// VolumeReplicationClass resources work unchanged; a DR orchestrator (Ramen)
// drives promote/demote for failover.
//
// Credentials: the per-instance provisioning user suffices for every mirror op
// (verified against Ceph -- `profile rbd` can enable/disable/promote/demote/resync
// and read status), so these use the normal connArgs path (request secrets honoured
// as a fallback) rather than a separate privileged user like NetworkFence needs.

// paramSchedulingInterval is the VolumeReplicationClass parameter giving the mirror
// snapshot schedule (e.g. "5m", "1h"); empty means no automatic schedule.
const paramSchedulingInterval = "schedulingInterval"

// paramFlattenMode (VolumeReplicationClass, ceph-csi parity) controls what happens
// when replication is enabled on a snapshot-restored volume -- a COW clone, which
// snapshot-based mirroring refuses while its parent is unmirrored ("mirroring is
// not enabled for the parent", verified live). Bard's default auto-detects that
// case, flattens the clone OUT OF BAND (dedup'd), and fails the attempt with a
// clear message so the csi-addons controller's retry succeeds once the image is
// parent-free -- the reconcile is never blocked on a multi-minute data copy
// (ceph-csi's flattenMode=force flattens inline). "never" opts out and surfaces
// the raw error.
const paramFlattenMode = "flattenMode"

func (b *Backend) EnableVolumeReplication(ctx context.Context, req *bardplugin.EnableReplicationRequest) error {
	conn, cleanup, spec, err := b.replConn(req.Volume.Instance, req.Volume, req.Secrets)
	if err != nil {
		return err
	}
	defer cleanup()
	// Idempotent: enabling an already-mirrored image is success.
	if _, err := b.run.Run(ctx, "rbd", appendArgs(conn, "mirror", "image", "enable", spec, "snapshot")...); err != nil {
		switch {
		case isAlreadyMirrored(err):
			// success
		case isParentNotMirrored(err) && req.Parameters[paramFlattenMode] != "never":
			b.flattenForMirror(req.Volume.Instance, req.Secrets, spec)
			return fmt.Errorf("ceph-rbd: %s is a clone whose parent is not mirrored; flattening it in the background, mirroring will enable on a retry: %w", spec, err)
		default:
			return fmt.Errorf("ceph-rbd: mirror image enable %s: %w", spec, err)
		}
	}
	// Snapshot-based mirroring needs a schedule to keep producing mirror snapshots.
	if interval := req.Parameters[paramSchedulingInterval]; interval != "" {
		pool, ns, image := splitImageSpec(req.Volume)
		args := []string{"mirror", "snapshot", "schedule", "add", "--pool", pool, "--image", image, interval}
		if ns != "" {
			args = append(args, "--namespace", ns)
		}
		if _, err := b.run.Run(ctx, "rbd", appendArgs(conn, args...)...); err != nil && !isAlreadyExists(err) {
			return fmt.Errorf("ceph-rbd: mirror snapshot schedule add %s: %w", spec, err)
		}
	}
	return nil
}

func (b *Backend) DisableVolumeReplication(ctx context.Context, req *bardplugin.DisableReplicationRequest) error {
	conn, cleanup, spec, err := b.replConn(req.Volume.Instance, req.Volume, req.Secrets)
	if err != nil {
		return err
	}
	defer cleanup()
	// Drop the schedule first (best effort -- it may not exist), then disable.
	pool, ns, image := splitImageSpec(req.Volume)
	rmArgs := []string{"mirror", "snapshot", "schedule", "remove", "--pool", pool, "--image", image}
	if ns != "" {
		rmArgs = append(rmArgs, "--namespace", ns)
	}
	_, _ = b.run.Run(ctx, "rbd", appendArgs(conn, rmArgs...)...)
	// --force so disable also works on a demoted (secondary) image -- a real DR path
	// is deleting the VolumeReplication on the secondary cluster, where the image is
	// non-primary and a plain disable is rejected.
	if _, err := b.run.Run(ctx, "rbd", appendArgs(conn, "mirror", "image", "disable", spec, "--force")...); err != nil {
		if !isNotMirrored(err) {
			return fmt.Errorf("ceph-rbd: mirror image disable %s: %w", spec, err)
		}
	}
	return nil
}

func (b *Backend) PromoteVolume(ctx context.Context, req *bardplugin.PromoteVolumeRequest) error {
	conn, cleanup, spec, err := b.replConn(req.Volume.Instance, req.Volume, req.Secrets)
	if err != nil {
		return err
	}
	defer cleanup()
	args := []string{"mirror", "image", "promote", spec}
	if req.Force {
		args = append(args, "--force")
	}
	if _, err := b.run.Run(ctx, "rbd", appendArgs(conn, args...)...); err != nil {
		if !isAlreadyPrimary(err) {
			return fmt.Errorf("ceph-rbd: mirror image promote %s: %w", spec, err)
		}
	}
	return nil
}

func (b *Backend) DemoteVolume(ctx context.Context, req *bardplugin.DemoteVolumeRequest) error {
	conn, cleanup, spec, err := b.replConn(req.Volume.Instance, req.Volume, req.Secrets)
	if err != nil {
		return err
	}
	defer cleanup()
	if _, err := b.run.Run(ctx, "rbd", appendArgs(conn, "mirror", "image", "demote", spec)...); err != nil {
		if !isAlreadySecondary(err) {
			return fmt.Errorf("ceph-rbd: mirror image demote %s: %w", spec, err)
		}
	}
	return nil
}

func (b *Backend) ResyncVolume(ctx context.Context, req *bardplugin.ResyncVolumeRequest) (*bardplugin.ResyncVolumeResponse, error) {
	conn, cleanup, spec, err := b.replConn(req.Volume.Instance, req.Volume, req.Secrets)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	if _, err := b.run.Run(ctx, "rbd", appendArgs(conn, "mirror", "image", "resync", spec)...); err != nil {
		return nil, fmt.Errorf("ceph-rbd: mirror image resync %s: %w", spec, err)
	}
	// Resync is asynchronous; report ready once the image is back up+replaying.
	st, _ := b.mirrorStatus(ctx, conn, spec)
	return &bardplugin.ResyncVolumeResponse{Ready: strings.Contains(st.State, "up") && strings.Contains(st.State, "replaying")}, nil
}

func (b *Backend) GetVolumeReplicationInfo(ctx context.Context, req *bardplugin.ReplicationInfoRequest) (*bardplugin.ReplicationInfoResponse, error) {
	conn, cleanup, spec, err := b.replConn(req.Volume.Instance, req.Volume, req.Secrets)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return &bardplugin.ReplicationInfoResponse{LastSyncTimeUnix: b.lastMirrorSnapshotUnix(ctx, conn, spec)}, nil
}

// replConn resolves the cluster, builds the rbd connection args, and returns the
// image spec (pool[/namespace]/image). Shared by every replication op.
func (b *Backend) replConn(instance string, vol bardplugin.VolumeRef, secrets map[string]string) ([]string, func(), string, error) {
	cc, err := b.cluster(instance)
	if err != nil {
		return nil, func() {}, "", err
	}
	conn, cleanup, err := b.connArgs(cc, instance, secrets)
	if err != nil {
		return nil, func() {}, "", err
	}
	return conn, cleanup, vol.Location + "/" + vol.Name, nil
}

// splitImageSpec breaks a VolumeRef into pool, rados namespace (may be ""), and
// image name -- the form `rbd mirror snapshot schedule` wants as flags. Location
// is pool or pool/namespace (see locator()).
func splitImageSpec(vol bardplugin.VolumeRef) (pool, namespace, image string) {
	pool = vol.Location
	if i := strings.IndexByte(vol.Location, '/'); i >= 0 {
		pool, namespace = vol.Location[:i], vol.Location[i+1:]
	}
	return pool, namespace, vol.Name
}

// mirrorStat is the slice of `rbd mirror image status --format json` Bard needs
// (only the daemon-reported state, used to decide resync readiness).
type mirrorStat struct {
	State string `json:"state"`
}

func (b *Backend) mirrorStatus(ctx context.Context, conn []string, spec string) (mirrorStat, error) {
	var st mirrorStat
	out, err := b.run.Run(ctx, "rbd", appendArgs(conn, "mirror", "image", "status", spec, "--format", "json")...)
	if err != nil {
		return st, fmt.Errorf("ceph-rbd: mirror image status %s: %w", spec, err)
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		return st, fmt.Errorf("ceph-rbd: parse mirror status %s: %w", spec, err)
	}
	return st, nil
}

// snapTimeLayout is rbd's snapshot timestamp format ("Thu Jun 25 23:12:57 2026"),
// rendered in the cluster's local time (no zone in the string).
const snapTimeLayout = "Mon Jan _2 15:04:05 2006"

// lastMirrorSnapshotUnix returns the Unix time of the most recent COMPLETE mirror
// snapshot of the image -- the last successful sync point. Unlike the daemon-
// reported status description (absent on a cluster running no local rbd-mirror
// daemon, e.g. the primary in one-way mirroring), the mirror snapshots are present
// on both primary and secondary, so this is the reliable RPO source. 0 = no sync yet.
func (b *Backend) lastMirrorSnapshotUnix(ctx context.Context, conn []string, spec string) int64 {
	out, err := b.run.Run(ctx, "rbd", appendArgs(conn, "snap", "ls", spec, "--all", "--format", "json")...)
	if err != nil {
		return 0
	}
	var snaps []struct {
		Timestamp string `json:"timestamp"`
		Namespace struct {
			Type     string `json:"type"`
			Complete bool   `json:"complete"`
		} `json:"namespace"`
	}
	if json.Unmarshal([]byte(out), &snaps) != nil {
		return 0
	}
	var latest int64
	for _, s := range snaps {
		if s.Namespace.Type != "mirror" || !s.Namespace.Complete {
			continue
		}
		t, err := time.ParseInLocation(snapTimeLayout, strings.TrimSpace(s.Timestamp), time.Local)
		if err == nil && t.Unix() > latest {
			latest = t.Unix()
		}
	}
	return latest
}

// Mirror-op idempotency classifiers: rbd returns a non-zero exit with these
// substrings when the requested state already holds, which we treat as success.
func isAlreadyMirrored(err error) bool { return errContains(err, "already", "enabled") }

// isParentNotMirrored matches librbd's refusal to enable snapshot-based mirroring
// on a COW clone: "image_enable: mirroring is not enabled for the parent"
// (captured live from Ceph 20.2).
func isParentNotMirrored(err error) bool { return errContains(err, "parent", "mirror") }
func isNotMirrored(err error) bool       { return errContains(err, "not", "mirror") }
func isAlreadyPrimary(err error) bool    { return errContains(err, "primary") }
func isAlreadySecondary(err error) bool {
	return errContains(err, "non-primary") || errContains(err, "secondary")
}

func errContains(err error, subs ...string) bool {
	s := strings.ToLower(errString(err))
	for _, sub := range subs {
		if !strings.Contains(s, strings.ToLower(sub)) {
			return false
		}
	}
	return true
}
