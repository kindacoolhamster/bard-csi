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

// CreateSnapshot gives an encrypted volume's snapshot its own copy of the key
// (idempotently: a retried create mints nothing new), and DeleteSnapshot reaps that
// copy and the snapshot's descriptor while leaving the source's key alone.
func TestSnapshotOwnsAndReapsItsKey(t *testing.T) {
	for _, kmsID := range []string{"k8s", "vault", "kmip"} {
		t.Run(kmsID, func(t *testing.T) {
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
					"k8s":   {Type: "secrets-metadata"},
					"vault": {Type: "vault", Address: srv.URL, Token: "dev-token"},
					"kmip":  {Type: "kmip", KMIPEndpoint: f.addr, InsecureSkipVerify: true},
				})
			// external counts the key material held outside the image, per provider.
			external := func() int {
				switch kmsID {
				case "vault":
					return len(fv.store)
				case "kmip":
					return f.count()
				}
				return 0
			}

			src, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
				Name: "src", CapacityBytes: 1 << 30, Instance: "east",
				Parameters: map[string]string{"pool": "replicapool", paramEncrypted: "true", paramEncryptionKMSID: kmsID},
			})
			if err != nil {
				t.Fatal(err)
			}
			srcRef := bardplugin.VolumeRef{Instance: "east", Location: src.Location, Name: src.Name}
			srcSpec := src.Location + "/" + src.Name
			srcPass, err := b.encryptionPassphrase(ctx, "east", kmsID, srcSpec, nil)
			if err != nil {
				t.Fatal(err)
			}
			before := external()

			snapReq := &bardplugin.CreateSnapshotRequest{Name: "s", SourceVolume: srcRef}
			snap, err := b.CreateSnapshot(ctx, snapReq)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := b.CreateSnapshot(ctx, snapReq); err != nil { // the CO's retry
				t.Fatal(err)
			}
			snapSpec := snap.Location + "/" + snap.Name
			cc, _ := b.cluster("east")
			conn, cleanup, err := b.connArgs(cc, "east", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			if got, _ := b.imageMetaGetChecked(ctx, conn, snapSpec, imgMetaKMSID); got != kmsID {
				t.Fatalf("the snapshot must record its own descriptor, kms id %q", got)
			}
			if kmsID != "k8s" && external() != before+1 {
				t.Fatalf("the snapshot must own exactly one copy of the key, external %d -> %d", before, external())
			}
			snapPass, err := b.encryptionPassphraseFor(ctx, nil, "east", kmsID, snapSpec, srcSpec, nil)
			if err != nil || snapPass != srcPass {
				t.Fatalf("the snapshot's copy must hold the snapshot-time key (err %v)", err)
			}

			snapRef := bardplugin.VolumeRef{Instance: "east", Location: snap.Location, Name: snap.Name}
			for i := 0; i < 2; i++ { // the second delete is the CO's retry: idempotent
				if err := b.DeleteSnapshot(ctx, &bardplugin.DeleteSnapshotRequest{Snapshot: snapRef}); err != nil {
					t.Fatalf("DeleteSnapshot #%d: %v", i+1, err)
				}
			}
			if external() != before {
				t.Fatalf("DeleteSnapshot must reap the snapshot's key copy, external %d, want %d", external(), before)
			}
			for _, k := range []string{imgMetaKMSID, imgMetaKeyID, imgMetaWrappedDEK, cephenc.MetaKMIPUID, cephenc.MetaKMIPOwner} {
				if v, _ := b.imageMetaGetChecked(ctx, conn, snapSpec, k); v != "" {
					t.Fatalf("the snapshot's descriptor key %s must be removed", k)
				}
			}
			if p, err := b.encryptionPassphrase(ctx, "east", kmsID, srcSpec, nil); err != nil || p != srcPass {
				t.Fatalf("the source's key must survive its snapshot's delete (err %v)", err)
			}
		})
	}
}
