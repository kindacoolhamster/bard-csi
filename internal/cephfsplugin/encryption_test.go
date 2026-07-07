package cephfsplugin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kindacoolhamster/bard-csi/internal/cephenc"
	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// encMetaRunner models the `ceph fs subvolume` control plane plus subvolume metadata
// over an in-memory map, so the encryption descriptor + KMS metadata round-trip like
// real subvolume metadata. (The fscrypt ioctls in NodeStage are live-proven only, like
// the RBD plugin's, so the node fscrypt path is not exercised here.)
type encMetaRunner struct {
	calls [][]string
	meta  map[string]string // "fs/sub|key" -> value
}

func newEncMetaRunner() *encMetaRunner {
	return &encMetaRunner{meta: map[string]string{}}
}

func (r *encMetaRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	a := strings.Join(args, " ")
	switch {
	case strings.Contains(a, "subvolume metadata set"):
		i := indexOfArg(args, "metadata") // ... metadata set <fs> <sub> <key> <val>
		fs, sub, key, val := args[i+2], args[i+3], args[i+4], args[i+5]
		r.meta[fs+"/"+sub+"|"+key] = val
		return "", nil
	case strings.Contains(a, "subvolume metadata get"):
		i := indexOfArg(args, "metadata")
		fs, sub, key := args[i+2], args[i+3], args[i+4]
		if v, ok := r.meta[fs+"/"+sub+"|"+key]; ok {
			return v + "\n", nil
		}
		return "", errors.New("Error ENOENT: key does not exist")
	case strings.Contains(a, "clone status"):
		return `{"status":{"state":"complete"}}`, nil
	case strings.Contains(a, "subvolume getpath"):
		return "/volumes/_nogroup/sub/uuid\n", nil
	}
	return "", nil
}

func (r *encMetaRunner) ran(parts ...string) bool {
	want := strings.Join(parts, " ")
	for _, c := range r.calls {
		if strings.Contains(strings.Join(c, " "), want) {
			return true
		}
	}
	return false
}

func indexOfArg(ss []string, want string) int {
	for i, s := range ss {
		if s == want {
			return i
		}
	}
	return -1
}

func encBackend(t *testing.T, mounter string) (*Backend, *encMetaRunner) {
	t.Helper()
	keyDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(keyDir, "east"), []byte("instance-master-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := newEncMetaRunner()
	b := New(map[string]ClusterConfig{"east": {Monitors: []string{"10.0.0.10:6789"}, FSName: "cephfs", UserID: "admin", Mounter: mounter}}, "", run).
		WithEncryption(keyDir).
		WithKMS(map[string]KMSConfig{"k8s": {Type: "secrets-metadata"}})
	return b, run
}

// CephFS rejects encryption configs it cannot honour: block (LUKS) mode, a non-kernel
// mounter, and shallow backing-snapshot volumes. A kernel-mounted file/default config
// is accepted.
func TestValidateEncryptionParams(t *testing.T) {
	b, _ := encBackend(t, "") // kernel (default)
	kernel := ClusterConfig{Mounter: ""}
	cases := []struct {
		name    string
		cc      ClusterConfig
		params  map[string]string
		wantErr string
	}{
		{"unencrypted ok", kernel, map[string]string{}, ""},
		{"kernel file ok", kernel, map[string]string{"encrypted": "true", "encryptionType": "file"}, ""},
		{"kernel default ok", kernel, map[string]string{"encrypted": "true"}, ""},
		{"block rejected", kernel, map[string]string{"encrypted": "true", "encryptionType": "block"}, "filesystem-level"},
		{"fuse rejected", ClusterConfig{Mounter: "fuse"}, map[string]string{"encrypted": "true"}, "kernel mounter"},
		{"nfs rejected", ClusterConfig{Mounter: "nfs"}, map[string]string{"encrypted": "true"}, "kernel mounter"},
		{"shallow rejected", kernel, map[string]string{"encrypted": "true", "backingSnapshot": "true"}, "cannot be encrypted"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := b.validateEncryptionParams(c.cc, c.params, false)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("want error containing %q, got %v", c.wantErr, err)
			}
		})
	}
}

// CreateVolume of an encrypted volume records the KMS id on the subvolume and carries
// the encryption decision (encrypted + fscrypt type + kms id) to the node.
func TestCreateVolumeEncryptedRecordsAndCarries(t *testing.T) {
	b, run := encBackend(t, "")
	resp, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "v", Instance: "east", CapacityBytes: 1 << 30,
		Parameters: map[string]string{"encrypted": "true", "encryptionKMSID": "k8s"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Context["encrypted"] != "true" || resp.Context["encryptionType"] != "file" || resp.Context["encryptionKMSID"] != "k8s" {
		t.Fatalf("volume context must carry the encryption decision, got %v", resp.Context)
	}
	if got := run.meta["cephfs/"+resp.Name+"|"+cephenc.MetaKMSID]; got != "k8s" {
		t.Fatalf("KMS id must be recorded on the subvolume, got %q", got)
	}
}

// A fresh derived-provider volume (no encryptionKMSID) carries the marker but records
// no KMS id (the derived provider stores nothing) and advertises no inherited keyID.
func TestCreateVolumeEncryptedDerivedNoRecord(t *testing.T) {
	b, run := encBackend(t, "")
	resp, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "v", Instance: "east", CapacityBytes: 1 << 30,
		Parameters: map[string]string{"encrypted": "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Context["encrypted"] != "true" || resp.Context["encryptionType"] != "file" {
		t.Fatalf("context: %v", resp.Context)
	}
	if _, ok := resp.Context["encryptionKMSID"]; ok {
		t.Fatalf("derived provider must not set a KMS id, got %v", resp.Context)
	}
	if _, ok := resp.Context[cephenc.CtxKeyID]; ok {
		t.Fatalf("a fresh volume must advertise no inherited keyID, got %v", resp.Context)
	}
	if got := run.meta["cephfs/"+resp.Name+"|"+cephenc.MetaKMSID]; got != "" {
		t.Fatalf("derived provider must record no KMS id, got %q", got)
	}
}

// An encrypted CephFS volume cannot be restored from a snapshot or cloned: CephFS
// subvolume clone does not preserve the fscrypt context (verified live -- the clone
// copies the ciphertext as opaque data and the encrypted tree is unmountable), so
// CreateVolume rejects the combination fail-fast rather than let NodeStage hang.
func TestEncryptedCloneRejected(t *testing.T) {
	b, _ := encBackend(t, "")
	ctx := context.Background()
	for _, c := range []struct {
		name string
		req  *bardplugin.CreateVolumeRequest
	}{
		{"from snapshot", &bardplugin.CreateVolumeRequest{
			Name: "restored", Instance: "east", Parameters: map[string]string{"encrypted": "true"},
			SourceSnapshot: &bardplugin.VolumeRef{Instance: "east", Location: "cephfs", Name: "bard-src@snap-x"}}},
		{"from volume", &bardplugin.CreateVolumeRequest{
			Name: "cloned", Instance: "east", Parameters: map[string]string{"encrypted": "true"},
			SourceVolume: &bardplugin.VolumeRef{Instance: "east", Location: "cephfs", Name: "bard-src"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := b.CreateVolume(ctx, c.req)
			if err == nil || !strings.Contains(err.Error(), "cannot be restored from a snapshot or cloned") {
				t.Fatalf("expected an encrypted-clone rejection, got %v", err)
			}
		})
	}

	// An UNencrypted clone is still fine (the restriction is encryption-specific).
	if _, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: "plainclone", Instance: "east",
		SourceVolume: &bardplugin.VolumeRef{Instance: "east", Location: "cephfs", Name: "bard-src"},
	}); err != nil {
		t.Fatalf("an unencrypted clone must still succeed: %v", err)
	}
}

// MetaGet/MetaSet round-trip a subvolume-metadata value and report "" for an absent key
// (the cephenc.Host store backing the KMS providers).
func TestHostSubvolumeMetadataRoundTrip(t *testing.T) {
	b, _ := encBackend(t, "")
	ctx := context.Background()
	if got := b.MetaGet(ctx, nil, "cephfs/sub", "bard.k"); got != "" {
		t.Fatalf("absent key must read empty, got %q", got)
	}
	if err := b.MetaSet(ctx, nil, "cephfs/sub", "bard.k", "v1"); err != nil {
		t.Fatal(err)
	}
	if got := b.MetaGet(ctx, nil, "cephfs/sub", "bard.k"); got != "v1" {
		t.Fatalf("round-trip failed, got %q", got)
	}
	if err := b.MetaSet(ctx, nil, "no-slash-spec", "k", "v"); err == nil {
		t.Fatal("a malformed spec must error on set")
	}
}
