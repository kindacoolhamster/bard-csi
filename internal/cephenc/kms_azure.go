package cephenc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Azure Key Vault provider (type "azure-kv"): the per-volume passphrase is stored as a
// Key Vault *secret*, mirroring ceph-csi's azure-kv so an adopter on that KMS keeps the
// same model. Structurally this is the Vault provider with a different REST surface and
// AAD auth: the passphrase lives only in Key Vault and node memory -- never in Ceph,
// the volume context, or the plugin config.
//
//   - first stage: generate a random passphrase, PUT it as a Key Vault secret named by
//     a hash of the volume identity.
//   - reopen: GET the secret back.
//   - clone: cloneKey duplicates the source's secret into the clone's own secret, so
//     the clone opens with the same passphrase yet deleteKey (DELETE) removes only its
//     own -- the same self-contained, no-ref-count-hazard property as Vault.
//
// Auth is AAD client-credentials (a service principal) by default, or a static bearer
// token (AuthMethod "token") for emulators / pre-acquired tokens. The REST and OAuth
// flows are hand-rolled on net/http (no Azure SDK), keeping the KMS layer stdlib-only.
// An AADEndpoint override + CAFile/InsecureSkipVerify let the same code run against a
// local emulator or an Azure Stack endpoint with a private CA.

type azureKVKeyService struct {
	vaultURL     string
	apiVersion   string
	secretPrefix string
	tok          *azureTokenSource
	httpc        *http.Client
}

func newAzureKVKeyService(c KMSConfig) (*azureKVKeyService, error) {
	if c.VaultURL == "" {
		return nil, fmt.Errorf("cephenc: azure-kv provider requires vaultUrl")
	}
	httpc, err := azureHTTPClient(c)
	if err != nil {
		return nil, err
	}
	return &azureKVKeyService{
		vaultURL:     strings.TrimRight(c.VaultURL, "/"),
		apiVersion:   "7.4",
		secretPrefix: orDefault(c.SecretPrefix, "bard-luks"),
		tok: &azureTokenSource{
			method:           orDefault(c.AuthMethod, "clientSecret"),
			token:            c.Token,
			tokenFile:        c.TokenFile,
			aadEndpoint:      strings.TrimRight(orDefault(c.AADEndpoint, "https://login.microsoftonline.com"), "/"),
			tenantID:         c.TenantID,
			clientID:         c.ClientID,
			clientSecret:     c.ClientSecret,
			clientSecretFile: c.ClientSecretFile,
			httpc:            httpc,
		},
		httpc: httpc,
	}, nil
}

// azureHTTPClient builds the HTTP client, optionally trusting a custom CA bundle (Azure
// Stack / emulator) or skipping verification (dev only).
func azureHTTPClient(c KMSConfig) (*http.Client, error) {
	tlsCfg := &tls.Config{InsecureSkipVerify: c.InsecureSkipVerify} //nolint:gosec // opt-in, dev/emulator only
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("cephenc: azure-kv read caFile: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("cephenc: azure-kv caFile %s held no certificates", c.CAFile)
		}
		tlsCfg.RootCAs = pool
	}
	return &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg}}, nil
}

// secretName is the Key Vault secret name for a volume: a hash so the name leaks no
// pool/image. Key Vault names must match ^[0-9a-zA-Z-]+$, which a hex hash satisfies.
func (a *azureKVKeyService) secretName(instance, spec string) string {
	h := sha256.Sum256([]byte(instance + "|" + spec))
	return a.secretPrefix + "-" + hex.EncodeToString(h[:20])
}

// passphrase keys off spec (the volume's own secret), not keyID: an encrypted clone
// gets its own secret holding a copy of the source's passphrase (written by cloneKey),
// so it opens the copied header yet deleteKey removes only its own.
func (a *azureKVKeyService) passphrase(ctx context.Context, _ []string, instance, spec, _ /*keyID*/ string, _ map[string]string) (string, error) {
	name := a.secretName(instance, spec)
	if v, found, err := a.getSecret(ctx, name); err != nil {
		return "", err
	} else if found {
		return v, nil
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("cephenc: azure-kv generate passphrase: %w", err)
	}
	pass := hex.EncodeToString(raw)
	if err := a.setSecret(ctx, name, pass); err != nil {
		// A concurrent writer may have created it first; prefer the stored value.
		if v, found, gerr := a.getSecret(ctx, name); gerr == nil && found {
			return v, nil
		}
		return "", err
	}
	return pass, nil
}

// cloneKey duplicates the source volume's secret into the clone's own secret so the
// clone opens the copied LUKS header with the same passphrase while owning an
// independent entry. A source with no stored secret (never staged) leaves nothing to
// copy -- the clone mints its own on first stage.
func (a *azureKVKeyService) cloneKey(ctx context.Context, _ []string, instance, sourceSpec, cloneSpec string, _ map[string]string) error {
	v, found, err := a.getSecret(ctx, a.secretName(instance, sourceSpec))
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	return a.setSecret(ctx, a.secretName(instance, cloneSpec), v)
}

// deleteKey removes the volume's secret (Azure soft-deletes it). Idempotent: a missing
// secret is success.
func (a *azureKVKeyService) deleteKey(ctx context.Context, _ []string, instance, spec string) error {
	resp, err := a.do(ctx, http.MethodDelete, "/secrets/"+a.secretName(instance, spec), nil)
	if err != nil {
		return fmt.Errorf("cephenc: azure-kv delete: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusNotFound:
		return nil
	}
	data, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("cephenc: azure-kv delete: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
}

// rotateKey mints a fresh passphrase, lets the node add it to the LUKS keyslot
// (apply) using the still-valid old one, then overwrites the Key Vault secret (a new
// secret version). Recoverable on a mid-rotation crash: until setSecret commits, the
// old secret value still resolves and opens the old keyslot.
func (a *azureKVKeyService) rotateKey(ctx context.Context, _ []string, instance, spec, _ string, _ map[string]string, apply func(string) error) error {
	pass, err := randomPassphrase()
	if err != nil {
		return err
	}
	if err := apply(pass); err != nil {
		return err
	}
	return a.setSecret(ctx, a.secretName(instance, spec), pass)
}

func (a *azureKVKeyService) getSecret(ctx context.Context, name string) (string, bool, error) {
	resp, err := a.do(ctx, http.MethodGet, "/secrets/"+name, nil)
	if err != nil {
		return "", false, fmt.Errorf("cephenc: azure-kv get: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("cephenc: azure-kv get %s: HTTP %d: %s", name, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", false, fmt.Errorf("cephenc: azure-kv parse get: %w", err)
	}
	if out.Value == "" {
		return "", false, nil
	}
	return out.Value, true, nil
}

func (a *azureKVKeyService) setSecret(ctx context.Context, name, value string) error {
	body, _ := json.Marshal(map[string]string{"value": value})
	resp, err := a.do(ctx, http.MethodPut, "/secrets/"+name, body)
	if err != nil {
		return fmt.Errorf("cephenc: azure-kv set: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	data, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("cephenc: azure-kv set %s: HTTP %d: %s", name, resp.StatusCode, strings.TrimSpace(string(data)))
}

// do issues a bearer-token request to a Key Vault path. A 401 (token expired or
// revoked sooner than estimated) triggers one token refresh and retry.
func (a *azureKVKeyService) do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	resp, err := a.doOnce(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		a.tok.invalidate()
		return a.doOnce(ctx, method, path, body)
	}
	return resp, nil
}

func (a *azureKVKeyService) doOnce(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	token, err := a.tok.get(ctx)
	if err != nil {
		return nil, err
	}
	u := a.vaultURL + path + "?api-version=" + a.apiVersion
	var r io.Reader
	if body != nil {
		r = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return a.httpc.Do(req)
}

// azureTokenSource yields an AAD bearer token: a static token (for emulators) or one
// obtained by the OAuth2 client-credentials flow, cached until shortly before it
// expires.
type azureTokenSource struct {
	method                                                          string // "clientSecret" | "token"
	token, tokenFile                                                string
	aadEndpoint, tenantID, clientID, clientSecret, clientSecretFile string
	httpc                                                           *http.Client

	mu     sync.Mutex
	cached string
	expiry time.Time
}

func (t *azureTokenSource) invalidate() {
	t.mu.Lock()
	t.cached = ""
	t.mu.Unlock()
}

func (t *azureTokenSource) get(ctx context.Context) (string, error) {
	if t.method == "token" {
		if t.tokenFile != "" {
			b, err := os.ReadFile(t.tokenFile)
			if err != nil {
				return "", fmt.Errorf("cephenc: azure-kv read token file: %w", err)
			}
			return strings.TrimSpace(string(b)), nil
		}
		if t.token != "" {
			return t.token, nil
		}
		return "", fmt.Errorf("cephenc: azure-kv token auth has neither token nor tokenFile")
	}
	return t.clientCredentials(ctx)
}

// clientCredentials runs the OAuth2 client-credentials grant against AAD and caches the
// token until ~30s before expiry.
func (t *azureTokenSource) clientCredentials(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cached != "" && time.Now().Before(t.expiry) {
		return t.cached, nil
	}
	if t.tenantID == "" || t.clientID == "" {
		return "", fmt.Errorf("cephenc: azure-kv client-credentials auth requires tenantId and clientId")
	}
	secret := t.clientSecret
	if t.clientSecretFile != "" {
		b, err := os.ReadFile(t.clientSecretFile)
		if err != nil {
			return "", fmt.Errorf("cephenc: azure-kv read client secret file: %w", err)
		}
		secret = strings.TrimSpace(string(b))
	}
	if secret == "" {
		return "", fmt.Errorf("cephenc: azure-kv client-credentials auth has no clientSecret")
	}
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {t.clientID},
		"client_secret": {secret},
		"scope":         {"https://vault.azure.net/.default"},
	}
	endpoint := t.aadEndpoint + "/" + t.tenantID + "/oauth2/v2.0/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := t.httpc.Do(req)
	if err != nil {
		return "", fmt.Errorf("cephenc: azure-kv AAD token: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("cephenc: azure-kv AAD token: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("cephenc: azure-kv parse AAD token: %w", err)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("cephenc: azure-kv AAD returned no token")
	}
	t.cached = out.AccessToken
	ttl := time.Duration(out.ExpiresIn) * time.Second
	if ttl <= time.Minute {
		ttl = time.Minute
	}
	t.expiry = time.Now().Add(ttl - 30*time.Second)
	return t.cached, nil
}
