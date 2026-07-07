package cephplugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kindacoolhamster/bard-csi/internal/fakerun"
	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// fakeVault is a minimal KV v2 store: GET returns 404 until a path is written,
// and write is create-only (cas) so a second write to the same path conflicts.
type fakeVault struct {
	mu     sync.Mutex
	store  map[string]string
	writes int
}

func newFakeVault() *fakeVault { return &fakeVault{store: map[string]string{}} }

func (f *fakeVault) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") == "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		// KV v2 deletes target the metadata endpoint; data ops the data endpoint.
		if r.Method == http.MethodDelete {
			delete(f.store, strings.TrimPrefix(r.URL.Path, "/v1/secret/metadata/"))
			w.WriteHeader(http.StatusNoContent)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/v1/secret/data/")
		switch r.Method {
		case http.MethodGet:
			v, ok := f.store[path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"data": map[string]string{"passphrase": v}},
			})
		case http.MethodPost:
			var body struct {
				Data    struct{ Passphrase string } `json:"data"`
				Options struct {
					Cas *int `json:"cas"`
				} `json:"options"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if _, exists := f.store[path]; exists && body.Options.Cas != nil && *body.Options.Cas == 0 {
				w.WriteHeader(http.StatusBadRequest) // cas conflict
				return
			}
			f.store[path] = body.Data.Passphrase
			f.writes++
			w.WriteHeader(http.StatusNoContent)
		}
	})
}

func vaultBackend(t *testing.T, addr string) *Backend {
	t.Helper()
	return New(map[string]ClusterConfig{"east": {}}, "", "", newEncRunner()).
		WithKMS(map[string]KMSConfig{"vault-test": {Type: "vault", Address: addr, Token: "dev-token"}})
}

// The vault provider generates a passphrase on first use, stores it, and returns
// the same one on later calls (so a restage can reopen the device).
func TestVaultKMSGenerateThenFetch(t *testing.T) {
	fv := newFakeVault()
	srv := httptest.NewServer(fv.handler())
	defer srv.Close()
	b := vaultBackend(t, srv.URL)
	ctx := context.Background()

	p1, err := b.encryptionPassphrase(ctx, "east", "vault-test", "replicapool/img", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p1) != 64 { // 32 random bytes -> hex
		t.Fatalf("expected a 64-char hex passphrase, got %d chars", len(p1))
	}
	p2, err := b.encryptionPassphrase(ctx, "east", "vault-test", "replicapool/img", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p1 != p2 {
		t.Fatalf("same volume must fetch the same stored passphrase: %q vs %q", p1, p2)
	}
	if fv.writes != 1 {
		t.Fatalf("passphrase must be written exactly once, got %d writes", fv.writes)
	}
	// A different volume gets a distinct passphrase.
	pOther, _ := b.encryptionPassphrase(ctx, "east", "vault-test", "replicapool/other", nil)
	if pOther == p1 {
		t.Fatal("distinct volumes must get distinct passphrases")
	}
}

// DeleteVolume removes the volume's stored passphrase from the KMS: CreateVolume
// records the KMS id on the image, the node generates+stores the key, and
// DeleteVolume reads the id back and deletes the key before removing the image.
func TestDeleteVolumeCleansUpKMSKey(t *testing.T) {
	fv := newFakeVault()
	srv := httptest.NewServer(fv.handler())
	defer srv.Close()
	b := New(map[string]ClusterConfig{"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin"}}, "", "", fakerun.New()).
		WithKMS(map[string]KMSConfig{"vault-test": {Type: "vault", Address: srv.URL, Token: "dev-token"}})
	ctx := context.Background()

	create, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: "v", CapacityBytes: 1 << 30, Instance: "east",
		Parameters: map[string]string{"pool": "replicapool", paramEncrypted: "true", paramEncryptionKMSID: "vault-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	vol := bardplugin.VolumeRef{Instance: "east", Location: create.Location, Name: create.Name}

	// The node would generate+store the passphrase on first stage; do that here.
	if _, err := b.encryptionPassphrase(ctx, "east", "vault-test", vol.Location+"/"+vol.Name, nil); err != nil {
		t.Fatal(err)
	}
	if len(fv.store) != 1 {
		t.Fatalf("expected the passphrase stored in Vault, have %d entries", len(fv.store))
	}

	if err := b.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{Volume: vol}); err != nil {
		t.Fatal(err)
	}
	if len(fv.store) != 0 {
		t.Fatalf("DeleteVolume must remove the Vault entry, %d left", len(fv.store))
	}
}

// k8sVault is a fake Vault that requires Kubernetes login: POST to the login
// path with the expected JWT issues a short-lived token, which subsequent KV ops
// must present. It counts logins (to prove caching) and can force the next KV op
// to 403 (to prove re-login on expiry).
type k8sVault struct {
	mu       sync.Mutex
	store    map[string]string
	wantJWT  string
	issued   string
	logins   int
	force403 bool
}

func newK8sVault(jwt string) *k8sVault {
	return &k8sVault{store: map[string]string{}, wantJWT: jwt}
}

func (k *k8sVault) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k.mu.Lock()
		defer k.mu.Unlock()
		if r.URL.Path == "/v1/auth/kubernetes/login" {
			var body struct{ Role, Jwt string }
			json.NewDecoder(r.Body).Decode(&body)
			if body.Jwt != k.wantJWT || body.Role == "" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			k.logins++
			k.issued = "s.token-" + body.Role
			json.NewEncoder(w).Encode(map[string]any{
				"auth": map[string]any{"client_token": k.issued, "lease_duration": 3600},
			})
			return
		}
		// KV op: must carry the issued token.
		if r.Header.Get("X-Vault-Token") != k.issued || k.issued == "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if k.force403 {
			k.force403 = false
			k.issued = "" // server-side revocation: old token no longer valid
			w.WriteHeader(http.StatusForbidden)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/v1/secret/data/")
		switch r.Method {
		case http.MethodGet:
			v, ok := k.store[path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": map[string]string{"passphrase": v}}})
		case http.MethodPost:
			var body struct {
				Data struct{ Passphrase string } `json:"data"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			k.store[path] = body.Data.Passphrase
			w.WriteHeader(http.StatusNoContent)
		}
	})
}

// Kubernetes auth: the provider logs in with the SA JWT, caches the token across
// calls, and re-logs-in when a request is rejected as expired.
func TestVaultKubernetesAuth(t *testing.T) {
	jwtFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(jwtFile, []byte("fake-sa-jwt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	kv := newK8sVault("fake-sa-jwt")
	srv := httptest.NewServer(kv.handler())
	defer srv.Close()
	b := New(map[string]ClusterConfig{"east": {}}, "", "", newEncRunner()).
		WithKMS(map[string]KMSConfig{"vault-k8s": {
			Type: "vault", Address: srv.URL, AuthMethod: "kubernetes", Role: "bard", SATokenFile: jwtFile,
		}})
	ctx := context.Background()

	p1, err := b.encryptionPassphrase(ctx, "east", "vault-k8s", "replicapool/img", nil)
	if err != nil {
		t.Fatalf("k8s-auth passphrase failed: %v", err)
	}
	if len(p1) != 64 {
		t.Fatalf("expected a 64-char passphrase, got %d", len(p1))
	}
	// A second op reuses the cached token (no new login).
	if _, err := b.encryptionPassphrase(ctx, "east", "vault-k8s", "replicapool/img", nil); err != nil {
		t.Fatal(err)
	}
	if kv.logins != 1 {
		t.Fatalf("token should be cached: expected 1 login, got %d", kv.logins)
	}
	// Force the next op to be rejected -> provider must re-login and succeed.
	kv.mu.Lock()
	kv.force403 = true
	kv.mu.Unlock()
	if _, err := b.encryptionPassphrase(ctx, "east", "vault-k8s", "replicapool/other", nil); err != nil {
		t.Fatalf("expected re-login on 403, got %v", err)
	}
	if kv.logins != 2 {
		t.Fatalf("expected a re-login after rejection, got %d logins", kv.logins)
	}
}

// Kubernetes auth without a role is a configuration error.
func TestVaultKubernetesAuthNeedsRole(t *testing.T) {
	jwtFile := filepath.Join(t.TempDir(), "token")
	os.WriteFile(jwtFile, []byte("jwt"), 0o600)
	b := New(map[string]ClusterConfig{"east": {}}, "", "", newEncRunner()).
		WithKMS(map[string]KMSConfig{"vault-k8s": {Type: "vault", Address: "http://127.0.0.1:1", AuthMethod: "kubernetes", SATokenFile: jwtFile}})
	_, err := b.encryptionPassphrase(context.Background(), "east", "vault-k8s", "replicapool/img", nil)
	if err == nil || !strings.Contains(err.Error(), "requires a role") {
		t.Fatalf("expected a 'requires a role' error, got %v", err)
	}
}

// A CSI secret passphrase overrides the KMS entirely (no Vault call needed).
func TestKMSSecretOverride(t *testing.T) {
	b := vaultBackend(t, "http://127.0.0.1:1") // unreachable; must not be contacted
	got, err := b.encryptionPassphrase(context.Background(), "east", "vault-test", "replicapool/img",
		map[string]string{secretEncryptionPassphrase: "explicit"})
	if err != nil || got != "explicit" {
		t.Fatalf("explicit secret must win without touching the KMS: got %q err %v", got, err)
	}
}

// An encryptionKMSID with no matching provider is an InvalidArgument, not a
// silent fallback to a different key source.
func TestUnknownKMSID(t *testing.T) {
	b := New(map[string]ClusterConfig{"east": {}}, "", "", newEncRunner()).WithEncryption(t.TempDir())
	_, err := b.encryptionPassphrase(context.Background(), "east", "does-not-exist", "replicapool/img", nil)
	var se *bardplugin.StatusError
	if err == nil {
		t.Fatal("expected an error for an unknown encryptionKMSID")
	}
	if !strings.Contains(err.Error(), "unknown encryptionKMSID") {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = se
}

// An unknown KMS provider type is registered but fails loudly at use.
func TestUnknownKMSType(t *testing.T) {
	b := New(map[string]ClusterConfig{"east": {}}, "", "", newEncRunner()).
		WithKMS(map[string]KMSConfig{"weird": {Type: "doesnotexist"}})
	_, err := b.encryptionPassphrase(context.Background(), "east", "weird", "replicapool/img", nil)
	if err == nil || !strings.Contains(err.Error(), "unknown KMS type") {
		t.Fatalf("expected an unknown-KMS-type error, got %v", err)
	}
}
