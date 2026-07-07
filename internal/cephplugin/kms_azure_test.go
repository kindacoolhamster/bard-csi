package cephplugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeAzureKV serves the two endpoints the provider needs: the AAD token endpoint
// (client-credentials) and the Key Vault secrets REST API (GET/PUT/DELETE).
type fakeAzureKV struct {
	mu           sync.Mutex
	secrets      map[string]string
	tokensIssued int
	sawBearer    bool
}

func newFakeAzureKV() *fakeAzureKV { return &fakeAzureKV{secrets: map[string]string{}} }

func (f *fakeAzureKV) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if strings.Contains(r.URL.Path, "/oauth2/v2.0/token") {
			f.tokensIssued++
			json.NewEncoder(w).Encode(map[string]any{"access_token": "aad-token", "expires_in": 3600})
			return
		}
		if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			f.sawBearer = true
		}
		name := strings.TrimPrefix(r.URL.Path, "/secrets/")
		switch r.Method {
		case http.MethodGet:
			v, ok := f.secrets[name]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"value": v})
		case http.MethodPut:
			var in struct{ Value string }
			json.NewDecoder(r.Body).Decode(&in)
			f.secrets[name] = in.Value
			json.NewEncoder(w).Encode(map[string]string{"value": in.Value})
		case http.MethodDelete:
			if _, ok := f.secrets[name]; !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			delete(f.secrets, name)
			json.NewEncoder(w).Encode(map[string]string{})
		}
	})
}

func azureBackend(t *testing.T, fa *fakeAzureKV, authMethod string) *Backend {
	t.Helper()
	srv := httptest.NewServer(fa.handler())
	t.Cleanup(srv.Close)
	cfg := KMSConfig{
		Type:     "azure-kv",
		VaultURL: srv.URL,
	}
	if authMethod == "token" {
		cfg.AuthMethod = "token"
		cfg.Token = "static-test-token"
	} else {
		cfg.AuthMethod = "clientSecret"
		cfg.TenantID = "tenant"
		cfg.ClientID = "client"
		cfg.ClientSecret = "secret"
		cfg.AADEndpoint = srv.URL
	}
	return New(map[string]ClusterConfig{"east": {}}, "", "", newEncRunner()).
		WithKMS(map[string]KMSConfig{"azure": cfg})
}

// The Azure provider stores a per-volume passphrase as a Key Vault secret: generate
// on first stage, fetch on reopen, via an AAD-obtained bearer token.
func TestAzureKVRoundTrip(t *testing.T) {
	fa := newFakeAzureKV()
	b := azureBackend(t, fa, "clientSecret")
	ctx := context.Background()

	p1, err := b.encryptionPassphrase(ctx, "east", "azure", "replicapool/img", nil)
	if err != nil {
		t.Fatalf("first stage: %v", err)
	}
	if len(p1) != 64 {
		t.Fatalf("expected a 64-char hex passphrase, got %d", len(p1))
	}
	if fa.tokensIssued < 1 || !fa.sawBearer {
		t.Fatal("expected an AAD token to be fetched and sent as a bearer")
	}

	// Reopen: same volume fetches the same stored secret.
	p2, err := b.encryptionPassphrase(ctx, "east", "azure", "replicapool/img", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p1 != p2 {
		t.Fatalf("restage must yield the same passphrase: %q vs %q", p1, p2)
	}
	// A different volume gets a distinct passphrase.
	pOther, _ := b.encryptionPassphrase(ctx, "east", "azure", "replicapool/other", nil)
	if pOther == p1 {
		t.Fatal("distinct volumes must get distinct passphrases")
	}
	if len(fa.secrets) != 2 {
		t.Fatalf("expected 2 stored secrets, got %d", len(fa.secrets))
	}

	// deleteKey removes the volume's secret.
	if err := b.kms.DeleteKey(ctx, nil, "east", "azure", "replicapool/img"); err != nil {
		t.Fatalf("deleteKey: %v", err)
	}
	if len(fa.secrets) != 1 {
		t.Fatalf("deleteKey must remove exactly the volume's secret, have %d", len(fa.secrets))
	}
}

// An encrypted clone gets its own copy of the source's secret, opens with the same
// passphrase, and deleting the clone leaves the source's secret intact.
func TestAzureKVCloneSelfContained(t *testing.T) {
	fa := newFakeAzureKV()
	b := azureBackend(t, fa, "clientSecret")
	ctx := context.Background()
	const src, clone = "replicapool/src", "replicapool/clone"

	srcPass, err := b.encryptionPassphrase(ctx, "east", "azure", src, nil)
	if err != nil {
		t.Fatal(err)
	}
	// CloneKey gives the clone its own secret copy (azure is a key-cloning provider);
	// the count going to 2 proves the clone hook actually ran.
	if err := b.kms.CloneKey(ctx, nil, "east", "azure", src, clone, nil); err != nil {
		t.Fatalf("cloneKey: %v", err)
	}
	if len(fa.secrets) != 2 {
		t.Fatalf("clone must own an independent secret, have %d", len(fa.secrets))
	}
	clonePass, err := b.encryptionPassphrase(ctx, "east", "azure", clone, nil)
	if err != nil {
		t.Fatal(err)
	}
	if clonePass != srcPass {
		t.Fatalf("clone must open with the source key: %q vs %q", srcPass, clonePass)
	}
	// Delete the clone -> source secret survives.
	if err := b.kms.DeleteKey(ctx, nil, "east", "azure", clone); err != nil {
		t.Fatal(err)
	}
	again, err := b.encryptionPassphrase(ctx, "east", "azure", src, nil)
	if err != nil || again != srcPass {
		t.Fatalf("source secret must survive clone delete: %v (%q vs %q)", err, again, srcPass)
	}
}

// A static bearer token (for emulators / pre-acquired tokens) skips AAD entirely.
func TestAzureKVStaticToken(t *testing.T) {
	fa := newFakeAzureKV()
	b := azureBackend(t, fa, "token")
	ctx := context.Background()
	if _, err := b.encryptionPassphrase(ctx, "east", "azure", "replicapool/img", nil); err != nil {
		t.Fatal(err)
	}
	if fa.tokensIssued != 0 {
		t.Fatal("static-token auth must not call the AAD token endpoint")
	}
	if !fa.sawBearer {
		t.Fatal("static token must be sent as a bearer")
	}
}

// A missing vaultUrl registers an erroring service (loud at first use, not a panic).
func TestAzureKVNeedsVaultURL(t *testing.T) {
	b := New(map[string]ClusterConfig{"east": {}}, "", "", newEncRunner()).
		WithKMS(map[string]KMSConfig{"azure": {Type: "azure-kv"}})
	if _, err := b.encryptionPassphrase(context.Background(), "east", "azure", "p/i", nil); err == nil {
		t.Fatal("azure-kv without vaultUrl must error at use")
	}
}
