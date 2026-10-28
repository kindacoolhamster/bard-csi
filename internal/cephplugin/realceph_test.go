package cephplugin

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kindacoolhamster/bard-csi/internal/cephenc"
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

// TestRealCephKMSFailClosed proves, against real `rbd image-meta`, that the metadata
// store the KMS providers sit on classifies real Ceph errors correctly: an absent key
// or image reads as absent (so a fresh volume's first stage still mints), while a
// failed read (here: a wrong cephx key) is an error, never "absent" -- the misread
// that let a provider mint over, and destroy, a stored key. Same env gate as
// TestRealCeph.
func TestRealCephKMSFailClosed(t *testing.T) {
	if os.Getenv("BARD_CEPH_TEST") != "1" {
		t.Skip("set BARD_CEPH_TEST=1 (and CEPH_MON/POOL/USER/KEY) to run against real Ceph")
	}
	mon, pool, user, key := os.Getenv("CEPH_MON"), os.Getenv("CEPH_POOL"), os.Getenv("CEPH_USER"), os.Getenv("CEPH_KEY")
	if mon == "" || pool == "" || user == "" || key == "" {
		t.Fatal("CEPH_MON, CEPH_POOL, CEPH_USER and CEPH_KEY must all be set")
	}
	const instance = "real"
	keyDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(keyDir, instance), []byte("real-ceph-test-master-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	be := New(map[string]ClusterConfig{instance: {Monitors: []string{mon}, Pool: pool, UserID: user}}, "", "", nil).
		WithEncryption(keyDir).
		WithKMS(map[string]KMSConfig{"k8s": {Type: "secrets-metadata"}})
	secrets := map[string]string{"userID": user, "userKey": key}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	name := "kms-failclosed-" + time.Now().Format("150405")
	vol, err := be.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: name, CapacityBytes: 16 << 20, Instance: instance,
		Parameters: map[string]string{"pool": pool}, Secrets: secrets,
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	volRef := bardplugin.VolumeRef{Instance: instance, Location: vol.Location, Name: vol.Name}
	defer func() {
		if err := be.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{Volume: volRef, Secrets: secrets}); err != nil {
			t.Errorf("DeleteVolume: %v", err)
		}
	}()
	spec := vol.Location + "/" + vol.Name
	conn, connCleanup, err := be.connArgs(be.clusters[instance], instance, secrets)
	if err != nil {
		t.Fatal(err)
	}
	defer connCleanup()

	if v, err := be.MetaGet(ctx, conn, spec, imgMetaWrappedDEK); v != "" || err != nil {
		t.Fatalf("absent key on a real image must read (\"\", nil), got %q, %v", v, err)
	}
	if v, err := be.MetaGet(ctx, conn, vol.Location+"/no-such-image-"+name, imgMetaWrappedDEK); v != "" || err != nil {
		t.Fatalf("absent image must read (\"\", nil), got %q, %v", v, err)
	}
	p1, err := be.encryptionPassphrase(ctx, instance, "k8s", spec, secrets)
	if err != nil {
		t.Fatalf("first stage must mint on a real absent key: %v", err)
	}
	stored, err := be.MetaGet(ctx, conn, spec, imgMetaWrappedDEK)
	if err != nil || stored == "" {
		t.Fatalf("expected the wrapped DEK in real image-meta, got %q, %v", stored, err)
	}

	// A real failed read: same mon, wrong cephx key.
	bad := []byte(key)
	if bad[20] == 'A' {
		bad[20] = 'B'
	} else {
		bad[20] = 'A'
	}
	badSecrets := map[string]string{"userID": user, "userKey": string(bad)}
	if _, err := be.encryptionPassphrase(ctx, instance, "k8s", spec, badSecrets); err == nil {
		t.Fatal("a stage whose metadata read fails must error, not mint")
	} else {
		t.Logf("failed read surfaced as: %v", err)
	}

	if got, err := be.MetaGet(ctx, conn, spec, imgMetaWrappedDEK); err != nil || got != stored {
		t.Fatalf("stored wrapped DEK changed: %q -> %q (%v)", stored, got, err)
	}
	p2, err := be.encryptionPassphrase(ctx, instance, "k8s", spec, secrets)
	if err != nil || p2 != p1 {
		t.Fatalf("restage must resolve the original key (err %v, same=%v)", err, p2 == p1)
	}
}

// TestRealCephEncryptedRestoreRetry drives a retried encrypted restore through real
// `rbd` (whose clone copies the parent's image-meta onto the child): the retry must
// advertise the same inherited key identity and resolve the SOURCE's passphrase, and
// for KMIP the clone must end up with its own HSM object, not the copied source UID.
// KMS servers are in-process fakes; the rbd side is real. Same env gate as TestRealCeph.
func TestRealCephEncryptedRestoreRetry(t *testing.T) {
	if os.Getenv("BARD_CEPH_TEST") != "1" {
		t.Skip("set BARD_CEPH_TEST=1 (and CEPH_MON/POOL/USER/KEY) to run against real Ceph")
	}
	mon, pool, user, key := os.Getenv("CEPH_MON"), os.Getenv("CEPH_POOL"), os.Getenv("CEPH_USER"), os.Getenv("CEPH_KEY")
	if mon == "" || pool == "" || user == "" || key == "" {
		t.Fatal("CEPH_MON, CEPH_POOL, CEPH_USER and CEPH_KEY must all be set")
	}
	const instance = "real"
	for _, kmsID := range []string{"" /*derived*/, "vault", "kmip"} {
		t.Run("kms="+orDefault(kmsID, "derived"), func(t *testing.T) {
			keyDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(keyDir, instance), []byte("real-ceph-test-master-key"), 0o600); err != nil {
				t.Fatal(err)
			}
			fv := newFakeVault()
			srv := httptest.NewServer(fv.handler())
			defer srv.Close()
			f := startFakeKMIP(t)
			be := New(map[string]ClusterConfig{instance: {Monitors: []string{mon}, Pool: pool, UserID: user}}, "", "", nil).
				WithEncryption(keyDir).
				WithKMS(map[string]KMSConfig{
					"vault": {Type: "vault", Address: srv.URL, Token: "dev-token"},
					"kmip":  {Type: "kmip", KMIPEndpoint: f.addr, InsecureSkipVerify: true},
				})
			secrets := map[string]string{"userID": user, "userKey": key}
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			params := map[string]string{"pool": pool, paramEncrypted: "true"}
			if kmsID != "" {
				params[paramEncryptionKMSID] = kmsID
			}
			name := "cloneretry-" + orDefault(kmsID, "derived") + "-" + time.Now().Format("150405")

			src, err := be.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{Name: name, CapacityBytes: 16 << 20, Instance: instance, Parameters: params, Secrets: secrets})
			if err != nil {
				t.Fatalf("CreateVolume: %v", err)
			}
			srcRef := bardplugin.VolumeRef{Instance: instance, Location: src.Location, Name: src.Name}
			srcSpec := src.Location + "/" + src.Name
			srcPass, err := be.encryptionPassphrase(ctx, instance, kmsID, srcSpec, secrets)
			if err != nil {
				t.Fatal(err)
			}
			snap, err := be.CreateSnapshot(ctx, &bardplugin.CreateSnapshotRequest{Name: name + "-snap", SourceVolume: srcRef, Secrets: secrets})
			if err != nil {
				t.Fatalf("CreateSnapshot: %v", err)
			}
			snapRef := bardplugin.VolumeRef{Instance: instance, Location: snap.Location, Name: snap.Name}
			restore := &bardplugin.CreateVolumeRequest{
				Name: name + "-restore", CapacityBytes: 16 << 20, Instance: instance, Parameters: params,
				Secrets: secrets, SourceSnapshot: &snapRef,
			}
			r1, err := be.CreateVolume(ctx, restore)
			if err != nil {
				t.Fatalf("restore: %v", err)
			}
			cloneRef := bardplugin.VolumeRef{Instance: instance, Location: r1.Location, Name: r1.Name}
			cloneSpec := r1.Location + "/" + r1.Name
			// Teardown in dependency order: restore, snapshot, source.
			defer func() {
				for _, step := range []func() error{
					func() error {
						return be.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{Volume: cloneRef, Secrets: secrets})
					},
					func() error {
						return be.DeleteSnapshot(ctx, &bardplugin.DeleteSnapshotRequest{Snapshot: snapRef, Secrets: secrets})
					},
					func() error {
						return be.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{Volume: srcRef, Secrets: secrets})
					},
				} {
					if err := step(); err != nil {
						t.Errorf("teardown: %v", err)
					}
				}
			}()
			conn, connCleanup, err := be.connArgs(be.clusters[instance], instance, secrets)
			if err != nil {
				t.Fatal(err)
			}
			defer connCleanup()

			// Rewind the clone to "provisioned, key never cloned": it keeps only what
			// real rbd clone copied from the parent.
			switch kmsID {
			case "vault":
				if err := be.kms.DeleteKey(ctx, conn, instance, "vault", cloneSpec); err != nil {
					t.Fatal(err)
				}
			case "kmip":
				srcUID, err := be.MetaGet(ctx, conn, srcSpec, cephenc.MetaKMIPUID)
				if err != nil {
					t.Fatal(err)
				}
				if err := be.kms.DeleteKey(ctx, conn, instance, "kmip", cloneSpec); err != nil {
					t.Fatal(err)
				}
				if err := be.imageMetaSet(ctx, conn, cloneSpec, cephenc.MetaKMIPUID, srcUID); err != nil {
					t.Fatal(err)
				}
				if err := be.imageMetaRemove(ctx, conn, cloneSpec, cephenc.MetaKMIPOwner); err != nil {
					t.Fatal(err)
				}
			}

			r2, err := be.CreateVolume(ctx, restore)
			if err != nil {
				t.Fatalf("retried restore: %v", err)
			}
			k1, k2 := r1.Context[ctxEncryptionKeyID], r2.Context[ctxEncryptionKeyID]
			if k1 == "" || k1 != k2 {
				t.Fatalf("retry must advertise the same key identity: %q vs %q", k1, k2)
			}
			clonePass, err := be.encryptionPassphraseFor(ctx, nil, instance, kmsID, cloneSpec, k2, secrets)
			if err != nil || clonePass != srcPass {
				t.Fatalf("retried restore must resolve the source's passphrase (err %v)", err)
			}
			if kmsID == "kmip" {
				srcUID, _ := be.MetaGet(ctx, conn, srcSpec, cephenc.MetaKMIPUID)
				cloneUID, _ := be.MetaGet(ctx, conn, cloneSpec, cephenc.MetaKMIPUID)
				if cloneUID == srcUID || f.count() != 3 { // source + snapshot + clone
					t.Fatalf("clone must own its KMIP object (clone %q src %q, %d objects)", cloneUID, srcUID, f.count())
				}
			}
			t.Logf("retry advertised %s and opened with the source key", k2)
		})
	}
}
