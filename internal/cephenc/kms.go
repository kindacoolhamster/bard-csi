package cephenc

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// KMS providers resolve the passphrase for an encrypted volume. A volume selects one
// by the encryptionKMSID StorageClass parameter; an empty id uses the built-in
// derived provider (a per-instance master key, HKDF-expanded). This mirrors ceph-csi's
// encryptionKMSID model, but the whole KMS lives inside the plugin -- core never sees
// a key and a plugin in any language can ship its own providers.

// KMSConfig configures one named KMS provider (the map key is the id matched by
// encryptionKMSID). Only the fields relevant to Type are used.
type KMSConfig struct {
	Type string `json:"type"` // "vault"
	// Vault:
	Address    string `json:"address,omitempty"`    // e.g. http://vault:8200
	KVMount    string `json:"kvMount,omitempty"`    // KV v2 mount (default "secret")
	PathPrefix string `json:"pathPrefix,omitempty"` // secret path prefix (default "bard-luks")
	// Auth. AuthMethod is "token" (default) or "kubernetes".
	AuthMethod string `json:"authMethod,omitempty"`
	Token      string `json:"token,omitempty"`     // token auth: inline token (prefer TokenFile)
	TokenFile  string `json:"tokenFile,omitempty"` // token auth: file holding the Vault token
	// Kubernetes auth: the plugin logs in with its ServiceAccount JWT to get a
	// short-lived Vault token, instead of holding a long-lived static one.
	Role        string `json:"role,omitempty"`        // Vault role to assume
	AuthMount   string `json:"authMount,omitempty"`   // k8s auth mount (default "kubernetes")
	SATokenFile string `json:"saTokenFile,omitempty"` // SA JWT path (default the projected token)
	// AWS KMS (type "aws-kms"): envelope encryption with a KMS key. See kms_aws.go.
	Region   string `json:"region,omitempty"`   // AWS region, e.g. us-east-1
	KeyID    string `json:"keyId,omitempty"`    // KMS key id / ARN / alias used to wrap DEKs
	Endpoint string `json:"endpoint,omitempty"` // override the regional endpoint (LocalStack, VPC endpoint)
	// Credentials. Prefer CredentialsFile (a mounted Secret in the AWS shared-creds
	// format); inline fields are for dev/LocalStack; env (AWS_ACCESS_KEY_ID etc.) is
	// the last fallback. Resolution order: CredentialsFile > inline > env.
	CredentialsFile string `json:"credentialsFile,omitempty"`
	AccessKeyID     string `json:"accessKeyId,omitempty"`
	SecretAccessKey string `json:"secretAccessKey,omitempty"`
	SessionToken    string `json:"sessionToken,omitempty"`
	// AWS STS web-identity (type "aws-sts-metadata"): no static keys -- credentials
	// come from STS AssumeRoleWithWebIdentity using a projected SA / IRSA token (the EKS
	// migration path). RoleARN is required; the rest default. See kms_aws.go.
	RoleARN              string `json:"roleArn,omitempty"`              // IAM role to assume
	RoleSessionName      string `json:"roleSessionName,omitempty"`      // STS session name (default bard-csi-aws-sts-metadata)
	WebIdentityTokenFile string `json:"webIdentityTokenFile,omitempty"` // OIDC token path (default the projected SA token)
	STSEndpoint          string `json:"stsEndpoint,omitempty"`          // override the STS endpoint (emulator / VPC endpoint)
	// Azure Key Vault (type "azure-kv"): per-volume passphrase stored as a Key Vault
	// secret. See kms_azure.go. Auth is AAD client-credentials (the default) or a
	// static token (AuthMethod "token", reusing Token/TokenFile) for emulators. The
	// Vault/AWS fields above are reused where they overlap (AuthMethod, Token,
	// TokenFile). TLS to a custom/emulator endpoint via CAFile or InsecureSkipVerify.
	VaultURL           string `json:"vaultUrl,omitempty"` // https://<name>.vault.azure.net
	TenantID           string `json:"tenantId,omitempty"`
	ClientID           string `json:"clientId,omitempty"`
	ClientSecret       string `json:"clientSecret,omitempty"`
	ClientSecretFile   string `json:"clientSecretFile,omitempty"`
	AADEndpoint        string `json:"aadEndpoint,omitempty"`  // default https://login.microsoftonline.com
	SecretPrefix       string `json:"secretPrefix,omitempty"` // default bard-luks
	CAFile             string `json:"caFile,omitempty"`       // trust a custom CA (Azure Stack / emulator / KMIP)
	InsecureSkipVerify bool   `json:"insecureSkipVerify,omitempty"`
	// KMIP (type "kmip"): per-volume passphrase stored as a KMIP managed object over
	// mutual-TLS. See kms_kmip.go. Reuses CAFile (server CA) + InsecureSkipVerify.
	KMIPEndpoint   string `json:"kmipEndpoint,omitempty"`   // host:port, default :5696
	ClientCertFile string `json:"clientCertFile,omitempty"` // mutual-TLS client cert (PEM)
	ClientKeyFile  string `json:"clientKeyFile,omitempty"`  // mutual-TLS client key (PEM)
	ServerName     string `json:"serverName,omitempty"`     // TLS server name override
}

// Register builds and adds named KMS providers from config (merging into any already
// registered). An unknown provider type is registered as an erroring service so a
// misconfiguration surfaces loudly the first time a volume uses it.
func (r *Registry) Register(cfgs map[string]KMSConfig) {
	for id, c := range cfgs {
		switch c.Type {
		case "vault":
			httpc, err := httpClientTLS(c, 15*time.Second)
			if err != nil {
				r.providers[id] = errKeyService{err.Error()}
				continue
			}
			r.providers[id] = &vaultKeyService{
				addr:         strings.TrimRight(c.Address, "/"),
				kvMount:      orDefault(c.KVMount, "secret"),
				pathPrefix:   orDefault(c.PathPrefix, "bard-luks"),
				authMethod:   orDefault(c.AuthMethod, "token"),
				token:        c.Token,
				tokenFile:    c.TokenFile,
				role:         c.Role,
				k8sAuthMount: orDefault(c.AuthMount, "kubernetes"),
				saTokenFile:  orDefault(c.SATokenFile, "/var/run/secrets/kubernetes.io/serviceaccount/token"),
				httpc:        httpc,
			}
		case "aws-kms":
			r.providers[id] = newAWSKMSKeyService(r.host, c)
		case "aws-sts-metadata":
			r.providers[id] = newAWSSTSMetadataKeyService(r.host, c)
		case "azure-kv":
			ks, err := newAzureKVKeyService(c)
			if err != nil {
				r.providers[id] = errKeyService{err.Error()}
			} else {
				r.providers[id] = ks
			}
		case "kmip":
			ks, err := newKMIPKeyService(r.host, c)
			if err != nil {
				r.providers[id] = errKeyService{err.Error()}
			} else {
				r.providers[id] = ks
			}
		case "secrets-metadata", "kubernetes":
			// ceph-csi's "secrets-metadata" model: a per-volume random key wrapped
			// with the instance master key and stored in the volume metadata. No
			// external KMS, no per-volume Secrets -- reuses the master key dir.
			r.providers[id] = &secretsMetadataKeyService{host: r.host}
		default:
			r.providers[id] = errKeyService{fmt.Sprintf("cephenc: unknown KMS type %q for encryptionKMSID %q", c.Type, id)}
		}
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// httpClientTLS builds a provider's HTTP client, honouring the config's CAFile /
// InsecureSkipVerify so a Vault behind a private CA (or an emulator) works like
// the Azure/KMIP providers already do.
func httpClientTLS(c KMSConfig, timeout time.Duration) (*http.Client, error) {
	tr := &http.Transport{}
	if c.CAFile != "" || c.InsecureSkipVerify {
		tlsCfg := &tls.Config{InsecureSkipVerify: c.InsecureSkipVerify} //nolint:gosec // opt-in, dev/emulator only
		if c.CAFile != "" {
			pem, err := os.ReadFile(c.CAFile)
			if err != nil {
				return nil, fmt.Errorf("cephenc: read caFile: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("cephenc: caFile %s held no certificates", c.CAFile)
			}
			tlsCfg.RootCAs = pool
		}
		tr.TLSClientConfig = tlsCfg
	}
	return &http.Client{Timeout: timeout, Transport: tr}, nil
}

// SecretTemp creates a temp file for short-lived secret material (cephx keys,
// LUKS passphrases handed to CLIs via --keyfile/--key-file). It prefers the
// tmpfs at /dev/shm so plaintext keys never touch a disk-backed filesystem --
// a container's default temp dir is typically overlayfs on the node's disk --
// falling back to the default temp dir where /dev/shm is unavailable. Callers
// remove the file when the command returns, exactly as with os.CreateTemp.
func SecretTemp(pattern string) (*os.File, error) {
	if fi, err := os.Stat("/dev/shm"); err == nil && fi.IsDir() {
		if f, err := os.CreateTemp("/dev/shm", pattern); err == nil {
			return f, nil
		}
	}
	return os.CreateTemp("", pattern)
}

// derivedKeyService derives a per-volume passphrase from a per-instance master key
// with HKDF -- no key is stored anywhere, the same volume always re-derives the same
// passphrase. The default when no encryptionKMSID is set.
type derivedKeyService struct{ keyDir string }

// passphrase derives from keyID (not spec) so a clone, which inherits its source's
// keyID, re-derives the source's passphrase to open the copied header.
func (d derivedKeyService) passphrase(_ context.Context, _ []string, instance, _ /*spec*/, keyID string, _ map[string]string) (string, error) {
	master, err := readMasterKey(d.keyDir, instance)
	if err != nil {
		return "", err
	}
	key, err := hkdf.Key(sha256.New, master, nil, "bard-luks:"+keyID, 32)
	if err != nil {
		return "", fmt.Errorf("cephenc: derive passphrase: %w", err)
	}
	return hex.EncodeToString(key), nil
}

// deleteKey is a no-op: a derived passphrase is computed on demand, never stored.
func (derivedKeyService) deleteKey(context.Context, []string, string, string) error { return nil }

// readMasterKey reads the per-instance master key file from a mounted Secret dir.
// Shared by the derived and secrets-metadata KMSes.
func readMasterKey(keyDir, instance string) ([]byte, error) {
	if keyDir == "" {
		return nil, fmt.Errorf("cephenc: volume is encrypted but no master key dir configured and no %s secret given", SecretPassphrase)
	}
	data, err := os.ReadFile(filepath.Join(keyDir, instance))
	if err != nil {
		return nil, fmt.Errorf("cephenc: read encryption master key for instance %q: %w", instance, err)
	}
	master := bytes.TrimSpace(data)
	if len(master) == 0 {
		return nil, fmt.Errorf("cephenc: empty encryption master key for instance %q", instance)
	}
	return master, nil
}

// secretsMetadataKeyService implements ceph-csi's "secrets-metadata" KMS: the
// passphrase is a per-volume random key (DEK), wrapped with a key-encryption key
// derived from the instance master key and stored in the volume's metadata. Each
// volume's key is independent random material, but it needs no external KMS and
// creates no per-volume Secrets. The wrapped key rides with the volume, so
// DeleteVolume removes it (deleteKey is a no-op).
type secretsMetadataKeyService struct {
	host Host
}

// passphrase keys off spec (the volume's own metadata), not keyID: an encrypted
// clone carries an independent copy of the source's wrapped DEK in its own metadata
// (copied at create), so it unwraps the same key while staying self-contained.
func (s *secretsMetadataKeyService) passphrase(ctx context.Context, conn []string, instance, spec, _ /*keyID*/ string, secrets map[string]string) (string, error) {
	kek, err := readMasterKey(s.host.MasterKeyDir(), instance)
	if err != nil {
		return "", err
	}
	conn, cleanup, err := s.host.ConnFor(conn, instance, secrets)
	if err != nil {
		return "", err
	}
	defer cleanup() // spec == pool/image or fs/subvolume

	// Reopen path: the wrapped DEK already exists, so unwrap and return it.
	if wrapped := s.host.MetaGet(ctx, conn, spec, MetaWrappedDEK); wrapped != "" {
		return unwrapDEK(kek, wrapped)
	}
	// First stage: mint a random DEK, store it wrapped, then use it.
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("cephenc: generate DEK: %w", err)
	}
	dek := hex.EncodeToString(raw)
	wrapped, err := wrapDEK(kek, dek)
	if err != nil {
		return "", err
	}
	if err := s.host.MetaSet(ctx, conn, spec, MetaWrappedDEK, wrapped); err != nil {
		// A concurrent first-stage may have stored its own DEK; prefer the stored one.
		if w := s.host.MetaGet(ctx, conn, spec, MetaWrappedDEK); w != "" {
			return unwrapDEK(kek, w)
		}
		return "", err
	}
	return dek, nil
}

// deleteKey is a no-op: the wrapped DEK lives in the volume metadata, so it is removed
// together with the volume by DeleteVolume.
func (*secretsMetadataKeyService) deleteKey(context.Context, []string, string, string) error {
	return nil
}

// rotateKey mints a fresh DEK, lets the node add it to the LUKS keyslot (apply) while
// the old DEK still works, then overwrites the wrapped DEK in the volume metadata so
// the new key becomes canonical. If apply fails the stored DEK is untouched (old key
// still resolves); if the MetaSet fails after apply, both keyslots are valid and the
// next stage re-reads the old DEK -- recoverable either way.
func (s *secretsMetadataKeyService) rotateKey(ctx context.Context, conn []string, instance, spec, _ string, secrets map[string]string, apply func(string) error) error {
	kek, err := readMasterKey(s.host.MasterKeyDir(), instance)
	if err != nil {
		return err
	}
	conn, cleanup, err := s.host.ConnFor(conn, instance, secrets)
	if err != nil {
		return err
	}
	defer cleanup()
	newDEK, err := randomPassphrase()
	if err != nil {
		return err
	}
	wrapped, err := wrapDEK(kek, newDEK)
	if err != nil {
		return err
	}
	if err := apply(newDEK); err != nil {
		return err
	}
	return s.host.MetaSet(ctx, conn, spec, MetaWrappedDEK, wrapped)
}

// randomPassphrase returns a fresh 32-byte random key as hex, the form used for a
// LUKS passphrase / DEK across the stored-key providers.
func randomPassphrase() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("cephenc: generate key: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// kekCipher derives a 32-byte AES key from the master key and returns an AES-GCM AEAD
// for wrapping/unwrapping a volume DEK.
func kekCipher(kek []byte) (cipher.AEAD, error) {
	aesKey, err := hkdf.Key(sha256.New, kek, nil, "bard-luks-kek", 32)
	if err != nil {
		return nil, fmt.Errorf("cephenc: derive KEK: %w", err)
	}
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// wrapDEK AES-GCM-encrypts a DEK with the master key; the nonce is prepended.
func wrapDEK(kek []byte, dek string) (string, error) {
	gcm, err := kekCipher(kek)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("cephenc: nonce: %w", err)
	}
	return hex.EncodeToString(gcm.Seal(nonce, nonce, []byte(dek), nil)), nil
}

// unwrapDEK reverses wrapDEK. A decryption failure (wrong/rotated master key or
// tampered metadata) is surfaced so the mount fails loudly rather than silently.
func unwrapDEK(kek []byte, wrapped string) (string, error) {
	gcm, err := kekCipher(kek)
	if err != nil {
		return "", err
	}
	raw, err := hex.DecodeString(wrapped)
	if err != nil || len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("cephenc: malformed wrapped DEK")
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("cephenc: unwrap DEK (wrong master key or corrupt metadata): %w", err)
	}
	return string(pt), nil
}

// errKeyService fails every request; used for a misconfigured provider so the error
// appears at use rather than being swallowed at startup.
type errKeyService struct{ msg string }

func (e errKeyService) passphrase(context.Context, []string, string, string, string, map[string]string) (string, error) {
	return "", bardplugin.Errorf(bardplugin.CodeInvalidArg, "%s", e.msg)
}

// deleteKey is a no-op for a misconfigured provider: a delete must not be blocked by
// KMS misconfig (there is nothing this provider could have stored).
func (errKeyService) deleteKey(context.Context, []string, string, string) error { return nil }

// vaultKeyService stores a random per-volume passphrase in HashiCorp Vault's KV v2
// engine: the first stage generates and writes it (create-only), later stages read it
// back. The plaintext passphrase therefore lives only in Vault and in node memory
// while mounting -- never in Ceph, the volume context, or the plugin config.
type vaultKeyService struct {
	addr, kvMount, pathPrefix string
	// token auth:
	token, tokenFile string
	// kubernetes auth:
	authMethod, role, k8sAuthMount, saTokenFile string
	httpc                                       *http.Client

	mu        sync.Mutex // guards the cached k8s-auth token
	cachedTok string
	tokExpiry time.Time
}

// tok returns a Vault token to authenticate a request: a static token, or -- for
// kubernetes auth -- a short-lived token obtained by logging in with the plugin's
// ServiceAccount JWT (cached until shortly before it expires).
func (v *vaultKeyService) tok(ctx context.Context) (string, error) {
	if v.authMethod == "kubernetes" {
		return v.kubernetesToken(ctx)
	}
	if v.tokenFile != "" {
		b, err := os.ReadFile(v.tokenFile)
		if err != nil {
			return "", fmt.Errorf("cephenc: read vault token file: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	if v.token != "" {
		return v.token, nil
	}
	return "", fmt.Errorf("cephenc: vault KMS has neither token nor tokenFile")
}

func (v *vaultKeyService) invalidate() {
	v.mu.Lock()
	v.cachedTok = ""
	v.mu.Unlock()
}

// kubernetesToken returns a cached login token if still valid, else logs in to Vault's
// Kubernetes auth method with the SA JWT and caches the new token until just before
// its lease expires.
func (v *vaultKeyService) kubernetesToken(ctx context.Context) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.cachedTok != "" && time.Now().Before(v.tokExpiry) {
		return v.cachedTok, nil
	}
	jwt, err := os.ReadFile(v.saTokenFile)
	if err != nil {
		return "", fmt.Errorf("cephenc: read service account token: %w", err)
	}
	if v.role == "" {
		return "", fmt.Errorf("cephenc: vault kubernetes auth requires a role")
	}
	body, _ := json.Marshal(map[string]string{"role": v.role, "jwt": strings.TrimSpace(string(jwt))})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.addr+"/v1/auth/"+v.k8sAuthMount+"/login", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := v.httpc.Do(req)
	if err != nil {
		return "", fmt.Errorf("cephenc: vault k8s login: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("cephenc: vault k8s login: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int    `json:"lease_duration"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("cephenc: parse vault login: %w", err)
	}
	if out.Auth.ClientToken == "" {
		return "", fmt.Errorf("cephenc: vault login returned no token")
	}
	v.cachedTok = out.Auth.ClientToken
	// Renew a little early; default to a short window if Vault reports no lease.
	ttl := time.Duration(out.Auth.LeaseDuration) * time.Second
	if ttl <= time.Minute {
		ttl = time.Minute
	}
	v.tokExpiry = time.Now().Add(ttl - 30*time.Second)
	return v.cachedTok, nil
}

// secretPath is the KV path for a volume: a hash so the path leaks no pool/name.
func (v *vaultKeyService) secretPath(instance, volumeKey string) string {
	h := sha256.Sum256([]byte(instance + "|" + volumeKey))
	return v.pathPrefix + "/" + hex.EncodeToString(h[:16])
}

// passphrase keys off spec (the volume's own KV path), not keyID: an encrypted clone
// gets its own KV entry holding a copy of the source's passphrase (written by cloneKey
// at create), so it opens the copied header yet deleteKey removes only its own entry.
func (v *vaultKeyService) passphrase(ctx context.Context, _ []string, instance, spec, _ /*keyID*/ string, _ map[string]string) (string, error) {
	path := v.secretPath(instance, spec)
	if existing, found, err := v.read(ctx, path); err != nil {
		return "", err
	} else if found {
		return existing, nil
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("cephenc: generate passphrase: %w", err)
	}
	pass := hex.EncodeToString(raw)
	if err := v.write(ctx, path, pass, true); err != nil {
		// Another node won the create race: read the stored value instead.
		if existing, found, gerr := v.read(ctx, path); gerr == nil && found {
			return existing, nil
		}
		return "", err
	}
	return pass, nil
}

// cloneKey duplicates the source volume's stored passphrase into the clone's own KV
// path so the clone opens the copied LUKS header with the same passphrase while owning
// an independent entry. Create-only (cas=0); idempotent on retry. A source with no
// stored passphrase (created but never staged) leaves nothing to copy -- the clone
// mints its own on first stage.
func (v *vaultKeyService) cloneKey(ctx context.Context, _ []string, instance, sourceSpec, cloneSpec string, _ map[string]string) error {
	pass, found, err := v.read(ctx, v.secretPath(instance, sourceSpec))
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	dst := v.secretPath(instance, cloneSpec)
	if err := v.write(ctx, dst, pass, true); err != nil {
		// A retry (or concurrent clone) already wrote the same value: success.
		if existing, ok, rerr := v.read(ctx, dst); rerr == nil && ok && existing == pass {
			return nil
		}
		return err
	}
	return nil
}

// rotateKey mints a fresh passphrase, lets the node add it to the LUKS keyslot
// (apply) using the still-valid old one, then overwrites the Vault entry (new KV
// version). If apply fails Vault is untouched (old passphrase still resolves); if the
// overwrite fails after apply, both keyslots are valid and the next stage re-reads the
// old passphrase -- recoverable.
func (v *vaultKeyService) rotateKey(ctx context.Context, _ []string, instance, spec, _ string, _ map[string]string, apply func(string) error) error {
	pass, err := randomPassphrase()
	if err != nil {
		return err
	}
	if err := apply(pass); err != nil {
		return err
	}
	return v.write(ctx, v.secretPath(instance, spec), pass, false)
}

// deleteKey removes all versions and metadata of a volume's passphrase from KV v2
// (DELETE on the metadata endpoint). Idempotent: a missing entry is success.
func (v *vaultKeyService) deleteKey(ctx context.Context, _ []string, instance, volumeKey string) error {
	path := v.secretPath(instance, volumeKey)
	resp, err := v.do(ctx, http.MethodDelete, "metadata/"+path, nil)
	if err != nil {
		return fmt.Errorf("cephenc: vault delete: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK, http.StatusNotFound:
		return nil
	}
	data, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("cephenc: vault delete %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(data)))
}

// do issues a token-authenticated request to a Vault API path. The data/metadata
// segment differs by operation, so callers pass the full sub-path after /v1/<mount>/.
// For kubernetes auth a 401/403 (token expired or revoked sooner than estimated)
// triggers one re-login and retry.
func (v *vaultKeyService) do(ctx context.Context, method, subPath string, body []byte) (*http.Response, error) {
	resp, err := v.doOnce(ctx, method, subPath, body)
	if err != nil {
		return nil, err
	}
	if v.authMethod == "kubernetes" && (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized) {
		resp.Body.Close()
		v.invalidate()
		return v.doOnce(ctx, method, subPath, body)
	}
	return resp, nil
}

func (v *vaultKeyService) doOnce(ctx context.Context, method, subPath string, body []byte) (*http.Response, error) {
	token, err := v.tok(ctx)
	if err != nil {
		return nil, err
	}
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, v.addr+"/v1/"+v.kvMount+"/"+subPath, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Vault-Token", token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return v.httpc.Do(req)
}

func (v *vaultKeyService) read(ctx context.Context, path string) (string, bool, error) {
	resp, err := v.do(ctx, http.MethodGet, "data/"+path, nil)
	if err != nil {
		return "", false, fmt.Errorf("cephenc: vault read: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("cephenc: vault read %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out struct {
		Data struct {
			Data struct {
				Passphrase string `json:"passphrase"`
			} `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", false, fmt.Errorf("cephenc: parse vault read: %w", err)
	}
	if out.Data.Data.Passphrase == "" {
		return "", false, nil
	}
	return out.Data.Data.Passphrase, true, nil
}

func (v *vaultKeyService) write(ctx context.Context, path, pass string, createOnly bool) error {
	// createOnly (cas=0) means a concurrent writer cannot clobber an existing
	// passphrase (which would orphan the data encrypted under the old one). Rotation
	// deliberately overwrites, so it omits cas to add a new KV version.
	payload := map[string]any{"data": map[string]string{"passphrase": pass}}
	if createOnly {
		payload["options"] = map[string]any{"cas": 0}
	}
	body, _ := json.Marshal(payload)
	resp, err := v.do(ctx, http.MethodPost, "data/"+path, body)
	if err != nil {
		return fmt.Errorf("cephenc: vault write: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	data, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("cephenc: vault write %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(data)))
}
