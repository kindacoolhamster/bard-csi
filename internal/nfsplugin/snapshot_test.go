package nfsplugin

import (
	"context"
	"strings"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// recRunner records every command so snapshot/restore/clone can be asserted.
type recRunner struct{ calls [][]string }

func (r *recRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	return "", nil
}

func (r *recRunner) ran(parts ...string) bool {
	want := strings.Join(parts, " ")
	for _, c := range r.calls {
		if strings.Contains(strings.Join(c, " "), want) {
			return true
		}
	}
	return false
}

func snapBackend(run Runner) *Backend {
	return New(map[string]InstanceConfig{"east": {Server: "10.0.0.9", Export: "/srv/nfs"}}, run)
}

// CreateSnapshot tars the source subdir into .snapshots/<id>.tar.gz; the handle
// encodes that id; DeleteSnapshot removes the archive.
func TestNFSSnapshotCreateDelete(t *testing.T) {
	run := &recRunner{}
	b := snapBackend(run)
	resp, err := b.CreateSnapshot(context.Background(), &bardplugin.CreateSnapshotRequest{
		Name:         "snap1",
		SourceVolume: bardplugin.VolumeRef{Instance: "east", Location: "/srv/nfs", Name: "bard-x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Name != snapName("snap1") || !resp.ReadyToUse {
		t.Fatalf("unexpected snapshot response %+v", resp)
	}
	if !run.ran("tar", "czf") || !run.ran("-C") {
		t.Fatalf("expected a tar czf of the source; calls: %v", run.calls)
	}
	// The archive path must encode the snapshot id under .snapshots.
	if !run.ran(snapArchive(resp.Name)) {
		t.Fatalf("archive path should be %s; calls: %v", snapArchive(resp.Name), run.calls)
	}

	run.calls = nil
	if err := b.DeleteSnapshot(context.Background(), &bardplugin.DeleteSnapshotRequest{
		Snapshot: bardplugin.VolumeRef{Instance: "east", Location: "/srv/nfs", Name: resp.Name},
	}); err != nil {
		t.Fatal(err)
	}
}

// Restore-from-snapshot extracts the archive into the new directory.
func TestNFSRestoreFromSnapshot(t *testing.T) {
	run := &recRunner{}
	b := snapBackend(run)
	resp, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name:           "restored",
		Instance:       "east",
		SourceSnapshot: &bardplugin.VolumeRef{Instance: "east", Location: "/srv/nfs", Name: "snap-abc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !run.ran("tar", "xzf") || !run.ran(snapArchive("snap-abc")) {
		t.Fatalf("expected a tar extract of the snapshot archive; calls: %v", run.calls)
	}
	if run.ran("cp", "-a") {
		t.Fatalf("restore must not also copy a volume; calls: %v", run.calls)
	}
	_ = resp
}

// Clone-from-volume copies the source directory contents.
func TestNFSCloneFromVolume(t *testing.T) {
	run := &recRunner{}
	b := snapBackend(run)
	if _, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name:         "cloned",
		Instance:     "east",
		SourceVolume: &bardplugin.VolumeRef{Instance: "east", Location: "/srv/nfs", Name: "bard-src"},
	}); err != nil {
		t.Fatal(err)
	}
	if !run.ran("cp", "-a") || !run.ran("bard-src/.") {
		t.Fatalf("expected a recursive copy of the source dir; calls: %v", run.calls)
	}
}

// The backend advertises snapshot support.
func TestNFSAdvertisesSnapshots(t *testing.T) {
	if !snapBackend(&recRunner{}).Info().Capabilities.Snapshots {
		t.Fatal("nfs backend must advertise Snapshots now that it implements them")
	}
}
