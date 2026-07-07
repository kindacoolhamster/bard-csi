package cephplugin

import (
	"context"
	"strings"
	"testing"

	"github.com/kindacoolhamster/bard-csi/internal/fakerun"
	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// volumeImagePrefix defaults to csi-vol- and rejects names that would break the
// pool/namespace/image locator or the command line.
func TestVolumeImagePrefix(t *testing.T) {
	if got, err := volumeImagePrefix(""); err != nil || got != volNamePrefix {
		t.Fatalf("empty must default to %q, got %q err=%v", volNamePrefix, got, err)
	}
	if got, err := volumeImagePrefix("pvc-"); err != nil || got != "pvc-" {
		t.Fatalf("custom prefix must pass through, got %q err=%v", got, err)
	}
	for _, bad := range []string{"a/b", "with space", "tab\tx", "line\nx"} {
		if _, err := volumeImagePrefix(bad); err == nil {
			t.Fatalf("prefix %q must be rejected", bad)
		}
	}
}

// isBardImageName recognises shortName output (any prefix + 16 hex) but not
// unrelated images -- so a custom volumeNamePrefix is still listable while a
// ceph-csi UUID image (a '-' in the trailing 16) and arbitrary names are skipped.
func TestIsBardImageName(t *testing.T) {
	yes := []string{shortName("csi-vol-", "x"), shortName("pvc-", "y"), shortName("t-", "z")}
	for _, n := range yes {
		if !isBardImageName(n) {
			t.Fatalf("%q should be recognised as Bard-managed", n)
		}
	}
	no := []string{
		"csi-vol-1b00f5f8-0000-1111-2222-333344445555", // ceph-csi UUID: '-' in trailing 16
		"short",
		"csi-vol-notallhexchars!!",
		"my-app-data-volume", // trailing 16 has non-hex
	}
	for _, n := range no {
		if isBardImageName(n) {
			t.Fatalf("%q should NOT be recognised as Bard-managed", n)
		}
	}
}

// A StorageClass volumeNamePrefix changes the rbd image name; the prefix rides in
// the volume handle (Name), so DeleteVolume -- which sees only the handle --
// removes the right image, and ListVolumes still finds the custom-prefixed image.
func TestVolumeNamePrefixLifecycle(t *testing.T) {
	run := fakerun.New()
	b := New(map[string]ClusterConfig{
		"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin"},
	}, "", "", run)
	ctx := context.Background()

	resp, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: "pvc-1", CapacityBytes: 1 << 30, Instance: "east",
		Parameters: map[string]string{paramVolumeNamePrefix: "team-a-"},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if !strings.HasPrefix(resp.Name, "team-a-") {
		t.Fatalf("image name must carry the custom prefix, got %q", resp.Name)
	}
	if resp.Name != shortName("team-a-", "pvc-1") {
		t.Fatalf("image name must be deterministic for idempotency, got %q", resp.Name)
	}

	imgs, err := b.listImages(ctx, nil, "replicapool", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) != 1 || imgs[0] != resp.Name {
		t.Fatalf("expected the prefixed image in the pool, got %v", imgs)
	}

	lv, err := b.ListVolumes(ctx, &bardplugin.ListVolumesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(lv.Entries) != 1 || lv.Entries[0].Volume.Name != resp.Name {
		t.Fatalf("ListVolumes must find the custom-prefixed image, got %+v", lv.Entries)
	}

	if err := b.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{
		Volume: bardplugin.VolumeRef{Instance: "east", Location: resp.Location, Name: resp.Name},
	}); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	imgs, _ = b.listImages(ctx, nil, "replicapool", "")
	if len(imgs) != 0 {
		t.Fatalf("DeleteVolume must remove the prefixed image, still have %v", imgs)
	}
}

// An invalid volumeNamePrefix fails CreateVolume fast (InvalidArgument), not at map time.
func TestVolumeNamePrefixInvalid(t *testing.T) {
	run := fakerun.New()
	b := New(map[string]ClusterConfig{
		"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin"},
	}, "", "", run)

	_, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "pvc-9", CapacityBytes: 1 << 30, Instance: "east",
		Parameters: map[string]string{paramVolumeNamePrefix: "bad/prefix"},
	})
	if err == nil {
		t.Fatal("a volumeNamePrefix with '/' must be rejected")
	}
}
