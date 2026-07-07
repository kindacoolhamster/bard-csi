package cephenc

import (
	"context"
	"strings"
	"testing"
)

// RotateKey on the secrets-metadata provider mints fresh material, hands it to apply
// (which the node splices into the LUKS keyslot), then overwrites the stored wrapped
// DEK so the new key becomes canonical -- and the old passphrase no longer resolves.
func TestRotateKeySecretsMetadata(t *testing.T) {
	host := newTestHost(t)
	reg := NewRegistry(host, map[string]KMSConfig{"sm": {Type: "secrets-metadata"}})
	ctx, spec := context.Background(), "pool/img"

	old, err := reg.Passphrase(ctx, nil, "east", "sm", spec, spec, nil)
	if err != nil {
		t.Fatal(err)
	}

	var applied string
	if err := reg.RotateKey(ctx, nil, "east", "sm", spec, spec, nil, func(newPass string) error {
		applied = newPass
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if applied == "" || applied == old {
		t.Fatalf("apply should receive a fresh passphrase != old (old=%q applied=%q)", old, applied)
	}
	// After rotation the provider resolves the NEW passphrase, not the old.
	now, err := reg.Passphrase(ctx, nil, "east", "sm", spec, spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	if now != applied {
		t.Fatalf("post-rotation passphrase = %q, want the rotated %q", now, applied)
	}
	if now == old {
		t.Fatal("rotation did not change the stored key")
	}
}

// If apply fails, the stored key must be untouched (the old passphrase still resolves)
// -- the LUKS keyslot was never changed, so a retry is safe.
func TestRotateKeyApplyFailureKeepsOldKey(t *testing.T) {
	host := newTestHost(t)
	reg := NewRegistry(host, map[string]KMSConfig{"sm": {Type: "secrets-metadata"}})
	ctx, spec := context.Background(), "pool/img"
	old, _ := reg.Passphrase(ctx, nil, "east", "sm", spec, spec, nil)

	wantErr := context.Canceled
	if err := reg.RotateKey(ctx, nil, "east", "sm", spec, spec, nil, func(string) error {
		return wantErr
	}); err != wantErr {
		t.Fatalf("RotateKey should surface the apply error, got %v", err)
	}
	if now, _ := reg.Passphrase(ctx, nil, "east", "sm", spec, spec, nil); now != old {
		t.Fatalf("a failed apply must leave the old key intact (old=%q now=%q)", old, now)
	}
}

// The derived provider's key is a deterministic function of the master key, so it
// cannot be rotated per-volume; an explicit passphrase secret likewise. Both error.
func TestRotateKeyUnsupported(t *testing.T) {
	host := newTestHost(t)
	reg := NewRegistry(host, map[string]KMSConfig{"sm": {Type: "secrets-metadata"}})
	ctx, spec := context.Background(), "pool/img"
	apply := func(string) error { return nil }

	// Derived provider (empty kmsID).
	if err := reg.RotateKey(ctx, nil, "east", "", spec, spec, nil, apply); err == nil || !strings.Contains(err.Error(), "does not support key rotation") {
		t.Fatalf("derived provider should reject rotation, got %v", err)
	}
	// Explicit passphrase secret.
	secrets := map[string]string{SecretPassphrase: "literal"}
	if err := reg.RotateKey(ctx, nil, "east", "sm", spec, spec, secrets, apply); err == nil || !strings.Contains(err.Error(), "encryptionPassphrase") {
		t.Fatalf("an explicit passphrase secret should reject rotation, got %v", err)
	}
}
