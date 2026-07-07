package cephplugin

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeKMS is a minimal stand-in for AWS KMS: GenerateDataKey mints a random
// plaintext data key and an opaque ciphertext that maps back to it; Decrypt
// reverses the mapping. It models the property that matters -- the ciphertext is
// opaque and only the KMS can recover the plaintext -- without real crypto.
type fakeKMS struct {
	mu       sync.Mutex
	store    map[string]string // ciphertext(b64) -> plaintext(b64)
	gen, dec int
	sawSigV4 bool
}

func newFakeKMS() *fakeKMS { return &fakeKMS{store: map[string]string{}} }

func (f *fakeKMS) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if strings.Contains(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256") {
			f.sawSigV4 = true
		}
		switch r.Header.Get("X-Amz-Target") {
		case "TrentService.GenerateDataKey":
			f.gen++
			pt := make([]byte, 32)
			rand.Read(pt)
			ctRaw := make([]byte, 24)
			rand.Read(ctRaw)
			ptB64 := base64.StdEncoding.EncodeToString(pt)
			ctB64 := base64.StdEncoding.EncodeToString(ctRaw)
			f.store[ctB64] = ptB64
			json.NewEncoder(w).Encode(map[string]string{"Plaintext": ptB64, "CiphertextBlob": ctB64, "KeyId": "test-key"})
		case "TrentService.Decrypt":
			f.dec++
			var in struct{ CiphertextBlob string }
			json.NewDecoder(r.Body).Decode(&in)
			pt, ok := f.store[in.CiphertextBlob]
			if !ok {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"__type":"InvalidCiphertextException"}`))
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"Plaintext": pt, "KeyId": "test-key"})
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	})
}

func awsKMSBackend(t *testing.T, endpoint string) (*Backend, *metaRunner) {
	t.Helper()
	run := &metaRunner{meta: map[string]string{}}
	b := New(map[string]ClusterConfig{"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin"}}, "", "", run).
		WithKMS(map[string]KMSConfig{"aws": {
			Type:            "aws-kms",
			Region:          "us-east-1",
			KeyID:           "alias/bard-test",
			Endpoint:        endpoint,
			AccessKeyID:     "test",
			SecretAccessKey: "test",
		}})
	return b, run
}

// The AWS KMS provider envelope-encrypts a per-volume DEK: GenerateDataKey on
// first stage (plaintext = passphrase, ciphertext stored in image metadata),
// Decrypt on reopen. Same volume round-trips to the same passphrase; distinct
// volumes get distinct keys; the plaintext never lands in the stored metadata.
func TestAWSKMSEnvelopeRoundTrip(t *testing.T) {
	fk := newFakeKMS()
	srv := httptest.NewServer(fk.handler())
	defer srv.Close()
	b, run := awsKMSBackend(t, srv.URL)
	ctx := context.Background()

	p1, err := b.encryptionPassphrase(ctx, "east", "aws", "replicapool/img", nil)
	if err != nil {
		t.Fatalf("first stage: %v", err)
	}
	if len(p1) != 64 { // 32 bytes hex
		t.Fatalf("expected a 64-char hex passphrase, got %d", len(p1))
	}
	blob := run.meta["replicapool/img|"+imgMetaWrappedDEK]
	if blob == "" {
		t.Fatalf("expected an encrypted DEK stored in image metadata; meta=%v", run.meta)
	}
	if strings.Contains(blob, p1) {
		t.Fatal("plaintext passphrase must not appear in the stored ciphertext")
	}
	if !fk.sawSigV4 {
		t.Fatal("request was not SigV4-signed")
	}

	// Reopen: same volume Decrypts to the same passphrase (no new GenerateDataKey).
	p2, err := b.encryptionPassphrase(ctx, "east", "aws", "replicapool/img", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p1 != p2 {
		t.Fatalf("restage must yield the same passphrase: %q vs %q", p1, p2)
	}
	if fk.gen != 1 {
		t.Fatalf("GenerateDataKey must run exactly once, got %d", fk.gen)
	}
	if fk.dec < 1 {
		t.Fatalf("reopen must Decrypt, got %d decrypts", fk.dec)
	}

	// A different volume gets independent key material.
	pOther, err := b.encryptionPassphrase(ctx, "east", "aws", "replicapool/other", nil)
	if err != nil {
		t.Fatal(err)
	}
	if pOther == p1 {
		t.Fatal("distinct volumes must get distinct passphrases")
	}

	// deleteKey is a no-op (the ciphertext rides with the image).
	if err := b.kms.DeleteKey(ctx, nil, "east", "aws", "replicapool/img"); err != nil {
		t.Fatalf("deleteKey must be a no-op: %v", err)
	}
}

// An encrypted clone inherits the source's AWS-KMS-wrapped DEK via the normal
// descriptor copy, so it Decrypts to the same passphrase and is self-contained
// (deleting the clone leaves the source's key untouched).
func TestAWSKMSCloneInheritsSourceKey(t *testing.T) {
	fk := newFakeKMS()
	srv := httptest.NewServer(fk.handler())
	defer srv.Close()
	b, run := awsKMSBackend(t, srv.URL)
	ctx := context.Background()
	const src, clone = "replicapool/csi-vol-src", "replicapool/csi-vol-clone"

	// Source first stage (and record its KMS id, as CreateVolume would).
	if err := b.imageMetaSet(ctx, nil, src, imgMetaKMSID, "aws"); err != nil {
		t.Fatal(err)
	}
	srcPass, err := b.encryptionPassphrase(ctx, "east", "aws", src, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Clone: inherit the descriptor onto the clone.
	keyID, err := b.inheritEncryption(ctx, nil, src, clone, "east", nil)
	if err != nil {
		t.Fatalf("inheritEncryption: %v", err)
	}
	if run.meta[clone+"|"+imgMetaWrappedDEK] != run.meta[src+"|"+imgMetaWrappedDEK] {
		t.Fatal("clone must carry an identical copy of the encrypted DEK")
	}
	if run.meta[clone+"|"+imgMetaKMSID] != "aws" {
		t.Fatal("clone must carry the KMS id so DeleteVolume routes correctly")
	}

	// The node resolves the clone's passphrase -- must match the source.
	clonePass, err := b.encryptionPassphraseFor(ctx, nil, "east", "aws", clone, keyID, nil)
	if err != nil {
		t.Fatalf("clone passphrase: %v", err)
	}
	if clonePass != srcPass {
		t.Fatalf("clone must open with the source key: src=%q clone=%q", srcPass, clonePass)
	}

	// Deleting the clone is a no-op on key material; the source still resolves.
	if err := b.kms.DeleteKey(ctx, nil, "east", "aws", clone); err != nil {
		t.Fatal(err)
	}
	again, err := b.encryptionPassphrase(ctx, "east", "aws", src, nil)
	if err != nil || again != srcPass {
		t.Fatalf("source key must survive clone delete: %v (%q vs %q)", err, again, srcPass)
	}
}
