package cephplugin

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/kindacoolhamster/bard-csi/internal/fakerun"
	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

func gsBackend(run Runner) *Backend {
	return New(map[string]ClusterConfig{
		"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin"},
		"west": {Monitors: []string{"10.0.0.20:6789"}, Pool: "otherpool", UserID: "admin"},
	}, "", "", run)
}

// gsVolumes provisions n volumes on the east instance and returns their refs.
func gsVolumes(t *testing.T, b *Backend, names ...string) []bardplugin.VolumeRef {
	t.Helper()
	ctx := context.Background()
	refs := make([]bardplugin.VolumeRef, 0, len(names))
	for _, name := range names {
		v, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
			Name: name, CapacityBytes: 1 << 30, Instance: "east",
		})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		refs = append(refs, bardplugin.VolumeRef{Instance: "east", Location: v.Location, Name: v.Name})
	}
	return refs
}

func codeOf(err error) bardplugin.ErrorCode {
	var se *bardplugin.StatusError
	if errors.As(err, &se) {
		return se.Code
	}
	return ""
}

// The rbd group is keyed on the MEMBER SET, not the CO's group snapshot name: the
// same set in any order (with duplicates) yields one group name, a different set
// yields another, and instance + pool are part of the identity.
func TestGroupSnapshotNameDerivation(t *testing.T) {
	mk := func(instance, pool, name string) bardplugin.VolumeRef {
		return bardplugin.VolumeRef{Instance: instance, Location: pool, Name: name}
	}
	a := mk("east", "replicapool", "csi-vol-a")
	bb := mk("east", "replicapool", "csi-vol-b")

	base := groupSnapshotName(groupSnapGroupPrefix, []bardplugin.VolumeRef{a, bb})
	if !strings.HasPrefix(base, groupSnapGroupPrefix) {
		t.Fatalf("group name must carry the %s prefix, got %q", groupSnapGroupPrefix, base)
	}
	// The csi-addons VolumeGroup feature must never collide with this one.
	if strings.HasPrefix(base, groupNamePrefix) {
		t.Fatalf("group snapshot groups must not share the csi-addons group prefix: %q", base)
	}
	for name, members := range map[string][]bardplugin.VolumeRef{
		"reordered":  {bb, a},
		"duplicated": {a, bb, a, bb},
	} {
		if got := groupSnapshotName(groupSnapGroupPrefix, members); got != base {
			t.Errorf("%s member set must derive the same group: %q != %q", name, got, base)
		}
	}
	for name, members := range map[string][]bardplugin.VolumeRef{
		"different member":   {a, mk("east", "replicapool", "csi-vol-c")},
		"different pool":     {mk("east", "otherpool", "csi-vol-a"), mk("east", "otherpool", "csi-vol-b")},
		"different instance": {mk("west", "replicapool", "csi-vol-a"), mk("west", "replicapool", "csi-vol-b")},
		"subset":             {a},
	} {
		if got := groupSnapshotName(groupSnapGroupPrefix, members); got == base {
			t.Errorf("%s must derive a different group, got the same %q", name, got)
		}
	}
}

// A member snapshot handle carries the member's numeric snap id behind the "@#"
// sigil, and decodes back to exactly that -- while an ordinary "image@snap"
// handle is never mistaken for one.
func TestGroupMemberHandleRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		image string
		id    int64
	}{{"csi-vol-abc", 1}, {"csi-vol-abc", 4096}, {"img-with-@-in-name", 7}} {
		name := groupMemberSnapName(tc.image, tc.id)
		image, id, ok := parseGroupMemberSnapName(name)
		if !ok || image != tc.image || id != tc.id {
			t.Errorf("round trip of %q: got (%q, %d, %v)", name, image, id, ok)
		}
		if !isGroupMemberSnapName(name) {
			t.Errorf("%q must be recognised as a group member", name)
		}
	}
	for _, name := range []string{
		"csi-vol-abc@csi-snap-0123456789abcdef", // an ordinary snapshot
		"csi-vol-abc",                           // a volume
		"csi-vol-abc@#",                         // no id
		"csi-vol-abc@#notanumber",
		"@#12", // no image
	} {
		if isGroupMemberSnapName(name) {
			t.Errorf("%q must NOT be read as a group member handle", name)
		}
	}
}

// The full lifecycle against the fake cluster: one atomic cut across two volumes,
// members addressed by snap id, get echoes them, delete removes the group snapshot
// and then disbands the now-empty group without touching the member images.
func TestGroupSnapshotLifecycle(t *testing.T) {
	run := &recordRunner{inner: fakerun.New()}
	b := gsBackend(run)
	ctx := context.Background()
	members := gsVolumes(t, b, "vol-a", "vol-b")

	created, err := b.CreateVolumeGroupSnapshot(ctx, &bardplugin.CreateVolumeGroupSnapshotRequest{
		Name: "gs-1", SourceVolumes: members,
	})
	if err != nil {
		t.Fatal(err)
	}
	// ONE atomic cut, not a snapshot per volume: that is the write-order guarantee.
	if n := countCalls(run, "group snap create replicapool/"+groupSnapGroupPrefix); n != 1 {
		t.Fatalf("expected exactly one `group snap create`, got %d: %v", n, run.calls)
	}
	if run.ran("snap create replicapool/csi-vol") {
		t.Fatalf("group members must not be cut as ordinary per-volume snapshots: %v", run.calls)
	}
	group, snap, ok := groupSnapSpec(created.GroupSnapshot.Name)
	if !ok || !strings.HasPrefix(group, groupSnapGroupPrefix) || !strings.HasPrefix(snap, groupSnapSnapPrefix) {
		t.Fatalf("group snapshot handle %q must be <group>@<snapshot>", created.GroupSnapshot.Name)
	}
	if len(created.Snapshots) != 2 || !created.ReadyToUse {
		t.Fatalf("expected 2 ready members, got %+v", created.Snapshots)
	}
	for _, m := range created.Snapshots {
		image, id, ok := parseGroupMemberSnapName(m.Snapshot.Name)
		if !ok || id <= 0 {
			t.Fatalf("member %q must encode a numeric snap id", m.Snapshot.Name)
		}
		if image != m.SourceVolume.Name || m.Snapshot.Instance != "east" || m.SizeBytes == 0 {
			t.Fatalf("member snapshot does not describe its source: %+v", m)
		}
	}

	// A retry of the same (member set, name) is idempotent: same objects, no second cut.
	again, err := b.CreateVolumeGroupSnapshot(ctx, &bardplugin.CreateVolumeGroupSnapshotRequest{
		Name: "gs-1", SourceVolumes: []bardplugin.VolumeRef{members[1], members[0]},
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.GroupSnapshot.Name != created.GroupSnapshot.Name {
		t.Fatalf("retry must resolve the same group snapshot: %q != %q", again.GroupSnapshot.Name, created.GroupSnapshot.Name)
	}
	if n := countCalls(run, "group snap create replicapool/"+groupSnapGroupPrefix); n != 2 {
		t.Fatalf("the retry should re-issue an idempotent create, got %d calls", n)
	}

	got, err := b.GetVolumeGroupSnapshot(ctx, &bardplugin.GetVolumeGroupSnapshotRequest{GroupSnapshot: created.GroupSnapshot})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Snapshots) != 2 || !got.ReadyToUse {
		t.Fatalf("get must report both members ready, got %+v", got)
	}
	if memberNames(got.Snapshots) != memberNames(created.Snapshots) {
		t.Fatalf("get must echo the created member handles: %v vs %v", memberNames(got.Snapshots), memberNames(created.Snapshots))
	}

	// A SECOND group snapshot of the same member set shares the one rbd group --
	// the retention case that keying the group on the CO name would have broken.
	second, err := b.CreateVolumeGroupSnapshot(ctx, &bardplugin.CreateVolumeGroupSnapshotRequest{
		Name: "gs-2", SourceVolumes: members,
	})
	if err != nil {
		t.Fatalf("a second group snapshot of the same volumes must succeed: %v", err)
	}
	secondGroup, secondSnap, _ := groupSnapSpec(second.GroupSnapshot.Name)
	if secondGroup != group || secondSnap == snap {
		t.Fatalf("second group snapshot should reuse the group with a new snapshot name, got %q", second.GroupSnapshot.Name)
	}

	// Deleting the first leaves the group alive (it still holds the second).
	if err := b.DeleteVolumeGroupSnapshot(ctx, &bardplugin.DeleteVolumeGroupSnapshotRequest{GroupSnapshot: created.GroupSnapshot}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.GetVolumeGroupSnapshot(ctx, &bardplugin.GetVolumeGroupSnapshotRequest{GroupSnapshot: created.GroupSnapshot}); codeOf(err) != bardplugin.CodeNotFound {
		t.Fatalf("a deleted group snapshot must Get as NotFound, got %v", err)
	}
	if _, err := b.GetVolumeGroupSnapshot(ctx, &bardplugin.GetVolumeGroupSnapshotRequest{GroupSnapshot: second.GroupSnapshot}); err != nil {
		t.Fatalf("the surviving group snapshot must still resolve: %v", err)
	}

	// Deleting the last one disbands the group. The member images survive.
	if err := b.DeleteVolumeGroupSnapshot(ctx, &bardplugin.DeleteVolumeGroupSnapshotRequest{GroupSnapshot: second.GroupSnapshot}); err != nil {
		t.Fatal(err)
	}
	if !run.ran("group remove replicapool/" + group) {
		t.Fatalf("the emptied group must be disbanded: %v", run.calls)
	}
	for _, m := range members {
		if _, err := b.imageInfo(ctx, nil, m.Location+"/"+m.Name); err != nil {
			t.Fatalf("member image %s must survive group snapshot deletion: %v", m.Name, err)
		}
	}
	// Idempotent: deleting an already-deleted group snapshot succeeds.
	if err := b.DeleteVolumeGroupSnapshot(ctx, &bardplugin.DeleteVolumeGroupSnapshotRequest{GroupSnapshot: second.GroupSnapshot}); err != nil {
		t.Fatalf("repeat delete must be a no-op, got %v", err)
	}
}

// A member snapshot restores into a new volume through `rbd clone --snap-id` --
// the positional pool/image@snap form cannot address a non-user-type snapshot.
func TestRestoreFromGroupSnapshotMember(t *testing.T) {
	run := &recordRunner{inner: fakerun.New()}
	b := gsBackend(run)
	ctx := context.Background()
	members := gsVolumes(t, b, "vol-a", "vol-b")

	created, err := b.CreateVolumeGroupSnapshot(ctx, &bardplugin.CreateVolumeGroupSnapshotRequest{
		Name: "gs-1", SourceVolumes: members,
	})
	if err != nil {
		t.Fatal(err)
	}
	member := created.Snapshots[0].Snapshot
	_, snapID, _ := parseGroupMemberSnapName(member.Name)

	restored, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: "restored", CapacityBytes: 1 << 30, Instance: "east",
		SourceSnapshot: &bardplugin.VolumeRef{Instance: "east", Location: member.Location, Name: member.Name},
	})
	if err != nil {
		t.Fatalf("restore from a group member must succeed: %v", err)
	}
	want := fmt.Sprintf("clone --pool replicapool --image %s --snap-id %d --dest-pool replicapool --dest %s",
		created.Snapshots[0].SourceVolume.Name, snapID, restored.Name)
	if !run.ran(want) {
		t.Fatalf("expected %q; calls: %v", want, run.calls)
	}
	if !run.ran("--rbd-default-clone-format 2") {
		t.Fatalf("the member clone must use clone format 2: %v", run.calls)
	}
	// The restored volume is an ordinary, independent image from here on.
	if parent, _, _ := b.imageParent(ctx, nil, restored.Location+"/"+restored.Name); parent == "" {
		t.Fatal("the restore should be a COW clone of the member snapshot")
	}
}

// CSI forbids reclaiming a group member through DeleteSnapshot, and the member's
// handle names no literal rbd snapshot anyway -- so refuse rather than issue a
// nonsense `rbd snap rm pool/image@#42`.
func TestDeleteSnapshotRefusesGroupMember(t *testing.T) {
	run := &recordRunner{inner: fakerun.New()}
	b := gsBackend(run)
	err := b.DeleteSnapshot(context.Background(), &bardplugin.DeleteSnapshotRequest{
		Snapshot: bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: groupMemberSnapName("csi-vol-a", 42)},
	})
	if codeOf(err) != bardplugin.CodeFailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v (%v)", codeOf(err), err)
	}
	if run.ran("snap rm") {
		t.Fatalf("nothing should have been issued to rbd: %v", run.calls)
	}
}

// A group snapshot cannot span backend instances: no storage system can cut one
// write-order-consistent snapshot across independent clusters.
func TestGroupSnapshotRejectsCrossInstance(t *testing.T) {
	run := &recordRunner{inner: fakerun.New()}
	b := gsBackend(run)
	_, err := b.CreateVolumeGroupSnapshot(context.Background(), &bardplugin.CreateVolumeGroupSnapshotRequest{
		Name: "gs-1",
		SourceVolumes: []bardplugin.VolumeRef{
			{Instance: "east", Location: "replicapool", Name: "csi-vol-a"},
			{Instance: "west", Location: "otherpool", Name: "csi-vol-b"},
		},
	})
	if codeOf(err) != bardplugin.CodeFailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v (%v)", codeOf(err), err)
	}
	if run.ran("group create") {
		t.Fatalf("a rejected request must not touch the cluster: %v", run.calls)
	}
}

// An image belongs to at most one rbd group, so a member set that OVERLAPS a set
// already held by a live group snapshot cannot be grouped. rbd reports the
// blocked add as "(17) File exists" -- which the already-exists classifier reads
// as success -- so without the post-add membership check the group snapshot would
// silently omit a requested volume. Fail loudly, name the blocker, leave nothing.
func TestGroupSnapshotRejectsOverlappingMemberSet(t *testing.T) {
	run := &recordRunner{inner: fakerun.New()}
	b := gsBackend(run)
	ctx := context.Background()
	vols := gsVolumes(t, b, "vol-a", "vol-b", "vol-c")
	a, bVol, c := vols[0], vols[1], vols[2]

	held, err := b.CreateVolumeGroupSnapshot(ctx, &bardplugin.CreateVolumeGroupSnapshotRequest{
		Name: "gs-1", SourceVolumes: []bardplugin.VolumeRef{a, bVol},
	})
	if err != nil {
		t.Fatal(err)
	}
	heldGroup, _, _ := groupSnapSpec(held.GroupSnapshot.Name)

	_, err = b.CreateVolumeGroupSnapshot(ctx, &bardplugin.CreateVolumeGroupSnapshotRequest{
		Name: "gs-2", SourceVolumes: []bardplugin.VolumeRef{a, c},
	})
	if codeOf(err) != bardplugin.CodeFailedPrecondition {
		t.Fatalf("an overlapping member set must be FailedPrecondition, got %v (%v)", codeOf(err), err)
	}
	if !strings.Contains(err.Error(), "replicapool/"+a.Name) {
		t.Fatalf("the error must name the volume that could not join: %v", err)
	}
	if !strings.Contains(err.Error(), heldGroup) {
		t.Fatalf("the error must name the group already holding it (%s): %v", heldGroup, err)
	}
	// No snapshot may be leaked, and the group built for the failed request is gone.
	newGroup := groupSnapshotName(groupSnapGroupPrefix, []bardplugin.VolumeRef{a, c})
	if _, err := b.groupSnapList(ctx, nil, "replicapool/"+newGroup); !isNotFound(err) {
		t.Fatalf("the failed request's group must be cleaned up, got %v", err)
	}
	// The existing group snapshot is untouched.
	if _, err := b.GetVolumeGroupSnapshot(ctx, &bardplugin.GetVolumeGroupSnapshotRequest{GroupSnapshot: held.GroupSnapshot}); err != nil {
		t.Fatalf("the pre-existing group snapshot must survive: %v", err)
	}
	// And volume c, which had joined the doomed group, is free to be grouped again.
	if _, err := b.CreateVolumeGroupSnapshot(ctx, &bardplugin.CreateVolumeGroupSnapshotRequest{
		Name: "gs-3", SourceVolumes: []bardplugin.VolumeRef{c},
	}); err != nil {
		t.Fatalf("cleanup must release the members it added: %v", err)
	}
}

// The version gate runs the real primitive chain -- including the clone that only
// Ceph Squid v19.2.0+ can serve -- once per instance, and cleans up after itself.
func TestGroupSnapshotVersionProbe(t *testing.T) {
	run := &recordRunner{inner: fakerun.New()}
	b := gsBackend(run)
	ctx := context.Background()
	members := gsVolumes(t, b, "vol-a")

	if _, err := b.CreateVolumeGroupSnapshot(ctx, &bardplugin.CreateVolumeGroupSnapshotRequest{
		Name: "gs-1", SourceVolumes: members,
	}); err != nil {
		t.Fatal(err)
	}
	probeImage := shortName(groupSnapProbePrefix, "image")
	probeGroup := shortName(groupSnapProbePrefix, "group")
	probeClone := shortName(groupSnapProbePrefix, "clone")
	for _, want := range []string{
		"create replicapool/" + probeImage + " --size 4",
		"group create replicapool/" + probeGroup,
		"group image add replicapool/" + probeGroup + " replicapool/" + probeImage,
		"group snap create replicapool/" + probeGroup + "@" + groupSnapProbeSnap,
		"snap ls replicapool/" + probeImage + " --all",
		"clone --pool replicapool --image " + probeImage + " --snap-id",
	} {
		if !run.ran(want) {
			t.Errorf("probe must run %q; calls: %v", want, run.calls)
		}
	}
	// Everything the probe made is reaped -- it must not litter the pool.
	for _, spec := range []string{"replicapool/" + probeImage, "replicapool/" + probeClone} {
		if _, err := b.imageInfo(ctx, nil, spec); !errors.Is(err, errImageNotFound) {
			t.Errorf("probe object %s must be cleaned up, got %v", spec, err)
		}
	}
	if _, err := b.groupSnapList(ctx, nil, "replicapool/"+probeGroup); !isNotFound(err) {
		t.Errorf("the probe group must be cleaned up, got %v", err)
	}

	// The answer is cached: a second group snapshot re-probes nothing.
	before := countCalls(run, "--snap-id")
	if _, err := b.CreateVolumeGroupSnapshot(ctx, &bardplugin.CreateVolumeGroupSnapshotRequest{
		Name: "gs-2", SourceVolumes: members,
	}); err != nil {
		t.Fatal(err)
	}
	if after := countCalls(run, "--snap-id"); after != before {
		t.Errorf("a positive probe must be cached, but it ran again (%d -> %d)", before, after)
	}
}

// oldCephRunner is a cluster too old for `rbd clone --snap-id`: every other group
// primitive works, but cloning a group-member snapshot is rejected -- exactly the
// shape a pre-Squid cluster presents, and invisible to any client-side check.
type oldCephRunner struct {
	inner *fakerun.Runner
}

func (r *oldCephRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	for _, a := range args {
		if a == "--snap-id" {
			return "", fmt.Errorf("rbd: unrecognised option '--snap-id'")
		}
	}
	return r.inner.Run(ctx, name, args...)
}

// A cluster that cannot restore an individual member must refuse the group
// snapshot outright: recording one the CO believes is restorable is worse than
// not having the feature. The refusal is FailedPrecondition -- the operator has
// to upgrade Ceph, and no retry of the same request can succeed before that.
func TestGroupSnapshotRejectedOnOldCluster(t *testing.T) {
	inner := fakerun.New()
	run := &recordRunner{inner: inner}
	b := gsBackend(&oldCephRunner{inner: inner})
	ctx := context.Background()
	// Provision through the permissive runner so only the probe meets the old cluster.
	members := gsVolumes(t, gsBackend(run), "vol-a")

	_, err := b.CreateVolumeGroupSnapshot(ctx, &bardplugin.CreateVolumeGroupSnapshotRequest{
		Name: "gs-1", SourceVolumes: members,
	})
	if codeOf(err) != bardplugin.CodeFailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v (%v)", codeOf(err), err)
	}
	if !strings.Contains(err.Error(), "--snap-id") {
		t.Fatalf("the error should name the missing primitive: %v", err)
	}
	// Nothing was cut: no group snapshot exists for that member set.
	group := groupSnapshotName(groupSnapGroupPrefix, members)
	if _, err := b.groupSnapList(ctx, nil, "replicapool/"+group); !isNotFound(err) {
		t.Fatalf("a refused request must leave no group behind, got %v", err)
	}
}

// A custom snapshotNamePrefix cannot start with '#': that would let a CO-chosen
// name masquerade as the "@#<id>" group-member encoding.
func TestSnapshotNamePrefixRejectsMemberSigil(t *testing.T) {
	if _, err := namePrefix(paramSnapshotNamePrefix, "#snap-", "csi-snap-"); codeOf(err) != bardplugin.CodeInvalidArg {
		t.Fatalf("a '#'-leading prefix must be InvalidArgument, got %v", err)
	}
	if got, err := namePrefix(paramSnapshotNamePrefix, "team#snap-", "csi-snap-"); err != nil || got != "team#snap-" {
		t.Fatalf("a '#' elsewhere in the prefix is fine: %q, %v", got, err)
	}
}

func countCalls(r *recordRunner, sub string) int {
	n := 0
	for _, c := range r.calls {
		if strings.Contains(strings.Join(c, " "), sub) {
			n++
		}
	}
	return n
}

// memberNames is order-independent: member ORDER is not part of the contract
// (create reports the requested order, get the group's listing order), so only
// the set of handles is compared. Core sorts before its own comparison too.
func memberNames(members []bardplugin.GroupSnapshotMember) string {
	names := make([]string, 0, len(members))
	for _, m := range members {
		names = append(names, m.Snapshot.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}
