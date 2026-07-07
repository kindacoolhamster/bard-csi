package cephplugin

import (
	"context"
	"fmt"
	"testing"

	"github.com/kindacoolhamster/bard-csi/internal/fakerun"
	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// cloneDepth walks the rbd parent chain, and flattenIfDeep severs it once the chain
// reaches the configured limit (so iterative snapshot->restore can't grow it without
// bound). Below the limit the parent link is preserved (a thin COW clone).
func TestCloneDepthAndFlatten(t *testing.T) {
	run := fakerun.New()
	b := New(map[string]ClusterConfig{
		"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin"},
	}, "", "", run) // default limit 4
	ctx := context.Background()

	// Build a chain v0 <- v1 <- v2 <- v3 (each a COW clone of the previous).
	_, _ = run.Run(ctx, "rbd", "create", "replicapool/v0", "--size", "1024")
	for i := 1; i <= 3; i++ {
		_, _ = run.Run(ctx, "rbd", "clone",
			fmt.Sprintf("replicapool/v%d@s", i-1), fmt.Sprintf("replicapool/v%d", i))
	}
	if d, _, err := b.cloneDepth(ctx, nil, "replicapool/v3"); err != nil || d != 3 {
		t.Fatalf("cloneDepth(v3) = %d, %v; want 3", d, err)
	}
	// At depth 3 (< 4) flattenIfDeep is a no-op: the parent link stays.
	if err := b.flattenIfDeep(ctx, nil, "replicapool/v3"); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := b.imageParent(ctx, nil, "replicapool/v3"); p == "" {
		t.Fatal("v3 (depth 3) must NOT be flattened below the limit")
	}

	// One more level reaches depth 4 == limit -> flatten severs the parent.
	_, _ = run.Run(ctx, "rbd", "clone", "replicapool/v3@s", "replicapool/v4")
	if d, _, _ := b.cloneDepth(ctx, nil, "replicapool/v4"); d != 4 {
		t.Fatalf("cloneDepth(v4) = %d; want 4", d)
	}
	if err := b.flattenIfDeep(ctx, nil, "replicapool/v4"); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := b.imageParent(ctx, nil, "replicapool/v4"); p != "" {
		t.Fatalf("v4 (depth == limit) must be flattened, still has parent %q", p)
	}
	if d, _, _ := b.cloneDepth(ctx, nil, "replicapool/v4"); d != 0 {
		t.Fatalf("after flatten cloneDepth(v4) = %d; want 0", d)
	}
}

// A limit of 0 disables flattening entirely (opt-out).
func TestCloneDepthDisabled(t *testing.T) {
	run := fakerun.New()
	b := New(map[string]ClusterConfig{
		"east": {Pool: "replicapool", UserID: "admin"},
	}, "", "", run).WithCloneDepthLimit(0)
	ctx := context.Background()
	_, _ = run.Run(ctx, "rbd", "create", "replicapool/v0", "--size", "1024")
	for i := 1; i <= 6; i++ {
		_, _ = run.Run(ctx, "rbd", "clone",
			fmt.Sprintf("replicapool/v%d@s", i-1), fmt.Sprintf("replicapool/v%d", i))
	}
	if err := b.flattenIfDeep(ctx, nil, "replicapool/v6"); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := b.imageParent(ctx, nil, "replicapool/v6"); p == "" {
		t.Fatal("with the limit disabled, a deep clone must NOT be flattened")
	}
}

// Driving the real CreateVolume/CreateSnapshot path, an iterative snapshot->restore
// chain is flattened automatically once it reaches the limit -- so the chain can't
// grow without bound toward rbd's hard parent-depth cap.
func TestCloneDepthFlattenViaCreateVolume(t *testing.T) {
	run := fakerun.New()
	b := New(map[string]ClusterConfig{
		"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin"},
	}, "", "", run).WithCloneDepthLimit(2)
	b.flattenAsync = false // flatten synchronously so the assertion is deterministic
	ctx := context.Background()

	base, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: "base", CapacityBytes: 1 << 30, Instance: "east",
	})
	if err != nil {
		t.Fatal(err)
	}
	src := bardplugin.VolumeRef{Instance: "east", Location: base.Location, Name: base.Name}

	var last string
	for i := 1; i <= 2; i++ {
		snap, err := b.CreateSnapshot(ctx, &bardplugin.CreateSnapshotRequest{
			Name: fmt.Sprintf("snap%d", i), SourceVolume: src,
		})
		if err != nil {
			t.Fatal(err)
		}
		vol, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
			Name: fmt.Sprintf("restore%d", i), CapacityBytes: 1 << 30, Instance: "east",
			SourceSnapshot: &bardplugin.VolumeRef{Instance: "east", Location: snap.Location, Name: snap.Name},
		})
		if err != nil {
			t.Fatal(err)
		}
		src = bardplugin.VolumeRef{Instance: "east", Location: vol.Location, Name: vol.Name}
		last = vol.Location + "/" + vol.Name
	}

	// restore1 (depth 1) keeps its parent; restore2 (depth 2 == limit) is flattened.
	if p, _, _ := b.imageParent(ctx, nil, last); p != "" {
		t.Fatalf("the restore that reached the depth limit must be flattened, still has parent %q", p)
	}
}
