package cephplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// CSI VolumeGroupSnapshot for Ceph RBD, built on `rbd group snap create` -- an
// atomic, quiesced cut across every image in an `rbd group`. That primitive is
// what supplies the write-order consistency guarantee CSI requires (and it is
// the reason the retired sequential-per-volume implementation was withdrawn
// rather than patched: no loop over CreateSnapshot can provide it).
//
// The other half of the contract -- being able to restore ONE member -- became
// possible in Ceph Squid v19.2.0, which added `rbd clone --snap-id` for cloning
// from non-user-type snapshots. A group member snapshot is exactly such a
// snapshot, so both halves now come from the same primitive.
//
// Distinct from the csi-addons VolumeGroup feature in volumegroup.go, which
// manages long-lived consistency groups as first-class objects. The two use
// separate rbd group name prefixes so their groups never collide in a pool.

const (
	// groupSnapGroupPrefix names the rbd group that backs CSI group snapshots.
	// Deliberately different from csi-addons' csi-group- prefix.
	groupSnapGroupPrefix = "csi-gsnap-"
	// groupSnapSnapPrefix names one group snapshot WITHIN such a group. It only
	// has to be unique inside the one group, so it is derived from the CO's name.
	groupSnapSnapPrefix = "csi-gs-"
	// groupMemberSigil marks a snapshot handle as a group MEMBER carrying a
	// numeric rbd snap id rather than a snapshot name: "image@#<id>". Ceph's own
	// name for a member snapshot (".group.<poolid>_<groupid>_<snapid>") is
	// unstable -- `rbd group snap rm` renames a still-referenced member into a
	// trash namespace -- while the numeric id survives that rename. The '#' sits
	// where a snapshot name would start, so volumeid's parser is untouched
	// (Handle.Name is opaque to it) and ordinary snapshot handles are unaffected:
	// Bard-minted snapshot names are <prefix><hex> and namePrefix() rejects a '#'
	// in a custom prefix.
	groupMemberSigil = "@#"
)

// groupSnapshotName derives the rbd group name from the MEMBER SET, not from the
// CO's group snapshot name.
//
// This is load-bearing. An rbd image belongs to at most one group, while
// CreateVolumeGroupSnapshotRequest.Name is a per-call idempotency key -- a backup
// schedule takes many differently-named group snapshots of the SAME volumes over
// time. Keying the group on the CO name would make the second such request try to
// move already-grouped images into a new group and fail. Keyed on the member set,
// one group holds many group snapshots over its lifetime, which is the normal
// retention case.
//
// The input is normalized (sorted, deduplicated, instance and pool included) so
// the same set always resolves to the same group whatever order the CO lists it in.
func groupSnapshotName(prefix string, members []bardplugin.VolumeRef) string {
	keys := make([]string, 0, len(members))
	seen := make(map[string]bool, len(members))
	for _, m := range members {
		k := m.Instance + "|" + m.Location + "/" + m.Name
		if seen[k] {
			continue
		}
		seen[k] = true
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return shortName(prefix, strings.Join(keys, "\n"))
}

// groupMemberSnapName encodes a member snapshot handle name: "image@#<snap-id>".
func groupMemberSnapName(image string, snapID int64) string {
	return image + groupMemberSigil + strconv.FormatInt(snapID, 10)
}

// parseGroupMemberSnapName decodes a member snapshot handle name back into the
// image and its numeric snap id. ok is false for an ordinary "image@snap" name.
func parseGroupMemberSnapName(name string) (image string, snapID int64, ok bool) {
	image, rest, found := strings.Cut(name, groupMemberSigil)
	if !found || image == "" {
		return "", 0, false
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		return "", 0, false
	}
	return image, id, true
}

// isGroupMemberSnapName reports whether a snapshot handle names a group member.
func isGroupMemberSnapName(name string) bool {
	_, _, ok := parseGroupMemberSnapName(name)
	return ok
}

// groupSnapSpec splits a group snapshot handle's Name ("group@snap") into its parts.
func groupSnapSpec(name string) (group, snap string, ok bool) {
	group, snap, ok = strings.Cut(name, "@")
	if !ok || group == "" || snap == "" {
		return "", "", false
	}
	return group, snap, true
}

// CreateVolumeGroupSnapshot cuts one write-order-consistent snapshot across every
// source volume. Idempotent on the (member set, CO name) pair: both the rbd group
// and the group snapshot inside it are derived deterministically, so a retry
// re-resolves the same objects instead of cutting a second snapshot.
func (b *Backend) CreateVolumeGroupSnapshot(ctx context.Context, req *bardplugin.CreateVolumeGroupSnapshotRequest) (*bardplugin.VolumeGroupSnapshotResponse, error) {
	if len(req.SourceVolumes) == 0 {
		return nil, bardplugin.Errorf(bardplugin.CodeInvalidArg, "ceph-rbd: a group snapshot needs at least one source volume")
	}
	instance := req.SourceVolumes[0].Instance
	for _, m := range req.SourceVolumes {
		if m.Instance != instance {
			return nil, bardplugin.Errorf(bardplugin.CodeFailedPrecondition,
				"ceph-rbd: volumes %q and %q are in different backend instances; one write-order-consistent snapshot cannot span Ceph clusters",
				instance, m.Instance)
		}
	}
	cc, err := b.cluster(instance)
	if err != nil {
		return nil, err
	}
	pool := cc.Pool
	if pool == "" {
		pool = req.SourceVolumes[0].Location
	}
	if pool == "" {
		return nil, bardplugin.Errorf(bardplugin.CodeInvalidArg, "ceph-rbd: no pool for group snapshot")
	}
	conn, cleanup, err := b.connArgs(cc, instance, req.Secrets)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	// Refuse before touching anything if this cluster cannot restore a group
	// member: a group snapshot whose members are unrestorable is worse than no
	// group snapshot, because the CO records it as usable.
	if err := b.groupSnapshotSupported(ctx, conn, instance, pool); err != nil {
		return nil, err
	}

	group := groupSnapshotName(groupSnapGroupPrefix, req.SourceVolumes)
	groupSpec := pool + "/" + group
	if _, err := b.run.Run(ctx, "rbd", appendArgs(conn, "group", "create", groupSpec)...); err != nil && !isAlreadyExists(err) {
		return nil, fmt.Errorf("ceph-rbd: rbd group create %s: %w", groupSpec, err)
	}
	for _, m := range req.SourceVolumes {
		if err := b.groupImageAdd(ctx, conn, groupSpec, m); err != nil {
			return nil, err
		}
	}

	// groupImageAdd tolerates "already exists" -- but rbd reports an image that
	// belongs to a DIFFERENT group with "(17) File exists" too, which that
	// classifier cannot tell apart. Without this check the group snapshot would
	// silently omit a requested volume. Verify membership is exactly what was
	// asked for before cutting anything.
	if err := b.verifyGroupMembers(ctx, conn, instance, pool, groupSpec, req.SourceVolumes); err != nil {
		return nil, err
	}

	snap := shortName(groupSnapSnapPrefix, req.Name)
	snapSpec := groupSpec + "@" + snap
	if _, err := b.run.Run(ctx, "rbd", appendArgs(conn, "group", "snap", "create", snapSpec)...); err != nil && !isAlreadyExists(err) {
		return nil, fmt.Errorf("ceph-rbd: rbd group snap create %s: %w", snapSpec, err)
	}

	members, err := b.groupSnapshotMembers(ctx, conn, instance, req.SourceVolumes, group, snap)
	if err != nil {
		return nil, err
	}
	return &bardplugin.VolumeGroupSnapshotResponse{
		GroupSnapshot:    bardplugin.VolumeRef{Instance: instance, Location: pool, Name: group + "@" + snap},
		Snapshots:        members,
		CreationTimeUnix: time.Now().Unix(),
		ReadyToUse:       true,
	}, nil
}

// verifyGroupMembers asserts the group's actual membership equals want exactly.
// On a mismatch it names the offending volume (and, best-effort, the group that
// already holds it) and tears down a group this call created and nothing else
// depends on -- CSI requires no snapshots be leaked when a group create fails.
func (b *Backend) verifyGroupMembers(ctx context.Context, conn []string, instance, pool, groupSpec string, want []bardplugin.VolumeRef) error {
	have, err := b.groupImageList(ctx, conn, instance, groupSpec)
	if err != nil {
		return err
	}
	haveSet := make(map[string]bool, len(have))
	for _, m := range have {
		haveSet[m.Location+"/"+m.Name] = true
	}
	wantSet := make(map[string]bool, len(want))
	var missing []string
	for _, m := range want {
		spec := m.Location + "/" + m.Name
		wantSet[spec] = true
		if !haveSet[spec] {
			missing = append(missing, spec)
		}
	}
	var extra []string
	for spec := range haveSet {
		if !wantSet[spec] {
			extra = append(extra, spec)
		}
	}
	if len(missing) == 0 && len(extra) == 0 {
		return nil
	}
	sort.Strings(missing)
	sort.Strings(extra)
	b.cleanupUnusedGroup(ctx, conn, groupSpec, want)
	if len(missing) > 0 {
		blocker := b.groupHolding(ctx, conn, instance, pool, missing[0], groupSpec)
		return bardplugin.Errorf(bardplugin.CodeFailedPrecondition,
			"ceph-rbd: volume %s could not join consistency group %s%s -- an rbd image belongs to at most one group, so these volumes cannot be group-snapshotted together while that holds",
			missing[0], groupSpec, blocker)
	}
	return bardplugin.Errorf(bardplugin.CodeFailedPrecondition,
		"ceph-rbd: consistency group %s also holds %s, which was not part of this group snapshot request",
		groupSpec, strings.Join(extra, ", "))
}

// groupHolding best-effort finds which OTHER Bard group snapshot group already
// holds imgSpec, so the failure names the actual blocker. Returns "" when it
// cannot tell; this runs only on the error path, so a miss just costs detail.
func (b *Backend) groupHolding(ctx context.Context, conn []string, instance, pool, imgSpec, exclude string) string {
	out, err := b.run.Run(ctx, "rbd", appendArgs(conn, "group", "list", pool, "--format", "json")...)
	if err != nil {
		return ""
	}
	var names []string
	if err := json.Unmarshal([]byte(out), &names); err != nil {
		return ""
	}
	sort.Strings(names)
	for _, name := range names {
		spec := pool + "/" + name
		if spec == exclude {
			continue
		}
		members, err := b.groupImageList(ctx, conn, instance, spec)
		if err != nil {
			continue
		}
		for _, m := range members {
			if m.Location+"/"+m.Name != imgSpec {
				continue
			}
			if snaps, err := b.groupSnapList(ctx, conn, spec); err == nil && len(snaps) > 0 {
				held := make([]string, 0, len(snaps))
				for _, s := range snaps {
					held = append(held, s.Name)
				}
				return fmt.Sprintf(" (it is held in group %s by group snapshot(s) %s)", spec, strings.Join(held, ", "))
			}
			return fmt.Sprintf(" (it is held in group %s)", spec)
		}
	}
	return ""
}

// cleanupUnusedGroup best-effort disbands a group left behind by a failed create,
// so a rejected request leaks nothing. It refuses to touch a group that holds any
// group snapshot -- that would be an existing, valid group snapshot of this same
// member set, and dismantling it would destroy real data. Entirely best-effort: a
// leftover EMPTY group costs nothing and the next attempt reuses it.
func (b *Backend) cleanupUnusedGroup(ctx context.Context, conn []string, groupSpec string, added []bardplugin.VolumeRef) {
	if snaps, err := b.groupSnapList(ctx, conn, groupSpec); err != nil || len(snaps) > 0 {
		return
	}
	for _, m := range added {
		_ = b.groupImageRemove(ctx, conn, groupSpec, m)
	}
	_, _ = b.run.Run(ctx, "rbd", appendArgs(conn, "group", "remove", groupSpec)...)
}

// groupSnapshotMembers resolves each source volume's member snapshot in the named
// group snapshot. A group member snapshot has no stable name, so the handle
// carries its numeric snap id (see groupMemberSigil). A member with no snapshot
// for this cut is an error: the whole point of the group primitive is that the
// cut is all-or-nothing, so a partial result must never be reported as ready.
func (b *Backend) groupSnapshotMembers(ctx context.Context, conn []string, instance string, sources []bardplugin.VolumeRef, group, snap string) ([]bardplugin.GroupSnapshotMember, error) {
	members := make([]bardplugin.GroupSnapshotMember, 0, len(sources))
	for _, src := range sources {
		spec := src.Location + "/" + src.Name
		snaps, err := b.listSnapsAll(ctx, conn, spec)
		if err != nil {
			return nil, err
		}
		found := false
		for _, s := range snaps {
			if s.Namespace.Type != "group" || s.Namespace.Group != group || s.Namespace.GroupSnap != snap {
				continue
			}
			size := s.Size
			if size == 0 {
				sizeMiB, err := b.imageInfo(ctx, conn, spec)
				if err != nil {
					return nil, err
				}
				size = sizeMiB * mib
			}
			members = append(members, bardplugin.GroupSnapshotMember{
				Snapshot: bardplugin.VolumeRef{
					Instance: instance,
					Location: src.Location,
					Name:     groupMemberSnapName(src.Name, s.ID),
				},
				SourceVolume:     src,
				SizeBytes:        size,
				CreationTimeUnix: time.Now().Unix(),
				ReadyToUse:       true,
			})
			found = true
			break
		}
		if !found {
			return nil, fmt.Errorf("ceph-rbd: group snapshot %s/%s@%s has no snapshot of member %s", src.Location, group, snap, spec)
		}
	}
	return members, nil
}

// DeleteVolumeGroupSnapshot removes the group snapshot and, once the group holds
// no more of them, disbands the group itself. Member images are never touched.
//
// `rbd group snap rm` is safe unconditionally: a member with no clone children is
// removed outright, and one with a live clone is moved to the trash namespace and
// auto-purged when its last clone goes -- the same lazy delete ordinary
// snapshot-with-clones deletion already relies on.
//
// Idempotent in both stages: an already-removed group snapshot still runs the
// empty-group collection, which is what lets a retry finish a delete that was
// interrupted between the two steps.
func (b *Backend) DeleteVolumeGroupSnapshot(ctx context.Context, req *bardplugin.DeleteVolumeGroupSnapshotRequest) error {
	group, snap, ok := groupSnapSpec(req.GroupSnapshot.Name)
	if !ok {
		return bardplugin.Errorf(bardplugin.CodeInvalidArg, "ceph-rbd: malformed group snapshot name %q (want group@snapshot)", req.GroupSnapshot.Name)
	}
	cc, err := b.cluster(req.GroupSnapshot.Instance)
	if err != nil {
		return err
	}
	conn, cleanup, err := b.connArgs(cc, req.GroupSnapshot.Instance, req.Secrets)
	if err != nil {
		return err
	}
	defer cleanup()

	groupSpec := req.GroupSnapshot.Location + "/" + group
	if _, err := b.run.Run(ctx, "rbd", appendArgs(conn, "group", "snap", "rm", groupSpec+"@"+snap)...); err != nil && !isNotFound(err) {
		return fmt.Errorf("ceph-rbd: rbd group snap rm %s@%s: %w", groupSpec, snap, err)
	}
	snaps, err := b.groupSnapList(ctx, conn, groupSpec)
	if err != nil {
		if isNotFound(err) {
			return nil // the group is already gone; nothing left to collect
		}
		return err
	}
	if len(snaps) > 0 {
		return nil // other group snapshots still live in this group
	}
	// `rbd group remove` succeeds with member images still attached; it disbands
	// the group without deleting them.
	if _, err := b.run.Run(ctx, "rbd", appendArgs(conn, "group", "remove", groupSpec)...); err != nil && !isNotFound(err) {
		return fmt.Errorf("ceph-rbd: rbd group remove %s: %w", groupSpec, err)
	}
	return nil
}

// GetVolumeGroupSnapshot reports a group snapshot and its current members, or
// CodeNotFound when it no longer exists (core needs that distinction for CSI's
// delete/get idempotency rules).
func (b *Backend) GetVolumeGroupSnapshot(ctx context.Context, req *bardplugin.GetVolumeGroupSnapshotRequest) (*bardplugin.VolumeGroupSnapshotResponse, error) {
	group, snap, ok := groupSnapSpec(req.GroupSnapshot.Name)
	if !ok {
		return nil, bardplugin.Errorf(bardplugin.CodeNotFound, "ceph-rbd: malformed group snapshot name %q", req.GroupSnapshot.Name)
	}
	cc, err := b.cluster(req.GroupSnapshot.Instance)
	if err != nil {
		return nil, err
	}
	conn, cleanup, err := b.connArgs(cc, req.GroupSnapshot.Instance, req.Secrets)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	groupSpec := req.GroupSnapshot.Location + "/" + group
	snaps, err := b.groupSnapList(ctx, conn, groupSpec)
	if err != nil {
		if isNotFound(err) {
			return nil, bardplugin.Errorf(bardplugin.CodeNotFound, "ceph-rbd: group snapshot %s@%s does not exist", groupSpec, snap)
		}
		return nil, err
	}
	ready := false
	found := false
	for _, s := range snaps {
		if s.Name == snap {
			found, ready = true, s.complete()
			break
		}
	}
	if !found {
		return nil, bardplugin.Errorf(bardplugin.CodeNotFound, "ceph-rbd: group snapshot %s@%s does not exist", groupSpec, snap)
	}
	sources, err := b.groupImageList(ctx, conn, req.GroupSnapshot.Instance, groupSpec)
	if err != nil {
		return nil, err
	}
	members, err := b.groupSnapshotMembers(ctx, conn, req.GroupSnapshot.Instance, sources, group, snap)
	if err != nil {
		return nil, err
	}
	return &bardplugin.VolumeGroupSnapshotResponse{
		GroupSnapshot: req.GroupSnapshot,
		Snapshots:     members,
		ReadyToUse:    ready,
	}, nil
}

// rbdGroupSnap is one entry of `rbd group snap list --format json`. rbd has
// spelled the name field both ways across releases, so accept either.
type rbdGroupSnap struct {
	Name     string `json:"name"`
	Snapshot string `json:"snapshot"`
	State    string `json:"state"`
}

// complete reports whether the group snapshot cut finished. rbd leaves an
// interrupted cut in an incomplete state; an unknown/absent state is treated as
// complete rather than failing on a spelling this Ceph release does not use.
func (s rbdGroupSnap) complete() bool {
	switch strings.ToLower(s.State) {
	case "", "ok", "complete":
		return true
	}
	return false
}

// groupSnapList lists a group's snapshots. A missing group surfaces as a
// not-found error for the caller to classify.
func (b *Backend) groupSnapList(ctx context.Context, conn []string, groupSpec string) ([]rbdGroupSnap, error) {
	out, err := b.run.Run(ctx, "rbd", appendArgs(conn, "group", "snap", "list", groupSpec, "--format", "json")...)
	if err != nil {
		if isNotFound(err) {
			return nil, err
		}
		return nil, fmt.Errorf("ceph-rbd: rbd group snap list %s: %w", groupSpec, err)
	}
	var raw []rbdGroupSnap
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("ceph-rbd: parse group snap list %s: %w", groupSpec, err)
	}
	for i := range raw {
		if raw[i].Name == "" {
			raw[i].Name = raw[i].Snapshot
		}
	}
	return raw, nil
}

// rbdSnapAll is one entry of `rbd snap ls --all --format json`, which unlike the
// plain listing includes non-user snapshots and their namespace. A group member
// snapshot carries namespace {"type":"group","group":...,"group snap":...} (that
// key has a literal space, not an underscore -- verified live against Ceph
// Tentacle v20.2.0), and its numeric id -- the only identity stable across
// `rbd group snap rm`'s rename into the trash namespace.
type rbdSnapAll struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	Namespace struct {
		Type      string `json:"type"`
		Pool      string `json:"pool"`
		Group     string `json:"group"`
		GroupSnap string `json:"group snap"` // real rbd emits a literal space in this key, not an underscore -- verified live vs Ceph Tentacle v20.2.0
	} `json:"namespace"`
}

func (b *Backend) listSnapsAll(ctx context.Context, conn []string, spec string) ([]rbdSnapAll, error) {
	out, err := b.run.Run(ctx, "rbd", appendArgs(conn, "snap", "ls", spec, "--all", "--format", "json")...)
	if err != nil {
		return nil, fmt.Errorf("ceph-rbd: rbd snap ls --all %s: %w", spec, err)
	}
	var raw []rbdSnapAll
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("ceph-rbd: parse snap ls --all %s: %w", spec, err)
	}
	return raw, nil
}
