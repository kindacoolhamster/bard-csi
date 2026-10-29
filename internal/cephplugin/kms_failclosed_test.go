package cephplugin

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kindacoolhamster/bard-csi/internal/cephenc"
)

// errMetaRead is a transient image-meta read failure: not "absent", so a provider
// must not treat it as a first stage.
var errMetaRead = errors.New("rbd: getting metadata failed: (110) Connection timed out")

// A provider that stores its key reference in image metadata must FAIL CLOSED when
// the read fails. Minting on a misread would overwrite the stored key (or repoint
// the KMIP UID), and the volume's existing LUKS header could never be opened again.
func TestKMSPassphraseFailsClosedOnMetaReadError(t *testing.T) {
	ctx := context.Background()
	const spec = "replicapool/img"

	keyDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(keyDir, "east"), []byte("instance-master-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	smRun := &metaRunner{meta: map[string]string{}}
	sm := New(map[string]ClusterConfig{"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin"}}, "", "", smRun).
		WithEncryption(keyDir).
		WithKMS(map[string]KMSConfig{"k8s": {Type: "secrets-metadata"}})

	fk := newFakeKMS()
	srv := httptest.NewServer(fk.handler())
	defer srv.Close()
	aws, awsRun := awsKMSBackend(t, srv.URL)

	f := startFakeKMIP(t)
	kmip, kmipRun := kmipBackend(t, f.addr)

	cases := []struct {
		name  string
		b     *Backend
		run   *metaRunner
		kmsID string
		key   string
	}{
		{"secrets-metadata", sm, smRun, "k8s", cephenc.MetaWrappedDEK},
		{"aws-kms", aws, awsRun, "aws", cephenc.MetaWrappedDEK},
		{"kmip", kmip, kmipRun, "kmip", cephenc.MetaKMIPUID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p1, err := tc.b.encryptionPassphrase(ctx, "east", tc.kmsID, spec, nil)
			if err != nil {
				t.Fatalf("first stage: %v", err)
			}
			stored := tc.run.meta[spec+"|"+tc.key]
			if stored == "" {
				t.Fatalf("first stage stored nothing under %s; meta=%v", tc.key, tc.run.meta)
			}

			tc.run.getErr = errMetaRead
			if _, err := tc.b.encryptionPassphrase(ctx, "east", tc.kmsID, spec, nil); err == nil {
				t.Fatal("a failed metadata read must fail the stage, not mint a new key")
			}
			if got := tc.run.meta[spec+"|"+tc.key]; got != stored {
				t.Fatalf("stored key reference was overwritten on a read error: %q -> %q", stored, got)
			}

			tc.run.getErr = nil
			p2, err := tc.b.encryptionPassphrase(ctx, "east", tc.kmsID, spec, nil)
			if err != nil {
				t.Fatal(err)
			}
			if p1 != p2 {
				t.Fatal("after the read recovers, the volume must resolve its ORIGINAL key")
			}
		})
	}
	if fk.gen != 1 {
		t.Fatalf("aws-kms: GenerateDataKey must run exactly once, got %d", fk.gen)
	}
	if f.count() != 1 {
		t.Fatalf("kmip: a read error must not register a new HSM object, got %d objects", f.count())
	}
}

// KMIP deleteKey must not report success on a failed UID read: DeleteVolume would
// then rbd rm the image, taking the recorded UID with it and orphaning the object.
func TestKMIPDeleteKeyFailsClosedOnMetaReadError(t *testing.T) {
	f := startFakeKMIP(t)
	b, run := kmipBackend(t, f.addr)
	ctx := context.Background()
	if _, err := b.encryptionPassphrase(ctx, "east", "kmip", "replicapool/img", nil); err != nil {
		t.Fatal(err)
	}
	run.getErr = errMetaRead
	if err := b.kms.DeleteKey(ctx, nil, "east", "kmip", "replicapool/img"); err == nil {
		t.Fatal("deleteKey must surface a metadata read error")
	}
	if f.count() != 1 {
		t.Fatalf("the HSM object must survive for a retry, got %d objects", f.count())
	}
}

// inheritEncryption must fail on a failed source read rather than give the clone an
// empty descriptor: the clone's first stage would then mint a key that can never
// open the LUKS header it copied from the source.
func TestInheritEncryptionFailsClosedOnSourceReadError(t *testing.T) {
	run := &metaRunner{meta: map[string]string{
		"replicapool/src|" + imgMetaKMSID:      "k8s",
		"replicapool/src|" + imgMetaWrappedDEK: "wrapped",
	}}
	b := New(map[string]ClusterConfig{"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin"}}, "", "", run).
		WithKMS(map[string]KMSConfig{"k8s": {Type: "secrets-metadata"}})
	run.getErr = errMetaRead
	if _, err := b.inheritEncryption(context.Background(), nil, "replicapool/src", "replicapool/clone", "east", nil); err == nil {
		t.Fatal("inheritEncryption must surface a source metadata read error")
	}
	for k := range run.meta {
		if strings.HasPrefix(k, "replicapool/clone|") {
			t.Fatalf("no descriptor may be written to the clone on a failed read; got %s", k)
		}
	}
}
