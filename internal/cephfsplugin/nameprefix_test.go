package cephfsplugin

import (
	"context"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// volumeNamePrefix (StorageClass) renames the subvolume; the name rides in the
// handle so delete works unchanged. snapshotNamePrefix does the same for the
// subvolume snapshot. Invalid prefixes are rejected up front.
func TestCephFSNamePrefixes(t *testing.T) {
	b := sgBackend()
	run := b.run.(*cephRunner)
	ctx := context.Background()

	resp, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: "pvc-1", CapacityBytes: 1 << 30, Instance: "east",
		Parameters: map[string]string{paramVolumeNamePrefix: "team-a-"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := subvolName("team-a-", "pvc-1"); resp.Name != want {
		t.Fatalf("subvolume name = %q, want %q (custom prefix, deterministic)", resp.Name, want)
	}

	snap, err := b.CreateSnapshot(ctx, &bardplugin.CreateSnapshotRequest{
		Name:         "snap-1",
		SourceVolume: bardplugin.VolumeRef{Instance: "east", Location: resp.Location, Name: resp.Name},
		Parameters:   map[string]string{paramSnapshotNamePrefix: "team-snap-"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := resp.Name + "@" + snapName("team-snap-", "snap-1"); snap.Name != want {
		t.Fatalf("snapshot handle = %q, want %q", snap.Name, want)
	}

	// The handle alone drives delete (prefix encoded there).
	if err := b.DeleteSnapshot(ctx, &bardplugin.DeleteSnapshotRequest{
		Snapshot: bardplugin.VolumeRef{Instance: "east", Location: snap.Location, Name: snap.Name},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{
		Volume: bardplugin.VolumeRef{Instance: "east", Location: resp.Location, Name: resp.Name},
	}); err != nil {
		t.Fatal(err)
	}
	if !run.ran("fs", "subvolume", "rm", "cephfs", resp.Name) {
		t.Fatalf("delete must address the prefixed subvolume; calls: %v", run.calls)
	}

	// Invalid prefixes ('/', '@', whitespace) are rejected before any ceph call.
	for _, bad := range []string{"a/b", "a@b", "with space"} {
		if _, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
			Name: "pvc-bad", CapacityBytes: 1 << 30, Instance: "east",
			Parameters: map[string]string{paramVolumeNamePrefix: bad},
		}); err == nil {
			t.Fatalf("volumeNamePrefix %q must be rejected", bad)
		}
		if _, err := b.CreateSnapshot(ctx, &bardplugin.CreateSnapshotRequest{
			Name:         "snap-bad",
			SourceVolume: bardplugin.VolumeRef{Instance: "east", Location: "cephfs/csi", Name: "bard-0011223344556677"},
			Parameters:   map[string]string{paramSnapshotNamePrefix: bad},
		}); err == nil {
			t.Fatalf("snapshotNamePrefix %q must be rejected", bad)
		}
	}
}

// isBardObjectName recognises shortName output with any prefix, but not foreign
// subvolume/snapshot names.
func TestIsBardObjectName(t *testing.T) {
	for _, n := range []string{subvolName("bard-", "x"), subvolName("team-a-", "y"), snapName("snap-", "z")} {
		if !isBardObjectName(n) {
			t.Fatalf("%q should be recognised as Bard-managed", n)
		}
	}
	for _, n := range []string{"stray", "manual-snap", "bard-x", "csi-vol-1b00f5f8-0000-1111-2222-333344445555"} {
		if isBardObjectName(n) {
			t.Fatalf("%q should NOT be recognised as Bard-managed", n)
		}
	}
}
