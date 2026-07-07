package cephfsplugin

import (
	"context"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

func sgBackend() *Backend {
	return New(map[string]ClusterConfig{
		"east": {Monitors: []string{"10.0.0.10:6789"}, FSName: "cephfs", UserID: "admin"},
	}, "", &cephRunner{})
}

// A new volume lands in the ceph-csi-compatible default group "csi": the group is
// created, the subvolume is created with --group-name csi, and the group rides in the
// response Location so the whole lifecycle addresses it in its group.
func TestSubvolumeGroupDefault(t *testing.T) {
	b := sgBackend()
	run := b.run.(*cephRunner)
	ctx := context.Background()

	resp, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: "pvc-1", CapacityBytes: 1 << 30, Instance: "east",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Location != "cephfs/csi" {
		t.Fatalf("Location must encode the group, got %q", resp.Location)
	}
	if !run.ran("fs", "subvolumegroup", "create", "cephfs", "csi") {
		t.Fatalf("expected the subvolumegroup to be created; calls: %v", run.calls)
	}
	if !run.ran("fs", "subvolume", "create", "cephfs", resp.Name, "--group-name", "csi") {
		// --size sits between the name and the flag, so check both fragments.
		if !run.ran("subvolume create cephfs "+resp.Name) || !run.ran("--group-name csi") {
			t.Fatalf("expected subvolume create in group csi; calls: %v", run.calls)
		}
	}

	// The handle round-trips: delete via the response addresses the csi group.
	if err := b.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{
		Volume: bardplugin.VolumeRef{Instance: "east", Location: resp.Location, Name: resp.Name},
	}); err != nil {
		t.Fatal(err)
	}
	if !run.ran("fs", "subvolume", "rm", "cephfs", resp.Name, "--group-name", "csi") {
		t.Fatalf("delete must address the csi group; calls: %v", run.calls)
	}
}

// A StorageClass subvolumeGroup param overrides the default, and a group-less handle
// (a pre-group-support volume) resolves to the cluster _nogroup default (no flag).
func TestSubvolumeGroupParamAndLegacy(t *testing.T) {
	b := sgBackend()
	run := b.run.(*cephRunner)
	ctx := context.Background()

	resp, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: "pvc-2", CapacityBytes: 1 << 30, Instance: "east",
		Parameters: map[string]string{paramSubvolumeGroup: "team-a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Location != "cephfs/team-a" {
		t.Fatalf("the SC param must select the group, got %q", resp.Location)
	}

	// A legacy handle with a bare-fs Location (no group) deletes from _nogroup -- no flag.
	run.calls = nil
	if err := b.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{
		Volume: bardplugin.VolumeRef{Instance: "east", Location: "cephfs", Name: "bard-legacy"},
	}); err != nil {
		t.Fatal(err)
	}
	if !run.ran("fs", "subvolume", "rm", "cephfs", "bard-legacy") {
		t.Fatalf("expected a subvolume rm; calls: %v", run.calls)
	}
	if run.ran("--group-name") {
		t.Fatalf("a group-less handle must NOT pass --group-name; calls: %v", run.calls)
	}
}

// A snapshot of a grouped volume addresses the subvolume in its group.
func TestSubvolumeGroupSnapshot(t *testing.T) {
	b := sgBackend()
	run := b.run.(*cephRunner)
	if _, err := b.CreateSnapshot(context.Background(), &bardplugin.CreateSnapshotRequest{
		Name: "snap1", SourceVolume: bardplugin.VolumeRef{Instance: "east", Location: "cephfs/csi", Name: "bard-x"},
	}); err != nil {
		t.Fatal(err)
	}
	if !run.ran("subvolume snapshot create cephfs bard-x") || !run.ran("--group-name csi") {
		t.Fatalf("snapshot create must address the csi group; calls: %v", run.calls)
	}
}
