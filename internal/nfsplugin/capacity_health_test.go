package nfsplugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// exportRunner models a no-op mount/umount of the export. On mount it optionally
// pre-creates volume subdirectories under the (real) temp mount target, so the
// health check can see a present-vs-missing subdir.
type exportRunner struct {
	present []string // volume dir names that exist on the export
}

func (r *exportRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	if name == "mount" {
		target := args[len(args)-1]
		for _, v := range r.present {
			_ = os.MkdirAll(filepath.Join(target, v), 0o755)
		}
	}
	return "", nil
}

func chBackend(run Runner) *Backend {
	return New(map[string]InstanceConfig{"east": {Server: "10.0.0.9", Export: "/srv/nfs"}}, run)
}

func TestNFSGetCapacity(t *testing.T) {
	b := chBackend(&exportRunner{})
	resp, err := b.GetCapacity(context.Background(), &bardplugin.GetCapacityRequest{Instance: "east"})
	if err != nil {
		t.Fatal(err)
	}
	// statfs of the real temp mount target reports the host fs -- value is
	// non-deterministic, but a working backing filesystem has bytes available.
	if resp.AvailableBytes <= 0 {
		t.Fatalf("available bytes = %d, want > 0", resp.AvailableBytes)
	}
}

func TestNFSGetVolumeHealth(t *testing.T) {
	vol := bardplugin.VolumeRef{Instance: "east", Location: "/srv/nfs", Name: "bard-x"}

	healthy := chBackend(&exportRunner{present: []string{"bard-x"}})
	resp, err := healthy.GetVolumeHealth(context.Background(), &bardplugin.GetVolumeHealthRequest{Volume: vol})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Abnormal {
		t.Fatalf("present subdirectory should be healthy: %+v", resp)
	}

	gone := chBackend(&exportRunner{}) // no subdirs created on mount
	resp, err = gone.GetVolumeHealth(context.Background(), &bardplugin.GetVolumeHealthRequest{Volume: vol})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Abnormal {
		t.Fatalf("missing subdirectory should be abnormal: %+v", resp)
	}
}

func TestNFSImplementsOptionalReporters(t *testing.T) {
	var b any = New(nil, &exportRunner{})
	if _, ok := b.(bardplugin.CapacityReporter); !ok {
		t.Fatal("nfs backend must implement CapacityReporter")
	}
	if _, ok := b.(bardplugin.HealthReporter); !ok {
		t.Fatal("nfs backend must implement HealthReporter")
	}
}
