package cephplugin

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// TestRealCephCloneOfStaticImageIsReaped proves against REAL Ceph that a restore or
// clone of a statically provisioned (admin-owned) image is an ordinary CSI volume:
// librbd copies the parent's image-meta onto every clone, bard.static included, so
// the clone's DeleteVolume used to no-op and leak the image. Skipped unless
// BARD_CEPH_TEST=1:
//
//	BARD_CEPH_TEST=1 CEPH_MON=192.168.1.225:3300 CEPH_POOL=k8s-csi-test \
//	CEPH_USER=k8s-csi-test CEPH_KEY=AQ...== \
//	go test ./internal/cephplugin/ -run TestRealCephCloneOfStaticImageIsReaped -v
func TestRealCephCloneOfStaticImageIsReaped(t *testing.T) {
	if os.Getenv("BARD_CEPH_TEST") != "1" {
		t.Skip("set BARD_CEPH_TEST=1 (and CEPH_MON/POOL/USER/KEY) to run against real Ceph")
	}
	mon, pool, user, key := os.Getenv("CEPH_MON"), os.Getenv("CEPH_POOL"), os.Getenv("CEPH_USER"), os.Getenv("CEPH_KEY")
	if mon == "" || pool == "" || user == "" || key == "" {
		t.Fatal("CEPH_MON, CEPH_POOL, CEPH_USER and CEPH_KEY must all be set")
	}
	const instance = "real"
	be := New(map[string]ClusterConfig{instance: {Monitors: []string{mon}, Pool: pool, UserID: user}}, "", "", nil)
	secrets := map[string]string{"userID": user, "userKey": key}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	conn, cleanup, err := be.connArgs(be.clusters[instance], instance, secrets)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	params := map[string]string{"pool": pool}
	name := "clonemeta-" + time.Now().Format("150405")

	src, err := be.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{Name: name, CapacityBytes: 16 << 20, Instance: instance, Parameters: params, Secrets: secrets})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	srcRef := bardplugin.VolumeRef{Instance: instance, Location: src.Location, Name: src.Name}
	srcSpec := src.Location + "/" + src.Name
	// Cleanup: un-mark the source so its own delete can reap it, then delete with
	// retries (a PVC clone's out-of-band flatten may still hold it briefly).
	defer func() {
		_ = be.imageMetaRemove(ctx, conn, srcSpec, imgMetaStatic)
		for i := 0; i < 30; i++ {
			if be.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{Volume: srcRef, Secrets: secrets}) == nil {
				return
			}
			time.Sleep(2 * time.Second)
		}
		t.Errorf("cleanup: source %s not deleted", srcSpec)
	}()
	if err := be.imageMetaSet(ctx, conn, srcSpec, imgMetaStatic, "true"); err != nil {
		t.Fatal(err)
	}
	if err := be.imageMetaSet(ctx, conn, srcSpec, "bard.pvname", "pv-of-the-source"); err != nil {
		t.Fatal(err)
	}
	snap, err := be.CreateSnapshot(ctx, &bardplugin.CreateSnapshotRequest{Name: name + "-snap", SourceVolume: srcRef, Secrets: secrets})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	snapRef := bardplugin.VolumeRef{Instance: instance, Location: snap.Location, Name: snap.Name}
	defer be.DeleteSnapshot(ctx, &bardplugin.DeleteSnapshotRequest{Snapshot: snapRef, Secrets: secrets})

	for _, tc := range []struct {
		kind string
		req  *bardplugin.CreateVolumeRequest
	}{
		{"restore", &bardplugin.CreateVolumeRequest{Name: name + "-restore", SourceSnapshot: &snapRef}},
		{"clone", &bardplugin.CreateVolumeRequest{Name: name + "-clone", SourceVolume: &srcRef}},
	} {
		tc.req.CapacityBytes, tc.req.Instance, tc.req.Parameters, tc.req.Secrets = 16<<20, instance, params, secrets
		cp, err := be.CreateVolume(ctx, tc.req)
		if err != nil {
			t.Fatalf("%s: CreateVolume: %v", tc.kind, err)
		}
		cpRef := bardplugin.VolumeRef{Instance: instance, Location: cp.Location, Name: cp.Name}
		cpSpec := cp.Location + "/" + cp.Name
		for _, k := range []string{imgMetaStatic, "bard.pvname"} {
			if v, err := be.imageMetaGetChecked(ctx, conn, cpSpec, k); err != nil || v != "" {
				t.Errorf("%s: inherited %s=%q (err %v)", tc.kind, k, v, err)
			}
		}
		if v, _ := be.imageMetaGetChecked(ctx, conn, cpSpec, imgMetaSource); v != sourceDesc(tc.req) {
			t.Errorf("%s: bard.source=%q, want %q", tc.kind, v, sourceDesc(tc.req))
		}
		var delErr error
		for i := 0; i < 30; i++ {
			if delErr = be.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{Volume: cpRef, Secrets: secrets}); delErr == nil {
				break
			}
			time.Sleep(2 * time.Second)
		}
		if delErr != nil {
			t.Fatalf("%s: DeleteVolume: %v", tc.kind, delErr)
		}
		if _, err := be.imageInfo(ctx, conn, cpSpec); !errors.Is(err, errImageNotFound) {
			t.Fatalf("%s: DeleteVolume reported success but the image remains (err %v) -- leaked", tc.kind, err)
		}
	}
	// The admin-owned source is still protected.
	if err := be.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{Volume: srcRef, Secrets: secrets}); err != nil {
		t.Fatalf("DeleteVolume of the static source: %v", err)
	}
	if _, err := be.imageInfo(ctx, conn, srcSpec); err != nil {
		t.Fatalf("the static source must survive DeleteVolume: %v", err)
	}
}
