package cephplugin

import (
	"context"
	"strings"
	"testing"

	"github.com/kindacoolhamster/bard-csi/internal/fakerun"
	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// recordRunner delegates to the full fakerun simulator but records every call, so a
// test can assert the exact flags a command was issued with.
type recordRunner struct {
	inner *fakerun.Runner
	calls [][]string
}

func (r *recordRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	return r.inner.Run(ctx, name, args...)
}

func (r *recordRunner) ran(sub string) bool {
	for _, c := range r.calls {
		if strings.Contains(strings.Join(c, " "), sub) {
			return true
		}
	}
	return false
}

// dataPool threads --data-pool onto a fresh create, a snapshot-restore clone, and a
// PVC-to-PVC cp -- so an erasure-coded backing pool is used for the new image's data
// in every provisioning path, not just plain create.
func TestDataPoolThreadsThroughProvisioning(t *testing.T) {
	run := &recordRunner{inner: fakerun.New()}
	b := New(map[string]ClusterConfig{
		"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin"},
	}, "", "", run)
	ctx := context.Background()
	params := map[string]string{paramDataPool: "ec-data"}

	// fresh create
	base, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: "base", CapacityBytes: 1 << 30, Instance: "east", Parameters: params,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !run.ran("create replicapool/" + base.Name + " --size 1024 --data-pool ec-data") {
		t.Fatalf("create must carry --data-pool ec-data; calls: %v", run.calls)
	}

	// snapshot-restore clone
	snap, err := b.CreateSnapshot(ctx, &bardplugin.CreateSnapshotRequest{
		Name: "s1", SourceVolume: bardplugin.VolumeRef{Instance: "east", Location: base.Location, Name: base.Name},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: "restore", CapacityBytes: 1 << 30, Instance: "east", Parameters: params,
		SourceSnapshot: &bardplugin.VolumeRef{Instance: "east", Location: snap.Location, Name: snap.Name},
	}); err != nil {
		t.Fatal(err)
	}
	if !run.ran("clone ") || !run.ran("--data-pool ec-data") {
		t.Fatalf("clone must carry --data-pool ec-data; calls: %v", run.calls)
	}

	// PVC-to-PVC clone (temp snap + COW clone; the clone writes the new image's
	// data, so it must carry the data pool)
	run.calls = nil
	if _, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: "copy", CapacityBytes: 1 << 30, Instance: "east", Parameters: params,
		SourceVolume: &bardplugin.VolumeRef{Instance: "east", Location: base.Location, Name: base.Name},
	}); err != nil {
		t.Fatal(err)
	}
	if !run.ran("@clonetmp-") || !run.ran("--data-pool ec-data") {
		t.Fatalf("volume clone must carry --data-pool ec-data; calls: %v", run.calls)
	}
}
