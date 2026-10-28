package cephplugin

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/kindacoolhamster/bard-csi/internal/cephenc"
	"github.com/kindacoolhamster/bard-csi/internal/fakerun"
	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// A retried CreateVolume of an encrypted restore -- the provisioner re-sends it after
// a timeout or a lost response, so the image already exists -- must take the same
// inheritance path as the first attempt. It used to skip inheritEncryption on the
// resumed path, so the retry advertised no encryptionKeyID and the node resolved a
// key that could not open the copied LUKS header. Each case also rewinds the
// clone's key state to "first attempt died before cloneKey ran" and asserts the retry
// completes it, never sharing the source's key material.
func TestEncryptedRestoreRetryKeepsInheritedKey(t *testing.T) {
	for _, kmsID := range []string{"" /*derived*/, "vault", "kmip"} {
		t.Run("kms="+orDefault(kmsID, "derived"), func(t *testing.T) {
			ctx := context.Background()
			keyDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(keyDir, "east"), []byte("instance-master-key"), 0o600); err != nil {
				t.Fatal(err)
			}
			fv := newFakeVault()
			srv := httptest.NewServer(fv.handler())
			defer srv.Close()
			f := startFakeKMIP(t)
			b := New(map[string]ClusterConfig{"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin"}}, "", "", fakerun.New()).
				WithEncryption(keyDir).
				WithKMS(map[string]KMSConfig{
					"vault": {Type: "vault", Address: srv.URL, Token: "dev-token"},
					"kmip":  {Type: "kmip", KMIPEndpoint: f.addr, InsecureSkipVerify: true},
				})
			params := map[string]string{"pool": "replicapool", paramEncrypted: "true"}
			if kmsID != "" {
				params[paramEncryptionKMSID] = kmsID
			}

			src, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{Name: "src", CapacityBytes: 1 << 30, Instance: "east", Parameters: params})
			if err != nil {
				t.Fatal(err)
			}
			srcSpec := src.Location + "/" + src.Name
			srcPass, err := b.encryptionPassphrase(ctx, "east", kmsID, srcSpec, nil)
			if err != nil {
				t.Fatal(err)
			}
			snap, err := b.CreateSnapshot(ctx, &bardplugin.CreateSnapshotRequest{
				Name: "s", SourceVolume: bardplugin.VolumeRef{Instance: "east", Location: src.Location, Name: src.Name},
			})
			if err != nil {
				t.Fatal(err)
			}
			restore := &bardplugin.CreateVolumeRequest{
				Name: "r", CapacityBytes: 1 << 30, Instance: "east", Parameters: params,
				SourceSnapshot: &bardplugin.VolumeRef{Instance: "east", Location: snap.Location, Name: snap.Name},
			}
			r1, err := b.CreateVolume(ctx, restore)
			if err != nil {
				t.Fatal(err)
			}
			cloneSpec := r1.Location + "/" + r1.Name
			cc, _ := b.cluster("east")
			conn, cleanup, err := b.connArgs(cc, "east", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()

			// Rewind to "the first attempt died after the clone was provisioned but
			// before its key was cloned": the clone carries only what librbd copied.
			switch kmsID {
			case "vault":
				if err := b.kms.DeleteKey(ctx, conn, "east", "vault", cloneSpec); err != nil {
					t.Fatal(err)
				}
				if len(fv.store) != 1 {
					t.Fatalf("rewind: only the source's Vault entry should remain, have %d", len(fv.store))
				}
			case "kmip":
				srcUID, _ := b.imageMetaGetChecked(ctx, conn, srcSpec, cephenc.MetaKMIPUID)
				ownUID, _ := b.imageMetaGetChecked(ctx, conn, cloneSpec, cephenc.MetaKMIPUID)
				if err := b.kms.DeleteKey(ctx, conn, "east", "kmip", cloneSpec); err != nil {
					t.Fatal(err)
				}
				if f.has(ownUID) || !f.has(srcUID) {
					t.Fatal("rewind: the clone's own object should be gone, the source's kept")
				}
				if err := b.imageMetaSet(ctx, conn, cloneSpec, cephenc.MetaKMIPUID, srcUID); err != nil {
					t.Fatal(err)
				}
			}

			r2, err := b.CreateVolume(ctx, restore) // the provisioner's retry
			if err != nil {
				t.Fatalf("retried restore: %v", err)
			}
			k1, k2 := r1.Context[ctxEncryptionKeyID], r2.Context[ctxEncryptionKeyID]
			if k1 == "" || k1 != k2 {
				t.Fatalf("the retry must advertise the same inherited key identity: first %q, retry %q", k1, k2)
			}
			clonePass, err := b.encryptionPassphraseFor(ctx, nil, "east", kmsID, cloneSpec, k2, nil)
			if err != nil {
				t.Fatal(err)
			}
			if clonePass != srcPass {
				t.Fatal("the retried restore must open with the SOURCE's key")
			}

			// A further retry of a fully inherited clone changes nothing (KMIP used to
			// register, and leak, a fresh HSM object on every cloneKey).
			if r3, err := b.CreateVolume(ctx, restore); err != nil || r3.Context[ctxEncryptionKeyID] != k2 {
				t.Fatalf("a second retry must be a no-op (err %v)", err)
			}

			// Self-contained: deleting the restore leaves the source's key intact.
			if kmsID == "kmip" {
				srcUID, _ := b.imageMetaGetChecked(ctx, conn, srcSpec, cephenc.MetaKMIPUID)
				cloneUID, _ := b.imageMetaGetChecked(ctx, conn, cloneSpec, cephenc.MetaKMIPUID)
				if cloneUID == srcUID {
					t.Fatal("the retry must give the clone its own KMIP object, not the copied source UID")
				}
				if f.count() != 2 {
					t.Fatalf("want exactly 2 KMIP objects (source + clone), have %d", f.count())
				}
			}
			if err := b.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{Volume: bardplugin.VolumeRef{Instance: "east", Location: r2.Location, Name: r2.Name}}); err != nil {
				t.Fatal(err)
			}
			if p, err := b.encryptionPassphrase(ctx, "east", kmsID, srcSpec, nil); err != nil || p != srcPass {
				t.Fatalf("deleting the restore must not touch the source's key (err %v)", err)
			}
		})
	}
}
