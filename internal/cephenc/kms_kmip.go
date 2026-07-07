package cephenc

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/gemalto/kmip-go"
	"github.com/gemalto/kmip-go/kmip14"
	"github.com/gemalto/kmip-go/ttlv"
)

// KMIP provider (type "kmip"): the per-volume passphrase is stored as a KMIP managed
// object (a SecretData) on a KMIP key manager -- the model ceph-csi's kmip KMS uses,
// and the one universally supported across KMIP servers / HSMs (Register, Get, Destroy
// are core KMIP 1.0). This is the on-prem / FIPS path: the passphrase lives in the HSM,
// never in Ceph, the volume context, or the plugin config.
//
//   - first stage: generate a random passphrase, Register it as a SecretData, and
//     record the returned object UID in the volume metadata.
//   - reopen: read the UID back and Get the object.
//   - clone: cloneKey reads the source's secret and Registers an INDEPENDENT copy for
//     the clone (its own UID), so the clone opens with the same passphrase yet
//     deleteKey Destroys only its own object.
//   - delete: Destroy the object (it lives in the HSM, so unlike the metadata providers
//     it is NOT removed when the volume is removed -- DeleteVolume must reap it
//     explicitly, which it does while the UID metadata still exists).
//
// Transport is KMIP-over-TLS with mutual authentication (a client cert). The TTLV wire
// codec + operations come from github.com/gemalto/kmip-go (the one KMS provider that
// pulls a dependency: TTLV is a binary protocol not worth hand-rolling).

type kmipKeyService struct {
	host       Host
	endpoint   string
	serverName string
	tlsCfg     *tls.Config
	pv         kmip.ProtocolVersion
	timeout    time.Duration
}

func newKMIPKeyService(host Host, c KMSConfig) (*kmipKeyService, error) {
	if c.KMIPEndpoint == "" {
		return nil, fmt.Errorf("cephenc: kmip provider requires kmipEndpoint")
	}
	endpoint := c.KMIPEndpoint
	if !strings.Contains(endpoint, ":") {
		endpoint += ":5696"
	}
	tlsCfg := &tls.Config{InsecureSkipVerify: c.InsecureSkipVerify} //nolint:gosec // opt-in, dev/emulator only
	// tls.Client (used per request) does not infer ServerName from the address the way
	// tls.Dial does, so set it for verification: an explicit override, else the
	// endpoint host (which must match the server cert's CN/SAN).
	if c.ServerName != "" {
		tlsCfg.ServerName = c.ServerName
	} else if host, _, err := net.SplitHostPort(endpoint); err == nil {
		tlsCfg.ServerName = host
	}
	if c.ClientCertFile != "" || c.ClientKeyFile != "" {
		cert, err := tls.LoadX509KeyPair(c.ClientCertFile, c.ClientKeyFile)
		if err != nil {
			return nil, fmt.Errorf("cephenc: kmip load client cert: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("cephenc: kmip read caFile: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("cephenc: kmip caFile %s held no certificates", c.CAFile)
		}
		tlsCfg.RootCAs = pool
	}
	return &kmipKeyService{
		host:       host,
		endpoint:   endpoint,
		serverName: c.ServerName,
		tlsCfg:     tlsCfg,
		pv:         kmip.ProtocolVersion{ProtocolVersionMajor: 1, ProtocolVersionMinor: 4},
		timeout:    20 * time.Second,
	}, nil
}

// passphrase resolves via the KMIP object whose UID is recorded for this volume (spec).
// An encrypted clone has its own UID (written by cloneKey), so it resolves an
// independent object holding the same passphrase.
func (k *kmipKeyService) passphrase(ctx context.Context, conn []string, instance, spec, _ /*keyID*/ string, secrets map[string]string) (string, error) {
	conn, cleanup, err := k.host.ConnFor(conn, instance, secrets)
	if err != nil {
		return "", err
	}
	defer cleanup()

	if uid := k.host.MetaGet(ctx, conn, spec, MetaKMIPUID); uid != "" {
		return k.get(ctx, uid)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("cephenc: kmip generate passphrase: %w", err)
	}
	pass := hex.EncodeToString(raw)
	uid, err := k.register(ctx, pass)
	if err != nil {
		return "", err
	}
	if err := k.host.MetaSet(ctx, conn, spec, MetaKMIPUID, uid); err != nil {
		// Recording the UID failed -- the object would be orphaned. Prefer a UID a
		// concurrent stage may have stored, else destroy ours and surface the error.
		if u := k.host.MetaGet(ctx, conn, spec, MetaKMIPUID); u != "" && u != uid {
			_ = k.destroy(ctx, uid)
			return k.get(ctx, u)
		}
		_ = k.destroy(ctx, uid)
		return "", err
	}
	return pass, nil
}

// cloneKey gives the clone its own KMIP object holding a copy of the source's
// passphrase, so the clone opens the copied LUKS header while Destroy on delete touches
// only its own object. A source never staged (no UID) leaves nothing to copy -- the
// clone mints its own on first stage.
func (k *kmipKeyService) cloneKey(ctx context.Context, conn []string, instance, sourceSpec, cloneSpec string, secrets map[string]string) error {
	conn, cleanup, err := k.host.ConnFor(conn, instance, secrets)
	if err != nil {
		return err
	}
	defer cleanup()
	srcUID := k.host.MetaGet(ctx, conn, sourceSpec, MetaKMIPUID)
	if srcUID == "" {
		return nil
	}
	pass, err := k.get(ctx, srcUID)
	if err != nil {
		return err
	}
	newUID, err := k.register(ctx, pass)
	if err != nil {
		return err
	}
	return k.host.MetaSet(ctx, conn, cloneSpec, MetaKMIPUID, newUID)
}

// rotateKey Registers a NEW SecretData on the HSM, lets the node add it to the LUKS
// keyslot (apply) while the old object still opens the old keyslot, repoints the
// volume's recorded UID at the new object, then Destroys the old one. On a crash
// before the UID is repointed the old object still resolves (the new one is a harmless
// orphan); after, the new object opens and the old is reaped best-effort.
func (k *kmipKeyService) rotateKey(ctx context.Context, conn []string, instance, spec, _ string, secrets map[string]string, apply func(string) error) error {
	conn, cleanup, err := k.host.ConnFor(conn, instance, secrets)
	if err != nil {
		return err
	}
	defer cleanup()
	oldUID := k.host.MetaGet(ctx, conn, spec, MetaKMIPUID)
	pass, err := randomPassphrase()
	if err != nil {
		return err
	}
	newUID, err := k.register(ctx, pass)
	if err != nil {
		return err
	}
	if err := apply(pass); err != nil {
		_ = k.destroy(ctx, newUID) // roll back the just-registered object
		return err
	}
	if err := k.host.MetaSet(ctx, conn, spec, MetaKMIPUID, newUID); err != nil {
		return err // old UID still recorded -> old key opens; new object is a harmless orphan
	}
	if oldUID != "" {
		_ = k.destroy(ctx, oldUID) // best-effort reap of the superseded HSM object
	}
	return nil
}

// deleteKey Destroys the volume's KMIP object (the passphrase lives in the HSM, so
// removing the volume does not reap it). Idempotent: a missing UID or already-destroyed
// object is success.
func (k *kmipKeyService) deleteKey(ctx context.Context, conn []string, instance, spec string) error {
	conn, cleanup, err := k.host.ConnFor(conn, instance, nil)
	if err != nil {
		return err
	}
	defer cleanup()
	uid := k.host.MetaGet(ctx, conn, spec, MetaKMIPUID)
	if uid == "" {
		return nil
	}
	return k.destroy(ctx, uid)
}

// --- KMIP operations ---

func (k *kmipKeyService) register(ctx context.Context, pass string) (string, error) {
	payload := kmip.RegisterRequestPayload{
		ObjectType: kmip14.ObjectTypeSecretData,
		SecretData: &kmip.SecretData{
			SecretDataType: kmip14.SecretDataTypePassword,
			KeyBlock: kmip.KeyBlock{
				KeyFormatType: kmip14.KeyFormatTypeOpaque,
				KeyValue:      &kmip.KeyValue{KeyMaterial: []byte(pass)},
			},
		},
	}
	pt, err := k.send(ctx, kmip14.OperationRegister, payload)
	if err != nil {
		return "", err
	}
	var rp kmip.RegisterResponsePayload
	if err := ttlv.Unmarshal(pt, &rp); err != nil {
		return "", fmt.Errorf("cephenc: kmip parse register response: %w", err)
	}
	if rp.UniqueIdentifier == "" {
		return "", fmt.Errorf("cephenc: kmip register returned no UID")
	}
	return rp.UniqueIdentifier, nil
}

func (k *kmipKeyService) get(ctx context.Context, uid string) (string, error) {
	pt, err := k.send(ctx, kmip14.OperationGet, kmip.GetRequestPayload{UniqueIdentifier: uid})
	if err != nil {
		return "", err
	}
	var gp kmip.GetResponsePayload
	if err := ttlv.Unmarshal(pt, &gp); err != nil {
		return "", fmt.Errorf("cephenc: kmip parse get response: %w", err)
	}
	if gp.SecretData == nil || gp.SecretData.KeyBlock.KeyValue == nil {
		return "", fmt.Errorf("cephenc: kmip get %s returned no secret material", uid)
	}
	switch v := gp.SecretData.KeyBlock.KeyValue.KeyMaterial.(type) {
	case []byte:
		return string(v), nil
	case string:
		return v, nil
	default:
		return "", fmt.Errorf("cephenc: kmip get %s: unexpected key material type %T", uid, v)
	}
}

func (k *kmipKeyService) destroy(ctx context.Context, uid string) error {
	_, err := k.send(ctx, kmip14.OperationDestroy, kmip.DestroyRequestPayload{UniqueIdentifier: uid})
	if err != nil {
		if isKMIPReason(err, kmip14.ResultReasonItemNotFound) {
			return nil // already gone
		}
		return err
	}
	return nil
}

// send issues a single-operation KMIP request over a fresh mutual-TLS connection and
// returns the response payload TTLV (a per-op connection keeps the client simple and
// the staging path is low frequency).
func (k *kmipKeyService) send(ctx context.Context, op kmip14.Operation, payload interface{}) (ttlv.TTLV, error) {
	d := net.Dialer{}
	raw, err := d.DialContext(ctx, "tcp", k.endpoint)
	if err != nil {
		return nil, fmt.Errorf("cephenc: kmip dial %s: %w", k.endpoint, err)
	}
	conn := tls.Client(raw, k.tlsCfg)
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(k.timeout))
	}
	if err := conn.HandshakeContext(ctx); err != nil {
		return nil, fmt.Errorf("cephenc: kmip TLS handshake: %w", err)
	}

	msg := kmip.RequestMessage{
		RequestHeader: kmip.RequestHeader{ProtocolVersion: k.pv, BatchCount: 1},
		BatchItem:     []kmip.RequestBatchItem{{Operation: op, RequestPayload: payload}},
	}
	reqTTLV, err := ttlv.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("cephenc: kmip marshal request: %w", err)
	}
	if _, err := conn.Write(reqTTLV); err != nil {
		return nil, fmt.Errorf("cephenc: kmip write: %w", err)
	}
	var resp kmip.ResponseMessage
	if err := ttlv.NewDecoder(bufio.NewReader(conn)).Decode(&resp); err != nil {
		return nil, fmt.Errorf("cephenc: kmip read response: %w", err)
	}
	if len(resp.BatchItem) == 0 {
		return nil, fmt.Errorf("cephenc: kmip empty response")
	}
	bi := resp.BatchItem[0]
	if bi.ResultStatus != kmip14.ResultStatusSuccess {
		return nil, &kmipError{op: op, reason: bi.ResultReason, msg: bi.ResultMessage}
	}
	pt, ok := bi.ResponsePayload.(ttlv.TTLV)
	if !ok {
		return nil, fmt.Errorf("cephenc: kmip %v: unexpected response payload %T", op, bi.ResponsePayload)
	}
	return pt, nil
}

// kmipError carries a KMIP operation failure with its result reason, so callers can
// distinguish e.g. ItemNotFound (idempotent delete) from a real error.
type kmipError struct {
	op     kmip14.Operation
	reason kmip14.ResultReason
	msg    string
}

func (e *kmipError) Error() string {
	return fmt.Sprintf("cephenc: kmip %v failed: %v: %s", e.op, e.reason, e.msg)
}

func isKMIPReason(err error, reason kmip14.ResultReason) bool {
	if ke, ok := err.(*kmipError); ok {
		return ke.reason == reason
	}
	return false
}
