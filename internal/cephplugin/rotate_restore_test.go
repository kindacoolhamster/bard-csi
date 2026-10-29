package cephplugin

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/kindacoolhamster/bard-csi/internal/fakerun"
	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// rrProvider is one stored-key KMS provider exercised by the rotate-then-restore
// harness: id is the encryptionKMSID, name labels the subtest.
type rrProvider struct{ id, name string }

// rrBackend is a fakerun-backed Backend with the instance master key dir and every
// rotatable provider wired: secrets-metadata, Vault (KV v2), KMIP (mutual-TLS fake
// server), and AWS KMS (SigV4 fake). The fakes are the same ones the per-provider
// unit tests use.
func rrBackend(t *testing.T) *Backend {
	t.Helper()
	keyDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(keyDir, "east"), []byte("instance-master-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	vault := httptest.NewServer(newFakeVault().handler())
	t.Cleanup(vault.Close)
	awsSrv := httptest.NewServer(newFakeKMS().handler())
	t.Cleanup(awsSrv.Close)
	kmip := startFakeKMIP(t)

	return New(map[string]ClusterConfig{"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin"}}, "", "", fakerun.New()).
		WithEncryption(keyDir).
		WithKMS(map[string]KMSConfig{
			"k8s":   {Type: "secrets-metadata"},
			"vault": {Type: "vault", Address: vault.URL, Token: "dev-token"},
			"kmip":  {Type: "kmip", KMIPEndpoint: kmip.addr, InsecureSkipVerify: true},
			"aws":   {Type: "aws-kms", Region: "us-east-1", KeyID: "alias/bard-test", Endpoint: awsSrv.URL, AccessKeyID: "test", SecretAccessKey: "test"},
		})
}

// rrSnapshotRestore drives: encrypted source -> first stage (oldPass) -> snapshot ->
// optional key rotation (newPass) -> restore-to-new-PVC -> the restore's passphrase
// resolved the way the node stages it (keyID from the restore's volume context).
// Harness steps that cannot proceed are fatal; the restore's own resolution error is
// returned so the caller can classify it.
func rrSnapshotRestore(t *testing.T, kmsID string, rotate bool) (oldPass, newPass, restorePass string, restoreErr error) {
	t.Helper()
	ctx := context.Background()
	b := rrBackend(t)
	params := map[string]string{paramPool: "replicapool", paramEncrypted: "true", paramEncryptionKMSID: kmsID}

	src, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: "rr-src", CapacityBytes: 1 << 30, Instance: "east", Parameters: params,
	})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	srcSpec := src.Location + "/" + src.Name

	// First stage: the node resolves (and for stored-key providers, persists) the
	// passphrase. This is the key the snapshot's LUKS header is sealed under.
	oldPass, err = b.encryptionPassphrase(ctx, "east", kmsID, srcSpec, nil)
	if err != nil {
		t.Fatalf("source first-stage passphrase: %v", err)
	}

	snap, err := b.CreateSnapshot(ctx, &bardplugin.CreateSnapshotRequest{
		Name:         "rr-snap",
		SourceVolume: bardplugin.VolumeRef{Instance: "east", Location: src.Location, Name: src.Name},
	})
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}

	if rotate {
		err := b.kms.RotateKey(ctx, nil, "east", kmsID, srcSpec, srcSpec, nil, func(p string) error {
			newPass = p
			return nil
		})
		if err != nil {
			t.Fatalf("rotate key: %v", err)
		}
		if newPass == "" || newPass == oldPass {
			t.Fatal("harness: rotation produced no new, distinct passphrase")
		}
		// Rotation must have taken effect on the source before we restore from it.
		now, err := b.encryptionPassphrase(ctx, "east", kmsID, srcSpec, nil)
		if err != nil {
			t.Fatalf("source passphrase after rotation: %v", err)
		}
		if now != newPass {
			t.Fatal("harness: the source did not resolve the rotated key")
		}
	}

	restore, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: "rr-restore", CapacityBytes: 1 << 30, Instance: "east", Parameters: params,
		SourceSnapshot: &bardplugin.VolumeRef{Instance: "east", Location: snap.Location, Name: snap.Name},
	})
	if err != nil {
		t.Fatalf("restore from snapshot: %v", err)
	}
	restSpec := restore.Location + "/" + restore.Name
	restorePass, restoreErr = b.encryptionPassphraseFor(ctx, nil, "east", kmsID, restSpec, restore.Context[ctxEncryptionKeyID], nil)
	return oldPass, newPass, restorePass, restoreErr
}

// Key rotation adds the new passphrase to the LUKS header of the LIVE volume, then
// replaces (and destroys or overwrites) the stored key. A snapshot taken BEFORE the
// rotation has a header holding only the OLD keyslot, so a restore from it must
// resolve the OLD passphrase -- which only works because CreateSnapshot gives the
// snapshot its own copy of the key. Restores used to inherit the source's CURRENT
// key and could never open. The control case (no rotation) proves the harness.
func TestRestoreFromPreRotationSnapshotUsesSnapshotKey(t *testing.T) {
	providers := []rrProvider{
		{id: "k8s", name: "secrets-metadata"},
		{id: "vault", name: "vault"},
		{id: "kmip", name: "kmip"},
		{id: "aws", name: "aws-kms"},
	}
	for _, p := range providers {
		t.Run(p.name+"/control-no-rotation", func(t *testing.T) {
			oldPass, _, restorePass, err := rrSnapshotRestore(t, p.id, false)
			if err != nil {
				t.Fatalf("restore passphrase errored without rotation: %v", err)
			}
			if restorePass != oldPass {
				t.Fatal("control: the restore must resolve the source passphrase")
			}
		})

		t.Run(p.name+"/rotate-then-restore", func(t *testing.T) {
			oldPass, newPass, restorePass, err := rrSnapshotRestore(t, p.id, true)
			if err != nil {
				t.Fatalf("restore passphrase errored after rotation (snapshot header holds the OLD keyslot): %v", err)
			}
			switch restorePass {
			case oldPass:
				// Correct: the restore opens the snapshot's header.
			case newPass:
				t.Fatalf("restore resolved the NEW (rotated) passphrase, not the snapshot's OLD one; " +
					"the pre-rotation snapshot header cannot be opened by this restore")
			default:
				t.Fatalf("restore resolved an unrelated passphrase (neither old nor new)")
			}
		})
	}
}
