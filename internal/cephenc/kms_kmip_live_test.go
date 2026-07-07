package cephenc

import (
	"context"
	"os"
	"testing"
)

// TestKMIPLive drives the real KMIP client (Register/Get/Destroy) against a live
// KMIP server -- e.g. PyKMIP, a different implementation than the in-process server
// the hermetic test uses, so it catches cross-implementation interop. Opt-in:
//
//	BARD_CSI_KMIP_TEST=1 \
//	KMIP_ENDPOINT=127.0.0.1:5696 \
//	KMIP_CLIENT_CERT=/tmp/pykmip/client.crt KMIP_CLIENT_KEY=/tmp/pykmip/client.key \
//	KMIP_CA=/tmp/pykmip/ca.crt \
//	go test ./internal/cephplugin -run TestKMIPLive -v
func TestKMIPLive(t *testing.T) {
	if os.Getenv("BARD_CSI_KMIP_TEST") != "1" {
		t.Skip("set BARD_CSI_KMIP_TEST=1 (+ KMIP_* env) to run against a live KMIP server")
	}
	ks, err := newKMIPKeyService(nil, KMSConfig{
		Type:               "kmip",
		KMIPEndpoint:       os.Getenv("KMIP_ENDPOINT"),
		ClientCertFile:     os.Getenv("KMIP_CLIENT_CERT"),
		ClientKeyFile:      os.Getenv("KMIP_CLIENT_KEY"),
		CAFile:             os.Getenv("KMIP_CA"),
		InsecureSkipVerify: os.Getenv("KMIP_INSECURE") == "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const secret = "bard-kmip-live-passphrase-deadbeef"

	uid, err := ks.register(ctx, secret)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Logf("registered object uid=%s", uid)

	got, err := ks.get(ctx, uid)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != secret {
		t.Fatalf("Get returned %q, want %q", got, secret)
	}

	if err := ks.destroy(ctx, uid); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	// A second Destroy (object gone) must be idempotent (ItemNotFound -> nil).
	if err := ks.destroy(ctx, uid); err != nil {
		t.Fatalf("Destroy of a gone object must be idempotent: %v", err)
	}
	t.Logf("round-trip + idempotent destroy OK against %s", os.Getenv("KMIP_ENDPOINT"))
}
