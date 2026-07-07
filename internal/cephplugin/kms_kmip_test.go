package cephplugin

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/gemalto/kmip-go"
	"github.com/gemalto/kmip-go/kmip14"

	"github.com/kindacoolhamster/bard-csi/internal/cephenc"
)

// fakeKMIPServer is an in-memory KMIP key manager built on the gemalto/kmip-go
// server, so the provider's client + TTLV codec are exercised over a real TLS
// KMIP round-trip (Register/Get/Destroy of SecretData objects).
type fakeKMIPServer struct {
	mu      sync.Mutex
	objects map[string][]byte
	nextID  int
	srv     *kmip.Server
	addr    string
}

func startFakeKMIP(t *testing.T) *fakeKMIPServer {
	t.Helper()
	f := &fakeKMIPServer{objects: map[string][]byte{}}

	mux := &kmip.OperationMux{}
	mux.Handle(kmip14.OperationRegister, &kmip.RegisterHandler{
		SkipValidation: true,
		RegisterFunc: func(_ context.Context, p *kmip.RegisterRequestPayload) (*kmip.RegisterResponsePayload, error) {
			mat, _ := p.SecretData.KeyBlock.KeyValue.KeyMaterial.([]byte)
			f.mu.Lock()
			f.nextID++
			uid := fmt.Sprintf("obj-%d", f.nextID)
			f.objects[uid] = append([]byte(nil), mat...)
			f.mu.Unlock()
			return &kmip.RegisterResponsePayload{UniqueIdentifier: uid}, nil
		},
	})
	mux.Handle(kmip14.OperationGet, &kmip.GetHandler{
		Get: func(_ context.Context, p *kmip.GetRequestPayload) (*kmip.GetResponsePayload, error) {
			f.mu.Lock()
			mat, ok := f.objects[p.UniqueIdentifier]
			f.mu.Unlock()
			if !ok {
				return nil, kmip.WithResultReason(fmt.Errorf("not found"), kmip14.ResultReasonItemNotFound)
			}
			return &kmip.GetResponsePayload{
				ObjectType:       kmip14.ObjectTypeSecretData,
				UniqueIdentifier: p.UniqueIdentifier,
				SecretData: &kmip.SecretData{
					SecretDataType: kmip14.SecretDataTypePassword,
					KeyBlock: kmip.KeyBlock{
						KeyFormatType: kmip14.KeyFormatTypeOpaque,
						KeyValue:      &kmip.KeyValue{KeyMaterial: mat},
					},
				},
			}, nil
		},
	})
	mux.Handle(kmip14.OperationDestroy, &kmip.DestroyHandler{
		Destroy: func(_ context.Context, p *kmip.DestroyRequestPayload) (*kmip.DestroyResponsePayload, error) {
			f.mu.Lock()
			_, ok := f.objects[p.UniqueIdentifier]
			delete(f.objects, p.UniqueIdentifier)
			f.mu.Unlock()
			if !ok {
				return nil, kmip.WithResultReason(fmt.Errorf("not found"), kmip14.ResultReasonItemNotFound)
			}
			return &kmip.DestroyResponsePayload{UniqueIdentifier: p.UniqueIdentifier}, nil
		},
	})

	f.srv = &kmip.Server{Handler: &kmip.StandardProtocolHandler{
		ProtocolVersion: kmip.ProtocolVersion{ProtocolVersionMajor: 1, ProtocolVersionMinor: 4},
		MessageHandler:  mux,
	}}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.addr = ln.Addr().String()
	tlsLn := tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{testTLSCert(t)}})
	go f.srv.Serve(tlsLn) //nolint:errcheck // closed in cleanup
	t.Cleanup(func() { f.srv.Close() })
	return f
}

func (f *fakeKMIPServer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.objects)
}

func (f *fakeKMIPServer) has(uid string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[uid]
	return ok
}

func testTLSCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "bard-kmip-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func kmipBackend(t *testing.T, addr string) (*Backend, *metaRunner) {
	t.Helper()
	run := &metaRunner{meta: map[string]string{}}
	b := New(map[string]ClusterConfig{"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin"}}, "", "", run).
		WithKMS(map[string]KMSConfig{"kmip": {Type: "kmip", KMIPEndpoint: addr, InsecureSkipVerify: true}})
	return b, run
}

// The KMIP provider stores a per-volume passphrase as a SecretData object: Register
// on first stage (UID recorded in image metadata), Get on reopen.
func TestKMIPRoundTrip(t *testing.T) {
	f := startFakeKMIP(t)
	b, run := kmipBackend(t, f.addr)
	ctx := context.Background()

	p1, err := b.encryptionPassphrase(ctx, "east", "kmip", "replicapool/img", nil)
	if err != nil {
		t.Fatalf("first stage: %v", err)
	}
	if len(p1) != 64 {
		t.Fatalf("expected a 64-char hex passphrase, got %d", len(p1))
	}
	uid := run.meta["replicapool/img|"+cephenc.MetaKMIPUID]
	if uid == "" || !f.has(uid) {
		t.Fatalf("expected a registered KMIP object recorded in image metadata; meta=%v", run.meta)
	}
	if f.count() != 1 {
		t.Fatalf("expected 1 KMIP object, got %d", f.count())
	}

	// Reopen: same volume Gets the same object.
	p2, err := b.encryptionPassphrase(ctx, "east", "kmip", "replicapool/img", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p1 != p2 {
		t.Fatalf("restage must yield the same passphrase: %q vs %q", p1, p2)
	}
	// A different volume gets a distinct object/passphrase.
	pOther, _ := b.encryptionPassphrase(ctx, "east", "kmip", "replicapool/other", nil)
	if pOther == p1 || f.count() != 2 {
		t.Fatalf("distinct volumes must get distinct objects (count=%d)", f.count())
	}

	// deleteKey Destroys the object in the HSM (not removed by rbd rm).
	if err := b.kms.DeleteKey(ctx, nil, "east", "kmip", "replicapool/img"); err != nil {
		t.Fatalf("deleteKey: %v", err)
	}
	if f.has(uid) {
		t.Fatal("deleteKey must Destroy the KMIP object")
	}
}

// An encrypted clone gets its own KMIP object holding the source's passphrase, opens
// with it, and deleting the clone Destroys only its own object (source survives).
func TestKMIPCloneSelfContained(t *testing.T) {
	f := startFakeKMIP(t)
	b, run := kmipBackend(t, f.addr)
	ctx := context.Background()
	const src, clone = "replicapool/src", "replicapool/clone"

	if err := b.imageMetaSet(ctx, nil, src, imgMetaKMSID, "kmip"); err != nil {
		t.Fatal(err)
	}
	srcPass, err := b.encryptionPassphrase(ctx, "east", "kmip", src, nil)
	if err != nil {
		t.Fatal(err)
	}
	srcUID := run.meta[src+"|"+cephenc.MetaKMIPUID]

	// Full clone path: inheritEncryption copies the KMS id and invokes cloneKey,
	// which registers an INDEPENDENT object for the clone.
	if _, err := b.inheritEncryption(ctx, nil, src, clone, "east", nil); err != nil {
		t.Fatalf("inheritEncryption: %v", err)
	}
	cloneUID := run.meta[clone+"|"+cephenc.MetaKMIPUID]
	if cloneUID == "" || cloneUID == srcUID {
		t.Fatalf("clone must own an independent KMIP object (src=%q clone=%q)", srcUID, cloneUID)
	}
	if f.count() != 2 {
		t.Fatalf("expected 2 KMIP objects after clone, got %d", f.count())
	}

	clonePass, err := b.encryptionPassphrase(ctx, "east", "kmip", clone, nil)
	if err != nil {
		t.Fatal(err)
	}
	if clonePass != srcPass {
		t.Fatalf("clone must open with the source key: %q vs %q", srcPass, clonePass)
	}

	// Delete the clone -> only its object is Destroyed; the source survives.
	if err := b.kms.DeleteKey(ctx, nil, "east", "kmip", clone); err != nil {
		t.Fatal(err)
	}
	if f.has(cloneUID) || !f.has(srcUID) {
		t.Fatal("deleting the clone must Destroy only the clone's object")
	}
	again, err := b.encryptionPassphrase(ctx, "east", "kmip", src, nil)
	if err != nil || again != srcPass {
		t.Fatalf("source key must survive clone delete: %v (%q vs %q)", err, again, srcPass)
	}
}

// A missing kmipEndpoint registers an erroring service (loud at first use).
func TestKMIPNeedsEndpoint(t *testing.T) {
	b := New(map[string]ClusterConfig{"east": {}}, "", "", &metaRunner{meta: map[string]string{}}).
		WithKMS(map[string]KMSConfig{"kmip": {Type: "kmip"}})
	if _, err := b.encryptionPassphrase(context.Background(), "east", "kmip", "p/i", nil); err == nil {
		t.Fatal("kmip without kmipEndpoint must error at use")
	}
}
