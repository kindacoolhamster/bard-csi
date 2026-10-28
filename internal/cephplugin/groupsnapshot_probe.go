package cephplugin

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// Restoring ONE member of a group snapshot needs `rbd clone --snap-id`, which
// arrived in Ceph Squid v19.2.0. Bard has to know whether the CLUSTER supports it
// before it cuts a group snapshot the CO would then record as restorable.
//
// It is deliberately NOT a version check. `ceph versions` reports daemon versions
// (a proxy: vendor backports create false negatives) and needs mon read caps the
// documented least-privilege `profile rbd` user may not have. Checking the rbd
// CLI's own help is worse still: that binary ships inside Bard's plugin image,
// pinned at build time, so it would report a constant of our own container and
// tell us nothing about the operator's cluster.
//
// So this runs the real primitive chain end to end on a throwaway 4MiB image in
// the instance's own pool. It is the only check that exercises the OSD-side piece
// (cls_rbd support for cloning a non-user-type snapshot), and it incidentally
// catches pool-cap and image-feature problems a version number would not.
//
// Per INSTANCE, not per process: separate registered ceph-rbd instances can be
// separate clusters at separate versions.

const (
	// groupSnapProbePrefix names the probe's throwaway objects. Deterministic, so
	// concurrent controllers and retries converge on the same ones instead of
	// littering the pool -- every step tolerates "already exists".
	groupSnapProbePrefix = "csi-gsprobe-"
	// groupSnapProbeSnap is the probe group snapshot's name inside its own group.
	groupSnapProbeSnap = "probe"
	// groupSnapProbeRetry is how long a FAILED probe is trusted before being
	// re-run, so an operator's cluster upgrade is picked up without restarting the
	// plugin. A successful probe is cached for the process lifetime (a cluster
	// does not lose the feature).
	groupSnapProbeRetry = 5 * time.Minute
)

// groupSnapProbe is a cached probe outcome for one instance.
type groupSnapProbe struct {
	err error
	at  time.Time
}

// groupSnapshotSupported reports whether this instance's cluster can serve group
// snapshots whose members are individually restorable, running the probe once and
// caching the answer. A negative answer is a FailedPrecondition: the operator must
// upgrade Ceph before a retry can succeed.
func (b *Backend) groupSnapshotSupported(ctx context.Context, conn []string, instance, pool string) error {
	b.gsProbeMu.Lock()
	cached, ok := b.gsProbe[instance]
	b.gsProbeMu.Unlock()
	if ok && (cached.err == nil || time.Since(cached.at) < groupSnapProbeRetry) {
		return cached.err
	}

	var result error
	if err := b.probeGroupSnapshotClone(ctx, conn, pool); err != nil {
		result = bardplugin.Errorf(bardplugin.CodeFailedPrecondition,
			"ceph-rbd: instance %q cannot serve group snapshots whose members are individually restorable: %v"+
				" -- cloning from a group-member snapshot needs `rbd clone --snap-id` (Ceph Squid v19.2.0 or newer,"+
				" on the OSDs as well as the client). Upgrade the cluster, then retry.", instance, err)
	}
	b.gsProbeMu.Lock()
	b.gsProbe[instance] = groupSnapProbe{err: result, at: time.Now()}
	b.gsProbeMu.Unlock()
	return result
}

// probeGroupSnapshotClone runs the full primitive chain and cleans up after
// itself: create image -> group create -> group image add -> group snap create ->
// resolve the member snap id -> clone --snap-id -> tear it all down.
func (b *Backend) probeGroupSnapshotClone(ctx context.Context, conn []string, pool string) error {
	image := shortName(groupSnapProbePrefix, "image")
	group := shortName(groupSnapProbePrefix, "group")
	clone := shortName(groupSnapProbePrefix, "clone")
	imageSpec, groupSpec, cloneSpec := pool+"/"+image, pool+"/"+group, pool+"/"+clone
	// Always tear down, including on the failure paths -- a half-built probe left
	// in the pool would be indistinguishable from a user's objects.
	defer b.cleanupGroupSnapshotProbe(ctx, conn, imageSpec, groupSpec, cloneSpec)

	if _, err := b.run.Run(ctx, "rbd", appendArgs(conn, "create", imageSpec, "--size", "4")...); err != nil && !isAlreadyExists(err) {
		return fmt.Errorf("rbd create %s: %w", imageSpec, err)
	}
	if _, err := b.run.Run(ctx, "rbd", appendArgs(conn, "group", "create", groupSpec)...); err != nil && !isAlreadyExists(err) {
		return fmt.Errorf("rbd group create %s: %w", groupSpec, err)
	}
	if err := b.groupImageAdd(ctx, conn, groupSpec, bardplugin.VolumeRef{Location: pool, Name: image}); err != nil {
		return err
	}
	snapSpec := groupSpec + "@" + groupSnapProbeSnap
	if _, err := b.run.Run(ctx, "rbd", appendArgs(conn, "group", "snap", "create", snapSpec)...); err != nil && !isAlreadyExists(err) {
		return fmt.Errorf("rbd group snap create %s: %w", snapSpec, err)
	}
	snaps, err := b.listSnapsAll(ctx, conn, imageSpec)
	if err != nil {
		return err
	}
	snapID := int64(-1)
	for _, s := range snaps {
		if s.Namespace.Type == "group" && s.Namespace.Group == group && s.Namespace.GroupSnap == groupSnapProbeSnap {
			snapID = s.ID
			break
		}
	}
	if snapID < 0 {
		return fmt.Errorf("group snapshot %s produced no member snapshot for %s", snapSpec, imageSpec)
	}
	args := groupMemberCloneArgs(conn, pool, image, snapID, pool, clone)
	if _, err := b.run.Run(ctx, "rbd", args...); err != nil && !isAlreadyExists(err) {
		return fmt.Errorf("rbd clone --snap-id %d %s: %w", snapID, imageSpec, err)
	}
	return nil
}

// cleanupGroupSnapshotProbe reaps the probe objects in dependency order. Entirely
// best-effort: the probe's answer does not depend on the teardown succeeding, and
// the next probe reuses whatever is left rather than duplicating it.
func (b *Backend) cleanupGroupSnapshotProbe(ctx context.Context, conn []string, imageSpec, groupSpec, cloneSpec string) {
	_, _ = b.run.Run(ctx, "rbd", appendArgs(conn, "rm", cloneSpec)...)
	_, _ = b.run.Run(ctx, "rbd", appendArgs(conn, "group", "snap", "rm", groupSpec+"@"+groupSnapProbeSnap)...)
	_, _ = b.run.Run(ctx, "rbd", appendArgs(conn, "group", "remove", groupSpec)...)
	_, _ = b.run.Run(ctx, "rbd", appendArgs(conn, "rm", imageSpec)...)
}

// groupMemberCloneArgs builds `rbd clone` for a GROUP MEMBER snapshot source.
// The positional "pool/image@snap" form addresses user-type snapshots only, which
// is precisely why --snap-id exists; a group member has no usable name anyway (see
// groupMemberSigil). Clone format 2 for the same reason ordinary restores use it:
// v1 demands a protected parent snapshot, and the plugin protects nothing.
func groupMemberCloneArgs(conn []string, pool, image string, snapID int64, destPool, dest string) []string {
	srcPool, srcNamespace := splitLocator(pool)
	dstPool, dstNamespace := splitLocator(destPool)
	args := appendArgs(conn, "clone", "--pool", srcPool)
	if srcNamespace != "" {
		args = append(args, "--namespace", srcNamespace)
	}
	args = append(args, "--image", image, "--snap-id", strconv.FormatInt(snapID, 10), "--dest-pool", dstPool)
	if dstNamespace != "" {
		args = append(args, "--dest-namespace", dstNamespace)
	}
	return append(args, "--dest", dest, "--rbd-default-clone-format", "2")
}

// splitLocator is the inverse of locator: a Location is "pool" or "pool/namespace".
func splitLocator(loc string) (pool, namespace string) {
	if p, ns, ok := strings.Cut(loc, "/"); ok {
		return p, ns
	}
	return loc, ""
}
