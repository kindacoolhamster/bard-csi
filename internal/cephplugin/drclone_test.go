package cephplugin

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/kindacoolhamster/bard-csi/internal/fakerun"
	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// mirrorCloneRunner models librbd's refusal to enable snapshot-based mirroring on
// a COW clone (live-captured error text), and a flatten that severs the parent.
type mirrorCloneRunner struct {
	calls     [][]string
	hasParent bool
}

func (r *mirrorCloneRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	switch {
	case has(args, "enable"):
		if r.hasParent {
			return "", fmt.Errorf("2026-07-02 librbd::api::Mirror: image_enable: mirroring is not enabled for the parent")
		}
		return "", nil
	case has(args, "flatten"):
		r.hasParent = false
		return "", nil
	}
	return "", nil
}

func (r *mirrorCloneRunner) flattens() int {
	n := 0
	for _, c := range r.calls {
		if c[0] == "rbd" && has(c, "flatten") {
			n++
		}
	}
	return n
}

func cloneVol() bardplugin.VolumeRef {
	return bardplugin.VolumeRef{Instance: "galileo", Location: "k8s-csi-test", Name: "csi-vol-clone"}
}

// Enabling replication on a snapshot-restored clone kicks a background flatten and
// fails with a clear retryable message; the retry (parent now severed) succeeds.
// ceph-csi's answer is an inline flattenMode=force copy that blocks the reconcile.
func TestEnableReplicationOnCloneFlattensAndRetries(t *testing.T) {
	run := &mirrorCloneRunner{hasParent: true}
	b := newReplBackend(run)
	b.flattenAsync = false // deterministic: flatten runs inline
	ctx := context.Background()

	err := b.EnableVolumeReplication(ctx, &bardplugin.EnableReplicationRequest{Volume: cloneVol()})
	if err == nil || !strings.Contains(err.Error(), "flattening") {
		t.Fatalf("first enable must fail with the flattening message, got %v", err)
	}
	if run.flattens() != 1 {
		t.Fatalf("expected exactly one background flatten, got %d; calls: %v", run.flattens(), run.calls)
	}

	// The csi-addons controller retries the CR; the image is parent-free now.
	if err := b.EnableVolumeReplication(ctx, &bardplugin.EnableReplicationRequest{Volume: cloneVol()}); err != nil {
		t.Fatalf("retry after flatten must succeed, got %v", err)
	}
}

// flattenMode=never (ceph-csi VolumeReplicationClass parity) opts out: the raw
// error surfaces and no flatten is kicked.
func TestEnableReplicationFlattenModeNever(t *testing.T) {
	run := &mirrorCloneRunner{hasParent: true}
	b := newReplBackend(run)
	b.flattenAsync = false

	err := b.EnableVolumeReplication(context.Background(), &bardplugin.EnableReplicationRequest{
		Volume:     cloneVol(),
		Parameters: map[string]string{paramFlattenMode: "never"},
	})
	if err == nil || strings.Contains(err.Error(), "flattening") {
		t.Fatalf("flattenMode=never must surface the raw enable error, got %v", err)
	}
	if run.flattens() != 0 {
		t.Fatalf("flattenMode=never must not flatten; calls: %v", run.calls)
	}
}

// Retries while a flatten is already in flight must not stack another one.
func TestEnableReplicationFlattenDedup(t *testing.T) {
	run := &mirrorCloneRunner{hasParent: true}
	b := newReplBackend(run)
	b.flattenAsync = false
	spec := cloneVol().Location + "/" + cloneVol().Name
	b.flattenMu.Lock()
	b.flattenInFlight = map[string]bool{spec: true} // a flatten is already running
	b.flattenMu.Unlock()

	err := b.EnableVolumeReplication(context.Background(), &bardplugin.EnableReplicationRequest{Volume: cloneVol()})
	if err == nil {
		t.Fatal("enable must still fail while the flatten is in flight")
	}
	if run.flattens() != 0 {
		t.Fatalf("an in-flight flatten must dedup the new request; calls: %v", run.calls)
	}
}

// trashedParentRunner models ceph-csi's open #4013: the clone's parent was moved
// to the rbd trash (rook does this), so the parent reports trash:true in the
// child's info and cannot be opened by name.
type trashedParentRunner struct {
	calls [][]string
}

func (r *trashedParentRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	switch {
	case has(args, "info") && has(args, "replicapool/child"):
		return `{"size":1073741824,"parent":{"pool":"replicapool","pool_namespace":"","image":"gone-parent","snapshot":"s","trash":true}}`, nil
	case has(args, "info"): // the trashed parent is not openable by name
		return "", fmt.Errorf("rbd: error opening image gone-parent: (2) No such file or directory")
	}
	return "", nil
}

// A chain with a trashed ancestor has an unknowable depth: flatten conservatively
// (even below the limit) instead of silently skipping -- assuming "shallow" is how
// ceph-csi's chains crept to rbd's hard cap and produced unmountable clones.
func TestFlattenConservativeOnTrashedParent(t *testing.T) {
	run := &trashedParentRunner{}
	b := New(map[string]ClusterConfig{
		"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin"},
	}, "", "", run) // default limit 4, but depth here is only 1
	if err := b.flattenIfDeep(context.Background(), nil, "replicapool/child"); err != nil {
		t.Fatal(err)
	}
	flattened := false
	for _, c := range run.calls {
		if c[0] == "rbd" && has(c, "flatten") && has(c, "replicapool/child") {
			flattened = true
		}
	}
	if !flattened {
		t.Fatalf("a trashed ancestor must trigger a conservative flatten; calls: %v", run.calls)
	}
}

// Concurrent clone + delete (ceph-csi #6321): deleting a source while a COW clone
// depends on it must fail loudly at every stage and converge by retry once the
// dependency is gone -- never silently orphan or take the clone's data. Pins the
// live-verified Ceph 20.2 semantics (modeled in fakerun): rm fails on live snaps,
// snap rm with children trashes the snap (which still blocks rm), and the trash
// releases when the last clone goes.
func TestSourceDeleteWhileCloneExists(t *testing.T) {
	run := fakerun.New()
	b := New(map[string]ClusterConfig{
		"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin"},
	}, "", "", run)
	b.flattenAsync = false
	ctx := context.Background()

	src, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: "src", CapacityBytes: 1 << 30, Instance: "east",
	})
	if err != nil {
		t.Fatal(err)
	}
	srcRef := bardplugin.VolumeRef{Instance: "east", Location: src.Location, Name: src.Name}
	snap, err := b.CreateSnapshot(ctx, &bardplugin.CreateSnapshotRequest{Name: "s1", SourceVolume: srcRef})
	if err != nil {
		t.Fatal(err)
	}
	clone, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: "clone", CapacityBytes: 1 << 30, Instance: "east",
		SourceSnapshot: &bardplugin.VolumeRef{Instance: "east", Location: snap.Location, Name: snap.Name},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 1. Source delete while its snapshot exists: must fail, not orphan.
	if err := b.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{Volume: srcRef}); err == nil {
		t.Fatal("deleting the source with a live snapshot must fail")
	}
	// 2. Snapshot delete succeeds (v2 trashes it under the live clone)...
	if err := b.DeleteSnapshot(ctx, &bardplugin.DeleteSnapshotRequest{
		Snapshot: bardplugin.VolumeRef{Instance: "east", Location: snap.Location, Name: snap.Name},
	}); err != nil {
		t.Fatal(err)
	}
	// ...but the source is still not deletable while the clone depends on it.
	if err := b.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{Volume: srcRef}); err == nil {
		t.Fatal("deleting the source with a dependent clone must still fail")
	}
	// 3. Once the clone is gone, the retry converges: the source deletes cleanly.
	if err := b.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{
		Volume: bardplugin.VolumeRef{Instance: "east", Location: clone.Location, Name: clone.Name},
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{Volume: srcRef}); err != nil {
		t.Fatalf("source delete must converge once the clone is gone: %v", err)
	}
}
