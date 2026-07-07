package cephplugin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// rotateRunner models the node-side rotation environment: findmnt resolves the
// published path to a dm-crypt mapper, cryptsetup status reports its backing device,
// and rbd image-meta get/set round-trips the KMS metadata in memory.
type rotateRunner struct {
	meta   map[string]string
	calls  [][]string
	mapper string
}

func (r *rotateRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	switch name {
	case "findmnt":
		return "/dev/mapper/" + r.mapper + "\n", nil
	case "cryptsetup":
		if has(args, "status") {
			return "  type:    LUKS2\n  device:  /dev/rbd0\n", nil
		}
		return "", nil // isLuks / luksAddKey / luksRemoveKey
	case "rbd":
		if has(args, "image-meta") {
			i := indexOf(args, "image-meta")
			op, spec, key := args[i+1], args[i+2], args[i+3]
			mk := spec + "|" + key
			switch op {
			case "set":
				r.meta[mk] = args[i+4]
				return "", nil
			case "get":
				if v, ok := r.meta[mk]; ok {
					return v + "\n", nil
				}
				return "", errors.New("rbd: image-meta get: (2) No such file or directory")
			}
		}
	}
	return "", nil
}

func (r *rotateRunner) cryptCalls() [][]string {
	var out [][]string
	for _, c := range r.calls {
		if c[0] == "cryptsetup" && (has(c, "luksAddKey") || has(c, "luksRemoveKey")) {
			out = append(out, c)
		}
	}
	return out
}

func rotateBackend(t *testing.T, run Runner) *Backend {
	t.Helper()
	keyDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(keyDir, "east"), []byte("instance-master-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	return New(map[string]ClusterConfig{"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin"}}, "", "", run).
		WithEncryption(keyDir).
		WithKMS(map[string]KMSConfig{"k8s": {Type: "secrets-metadata"}})
}

// RotateEncryptionKey adds a new LUKS keyslot, persists the new key, then removes the
// old slot -- and the stored key changes so the old passphrase no longer resolves.
func TestRotateEncryptionKeyOrchestration(t *testing.T) {
	run := &rotateRunner{meta: map[string]string{}, mapper: luksMapperPrefix + "abc123"}
	b := rotateBackend(t, run)
	ctx := context.Background()
	spec := "replicapool/csi-vol-x"
	// The volume is a secrets-metadata-encrypted image.
	run.meta[spec+"|"+imgMetaKMSID] = "k8s"

	// Establish the original key (mints + stores the first DEK).
	conn, cleanup, _ := b.connArgs(func() ClusterConfig { cc, _ := b.cluster("east"); return cc }(), "east", nil)
	defer cleanup()
	oldDEK := run.meta[spec+"|"+imgMetaWrappedDEK]
	_, _ = b.encryptionPassphraseFor(ctx, conn, "east", "k8s", spec, spec, nil) // first-stage stores a DEK
	oldDEK = run.meta[spec+"|"+imgMetaWrappedDEK]
	if oldDEK == "" {
		t.Fatal("setup: expected a stored DEK after first passphrase")
	}

	err := b.RotateEncryptionKey(ctx, &bardplugin.RotateEncryptionKeyRequest{
		Volume:     bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "csi-vol-x"},
		VolumePath: "/var/lib/kubelet/pods/p/volumes/x/mount",
	})
	if err != nil {
		t.Fatal(err)
	}
	cc := run.cryptCalls()
	if len(cc) != 2 || !has(cc[0], "luksAddKey") || !has(cc[1], "luksRemoveKey") {
		t.Fatalf("expected luksAddKey then luksRemoveKey, got %v", cc)
	}
	// luksAddKey/RemoveKey target the backing device, not the mapper.
	if !has(cc[0], "/dev/rbd0") {
		t.Errorf("luksAddKey should target the backing device /dev/rbd0: %v", cc[0])
	}
	if newDEK := run.meta[spec+"|"+imgMetaWrappedDEK]; newDEK == "" || newDEK == oldDEK {
		t.Fatalf("rotation should overwrite the stored DEK (old=%q new=%q)", oldDEK, run.meta[spec+"|"+imgMetaWrappedDEK])
	}
}

// With no published path (the csi-addons controller sends none), rotation locates
// the volume's staged device record by handle and rotates the recorded backing
// device -- same luksAddKey-then-luksRemoveKey, same DEK change.
func TestRotateEncryptionKeyNoVolumePath(t *testing.T) {
	run := &rotateRunner{meta: map[string]string{}, mapper: luksMapperPrefix + "abc123"}
	keyDir, stateDir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(keyDir, "east"), []byte("instance-master-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := New(map[string]ClusterConfig{"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin"}}, "", stateDir, run).
		WithEncryption(keyDir).WithKMS(map[string]KMSConfig{"k8s": {Type: "secrets-metadata"}})
	ctx := context.Background()
	spec := "replicapool/csi-vol-x"
	run.meta[spec+"|"+imgMetaKMSID] = "k8s"

	// The volume is staged: a device record maps its handle to the rbd backing device.
	if err := b.recordDevice("/var/lib/kubelet/stage/x", deviceRecord{
		Device: "/dev/rbd0", Instance: "east", Pool: "replicapool", Image: "csi-vol-x",
	}); err != nil {
		t.Fatal(err)
	}
	conn, cleanup, _ := b.connArgs(func() ClusterConfig { cc, _ := b.cluster("east"); return cc }(), "east", nil)
	defer cleanup()
	_, _ = b.encryptionPassphraseFor(ctx, conn, "east", "k8s", spec, spec, nil)
	oldDEK := run.meta[spec+"|"+imgMetaWrappedDEK]

	// No VolumePath -- resolved from the device record.
	if err := b.RotateEncryptionKey(ctx, &bardplugin.RotateEncryptionKeyRequest{
		Volume: bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "csi-vol-x"},
	}); err != nil {
		t.Fatal(err)
	}
	cc := run.cryptCalls()
	if len(cc) != 2 || !has(cc[0], "luksAddKey") || !has(cc[1], "luksRemoveKey") || !has(cc[0], "/dev/rbd0") {
		t.Fatalf("expected luksAddKey then luksRemoveKey on /dev/rbd0, got %v", cc)
	}
	if newDEK := run.meta[spec+"|"+imgMetaWrappedDEK]; newDEK == "" || newDEK == oldDEK {
		t.Fatalf("rotation should overwrite the stored DEK (old=%q new=%q)", oldDEK, run.meta[spec+"|"+imgMetaWrappedDEK])
	}
}

// With no published path and no staged record, rotation fails NotFound -- it is a
// node op that needs the volume mounted here.
func TestRotateEncryptionKeyNoVolumePathNotStaged(t *testing.T) {
	run := &rotateRunner{meta: map[string]string{}, mapper: luksMapperPrefix + "abc123"}
	b := rotateBackend(t, run) // stateDir "" -> no records
	err := b.RotateEncryptionKey(context.Background(), &bardplugin.RotateEncryptionKeyRequest{
		Volume: bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "csi-vol-x"},
	})
	if err == nil || !strings.Contains(err.Error(), "not staged") {
		t.Fatalf("unstaged volume with no path should be rejected, got %v", err)
	}
}

// A non-LUKS volume (findmnt resolves to a plain device) is rejected: key rotation
// applies to block (LUKS) encryption.
func TestRotateEncryptionKeyNonLuks(t *testing.T) {
	b := rotateBackend(t, &plainDevRunner{rotateRunner{meta: map[string]string{}}})
	err := b.RotateEncryptionKey(context.Background(), &bardplugin.RotateEncryptionKeyRequest{
		Volume:     bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "csi-vol-x"},
		VolumePath: "/var/lib/kubelet/pods/p/volumes/x/mount",
	})
	if err == nil || !strings.Contains(err.Error(), "not a LUKS") {
		t.Fatalf("non-LUKS volume should be rejected, got %v", err)
	}
}

// plainDevRunner makes findmnt resolve to a plain (non-dm-crypt) device.
type plainDevRunner struct{ rotateRunner }

func (r *plainDevRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if name == "findmnt" {
		return "/dev/rbd0\n", nil
	}
	return r.rotateRunner.Run(ctx, name, args...)
}
