package nfsplugin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// seedingRunner plays the export: when the backend mounts it (a temp dir), the
// runner drops a pre-existing snapshot provenance sidecar into it, simulating an
// archive already taken from another volume.
type seedingRunner struct {
	recRunner
	snapID  string
	sidecar string
}

func (r *seedingRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if name == "mount" && r.sidecar != "" {
		target := args[len(args)-1]
		_ = os.MkdirAll(filepath.Join(target, snapDir), 0o700)
		_ = os.WriteFile(filepath.Join(target, snapSrcFile(r.snapID)), []byte(r.sidecar), 0o600)
	}
	return r.recRunner.Run(ctx, name, args...)
}

// The archive name is derived from the CSI snapshot name: the same name against
// a DIFFERENT source must be AlreadyExists -- overwriting would silently destroy
// the other source's snapshot. A retry against the same source re-archives.
func TestNFSSnapshotSourceConflict(t *testing.T) {
	snapID := snapName("s1")

	run := &seedingRunner{snapID: snapID, sidecar: "bard-other"}
	b := snapBackend(run)
	if _, err := b.CreateSnapshot(context.Background(), &bardplugin.CreateSnapshotRequest{
		Name:         "s1",
		SourceVolume: bardplugin.VolumeRef{Instance: "east", Location: "/srv/nfs", Name: "bard-x"},
	}); err == nil || !strings.Contains(err.Error(), "different source") {
		t.Fatalf("mismatched source must be AlreadyExists, got %v", err)
	}
	if run.ran("tar", "czf") {
		t.Fatalf("the existing archive must NOT be overwritten; calls: %v", run.calls)
	}

	run = &seedingRunner{snapID: snapID, sidecar: "bard-x"}
	b = snapBackend(run)
	if _, err := b.CreateSnapshot(context.Background(), &bardplugin.CreateSnapshotRequest{
		Name:         "s1",
		SourceVolume: bardplugin.VolumeRef{Instance: "east", Location: "/srv/nfs", Name: "bard-x"},
	}); err != nil {
		t.Fatalf("matching-source retry must succeed, got %v", err)
	}
	if !run.ran("tar", "czf") {
		t.Fatalf("a matching retry re-archives; calls: %v", run.calls)
	}
}

// A subDir template must not be able to address the export's reserved paths.
func TestNFSReservedVolumeNames(t *testing.T) {
	b := snapBackend(&recRunner{})
	for _, tmpl := range []string{".snapshots", "archived-x", "../escape"} {
		_, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
			Name: "v", Instance: "east", CapacityBytes: 1 << 20,
			Parameters: map[string]string{paramSubDir: tmpl},
		})
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("subDir %q must be rejected, got %v", tmpl, err)
		}
	}
}
