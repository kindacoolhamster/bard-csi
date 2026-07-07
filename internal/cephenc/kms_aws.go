package cephenc

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// AWS KMS provider (type "aws-kms"): envelope encryption with a KMS key (CMK). Like
// the secrets-metadata provider, the passphrase is a per-volume data key and only
// ciphertext touches the volume metadata -- but the wrapping is done by AWS KMS, not a
// local master key, so the key-encryption key never leaves AWS:
//
//   - first stage: GenerateDataKey returns a random plaintext DEK + its KMS-encrypted
//     form. The plaintext (hex) is the passphrase; the encrypted blob is stored in the
//     volume metadata (MetaWrappedDEK, the same key the secrets-metadata provider
//     uses) and the plaintext is discarded.
//   - reopen: read the blob back and Decrypt it via KMS to recover the passphrase.
//
// Because the encrypted DEK rides in volume metadata, an encrypted clone inherits it
// through the normal descriptor copy and Decrypts the same key -- so clone-from-
// snapshot works with no extra hook -- and deleteKey is a no-op (deleting the volume
// removes the metadata; the shared CMK is admin-owned, never per-volume).
//
// The AWS calls are the KMS JSON 1.1 API signed with SigV4, implemented here on the
// standard library (no AWS SDK) to keep the plugin image lean. An Endpoint override
// points the same code at a local emulator (LocalStack) or a VPC endpoint.

// awsKMSKeyService implements keyService against AWS KMS.
type awsKMSKeyService struct {
	host     Host
	region   string
	keyID    string
	endpoint string // full base URL, e.g. https://kms.us-east-1.amazonaws.com
	creds    awsCredsResolver
	httpc    *http.Client
}

// newAWSKMSBase builds the KMS service shared by the static-credential ("aws-kms") and
// STS/web-identity ("aws-sts-metadata") providers -- they differ ONLY in how AWS
// credentials are obtained; the envelope-encryption path (GenerateDataKey, the wrapped
// DEK in image-meta, clone/delete/rotate) is identical. The caller sets b.creds.
func newAWSKMSBase(host Host, c KMSConfig) *awsKMSKeyService {
	region := orDefault(c.Region, "us-east-1")
	ep := c.Endpoint
	if ep == "" {
		ep = "https://kms." + region + ".amazonaws.com"
	}
	return &awsKMSKeyService{
		host:     host,
		region:   region,
		keyID:    c.KeyID,
		endpoint: strings.TrimRight(ep, "/"),
		httpc:    &http.Client{Timeout: 15 * time.Second},
	}
}

func newAWSKMSKeyService(host Host, c KMSConfig) keyService {
	b := newAWSKMSBase(host, c)
	b.creds = awsCredsSource{
		file:   c.CredentialsFile,
		static: awsCreds{accessKeyID: c.AccessKeyID, secretAccessKey: c.SecretAccessKey, sessionToken: c.SessionToken},
	}
	return b
}

// newAWSSTSMetadataKeyService builds the "aws-sts-metadata" provider: identical KMS
// envelope encryption, but credentials come from STS AssumeRoleWithWebIdentity using a
// projected ServiceAccount / IRSA token -- so no static AWS keys are stored, the EKS
// IRSA migration path. A missing role ARN is a config error surfaced at first use.
func newAWSSTSMetadataKeyService(host Host, c KMSConfig) keyService {
	b := newAWSKMSBase(host, c)
	if c.RoleARN == "" {
		return errKeyService{"cephenc: aws-sts-metadata provider requires roleArn"}
	}
	stsEP := c.STSEndpoint
	if stsEP == "" {
		stsEP = "https://sts." + b.region + ".amazonaws.com"
	}
	b.creds = &awsSTSCredsSource{
		endpoint:    strings.TrimRight(stsEP, "/"),
		region:      b.region,
		roleARN:     c.RoleARN,
		sessionName: orDefault(c.RoleSessionName, "bard-csi-aws-sts-metadata"),
		tokenFile:   orDefault(c.WebIdentityTokenFile, "/var/run/secrets/kubernetes.io/serviceaccount/token"),
		httpc:       &http.Client{Timeout: 15 * time.Second},
	}
	return b
}

// passphrase resolves the passphrase via KMS envelope encryption, keyed off spec (the
// volume's own metadata) -- an encrypted clone carries an independent copy of the
// encrypted DEK and Decrypts the same key.
func (a *awsKMSKeyService) passphrase(ctx context.Context, conn []string, instance, spec, _ /*keyID*/ string, secrets map[string]string) (string, error) {
	if a.keyID == "" {
		return "", fmt.Errorf("cephenc: aws-kms provider has no keyId configured")
	}
	conn, cleanup, err := a.host.ConnFor(conn, instance, secrets)
	if err != nil {
		return "", err
	}
	defer cleanup()

	// Reopen path: the encrypted DEK already exists, so Decrypt and return it.
	if blob := a.host.MetaGet(ctx, conn, spec, MetaWrappedDEK); blob != "" {
		pt, err := a.decrypt(ctx, blob)
		if err != nil {
			return "", err
		}
		return hex.EncodeToString(pt), nil
	}
	// First stage: mint a DEK in KMS, store its ciphertext, use the plaintext.
	pt, blob, err := a.generateDataKey(ctx)
	if err != nil {
		return "", err
	}
	if err := a.host.MetaSet(ctx, conn, spec, MetaWrappedDEK, blob); err != nil {
		// A concurrent first-stage may have stored its own DEK; prefer the stored one.
		if b := a.host.MetaGet(ctx, conn, spec, MetaWrappedDEK); b != "" {
			if p, derr := a.decrypt(ctx, b); derr == nil {
				return hex.EncodeToString(p), nil
			}
		}
		return "", err
	}
	return hex.EncodeToString(pt), nil
}

// deleteKey is a no-op: the encrypted DEK lives in the volume metadata (removed with
// the volume), and the KMS key is a shared, admin-owned resource, never per-volume.
func (*awsKMSKeyService) deleteKey(context.Context, []string, string, string) error { return nil }

// rotateKey GenerateDataKeys a fresh DEK, lets the node add the new plaintext DEK to
// the LUKS keyslot (apply) while the old one still works, then overwrites the encrypted
// blob in the volume metadata. Recoverable on a mid-rotation crash like the other
// metadata-stored providers.
func (a *awsKMSKeyService) rotateKey(ctx context.Context, conn []string, instance, spec, _ string, secrets map[string]string, apply func(string) error) error {
	conn, cleanup, err := a.host.ConnFor(conn, instance, secrets)
	if err != nil {
		return err
	}
	defer cleanup()
	pt, blob, err := a.generateDataKey(ctx)
	if err != nil {
		return err
	}
	if err := apply(hex.EncodeToString(pt)); err != nil {
		return err
	}
	return a.host.MetaSet(ctx, conn, spec, MetaWrappedDEK, blob)
}

// --- AWS KMS JSON API (signed with SigV4) ---

// generateDataKey asks KMS for a 256-bit data key, returning the plaintext key and its
// KMS-encrypted blob (base64, as stored in volume metadata).
func (a *awsKMSKeyService) generateDataKey(ctx context.Context) (plaintext []byte, ciphertextB64 string, err error) {
	body, _ := json.Marshal(map[string]any{"KeyId": a.keyID, "KeySpec": "AES_256"})
	var out struct {
		Plaintext      string `json:"Plaintext"`
		CiphertextBlob string `json:"CiphertextBlob"`
	}
	if err := a.call(ctx, "TrentService.GenerateDataKey", body, &out); err != nil {
		return nil, "", err
	}
	pt, err := base64.StdEncoding.DecodeString(out.Plaintext)
	if err != nil || len(pt) == 0 {
		return nil, "", fmt.Errorf("cephenc: aws-kms GenerateDataKey returned no plaintext")
	}
	if out.CiphertextBlob == "" {
		return nil, "", fmt.Errorf("cephenc: aws-kms GenerateDataKey returned no ciphertext")
	}
	return pt, out.CiphertextBlob, nil
}

// decrypt recovers a plaintext data key from its stored (base64) KMS ciphertext.
func (a *awsKMSKeyService) decrypt(ctx context.Context, ciphertextB64 string) ([]byte, error) {
	body, _ := json.Marshal(map[string]any{"CiphertextBlob": ciphertextB64, "KeyId": a.keyID})
	var out struct {
		Plaintext string `json:"Plaintext"`
	}
	if err := a.call(ctx, "TrentService.Decrypt", body, &out); err != nil {
		return nil, err
	}
	pt, err := base64.StdEncoding.DecodeString(out.Plaintext)
	if err != nil || len(pt) == 0 {
		return nil, fmt.Errorf("cephenc: aws-kms Decrypt returned no plaintext")
	}
	return pt, nil
}

// call issues a signed KMS JSON 1.1 request and decodes the result into out.
func (a *awsKMSKeyService) call(ctx context.Context, target string, body []byte, out any) error {
	creds, err := a.creds.resolve()
	if err != nil {
		return err
	}
	req, err := a.signedRequest(ctx, target, body, creds, time.Now().UTC())
	if err != nil {
		return err
	}
	resp, err := a.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("cephenc: aws-kms %s: %w", target, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cephenc: aws-kms %s: HTTP %d: %s", target, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("cephenc: aws-kms %s: parse response: %w", target, err)
	}
	return nil
}

// signedRequest builds a POST to the KMS endpoint signed with AWS Signature V4.
func (a *awsKMSKeyService) signedRequest(ctx context.Context, target string, body []byte, creds awsCreds, t time.Time) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint+"/", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	amzDate := t.Format("20060102T150405Z")
	dateStamp := t.Format("20060102")
	payloadHash := sha256Hex(body)

	headers := map[string]string{
		"content-type":         "application/x-amz-json-1.1",
		"host":                 req.URL.Host,
		"x-amz-content-sha256": payloadHash,
		"x-amz-date":           amzDate,
		"x-amz-target":         target,
	}
	if creds.sessionToken != "" {
		headers["x-amz-security-token"] = creds.sessionToken
	}

	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canon, signed strings.Builder
	for i, k := range names {
		canon.WriteString(k + ":" + headers[k] + "\n")
		if i > 0 {
			signed.WriteByte(';')
		}
		signed.WriteString(k)
	}
	signedHeaders := signed.String()

	canonicalRequest := strings.Join([]string{"POST", "/", "", canon.String(), signedHeaders, payloadHash}, "\n")
	scope := dateStamp + "/" + a.region + "/kms/aws4_request"
	stringToSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, sha256Hex([]byte(canonicalRequest))}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+creds.secretAccessKey), dateStamp)
	kRegion := hmacSHA256(kDate, a.region)
	kService := hmacSHA256(kRegion, "kms")
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+creds.accessKeyID+"/"+scope+
		", SignedHeaders="+signedHeaders+", Signature="+signature)
	return req, nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// --- credentials ---

type awsCreds struct {
	accessKeyID, secretAccessKey, sessionToken string
}

// awsCredsResolver yields AWS credentials for a KMS call. Two implementations: static
// (file/inline/env) for "aws-kms", and STS web-identity for "aws-sts-metadata".
type awsCredsResolver interface {
	resolve() (awsCreds, error)
}

// awsCredsSource resolves credentials each call (so a rotated mounted Secret is picked
// up): a shared-credentials file wins, else inline config, else the standard AWS_*
// environment variables.
type awsCredsSource struct {
	file   string
	static awsCreds
}

func (s awsCredsSource) resolve() (awsCreds, error) {
	if s.file != "" {
		data, err := os.ReadFile(s.file)
		if err != nil {
			return awsCreds{}, fmt.Errorf("cephenc: aws-kms read credentials file: %w", err)
		}
		c := parseAWSCredentials(string(data))
		if c.accessKeyID == "" || c.secretAccessKey == "" {
			return awsCreds{}, fmt.Errorf("cephenc: aws-kms credentials file %s missing access/secret key", s.file)
		}
		return c, nil
	}
	if s.static.accessKeyID != "" && s.static.secretAccessKey != "" {
		return s.static, nil
	}
	if id := os.Getenv("AWS_ACCESS_KEY_ID"); id != "" {
		return awsCreds{
			accessKeyID:     id,
			secretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
			sessionToken:    os.Getenv("AWS_SESSION_TOKEN"),
		}, nil
	}
	return awsCreds{}, fmt.Errorf("cephenc: aws-kms has no credentials (set credentialsFile, inline keys, or AWS_* env)")
}

// parseAWSCredentials reads the access/secret/session keys from the AWS shared
// credentials ini format, taking the first occurrence of each (single-profile Secret),
// ignoring section headers and comments.
func parseAWSCredentials(s string) awsCreds {
	var c awsCreds
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "aws_access_key_id":
			if c.accessKeyID == "" {
				c.accessKeyID = v
			}
		case "aws_secret_access_key":
			if c.secretAccessKey == "" {
				c.secretAccessKey = v
			}
		case "aws_session_token":
			if c.sessionToken == "" {
				c.sessionToken = v
			}
		}
	}
	return c
}

// --- STS web-identity credentials (the "aws-sts-metadata" / IRSA path) ---

// awsSTSCredsSource obtains temporary AWS credentials by exchanging a projected
// ServiceAccount / IRSA web-identity (OIDC) token for an IAM role via STS
// AssumeRoleWithWebIdentity. The token IS the authentication, so the request is
// unsigned. Credentials are cached until shortly before they expire and refreshed by
// re-reading the (rotating) token file -- so no long-lived AWS secret is ever stored.
type awsSTSCredsSource struct {
	endpoint    string // STS base URL, e.g. https://sts.us-east-1.amazonaws.com
	region      string
	roleARN     string
	sessionName string
	tokenFile   string
	httpc       *http.Client

	mu     sync.Mutex
	cached awsCreds
	expiry time.Time
}

func (s *awsSTSCredsSource) resolve() (awsCreds, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cached.accessKeyID != "" && time.Now().Before(s.expiry) {
		return s.cached, nil
	}
	token, err := os.ReadFile(s.tokenFile)
	if err != nil {
		return awsCreds{}, fmt.Errorf("cephenc: aws-sts-metadata read web identity token %q: %w", s.tokenFile, err)
	}
	creds, exp, err := s.assumeRole(strings.TrimSpace(string(token)))
	if err != nil {
		return awsCreds{}, err
	}
	s.cached = creds
	// Refresh a little before expiry; tolerate STS not returning one.
	if exp.IsZero() {
		exp = time.Now().Add(15 * time.Minute)
	}
	s.expiry = exp.Add(-2 * time.Minute)
	return creds, nil
}

// assumeRole POSTs AssumeRoleWithWebIdentity to the STS query API and parses the XML
// response for temporary credentials and their expiry. No SigV4: the web identity token
// authenticates the call.
func (s *awsSTSCredsSource) assumeRole(token string) (awsCreds, time.Time, error) {
	form := url.Values{
		"Action":           {"AssumeRoleWithWebIdentity"},
		"Version":          {"2011-06-15"},
		"RoleArn":          {s.roleARN},
		"RoleSessionName":  {s.sessionName},
		"WebIdentityToken": {token},
	}
	req, err := http.NewRequest(http.MethodPost, s.endpoint+"/", strings.NewReader(form.Encode()))
	if err != nil {
		return awsCreds{}, time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/xml")
	resp, err := s.httpc.Do(req)
	if err != nil {
		return awsCreds{}, time.Time{}, fmt.Errorf("cephenc: aws-sts-metadata AssumeRoleWithWebIdentity: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return awsCreds{}, time.Time{}, fmt.Errorf("cephenc: aws-sts-metadata AssumeRoleWithWebIdentity: HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out struct {
		Result struct {
			Credentials struct {
				AccessKeyID     string    `xml:"AccessKeyId"`
				SecretAccessKey string    `xml:"SecretAccessKey"`
				SessionToken    string    `xml:"SessionToken"`
				Expiration      time.Time `xml:"Expiration"`
			} `xml:"Credentials"`
		} `xml:"AssumeRoleWithWebIdentityResult"`
	}
	if err := xml.Unmarshal(data, &out); err != nil {
		return awsCreds{}, time.Time{}, fmt.Errorf("cephenc: aws-sts-metadata parse STS response: %w", err)
	}
	c := out.Result.Credentials
	if c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return awsCreds{}, time.Time{}, fmt.Errorf("cephenc: aws-sts-metadata STS returned no credentials")
	}
	return awsCreds{accessKeyID: c.AccessKeyID, secretAccessKey: c.SecretAccessKey, sessionToken: c.SessionToken}, c.Expiration, nil
}
