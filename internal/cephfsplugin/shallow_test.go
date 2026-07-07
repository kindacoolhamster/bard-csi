package cephfsplugin

import (
	"context"
	"strings"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// A backingSnapshot restore makes a shallow read-only volume: no clone, no
// subvolume create -- just the snapshot's .snap directory handed to the node.
func TestCephFSShallowVolume(t *testing.T) {
	b := snapBackend()
	run := b.run.(*cephRunner)
	resp, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name:           "ro",
		Instance:       "east",
		Parameters:     map[string]string{paramBackingSnapshot: "true"},
		SourceSnapshot: &bardplugin.VolumeRef{Instance: "east", Location: "cephfs", Name: "bard-x@snap-abc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resp.Name, shallowPrefix) {
		t.Fatalf("shallow volume handle must be marked, got %q", resp.Name)
	}
	// The node is pointed at the snapshot's .snap directory.
	if got := resp.Context[ctxPath]; !strings.HasSuffix(got, "/.snap/snap-abc") {
		t.Fatalf("expected a .snap path, got %q", got)
	}
	// Crucially: no clone and no subvolume create happened (zero-copy).
	if run.ran("fs", "subvolume", "snapshot", "clone") || run.ran("fs", "subvolume", "create") {
		t.Fatalf("shallow volume must not clone or create a subvolume; calls: %v", run.calls)
	}
}

// Deleting a shallow volume is a no-op -- it must never remove the source
// subvolume or the snapshot (which belong to others).
func TestCephFSShallowDeleteNoop(t *testing.T) {
	b := snapBackend()
	run := b.run.(*cephRunner)
	err := b.DeleteVolume(context.Background(), &bardplugin.DeleteVolumeRequest{
		Volume: bardplugin.VolumeRef{Instance: "east", Location: "cephfs", Name: shallowPrefix + "bard-x@snap-abc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.ran("fs", "subvolume", "rm") || run.ran("snapshot", "rm") {
		t.Fatalf("shallow delete must not remove any subvolume or snapshot; calls: %v", run.calls)
	}
}

// A backingSnapshot restore is rejected with the nfs mounter (no native .snap mount).
func TestCephFSShallowRejectsNFSMounter(t *testing.T) {
	b := nfsMounterBackend(&cephRunner{})
	_, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name:           "ro",
		Instance:       "east",
		Parameters:     map[string]string{paramBackingSnapshot: "true"},
		SourceSnapshot: &bardplugin.VolumeRef{Instance: "east", Location: "cephfs", Name: "bard-x@snap-abc"},
	})
	if err == nil || !strings.Contains(err.Error(), "nfs mounter") {
		t.Fatalf("shallow + nfs mounter must be rejected, got %v", err)
	}
}

// Without backingSnapshot, a snapshot restore still does a full clone (unchanged).
func TestCephFSSnapshotRestoreStillClonesByDefault(t *testing.T) {
	b := snapBackend()
	run := b.run.(*cephRunner)
	if _, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name:           "full",
		Instance:       "east",
		SourceSnapshot: &bardplugin.VolumeRef{Instance: "east", Location: "cephfs", Name: "bard-x@snap-abc"},
	}); err != nil {
		t.Fatal(err)
	}
	if !run.ran("fs", "subvolume", "snapshot", "clone") {
		t.Fatalf("a normal restore must still clone; calls: %v", run.calls)
	}
}
