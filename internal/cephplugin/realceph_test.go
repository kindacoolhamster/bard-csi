package cephplugin

import (
	"context"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// TestRealCeph exercises the Ceph RBD plugin's control plane against a *real*
// cluster using the real `rbd` CLI. Skipped unless BARD_CEPH_TEST=1. Run from a
// host that can reach the mons:
//
//	BARD_CEPH_TEST=1 CEPH_MON=192.168.1.225:3300 CEPH_POOL=k8s-csi-test \
//	CEPH_USER=k8s-csi-test CEPH_KEY=AQ...== \
//	go test ./internal/cephplugin/ -run TestRealCeph -v
func TestRealCeph(t *testing.T) {
	if os.Getenv("BARD_CEPH_TEST") != "1" {
		t.Skip("set BARD_CEPH_TEST=1 (and CEPH_MON/POOL/USER/KEY) to run against real Ceph")
	}
	mon, pool, user, key := os.Getenv("CEPH_MON"), os.Getenv("CEPH_POOL"), os.Getenv("CEPH_USER"), os.Getenv("CEPH_KEY")
	if mon == "" || pool == "" || user == "" || key == "" {
		t.Fatal("CEPH_MON, CEPH_POOL, CEPH_USER and CEPH_KEY must all be set")
	}

	const instance = "real"
	be := New(map[string]ClusterConfig{
		instance: {Monitors: []string{mon}, Pool: pool, UserID: user},
	}, "", "", nil) // keyDir/stateDir "" => use CSI secret, findmnt fallback; nil => real ExecRunner

	secrets := map[string]string{"userID": user, "userKey": key}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	name := "sanity-real-" + time.Now().Format("150405")
	vol, err := be.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: name, CapacityBytes: 64 << 20, Instance: instance,
		Parameters: map[string]string{"pool": pool}, Secrets: secrets,
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	t.Logf("created volume %s/%s", vol.Location, vol.Name)
	volRef := bardplugin.VolumeRef{Instance: instance, Location: vol.Location, Name: vol.Name}
	defer func() {
		if err := be.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{Volume: volRef, Secrets: secrets}); err != nil {
			t.Errorf("DeleteVolume: %v", err)
		}
	}()

	// Idempotent re-create returns without error.
	if _, err := be.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: name, CapacityBytes: 64 << 20, Instance: instance,
		Parameters: map[string]string{"pool": pool}, Secrets: secrets,
	}); err != nil {
		t.Fatalf("CreateVolume (idempotent retry): %v", err)
	}

	snap, err := be.CreateSnapshot(ctx, &bardplugin.CreateSnapshotRequest{
		Name: name + "-snap", SourceVolume: volRef, Secrets: secrets,
	})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	t.Logf("created snapshot %s/%s", snap.Location, snap.Name)
	defer func() {
		snapRef := bardplugin.VolumeRef{Instance: instance, Location: snap.Location, Name: snap.Name}
		if err := be.DeleteSnapshot(ctx, &bardplugin.DeleteSnapshotRequest{Snapshot: snapRef, Secrets: secrets}); err != nil {
			t.Errorf("DeleteSnapshot: %v", err)
		}
	}()

	if _, err := be.ExpandVolume(ctx, &bardplugin.ExpandVolumeRequest{Volume: volRef, NewSizeBytes: 128 << 20, Secrets: secrets}); err != nil {
		t.Fatalf("ExpandVolume: %v", err)
	}
	t.Log("expanded volume to 128 MiB")

	// PVC-PVC clone: temp snapshot + COW clone, grown to the (larger) request,
	// flattened out of band. Proven independent by deleting the source while the
	// clone is still alive.
	clone, err := be.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: name + "-clone", CapacityBytes: 256 << 20, Instance: instance,
		Parameters: map[string]string{"pool": pool}, Secrets: secrets,
		SourceVolume: &volRef,
	})
	if err != nil {
		t.Fatalf("CreateVolume (clone): %v", err)
	}
	t.Logf("cloned volume %s/%s", clone.Location, clone.Name)
	cloneRef := bardplugin.VolumeRef{Instance: instance, Location: clone.Location, Name: clone.Name}
	defer func() {
		if err := be.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{Volume: cloneRef, Secrets: secrets}); err != nil {
			t.Errorf("DeleteVolume (clone): %v", err)
		}
	}()
	if clone.CapacityBytes != 256<<20 {
		t.Fatalf("clone capacity = %d, want %d (grown past the 128MiB source)", clone.CapacityBytes, int64(256<<20))
	}

	conn, connCleanup, err := be.connArgs(be.clusters[instance], instance, secrets)
	if err != nil {
		t.Fatal(err)
	}
	defer connCleanup()
	cloneSpec := clone.Location + "/" + clone.Name
	if got, err := be.imageInfo(ctx, conn, cloneSpec); err != nil || got != 256 {
		t.Fatalf("clone image size = %dMiB (err %v), want 256", got, err)
	}
	// Wait out the background flatten: the parent link must be severed.
	for deadline := time.Now().Add(45 * time.Second); ; {
		parent, _, perr := be.imageParent(ctx, conn, cloneSpec)
		if perr != nil {
			t.Fatalf("imageParent: %v", perr)
		}
		if parent == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("clone still has parent %q after 45s; background flatten did not land", parent)
		}
		time.Sleep(2 * time.Second)
	}
	t.Log("clone flattened (parent link severed)")

	// With the snapshot and then the source gone, the clone must live on -- the
	// live proof that the COW clone converges to rbd-cp independence. The deferred
	// deletes for both become idempotent no-ops.
	snapRef := bardplugin.VolumeRef{Instance: instance, Location: snap.Location, Name: snap.Name}
	if err := be.DeleteSnapshot(ctx, &bardplugin.DeleteSnapshotRequest{Snapshot: snapRef, Secrets: secrets}); err != nil {
		t.Fatalf("DeleteSnapshot (before source delete): %v", err)
	}
	if err := be.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{Volume: volRef, Secrets: secrets}); err != nil {
		t.Fatalf("DeleteVolume (source, while clone alive): %v", err)
	}
	if got, err := be.imageInfo(ctx, conn, cloneSpec); err != nil || got != 256 {
		t.Fatalf("clone must survive the source delete: size %dMiB, err %v", got, err)
	}
	t.Log("source deleted while the clone lives; clone intact")
}

// TestRealCephDeleteWithSnapshotKeepsKey proves the DeleteVolume snapshot guard
// against real `rbd`: an encrypted volume with a live snapshot is refused with its
// KMS key intact, while a snapshot that was deleted but still has a linked restore
// (clone v2 moves it to the trash namespace) does NOT block -- the key is reaped and
// the delete converges once the restore is flattened. The KMS is an in-process fake
// Vault; the rbd side is real. Same env gate as TestRealCeph.
func TestRealCephDeleteWithSnapshotKeepsKey(t *testing.T) {
	if os.Getenv("BARD_CEPH_TEST") != "1" {
		t.Skip("set BARD_CEPH_TEST=1 (and CEPH_MON/POOL/USER/KEY) to run against real Ceph")
	}
	mon, pool, user, key := os.Getenv("CEPH_MON"), os.Getenv("CEPH_POOL"), os.Getenv("CEPH_USER"), os.Getenv("CEPH_KEY")
	if mon == "" || pool == "" || user == "" || key == "" {
		t.Fatal("CEPH_MON, CEPH_POOL, CEPH_USER and CEPH_KEY must all be set")
	}
	fv := newFakeVault()
	srv := httptest.NewServer(fv.handler())
	defer srv.Close()
	const instance = "real"
	be := New(map[string]ClusterConfig{instance: {Monitors: []string{mon}, Pool: pool, UserID: user}}, "", "", nil).
		WithKMS(map[string]KMSConfig{"vault-test": {Type: "vault", Address: srv.URL, Token: "dev-token"}})
	secrets := map[string]string{"userID": user, "userKey": key}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	params := map[string]string{"pool": pool, paramEncrypted: "true", paramEncryptionKMSID: "vault-test"}

	name := "delkey-" + time.Now().Format("150405")
	vol, err := be.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: name, CapacityBytes: 16 << 20, Instance: instance, Parameters: params, Secrets: secrets,
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	volRef := bardplugin.VolumeRef{Instance: instance, Location: vol.Location, Name: vol.Name}
	defer be.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{Volume: volRef, Secrets: secrets})
	if _, err := be.encryptionPassphrase(ctx, instance, "vault-test", vol.Location+"/"+vol.Name, secrets); err != nil {
		t.Fatal(err)
	}
	snap, err := be.CreateSnapshot(ctx, &bardplugin.CreateSnapshotRequest{Name: name + "-snap", SourceVolume: volRef, Secrets: secrets})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	snapRef := bardplugin.VolumeRef{Instance: instance, Location: snap.Location, Name: snap.Name}
	defer be.DeleteSnapshot(ctx, &bardplugin.DeleteSnapshotRequest{Snapshot: snapRef, Secrets: secrets})

	// 1. Live snapshot: refused, volume untouched (key still in the KMS).
	if err := be.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{Volume: volRef, Secrets: secrets}); err == nil {
		t.Fatal("DeleteVolume must be refused while a live snapshot exists")
	} else {
		t.Logf("refused as expected: %v", err)
	}
	if len(fv.store) != 1 {
		t.Fatalf("the source key must survive a refused delete, %d entries", len(fv.store))
	}

	// 2. Restore from the snapshot (gets its own key copy), then delete the snapshot:
	// with a linked clone it moves to the trash namespace instead of vanishing.
	restore, err := be.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: name + "-restore", CapacityBytes: 16 << 20, Instance: instance, Parameters: params,
		Secrets: secrets, SourceSnapshot: &snapRef,
	})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	restoreRef := bardplugin.VolumeRef{Instance: instance, Location: restore.Location, Name: restore.Name}
	defer be.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{Volume: restoreRef, Secrets: secrets})
	if len(fv.store) != 2 {
		t.Fatalf("the restore must hold its own key copy, %d entries", len(fv.store))
	}
	if err := be.DeleteSnapshot(ctx, &bardplugin.DeleteSnapshotRequest{Snapshot: snapRef, Secrets: secrets}); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}

	// 3. Only a trashed snapshot remains: the guard must not block. The delete may
	// fail on the linked clone until the flatten it kicks lands; it must converge.
	var lastErr error
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		if lastErr = be.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{Volume: volRef, Secrets: secrets}); lastErr == nil {
			break
		}
		t.Logf("delete retry: %v", lastErr)
	}
	if lastErr != nil {
		t.Fatalf("DeleteVolume did not converge with only a trashed snapshot: %v", lastErr)
	}
	if len(fv.store) != 1 {
		t.Fatalf("source key reaped and restore key kept: want 1 entry, have %d", len(fv.store))
	}
	if err := be.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{Volume: restoreRef, Secrets: secrets}); err != nil {
		t.Fatalf("DeleteVolume (restore): %v", err)
	}
	if len(fv.store) != 0 {
		t.Fatalf("all keys reaped: %d entries left", len(fv.store))
	}
}
