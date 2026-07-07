package cephplugin

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kindacoolhamster/bard-csi/internal/fakerun"
	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

func cloneTestBackend(run Runner) *Backend {
	return New(map[string]ClusterConfig{
		"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin"},
	}, "", "", run)
}

// A PVC-PVC clone must be a temp-snapshot + COW clone flattened out of band --
// never `rbd cp`: a full copy killed by the CSI deadline leaves a partially
// copied destination at full size, which a retry would accept as an idempotent
// hit (silent corruption), and its duration scales unboundedly with volume size.
func TestVolumeCloneIsCOWNotFullCopy(t *testing.T) {
	run := &recordRunner{inner: fakerun.New()}
	b := cloneTestBackend(run)
	b.flattenAsync = false
	ctx := context.Background()

	src, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{Name: "src", CapacityBytes: 1 << 30, Instance: "east"})
	if err != nil {
		t.Fatal(err)
	}
	srcRef := bardplugin.VolumeRef{Instance: "east", Location: src.Location, Name: src.Name}
	clone, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: "copy", CapacityBytes: 1 << 30, Instance: "east", SourceVolume: &srcRef,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.ran(" cp ") {
		t.Fatalf("PVC-PVC clone must not use rbd cp; calls: %v", run.calls)
	}
	srcSpec := src.Location + "/" + src.Name
	if !run.ran("snap create "+srcSpec+"@clonetmp-") || !run.ran("--rbd-default-clone-format 2") {
		t.Fatalf("expected temp snapshot + clone-v2; calls: %v", run.calls)
	}
	if !run.ran("snap rm " + srcSpec + "@clonetmp-") {
		t.Fatalf("temp snapshot must be removed; calls: %v", run.calls)
	}
	// The out-of-band flatten (synchronous here) severed the parent link, so the
	// clone is fully independent -- rbd cp semantics.
	cloneSpec := clone.Location + "/" + clone.Name
	if p, _, err := b.imageParent(ctx, nil, cloneSpec); err != nil || p != "" {
		t.Fatalf("clone must be flattened (no parent), got parent=%q err=%v", p, err)
	}
	// ...and the source is immediately deletable (the trashed temp snap released).
	if err := b.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{Volume: srcRef}); err != nil {
		t.Fatalf("source must be deletable right after the clone: %v", err)
	}
	if _, err := b.imageInfo(ctx, nil, cloneSpec); err != nil {
		t.Fatalf("clone must survive the source delete: %v", err)
	}
}

// A clone starts at its SOURCE's size; a clone/restore to a larger PVC must grow
// the image to the request and report the real capacity.
func TestCloneGrowsToRequestedSize(t *testing.T) {
	run := &recordRunner{inner: fakerun.New()}
	b := cloneTestBackend(run)
	b.flattenAsync = false
	ctx := context.Background()

	src, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{Name: "src", CapacityBytes: 1 << 30, Instance: "east"})
	if err != nil {
		t.Fatal(err)
	}
	srcRef := bardplugin.VolumeRef{Instance: "east", Location: src.Location, Name: src.Name}

	// PVC-PVC clone to 2GiB from a 1GiB source.
	clone, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: "bigcopy", CapacityBytes: 2 << 30, Instance: "east", SourceVolume: &srcRef,
	})
	if err != nil {
		t.Fatal(err)
	}
	if clone.CapacityBytes != 2<<30 {
		t.Fatalf("clone capacity = %d, want %d", clone.CapacityBytes, int64(2<<30))
	}
	if got, _ := b.imageInfo(ctx, nil, clone.Location+"/"+clone.Name); got != 2048 {
		t.Fatalf("clone image size = %dMiB, want 2048", got)
	}

	// Snapshot restore to 2GiB from the same 1GiB source.
	snap, err := b.CreateSnapshot(ctx, &bardplugin.CreateSnapshotRequest{Name: "s-grow", SourceVolume: srcRef})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: "bigrestore", CapacityBytes: 2 << 30, Instance: "east",
		SourceSnapshot: &bardplugin.VolumeRef{Instance: "east", Location: snap.Location, Name: snap.Name},
	})
	if err != nil {
		t.Fatal(err)
	}
	if restored.CapacityBytes != 2<<30 {
		t.Fatalf("restore capacity = %d, want %d", restored.CapacityBytes, int64(2<<30))
	}
	if got, _ := b.imageInfo(ctx, nil, restored.Location+"/"+restored.Name); got != 2048 {
		t.Fatalf("restored image size = %dMiB, want 2048", got)
	}
}

// A clone-create resumed after a crash converges: the destination exists at the
// source's size (the resize/snap-rm/flatten never ran), and the retried
// CreateVolume must finish the recipe instead of failing AlreadyExists. A plain
// create (no content source) stays strict on a size mismatch.
func TestCloneCreateResumesAfterInterruption(t *testing.T) {
	run := &recordRunner{inner: fakerun.New()}
	b := cloneTestBackend(run)
	b.flattenAsync = false
	ctx := context.Background()

	src, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{Name: "src", CapacityBytes: 1 << 30, Instance: "east"})
	if err != nil {
		t.Fatal(err)
	}
	srcRef := bardplugin.VolumeRef{Instance: "east", Location: src.Location, Name: src.Name}
	srcSpec := src.Location + "/" + src.Name

	// Simulate the crashed first attempt: temp snap + clone exist, nothing after.
	tmpSnap := srcSpec + "@" + shortName(tmpClonePrefix, "copy2")
	destSpec := src.Location + "/" + shortName(volNamePrefix, "copy2")
	for _, c := range [][]string{
		{"snap", "create", tmpSnap},
		{"clone", tmpSnap, destSpec, "--rbd-default-clone-format", "2"},
	} {
		if _, err := run.Run(ctx, "rbd", c...); err != nil {
			t.Fatal(err)
		}
	}

	clone, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: "copy2", CapacityBytes: 2 << 30, Instance: "east", SourceVolume: &srcRef,
	})
	if err != nil {
		t.Fatalf("resumed clone create must converge, got %v", err)
	}
	if clone.CapacityBytes != 2<<30 {
		t.Fatalf("resumed clone capacity = %d, want %d", clone.CapacityBytes, int64(2<<30))
	}
	if p, _, _ := b.imageParent(ctx, nil, destSpec); p != "" {
		t.Fatalf("resumed clone must be flattened, still has parent %q", p)
	}
	if err := b.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{Volume: srcRef}); err != nil {
		t.Fatalf("source must be deletable after the resumed clone: %v", err)
	}

	// Plain create with the same name but a different size stays AlreadyExists.
	if _, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: "copy2", CapacityBytes: 4 << 30, Instance: "east",
	}); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Fatalf("plain create over an existing image at another size must fail AlreadyExists, got %v", err)
	}
}

// DeleteVolume blocked by clone-linked trashed snapshots (a lost out-of-band
// flatten, or a deleted snapshot with live restores) must kick the children's
// flatten so the retry converges without operator action.
func TestSourceDeleteConvergesByFlatteningChildren(t *testing.T) {
	run := fakerun.New()
	b := cloneTestBackend(run)
	b.flattenAsync = false
	ctx := context.Background()

	src, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{Name: "src", CapacityBytes: 1 << 30, Instance: "east"})
	if err != nil {
		t.Fatal(err)
	}
	srcRef := bardplugin.VolumeRef{Instance: "east", Location: src.Location, Name: src.Name}
	snap, err := b.CreateSnapshot(ctx, &bardplugin.CreateSnapshotRequest{Name: "s1", SourceVolume: srcRef})
	if err != nil {
		t.Fatal(err)
	}
	clone, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: "restore", CapacityBytes: 1 << 30, Instance: "east",
		SourceSnapshot: &bardplugin.VolumeRef{Instance: "east", Location: snap.Location, Name: snap.Name},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Snapshot deleted while the restore depends on it: v2-trashed on the source.
	if err := b.DeleteSnapshot(ctx, &bardplugin.DeleteSnapshotRequest{
		Snapshot: bardplugin.VolumeRef{Instance: "east", Location: snap.Location, Name: snap.Name},
	}); err != nil {
		t.Fatal(err)
	}
	// First source delete fails (linked clones) but flattens the child inline.
	if err := b.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{Volume: srcRef}); err == nil {
		t.Fatal("delete blocked by a clone-linked trashed snapshot must fail for the retry")
	}
	cloneSpec := clone.Location + "/" + clone.Name
	if p, _, _ := b.imageParent(ctx, nil, cloneSpec); p != "" {
		t.Fatalf("blocked delete must flatten the child; parent still %q", p)
	}
	// The retry converges, with the restored clone alive and independent.
	if err := b.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{Volume: srcRef}); err != nil {
		t.Fatalf("source delete retry must converge after the child flatten: %v", err)
	}
	if _, err := b.imageInfo(ctx, nil, cloneSpec); err != nil {
		t.Fatalf("restored clone must survive: %v", err)
	}
}

// A deleted snapshot's CSI name must be reusable against a different source (the
// in-memory uniqueness index has to release it on delete).
func TestSnapshotNameReusableAfterDelete(t *testing.T) {
	run := fakerun.New()
	b := cloneTestBackend(run)
	ctx := context.Background()

	mkVol := func(name string) bardplugin.VolumeRef {
		v, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{Name: name, CapacityBytes: 1 << 30, Instance: "east"})
		if err != nil {
			t.Fatal(err)
		}
		return bardplugin.VolumeRef{Instance: "east", Location: v.Location, Name: v.Name}
	}
	volA, volB := mkVol("a"), mkVol("b")

	snap, err := b.CreateSnapshot(ctx, &bardplugin.CreateSnapshotRequest{Name: "s-reuse", SourceVolume: volA})
	if err != nil {
		t.Fatal(err)
	}
	// While it exists, the name is taken for any other source.
	if _, err := b.CreateSnapshot(ctx, &bardplugin.CreateSnapshotRequest{Name: "s-reuse", SourceVolume: volB}); err == nil {
		t.Fatal("same CSI name against a different source must fail while the snapshot exists")
	}
	if err := b.DeleteSnapshot(ctx, &bardplugin.DeleteSnapshotRequest{
		Snapshot: bardplugin.VolumeRef{Instance: "east", Location: snap.Location, Name: snap.Name},
	}); err != nil {
		t.Fatal(err)
	}
	// After the delete the name is free again.
	if _, err := b.CreateSnapshot(ctx, &bardplugin.CreateSnapshotRequest{Name: "s-reuse", SourceVolume: volB}); err != nil {
		t.Fatalf("a deleted snapshot's name must be reusable, got %v", err)
	}
}

// NodeStage must grow the filesystem to the device: a clone/restore into a
// larger volume carries the SOURCE's smaller filesystem inside a right-sized
// image (live-confirmed: a 2Gi clone of a 1Gi source mounted with a ~1Gi fs).
func TestNodeStageGrowsFilesystem(t *testing.T) {
	run := &recordRunner{inner: fakerun.New()}
	b := cloneTestBackend(run)
	b.flattenAsync = false
	ctx := context.Background()

	src, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{Name: "src", CapacityBytes: 1 << 30, Instance: "east"})
	if err != nil {
		t.Fatal(err)
	}
	srcRef := bardplugin.VolumeRef{Instance: "east", Location: src.Location, Name: src.Name}
	clone, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: "bigcopy", CapacityBytes: 2 << 30, Instance: "east", SourceVolume: &srcRef,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.NodeStage(ctx, &bardplugin.NodeStageRequest{
		Volume:      bardplugin.VolumeRef{Instance: "east", Location: clone.Location, Name: clone.Name},
		StagingPath: t.TempDir(),
		FsType:      "ext4",
	}); err != nil {
		t.Fatal(err)
	}
	if !run.ran("resize2fs") {
		t.Fatalf("NodeStage must grow the filesystem to the device; calls: %v", run.calls)
	}
}

// An existing image is an idempotent CreateVolume hit only when the retry names
// the SAME content source; a different snapshot/volume/none is an incompatible
// AlreadyExists per CSI (previously accepted, silently handing back a volume
// with the wrong lineage).
func TestCreateVolumeVerifiesContentSource(t *testing.T) {
	run := fakerun.New()
	b := cloneTestBackend(run)
	b.flattenAsync = false
	ctx := context.Background()

	mkVol := func(name string) bardplugin.VolumeRef {
		v, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{Name: name, CapacityBytes: 1 << 30, Instance: "east"})
		if err != nil {
			t.Fatal(err)
		}
		return bardplugin.VolumeRef{Instance: "east", Location: v.Location, Name: v.Name}
	}
	volA, volB := mkVol("a"), mkVol("b")
	snapA, err := b.CreateSnapshot(ctx, &bardplugin.CreateSnapshotRequest{Name: "sa", SourceVolume: volA})
	if err != nil {
		t.Fatal(err)
	}
	snapB, err := b.CreateSnapshot(ctx, &bardplugin.CreateSnapshotRequest{Name: "sb", SourceVolume: volB})
	if err != nil {
		t.Fatal(err)
	}
	snapARef := &bardplugin.VolumeRef{Instance: "east", Location: snapA.Location, Name: snapA.Name}
	snapBRef := &bardplugin.VolumeRef{Instance: "east", Location: snapB.Location, Name: snapB.Name}

	restore := &bardplugin.CreateVolumeRequest{
		Name: "restore", CapacityBytes: 1 << 30, Instance: "east", SourceSnapshot: snapARef,
	}
	if _, err := b.CreateVolume(ctx, restore); err != nil {
		t.Fatal(err)
	}
	// Identical retry: idempotent hit.
	if _, err := b.CreateVolume(ctx, restore); err != nil {
		t.Fatalf("identical retry must be an idempotent hit, got %v", err)
	}
	// Same name, different snapshot source: incompatible.
	if _, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: "restore", CapacityBytes: 1 << 30, Instance: "east", SourceSnapshot: snapBRef,
	}); err == nil || !strings.Contains(err.Error(), "content source") {
		t.Fatalf("retry with a different source must be AlreadyExists, got %v", err)
	}
	// Same name, no source at all: also incompatible.
	if _, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: "restore", CapacityBytes: 1 << 30, Instance: "east",
	}); err == nil || !strings.Contains(err.Error(), "content source") {
		t.Fatalf("plain-create retry over a clone must be AlreadyExists, got %v", err)
	}
}

// metaFailRunner injects a transient (non-not-found) failure into image-meta
// reads, as a mon outage would.
type metaFailRunner struct {
	inner *fakerun.Runner
	fail  bool
}

func (r *metaFailRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if r.fail {
		for i, a := range args {
			if a == "image-meta" && i+1 < len(args) && args[i+1] == "get" {
				return "", errors.New("rbd: connection timed out")
			}
		}
	}
	return r.inner.Run(ctx, name, args...)
}

// DeleteVolume must FAIL (for a retry) when the image-meta reads backing the
// static guard / KMS-id lookup fail transiently -- a misread "" would reap an
// admin-owned static image or leak an external KMS key.
func TestDeleteVolumeFailsOnTransientMetaError(t *testing.T) {
	run := &metaFailRunner{inner: fakerun.New()}
	b := cloneTestBackend(run)
	ctx := context.Background()

	vol, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{Name: "v", CapacityBytes: 1 << 30, Instance: "east"})
	if err != nil {
		t.Fatal(err)
	}
	ref := bardplugin.VolumeRef{Instance: "east", Location: vol.Location, Name: vol.Name}

	run.fail = true
	if err := b.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{Volume: ref}); err == nil {
		t.Fatal("delete must fail when the static/KMS meta read fails transiently")
	}
	if _, err := b.imageInfo(ctx, nil, vol.Location+"/"+vol.Name); err != nil {
		t.Fatalf("image must survive the failed delete: %v", err)
	}
	run.fail = false
	if err := b.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{Volume: ref}); err != nil {
		t.Fatalf("delete retry must succeed once the meta read recovers: %v", err)
	}
}
