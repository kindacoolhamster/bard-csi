package cephenc

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSTS models the STS AssumeRoleWithWebIdentity query API: it returns static
// temporary credentials (with a controllable expiry) as XML, and records what it was
// asked. It does NOT validate the web identity token (a real STS validates the OIDC
// signature; the emulated path proves the wiring, not AWS's federation).
type fakeSTS struct {
	mu        sync.Mutex
	calls     int
	lastToken string
	lastRole  string
	expiresIn time.Duration // 0 => 1h
}

func (s *fakeSTS) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.calls++
		_ = r.ParseForm()
		if r.Form.Get("Action") != "AssumeRoleWithWebIdentity" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.lastToken = r.Form.Get("WebIdentityToken")
		s.lastRole = r.Form.Get("RoleArn")
		ttl := s.expiresIn
		if ttl == 0 {
			ttl = time.Hour
		}
		exp := time.Now().Add(ttl).UTC().Format(time.RFC3339)
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(w, `<AssumeRoleWithWebIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">
  <AssumeRoleWithWebIdentityResult>
    <Credentials>
      <AccessKeyId>ASIASTSTEMP</AccessKeyId>
      <SecretAccessKey>sts-secret-key</SecretAccessKey>
      <SessionToken>sts-session-token-%d</SessionToken>
      <Expiration>%s</Expiration>
    </Credentials>
  </AssumeRoleWithWebIdentityResult>
</AssumeRoleWithWebIdentityResponse>`, s.calls, exp)
	})
}

// stsKMS is a minimal AWS KMS stand-in (GenerateDataKey/Decrypt) that also records
// whether the caller presented an STS session token, proving the credentials flowed
// from AssumeRoleWithWebIdentity rather than a static key.
type stsKMS struct {
	mu                  sync.Mutex
	store               map[string]string // ciphertext(b64) -> plaintext(b64)
	sawSessionToken     bool
	sawSessionTokenLast string
}

func (f *stsKMS) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if tok := r.Header.Get("X-Amz-Security-Token"); tok != "" {
			f.sawSessionToken = true
			f.sawSessionTokenLast = tok
		}
		switch r.Header.Get("X-Amz-Target") {
		case "TrentService.GenerateDataKey":
			pt := make([]byte, 32)
			rand.Read(pt)
			ct := make([]byte, 24)
			rand.Read(ct)
			ptB64 := base64.StdEncoding.EncodeToString(pt)
			ctB64 := base64.StdEncoding.EncodeToString(ct)
			f.store[ctB64] = ptB64
			json.NewEncoder(w).Encode(map[string]string{"Plaintext": ptB64, "CiphertextBlob": ctB64})
		case "TrentService.Decrypt":
			var in struct{ CiphertextBlob string }
			json.NewDecoder(r.Body).Decode(&in)
			pt, ok := f.store[in.CiphertextBlob]
			if !ok {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"Plaintext": pt})
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	})
}

func writeTokenFile(t *testing.T, content string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "oidc-token")
	if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// The STS credential source exchanges the web identity token for temporary creds,
// caches them until near expiry, and refreshes (re-reading the rotating token) once
// they lapse.
func TestAWSSTSCredsSourceCachesAndRefreshes(t *testing.T) {
	sts := &fakeSTS{expiresIn: 3 * time.Minute} // -2m skew => expiry already in the past
	srv := httptest.NewServer(sts.handler())
	defer srv.Close()

	src := &awsSTSCredsSource{
		endpoint:    srv.URL,
		region:      "us-east-1",
		roleARN:     "arn:aws:iam::123456789012:role/bard",
		sessionName: "bard-test",
		tokenFile:   writeTokenFile(t, "oidc-jwt-1"),
		httpc:       srv.Client(),
	}

	c, err := src.resolve()
	if err != nil {
		t.Fatal(err)
	}
	if c.accessKeyID != "ASIASTSTEMP" || c.secretAccessKey != "sts-secret-key" || c.sessionToken == "" {
		t.Fatalf("unexpected creds: %+v", c)
	}
	if sts.lastToken != "oidc-jwt-1" || sts.lastRole != "arn:aws:iam::123456789012:role/bard" {
		t.Fatalf("STS did not get the token/role: token=%q role=%q", sts.lastToken, sts.lastRole)
	}
	// expiresIn 3m minus the 2m refresh skew => ~1m of validity, so a second immediate
	// resolve serves from cache (no new STS call).
	if _, err := src.resolve(); err != nil {
		t.Fatal(err)
	}
	if sts.calls != 1 {
		t.Fatalf("second resolve should hit cache; STS calls=%d", sts.calls)
	}

	// Force expiry and rotate the token: resolve must re-call STS with the new token.
	src.expiry = time.Now().Add(-time.Second)
	if err := os.WriteFile(src.tokenFile, []byte("oidc-jwt-2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := src.resolve(); err != nil {
		t.Fatal(err)
	}
	if sts.calls != 2 || sts.lastToken != "oidc-jwt-2" {
		t.Fatalf("expected a refresh with the rotated token; calls=%d token=%q", sts.calls, sts.lastToken)
	}
}

// A missing token file is a clear error, not a silent empty credential.
func TestAWSSTSCredsSourceMissingToken(t *testing.T) {
	src := &awsSTSCredsSource{tokenFile: filepath.Join(t.TempDir(), "does-not-exist"), httpc: http.DefaultClient}
	if _, err := src.resolve(); err == nil || !strings.Contains(err.Error(), "web identity token") {
		t.Fatalf("expected a token-read error, got %v", err)
	}
}

// End to end through the registry: the aws-sts-metadata provider envelope-encrypts a
// per-volume DEK exactly like aws-kms, but the KMS calls carry STS-minted session
// credentials. Same volume round-trips; the wrapped DEK lands in metadata; no plaintext.
func TestAWSSTSMetadataRoundTrip(t *testing.T) {
	sts := &fakeSTS{}
	stsSrv := httptest.NewServer(sts.handler())
	defer stsSrv.Close()
	kms := &stsKMS{store: map[string]string{}}
	kmsSrv := httptest.NewServer(kms.handler())
	defer kmsSrv.Close()

	host := newTestHost(t)
	reg := NewRegistry(host, map[string]KMSConfig{"sts": {
		Type:                 "aws-sts-metadata",
		Region:               "us-east-1",
		KeyID:                "alias/bard-test",
		Endpoint:             kmsSrv.URL,
		STSEndpoint:          stsSrv.URL,
		RoleARN:              "arn:aws:iam::123456789012:role/bard",
		WebIdentityTokenFile: writeTokenFile(t, "projected-sa-jwt"),
	}})
	ctx := context.Background()

	p1, err := reg.Passphrase(ctx, nil, "east", "sts", "replicapool/img", "replicapool/img", nil)
	if err != nil {
		t.Fatalf("first stage: %v", err)
	}
	if len(p1) != 64 { // 32 bytes hex
		t.Fatalf("expected a 64-char hex passphrase, got %d", len(p1))
	}
	if !kms.sawSessionToken {
		t.Fatal("KMS request did not carry an STS session token -- creds did not flow from STS")
	}
	blob := host.meta["replicapool/img|"+MetaWrappedDEK]
	if blob == "" {
		t.Fatalf("expected an encrypted DEK in volume metadata; meta=%v", host.meta)
	}
	if strings.Contains(blob, p1) {
		t.Fatal("plaintext passphrase must not appear in the stored ciphertext")
	}
	if sts.calls == 0 {
		t.Fatal("STS was never called")
	}

	// Reopen: same volume Decrypts to the same passphrase.
	p2, err := reg.Passphrase(ctx, nil, "east", "sts", "replicapool/img", "replicapool/img", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p1 != p2 {
		t.Fatalf("restage must yield the same passphrase: %q vs %q", p1, p2)
	}

	// A different volume gets independent key material.
	pOther, err := reg.Passphrase(ctx, nil, "east", "sts", "replicapool/other", "replicapool/other", nil)
	if err != nil {
		t.Fatal(err)
	}
	if pOther == p1 {
		t.Fatal("distinct volumes must get distinct passphrases")
	}

	// deleteKey is a no-op (the ciphertext rides with the volume, like aws-kms).
	if err := reg.DeleteKey(ctx, nil, "east", "sts", "replicapool/img"); err != nil {
		t.Fatalf("deleteKey must be a no-op: %v", err)
	}
}

// Without a role ARN the provider registers as an erroring service that fails loudly at
// first use (a misconfiguration must not be swallowed at startup).
func TestAWSSTSMetadataMissingRole(t *testing.T) {
	reg := NewRegistry(newTestHost(t), map[string]KMSConfig{"sts": {
		Type:   "aws-sts-metadata",
		Region: "us-east-1",
		KeyID:  "alias/x",
	}})
	_, err := reg.Passphrase(context.Background(), nil, "east", "sts", "p/i", "p/i", nil)
	if err == nil || !strings.Contains(err.Error(), "roleArn") {
		t.Fatalf("expected a missing-roleArn error at use, got %v", err)
	}
}
