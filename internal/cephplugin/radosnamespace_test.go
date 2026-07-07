package cephplugin

import (
	"context"
	"strings"
	"testing"

	"github.com/kindacoolhamster/bard-csi/internal/fakerun"
	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

func TestLocator(t *testing.T) {
	if got := locator("replicapool", ""); got != "replicapool" {
		t.Fatalf("no namespace must be the bare pool, got %q", got)
	}
	if got := locator("replicapool", "tenant-a"); got != "replicapool/tenant-a" {
		t.Fatalf("namespace must encode as pool/namespace, got %q", got)
	}
}

// A rados namespace threads through the whole lifecycle via the volume's Location
// (pool/namespace). Provision creates the namespace and the image inside it,
// reports Location=pool/namespace, ListVolumes finds it there, and DeleteVolume --
// which sees only the handle -- removes the namespaced image.
func TestRadosNamespaceLifecycle(t *testing.T) {
	run := fakerun.New()
	b := New(map[string]ClusterConfig{
		"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin", RadosNamespace: "tenant-a"},
	}, "", "", run)
	ctx := context.Background()

	resp, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: "pvc-1", CapacityBytes: 1 << 30, Instance: "east",
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if resp.Location != "replicapool/tenant-a" {
		t.Fatalf("Location must be pool/namespace, got %q", resp.Location)
	}

	// The image must live in the namespace, not the pool's default namespace.
	def, err := b.listImages(ctx, nil, "replicapool", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(def) != 0 {
		t.Fatalf("default namespace must be empty, got %v", def)
	}
	inNs, err := b.listImages(ctx, nil, "replicapool", "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(inNs) != 1 || inNs[0] != resp.Name {
		t.Fatalf("image must be in tenant-a namespace, got %v", inNs)
	}

	// ListVolumes reports it with the namespaced Location.
	lv, err := b.ListVolumes(ctx, &bardplugin.ListVolumesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(lv.Entries) != 1 || lv.Entries[0].Volume.Location != "replicapool/tenant-a" {
		t.Fatalf("ListVolumes must report the namespaced volume, got %+v", lv.Entries)
	}

	// DeleteVolume gets only the handle (Instance/Location/Name) -- the namespace
	// rides in Location, so the namespaced image is removed.
	if err := b.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{
		Volume: bardplugin.VolumeRef{Instance: "east", Location: resp.Location, Name: resp.Name},
	}); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	inNs, _ = b.listImages(ctx, nil, "replicapool", "tenant-a")
	if len(inNs) != 0 {
		t.Fatalf("DeleteVolume must remove the namespaced image, still have %v", inNs)
	}
}

// A StorageClass radosNamespace parameter overrides the instance's namespace.
func TestRadosNamespaceParamOverridesInstance(t *testing.T) {
	run := fakerun.New()
	b := New(map[string]ClusterConfig{
		"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin", RadosNamespace: "inst-ns"},
	}, "", "", run)

	resp, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "pvc-2", CapacityBytes: 1 << 30, Instance: "east",
		Parameters: map[string]string{paramRadosNamespace: "sc-ns"},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if resp.Location != "replicapool/sc-ns" {
		t.Fatalf("the SC param must win over the instance namespace, got %q", resp.Location)
	}
}

// Without a namespace the Location is the bare pool -- backward compatible with
// every existing volume handle (no "/" in Location).
func TestNoRadosNamespaceIsBarePool(t *testing.T) {
	run := fakerun.New()
	b := New(map[string]ClusterConfig{
		"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin"},
	}, "", "", run)

	resp, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "pvc-3", CapacityBytes: 1 << 30, Instance: "east",
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if resp.Location != "replicapool" || strings.Contains(resp.Location, "/") {
		t.Fatalf("no namespace must be the bare pool, got %q", resp.Location)
	}
}
