package cephfsplugin

import (
	"context"
	"strings"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// failCloneRunner reports every subvolume clone as terminally failed.
type failCloneRunner struct {
	calls [][]string
}

func (r *failCloneRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if strings.Contains(strings.Join(args, " "), "clone status") {
		return `{"status":{"state":"failed"}}`, nil
	}
	return "", nil
}

func (r *failCloneRunner) ran(parts ...string) bool {
	want := strings.Join(parts, " ")
	for _, c := range r.calls {
		if strings.Contains(strings.Join(c, " "), want) {
			return true
		}
	}
	return false
}

// A terminally failed clone must be reaped before the error returns: the dead
// target would otherwise wedge every retry (the re-issued clone hits
// AlreadyExists against a target that will never complete).
func TestFailedCloneIsReaped(t *testing.T) {
	run := &failCloneRunner{}
	b := New(map[string]ClusterConfig{
		"east": {Monitors: []string{"10.0.0.10:6789"}, FSName: "cephfs", UserID: "admin"},
	}, "", run)

	err := b.waitClone(context.Background(), nil, "cephfs", "csi", "bard-target")
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("a failed clone must surface an error, got %v", err)
	}
	if !run.ran("subvolume rm cephfs bard-target --force") {
		t.Fatalf("a failed clone target must be reaped for the retry; calls: %v", run.calls)
	}
	if !run.ran("--group-name csi") {
		t.Fatalf("the reap must address the target's group; calls: %v", run.calls)
	}
}

// A cloned subvolume inherits its source's quota; CreateVolume must grow it to
// the request (--no_shrink) so the pod gets the space the PV claims.
func TestCloneResizedToRequest(t *testing.T) {
	b := snapBackend()
	run := b.run.(*cephRunner)
	ctx := context.Background()
	srcRef := bardplugin.VolumeRef{Instance: "east", Location: "cephfs/csi", Name: "bard-src1234567890ab"}

	vol, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: "bigclone", CapacityBytes: 2 << 30, Instance: "east", SourceVolume: &srcRef,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !run.ran("subvolume", "resize", "cephfs", vol.Name, "2147483648", "--no_shrink") {
		t.Fatalf("clone must be resized to the request with --no_shrink; calls: %v", run.calls)
	}
}

// An abandoned PVC-PVC clone leaves its clonetmp- snapshot on the SOURCE
// subvolume; `subvolume rm` refuses while snapshots exist, so DeleteVolume must
// reap ours first or the source is undeletable forever.
type tmpSnapRunner struct {
	cephRunner
}

func (r *tmpSnapRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if strings.Contains(strings.Join(args, " "), "snapshot ls") {
		r.calls = append(r.calls, append([]string{name}, args...))
		return `[{"name":"clonetmp-bard-deadbeef00000000"},{"name":"snap-1234567890abcdef"}]`, nil
	}
	return r.cephRunner.Run(ctx, name, args...)
}

func TestDeleteVolumeReapsAbandonedCloneSnaps(t *testing.T) {
	run := &tmpSnapRunner{}
	b := New(map[string]ClusterConfig{
		"east": {Monitors: []string{"10.0.0.10:6789"}, FSName: "cephfs", UserID: "admin"},
	}, "", run)

	if err := b.DeleteVolume(context.Background(), &bardplugin.DeleteVolumeRequest{
		Volume: bardplugin.VolumeRef{Instance: "east", Location: "cephfs/csi", Name: "bard-src1234567890ab"},
	}); err != nil {
		t.Fatal(err)
	}
	if !run.ran("snapshot", "rm", "cephfs", "bard-src1234567890ab", "clonetmp-bard-deadbeef00000000", "--force") {
		t.Fatalf("DeleteVolume must reap the abandoned clonetmp- snapshot; calls: %v", run.calls)
	}
	// A real (VolumeSnapshot-owned) snapshot must NOT be touched.
	if run.ran("snapshot", "rm", "cephfs", "bard-src1234567890ab", "snap-1234567890abcdef") {
		t.Fatalf("DeleteVolume must not touch CSI snapshots; calls: %v", run.calls)
	}
	if !run.ran("subvolume", "rm", "cephfs", "bard-src1234567890ab") {
		t.Fatalf("subvolume rm must still run; calls: %v", run.calls)
	}
}

// A deleted snapshot's CSI name must be reusable against a different source
// (mirrors the ceph-rbd behaviour; the in-memory index releases it on delete).
func TestCephFSSnapshotNameReusableAfterDelete(t *testing.T) {
	b := snapBackend()
	ctx := context.Background()
	volA := bardplugin.VolumeRef{Instance: "east", Location: "cephfs", Name: "bard-a"}
	volB := bardplugin.VolumeRef{Instance: "east", Location: "cephfs", Name: "bard-b"}

	snap, err := b.CreateSnapshot(ctx, &bardplugin.CreateSnapshotRequest{Name: "s-reuse", SourceVolume: volA})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.CreateSnapshot(ctx, &bardplugin.CreateSnapshotRequest{Name: "s-reuse", SourceVolume: volB}); err == nil {
		t.Fatal("same CSI name against a different source must fail while the snapshot exists")
	}
	if err := b.DeleteSnapshot(ctx, &bardplugin.DeleteSnapshotRequest{
		Snapshot: bardplugin.VolumeRef{Instance: "east", Location: snap.Location, Name: snap.Name},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.CreateSnapshot(ctx, &bardplugin.CreateSnapshotRequest{Name: "s-reuse", SourceVolume: volB}); err != nil {
		t.Fatalf("a deleted snapshot's name must be reusable, got %v", err)
	}
}
