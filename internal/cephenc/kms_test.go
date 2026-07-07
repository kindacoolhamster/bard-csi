package cephenc

import (
	"context"
	"strings"
	"testing"
)

// The secrets-metadata KMS stores a wrapped per-volume DEK in the volume metadata
// (via the Host), round-trips to the same passphrase, keeps volumes independent, and
// never stores plaintext.
func TestSecretsMetadataRoundTrip(t *testing.T) {
	host := newTestHost(t)
	reg := NewRegistry(host, map[string]KMSConfig{"k8s": {Type: "secrets-metadata"}})
	ctx := context.Background()

	p1, err := reg.Passphrase(ctx, nil, "east", "k8s", "replicapool/img", "replicapool/img", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p1) != 64 { // 32 random bytes hex-encoded
		t.Fatalf("expected a 64-char hex DEK, got %d chars", len(p1))
	}
	wrapped := host.meta["replicapool/img|"+MetaWrappedDEK]
	if wrapped == "" {
		t.Fatalf("expected a wrapped DEK in volume metadata; meta=%v", host.meta)
	}
	if wrapped == p1 || strings.Contains(wrapped, p1) {
		t.Fatal("plaintext DEK must not appear in the stored metadata")
	}

	// Reopen: same volume unwraps to the same passphrase.
	p2, err := reg.Passphrase(ctx, nil, "east", "k8s", "replicapool/img", "replicapool/img", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p1 != p2 {
		t.Fatalf("restage must yield the same DEK: %q vs %q", p1, p2)
	}

	// A different volume gets independent random key material.
	p3, err := reg.Passphrase(ctx, nil, "east", "k8s", "replicapool/other", "replicapool/other", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p3 == p1 {
		t.Fatal("each volume must get an independent DEK")
	}

	// deleteKey is a no-op (the wrapped DEK rides with the volume).
	if err := reg.DeleteKey(ctx, nil, "east", "k8s", "replicapool/img"); err != nil {
		t.Fatalf("deleteKey must be a no-op: %v", err)
	}
}

// A wrong master key cannot unwrap a stored DEK -- the mount fails loudly.
func TestWrapUnwrapWrongMasterKeyFails(t *testing.T) {
	wrapped, err := wrapDEK([]byte("master-A"), "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unwrapDEK([]byte("master-A"), wrapped); err != nil {
		t.Fatalf("correct key must unwrap: %v", err)
	}
	if _, err := unwrapDEK([]byte("master-B"), wrapped); err == nil {
		t.Fatal("a different master key must fail to unwrap")
	}
}

// The "kubernetes" alias maps to secrets-metadata, and an unknown KMS type registers
// an erroring service that fails loudly at first use (not silently at startup).
func TestKMSAliasAndUnknownType(t *testing.T) {
	host := newTestHost(t)
	reg := NewRegistry(host, map[string]KMSConfig{"a": {Type: "kubernetes"}, "b": {Type: "bogus"}})
	if _, ok := reg.providers["a"].(*secretsMetadataKeyService); !ok {
		t.Fatalf("the kubernetes alias must map to secrets-metadata, got %T", reg.providers["a"])
	}
	if _, err := reg.Passphrase(context.Background(), nil, "east", "b", "p/i", "p/i", nil); err == nil ||
		!strings.Contains(err.Error(), "unknown KMS type") {
		t.Fatalf("an unknown KMS type must error at use, got %v", err)
	}
}

// An encryptionKMSID with no matching provider is an error, not a silent fallback.
func TestUnknownKMSID(t *testing.T) {
	reg := NewRegistry(newTestHost(t), nil)
	_, err := reg.Passphrase(context.Background(), nil, "east", "does-not-exist", "p/i", "p/i", nil)
	if err == nil || !strings.Contains(err.Error(), "unknown encryptionKMSID") {
		t.Fatalf("expected an unknown-encryptionKMSID error, got %v", err)
	}
}

// An explicit CSI secret passphrase overrides any KMS provider without touching it.
func TestSecretPassphraseOverride(t *testing.T) {
	reg := NewRegistry(newTestHost(t), nil)
	got, err := reg.Passphrase(context.Background(), nil, "east", "missing-provider", "p/i", "p/i",
		map[string]string{SecretPassphrase: "explicit"})
	if err != nil || got != "explicit" {
		t.Fatalf("explicit secret must win without resolving a provider: got %q err %v", got, err)
	}
}
