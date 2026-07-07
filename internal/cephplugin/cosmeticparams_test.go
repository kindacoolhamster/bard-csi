package cephplugin

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kindacoolhamster/bard-csi/internal/fakerun"
	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

func cosmeticBackend(run Runner) *Backend {
	return New(map[string]ClusterConfig{
		"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin"},
	}, "", "", run)
}

// snapshotNamePrefix (VolumeSnapshotClass) renames the rbd snapshot; the name rides
// in the snapshot handle so DeleteSnapshot works unchanged, and ListSnapshots still
// recognises the custom-prefixed snapshot as Bard-managed.
func TestSnapshotNamePrefixLifecycle(t *testing.T) {
	run := fakerun.New()
	b := cosmeticBackend(run)
	ctx := context.Background()

	vol, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: "pvc-1", CapacityBytes: 1 << 30, Instance: "east",
	})
	if err != nil {
		t.Fatal(err)
	}
	src := bardplugin.VolumeRef{Instance: "east", Location: vol.Location, Name: vol.Name}

	snap, err := b.CreateSnapshot(ctx, &bardplugin.CreateSnapshotRequest{
		Name: "snap-1", SourceVolume: src,
		Parameters: map[string]string{paramSnapshotNamePrefix: "team-snap-"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := vol.Name + "@" + shortName("team-snap-", "snap-1")
	if snap.Name != want {
		t.Fatalf("snapshot handle name = %q, want %q (custom prefix, deterministic)", snap.Name, want)
	}

	// The custom-prefixed snapshot is still listed as Bard-managed.
	list, err := b.ListSnapshots(ctx, &bardplugin.ListSnapshotsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Entries) != 1 || list.Entries[0].Snapshot.Name != want {
		t.Fatalf("expected the custom-prefixed snapshot listed, got %+v", list.Entries)
	}

	// Delete via the handle alone (the prefix is encoded there).
	if err := b.DeleteSnapshot(ctx, &bardplugin.DeleteSnapshotRequest{
		Snapshot: bardplugin.VolumeRef{Instance: "east", Location: snap.Location, Name: snap.Name},
	}); err != nil {
		t.Fatal(err)
	}
}

// An invalid snapshotNamePrefix is rejected up front: InvalidArgument, and no
// snapshot exists afterwards (nothing to clean up).
func TestSnapshotNamePrefixInvalid(t *testing.T) {
	run := fakerun.New()
	b := cosmeticBackend(run)
	ctx := context.Background()
	vol, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: "pvc-1", CapacityBytes: 1 << 30, Instance: "east",
	})
	if err != nil {
		t.Fatal(err)
	}
	src := bardplugin.VolumeRef{Instance: "east", Location: vol.Location, Name: vol.Name}
	for _, bad := range []string{"a/b", "a@b", "with space"} {
		_, err := b.CreateSnapshot(ctx, &bardplugin.CreateSnapshotRequest{
			Name: "snap-bad", SourceVolume: src,
			Parameters: map[string]string{paramSnapshotNamePrefix: bad},
		})
		if err == nil {
			t.Fatalf("prefix %q must be rejected", bad)
		}
	}
	list, err := b.ListSnapshots(ctx, &bardplugin.ListSnapshotsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Entries) != 0 {
		t.Fatalf("rejected snapshots must leave nothing behind, got %+v", list.Entries)
	}
}

// volumeGroupNamePrefix renames the rbd group; the name rides in the group ref, and
// ListVolumeGroups recognises the custom-prefixed group by its shortName shape.
func TestVolumeGroupNamePrefix(t *testing.T) {
	run := newGroupRunner()
	b := groupBackend(run)
	ctx := context.Background()

	resp, err := b.CreateVolumeGroup(ctx, &bardplugin.CreateVolumeGroupRequest{
		Instance: "galileo", Name: "vg-1",
		Parameters: map[string]string{paramVolumeGroupNamePrefix: "team-group-"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := shortName("team-group-", "vg-1"); resp.Group.Name != want {
		t.Fatalf("group name = %q, want %q", resp.Group.Name, want)
	}
	list, err := b.ListVolumeGroups(ctx, &bardplugin.ListVolumeGroupsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Groups) != 1 || list.Groups[0].Group.Name != resp.Group.Name {
		t.Fatalf("expected the custom-prefixed group listed, got %+v", list.Groups)
	}

	if _, err := b.CreateVolumeGroup(ctx, &bardplugin.CreateVolumeGroupRequest{
		Instance: "galileo", Name: "vg-bad",
		Parameters: map[string]string{paramVolumeGroupNamePrefix: "a/b"},
	}); err == nil {
		t.Fatal("invalid volumeGroupNamePrefix must be rejected")
	}
}

// An instance with clusterName stamps every created image with ceph-csi's
// cluster-name metadata key; without it, no such metadata is written.
func TestClusterNameMetadata(t *testing.T) {
	run := fakerun.New()
	b := New(map[string]ClusterConfig{
		"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin", ClusterName: "prod-east"},
	}, "", "", run)
	ctx := context.Background()
	vol, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: "pvc-1", CapacityBytes: 1 << 30, Instance: "east",
	})
	if err != nil {
		t.Fatal(err)
	}
	spec := vol.Location + "/" + vol.Name
	if got := b.imageMetaGet(ctx, nil, spec, imgMetaClusterName); got != "prod-east" {
		t.Fatalf("image %s must carry %s=prod-east, got %q", spec, imgMetaClusterName, got)
	}

	plain := fakerun.New()
	b2 := cosmeticBackend(plain)
	vol2, err := b2.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: "pvc-2", CapacityBytes: 1 << 30, Instance: "east",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := b2.imageMetaGet(ctx, nil, vol2.Location+"/"+vol2.Name, imgMetaClusterName); got != "" {
		t.Fatalf("no clusterName configured, but image carries %q", got)
	}
}

// cephLogDir places the rbd-nbd client log per volume; the path + strategy are
// persisted in the device record, and NodeUnstage disposes of the log after a clean
// unmap (default: remove).
func TestCephLogDirNbdMap(t *testing.T) {
	dir := t.TempDir()
	run := newMounterRunner(true) // krbd fails -> nbd fallback maps
	b := newFenceBackend(dir, run)
	stage := filepath.Join(dir, "stage")
	err := b.NodeStage(context.Background(), stageReq(dir, map[string]string{
		paramTryOtherMounters: "true",
		paramCephLogDir:       "/var/log/bard",
		paramCephLogStrategy:  "compress",
	}))
	if err != nil {
		t.Fatal(err)
	}
	wantLog := "/var/log/bard/rbd-nbd-replicapool-img.log"
	found := false
	for _, c := range run.calls {
		if c[0] == "rbd-nbd" && has(c, "map") && has(c, "--log-file") && has(c, wantLog) {
			found = true
		}
	}
	if !found {
		t.Fatalf("rbd-nbd map must carry --log-file %s; calls: %v", wantLog, run.calls)
	}
	rec := b.readDeviceRecord(stage)
	if rec.LogFile != wantLog || rec.LogStrategy != "compress" {
		t.Fatalf("record must persist log file+strategy, got %+v", rec)
	}

	if err := b.NodeUnstage(context.Background(), &bardplugin.NodeUnstageRequest{
		Volume:      bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img"},
		StagingPath: stage,
	}); err != nil {
		t.Fatal(err)
	}
	if !run.ranBin("gzip", wantLog) {
		t.Fatalf("compress strategy must gzip the log after unmap; calls: %v", run.calls)
	}
}

// The default strategy removes the log; a krbd map never writes or manages one.
func TestCephLogDirDefaultsAndKrbd(t *testing.T) {
	// Default strategy (unset) = remove.
	dir := t.TempDir()
	run := newMounterRunner(true)
	b := newFenceBackend(dir, run)
	stage := filepath.Join(dir, "stage")
	if err := b.NodeStage(context.Background(), stageReq(dir, map[string]string{
		paramTryOtherMounters: "true",
		paramCephLogDir:       "/var/log/bard",
	})); err != nil {
		t.Fatal(err)
	}
	if err := b.NodeUnstage(context.Background(), &bardplugin.NodeUnstageRequest{
		Volume:      bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img"},
		StagingPath: stage,
	}); err != nil {
		t.Fatal(err)
	}
	if !run.ranBin("rm", "/var/log/bard/rbd-nbd-replicapool-img.log") {
		t.Fatalf("default strategy must remove the log after unmap; calls: %v", run.calls)
	}

	// krbd map: no client log to place or manage, even with cephLogDir set.
	dir2 := t.TempDir()
	krun := newMounterRunner(false) // krbd succeeds
	kb := newFenceBackend(dir2, krun)
	kstage := filepath.Join(dir2, "stage")
	if err := kb.NodeStage(context.Background(), stageReq(dir2, map[string]string{
		paramCephLogDir: "/var/log/bard",
	})); err != nil {
		t.Fatal(err)
	}
	if rec := kb.readDeviceRecord(kstage); rec.LogFile != "" {
		t.Fatalf("krbd stage must not record a log file, got %+v", rec)
	}
	if err := kb.NodeUnstage(context.Background(), &bardplugin.NodeUnstageRequest{
		Volume:      bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img"},
		StagingPath: kstage,
	}); err != nil {
		t.Fatal(err)
	}
	if krun.ranBin("rm", "-f") || krun.ranBin("gzip", "-f") {
		t.Fatalf("krbd unstage must not manage any log; calls: %v", krun.calls)
	}
}

// An unknown cephLogStrategy is rejected at CreateVolume, before any image exists.
func TestCephLogStrategyValidatedUpFront(t *testing.T) {
	run := fakerun.New()
	b := cosmeticBackend(run)
	_, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "pvc-1", CapacityBytes: 1 << 30, Instance: "east",
		Parameters: map[string]string{paramCephLogStrategy: "shred"},
	})
	if err == nil || !strings.Contains(err.Error(), "cephLogStrategy") {
		t.Fatalf("bad strategy must be rejected, got %v", err)
	}
	vols, lerr := b.ListVolumes(context.Background(), &bardplugin.ListVolumesRequest{})
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(vols.Entries) != 0 {
		t.Fatalf("rejected create must leave no image behind, got %+v", vols.Entries)
	}
}
