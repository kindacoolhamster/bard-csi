package cephplugin

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// cloneBackend is a metaRunner-backed Backend (image-meta modelled in memory)
// with a master key dir and all three KMS shapes wired: derived (empty id),
// secrets-metadata ("k8s"), and Vault ("vault") against a fake KV store.
func cloneBackend(t *testing.T) (*Backend, *metaRunner) {
	t.Helper()
	keyDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(keyDir, "east"), []byte("instance-master-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	fv := newFakeVault()
	srv := httptest.NewServer(fv.handler())
	t.Cleanup(srv.Close)
	run := &metaRunner{meta: map[string]string{}}
	b := New(map[string]ClusterConfig{"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin"}}, "", "", run).
		WithEncryption(keyDir).
		WithKMS(map[string]KMSConfig{
			"k8s":   {Type: "secrets-metadata"},
			"vault": {Type: "vault", Address: srv.URL, Token: "dev-token"},
		})
	return b, run
}

// An encrypted clone copies its source's LUKS header byte-for-byte, so it must
// open with the SOURCE's passphrase -- not one derived from (or stored under) its
// own new identity. This drives the create-time inheritance + node-time
// resolution for every provider and asserts the clone resolves the source's key.
func TestEncryptedCloneInheritsSourceKey(t *testing.T) {
	ctx := context.Background()
	const src, clone = "replicapool/csi-vol-src", "replicapool/csi-vol-clone"

	for _, kmsID := range []string{"" /*derived*/, "k8s", "vault"} {
		t.Run("kms="+orDefault(kmsID, "derived"), func(t *testing.T) {
			b, run := cloneBackend(t)
			cc, _ := b.cluster("east")
			conn, cleanup, err := b.connArgs(cc, "east", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()

			// Source first stage: resolve (and, for stateful providers, persist) its
			// passphrase, and record its KMS id on the image as CreateVolume would.
			if kmsID != "" {
				if err := b.imageMetaSet(ctx, conn, src, imgMetaKMSID, kmsID); err != nil {
					t.Fatal(err)
				}
			}
			srcPass, err := b.encryptionPassphraseFor(ctx, nil, "east", kmsID, src, src, nil)
			if err != nil {
				t.Fatalf("source passphrase: %v", err)
			}

			// Clone: inherit the source's encryption descriptor onto the clone.
			keyID, err := b.inheritEncryption(ctx, conn, src, clone, "east", nil)
			if err != nil {
				t.Fatalf("inheritEncryption: %v", err)
			}

			// The node resolves the clone's passphrase using the inherited keyID.
			clonePass, err := b.encryptionPassphraseFor(ctx, nil, "east", kmsID, clone, keyID, nil)
			if err != nil {
				t.Fatalf("clone passphrase: %v", err)
			}
			if clonePass != srcPass {
				t.Fatalf("clone must open with the source key: src=%q clone=%q", srcPass, clonePass)
			}

			// The KMS id is carried to the clone so DeleteVolume cleans up the right
			// provider (derived records nothing).
			if got := run.meta[clone+"|"+imgMetaKMSID]; got != kmsID {
				t.Fatalf("clone KMS id = %q, want %q", got, kmsID)
			}

			// Deleting the clone must not destroy a key the source still relies on.
			if err := b.kms.DeleteKey(ctx, nil, "east", kmsID, clone); err != nil {
				t.Fatalf("clone deleteKey: %v", err)
			}
			again, err := b.encryptionPassphraseFor(ctx, nil, "east", kmsID, src, src, nil)
			if err != nil {
				t.Fatalf("source passphrase after clone delete: %v", err)
			}
			if again != srcPass {
				t.Fatalf("deleting the clone changed the source key: %q -> %q", srcPass, again)
			}
		})
	}
}

// The derived provider is the one that re-derives from identity, so prove the
// inheritance is load-bearing: resolving the clone by its OWN identity (the
// pre-fix behaviour) yields a DIFFERENT passphrase that would fail to open the
// copied header; resolving by the inherited keyID matches the source. Also prove
// a clone-of-a-clone keeps deriving the one root identity.
func TestDerivedCloneIdentityAndChain(t *testing.T) {
	ctx := context.Background()
	b, run := cloneBackend(t)
	cc, _ := b.cluster("east")
	conn, cleanup, _ := b.connArgs(cc, "east", nil)
	defer cleanup()

	const root, c1, c2 = "replicapool/root", "replicapool/c1", "replicapool/c2"
	rootPass, err := b.encryptionPassphraseFor(ctx, nil, "east", "", root, root, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Clone 1 from root.
	k1, err := b.inheritEncryption(ctx, conn, root, c1, "east", nil)
	if err != nil {
		t.Fatal(err)
	}
	if k1 != root {
		t.Fatalf("clone key id should be the root identity, got %q", k1)
	}
	if got := run.meta[c1+"|"+imgMetaKeyID]; got != root {
		t.Fatalf("clone must persist the root key id, got %q", got)
	}

	// Pre-fix behaviour (derive from the clone's own identity) must NOT match --
	// that is exactly why an encrypted restore used to fail to open.
	wrong, _ := b.encryptionPassphraseFor(ctx, nil, "east", "", c1, c1, nil)
	if wrong == rootPass {
		t.Fatal("derive-from-own-identity unexpectedly matched; test no longer guards the bug")
	}
	// Inherited keyID matches the source.
	if got, _ := b.encryptionPassphraseFor(ctx, nil, "east", "", c1, k1, nil); got != rootPass {
		t.Fatalf("clone must derive the root passphrase, got %q want %q", got, rootPass)
	}

	// Clone 2 from clone 1: still the root identity (chain copies it down).
	k2, err := b.inheritEncryption(ctx, conn, c1, c2, "east", nil)
	if err != nil {
		t.Fatal(err)
	}
	if k2 != root {
		t.Fatalf("clone-of-clone key id should chain to the root, got %q", k2)
	}
	if got, _ := b.encryptionPassphraseFor(ctx, nil, "east", "", c2, k2, nil); got != rootPass {
		t.Fatalf("grandchild must derive the root passphrase, got %q want %q", got, rootPass)
	}
}
