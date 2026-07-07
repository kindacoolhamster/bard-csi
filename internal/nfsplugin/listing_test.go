package nfsplugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

var (
	_ bardplugin.VolumeLister   = (*Backend)(nil)
	_ bardplugin.SnapshotLister = (*Backend)(nil)
)

// listRunner populates the (real) temp mount target on mount: volume subdirs, an
// onDelete=archive leftover that must NOT list, and snapshot tarballs + their
// source-provenance sidecars.
type listRunner struct {
	vols  []string
	snaps map[string]string // snapID -> source dir ("" = no sidecar)
}

func (r *listRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	if name != "mount" {
		return "", nil
	}
	t := args[len(args)-1]
	for _, v := range r.vols {
		_ = os.MkdirAll(filepath.Join(t, v), 0o755)
	}
	_ = os.MkdirAll(filepath.Join(t, "archived-old"), 0o755) // must be excluded
	if len(r.snaps) > 0 {
		_ = os.MkdirAll(filepath.Join(t, snapDir), 0o755)
		for id, src := range r.snaps {
			_ = os.WriteFile(filepath.Join(t, snapDir, id+".tar.gz"), []byte("x"), 0o644)
			if src != "" {
				_ = os.WriteFile(filepath.Join(t, snapDir, id+".src"), []byte(src), 0o644)
			}
		}
	}
	return "", nil
}

func TestNFSListVolumes(t *testing.T) {
	b := chBackend(&listRunner{vols: []string{"bard-a", "bard-b"}, snaps: map[string]string{"snap-x": "bard-a"}})
	resp, err := b.ListVolumes(context.Background(), &bardplugin.ListVolumesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// the two volume dirs, not .snapshots and not archived-old
	got := map[string]bool{}
	for _, e := range resp.Entries {
		got[e.Volume.Name] = true
	}
	if len(resp.Entries) != 2 || !got["bard-a"] || !got["bard-b"] {
		t.Fatalf("expected [bard-a bard-b], got %+v", resp.Entries)
	}
}

func TestNFSListSnapshots(t *testing.T) {
	b := chBackend(&listRunner{snaps: map[string]string{"snap-a": "bard-a"}})
	resp, err := b.ListSnapshots(context.Background(), &bardplugin.ListSnapshotsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Entries) != 1 {
		t.Fatalf("expected one snapshot, got %+v", resp.Entries)
	}
	e := resp.Entries[0]
	if e.Snapshot.Name != "snap-a" || e.SourceVolume.Name != "bard-a" {
		t.Fatalf("expected snap-a sourced from bard-a (read from the .src sidecar), got %+v", e)
	}
}
