package driver

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/csi-addons/spec/lib/go/encryptionkeyrotation"
	"github.com/csi-addons/spec/lib/go/identity"

	"github.com/kindacoolhamster/bard-csi/internal/backend"
	"github.com/kindacoolhamster/bard-csi/internal/volumeid"
)

func rotateWith(t *testing.T, fb *fakeBackend) (*encryptionKeyRotationServer, string) {
	t.Helper()
	reg := backend.NewRegistry()
	reg.Register(fb)
	d := New(Options{Registry: reg, Dispatch: mustDisp(t), Mode: Mode{Node: true}})
	h := volumeid.Handle{Backend: "ceph-rbd", Instance: "east", Location: "replicapool", Name: "csi-vol-abc"}
	if err := h.Validate(); err != nil {
		t.Fatal(err)
	}
	return &encryptionKeyRotationServer{driver: d}, h.String()
}

// EncryptionKeyRotate dispatches to the owning backend with the volume path.
func TestEncryptionKeyRotateDispatches(t *testing.T) {
	var got string
	rs, id := rotateWith(t, &fakeBackend{rotateKey: func(h volumeid.Handle, path string) error {
		got = path
		return nil
	}})
	_, err := rs.EncryptionKeyRotate(context.Background(), &encryptionkeyrotation.EncryptionKeyRotateRequest{
		VolumeId: id, VolumePath: "/var/lib/kubelet/pods/p/volumes/x/mount",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "/var/lib/kubelet/pods/p/volumes/x/mount" {
		t.Fatalf("volume path not threaded, got %q", got)
	}
}

// Missing volume id is InvalidArgument; volume_path is optional (the csi-addons
// controller sends none, so the backend resolves the staged device from the id); a
// backend that can't rotate is Unimplemented; the node Identity advertises
// EncryptionKeyRotation only when a backend can rotate.
func TestEncryptionKeyRotateErrorsAndGating(t *testing.T) {
	rs, id := rotateWith(t, &fakeBackend{}) // rotateKey nil -> cap false
	if _, err := rs.EncryptionKeyRotate(context.Background(), &encryptionkeyrotation.EncryptionKeyRotateRequest{VolumePath: "/p"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("missing volume id should be InvalidArgument, got %v", err)
	}
	// No volume_path is fine -- it dispatches; this non-rotating backend yields Unimplemented.
	if _, err := rs.EncryptionKeyRotate(context.Background(), &encryptionkeyrotation.EncryptionKeyRotateRequest{VolumeId: id}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("a non-rotating backend should be Unimplemented (path optional), got %v", err)
	}
	if _, err := rs.EncryptionKeyRotate(context.Background(), &encryptionkeyrotation.EncryptionKeyRotateRequest{VolumeId: id, VolumePath: "/p"}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("a non-rotating backend should be Unimplemented, got %v", err)
	}

	hasCap := func(fb *fakeBackend) bool {
		reg := backend.NewRegistry()
		if fb != nil {
			reg.Register(fb)
		}
		d := New(Options{Registry: reg, Dispatch: mustDisp(t), Mode: Mode{Node: true}})
		resp, err := (&csiAddonsIdentityServer{driver: d}).GetCapabilities(context.Background(), &identity.GetCapabilitiesRequest{})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range resp.GetCapabilities() {
			if c.GetEncryptionKeyRotation().GetType() == identity.Capability_EncryptionKeyRotation_ENCRYPTIONKEYROTATION {
				return true
			}
		}
		return false
	}
	if hasCap(&fakeBackend{}) {
		t.Fatal("a non-rotating backend must not advertise EncryptionKeyRotation")
	}
	if !hasCap(&fakeBackend{rotateKey: func(volumeid.Handle, string) error { return nil }}) {
		t.Fatal("a rotating backend must advertise EncryptionKeyRotation")
	}
}
