package cephenc

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// fakeHost is an in-memory Host for unit tests: a real master-key dir, a no-op Ceph
// connection, and a per-volume metadata store backed by a map (modelling rbd image-meta
// / cephfs subvolume metadata).
type fakeHost struct {
	keyDir string
	meta   map[string]string // "spec|key" -> value
}

func (h *fakeHost) MasterKeyDir() string { return h.keyDir }

func (h *fakeHost) ConnFor(conn []string, _ string, _ map[string]string) ([]string, func(), error) {
	return conn, func() {}, nil
}

func (h *fakeHost) MetaGet(_ context.Context, _ []string, spec, key string) string {
	return h.meta[spec+"|"+key]
}

func (h *fakeHost) MetaSet(_ context.Context, _ []string, spec, key, value string) error {
	h.meta[spec+"|"+key] = value
	return nil
}

// newTestHost returns a fakeHost with a per-instance master key written for "east".
func newTestHost(t *testing.T) *fakeHost {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "east"), []byte("instance-master-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &fakeHost{keyDir: dir, meta: map[string]string{}}
}
