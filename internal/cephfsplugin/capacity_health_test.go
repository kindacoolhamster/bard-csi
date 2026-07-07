package cephfsplugin

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// dfHealthRunner models `ceph df` and `ceph fs subvolume info`, with a toggle to
// make subvolume info report a missing subvolume (the abnormal health path).
type dfHealthRunner struct {
	missing bool
}

func (r *dfHealthRunner) Run(_ context.Context, _ string, args ...string) (string, error) {
	a := strings.Join(args, " ")
	switch {
	case strings.Contains(a, "df"):
		return `{"stats":{"total_avail_bytes":4294967296}}`, nil
	case strings.Contains(a, "subvolume info"):
		if r.missing {
			return "", errors.New("Error ENOENT: subvolume 'bard-x' does not exist")
		}
		return `{"bytes_quota":1073741824}`, nil
	}
	return "", nil
}

func chBackend(run Runner) *Backend {
	return New(map[string]ClusterConfig{"east": {Monitors: []string{"10.0.0.10:6789"}, FSName: "cephfs", UserID: "admin"}}, "", run)
}

func TestCephFSGetCapacity(t *testing.T) {
	// The fake's `fs get` returns nothing parseable, so capacity falls back to
	// the cluster-wide total (the pre-`fs get` behaviour, and the path taken
	// when the user's caps don't allow `fs get`).
	b := chBackend(&dfHealthRunner{})
	resp, err := b.GetCapacity(context.Background(), &bardplugin.GetCapacityRequest{Instance: "east"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.AvailableBytes != 4294967296 {
		t.Fatalf("available bytes = %d, want 4294967296", resp.AvailableBytes)
	}
}

// fsScopedDfRunner models a multi-pool cluster: the filesystem owns data pool 3
// only, so capacity must report that pool's max_avail -- not the cluster-wide
// total, and not the metadata pool or a foreign pool.
type fsScopedDfRunner struct{}

func (fsScopedDfRunner) Run(_ context.Context, _ string, args ...string) (string, error) {
	a := strings.Join(args, " ")
	switch {
	case strings.Contains(a, "fs get"):
		return `{"mdsmap":{"data_pools":[3],"metadata_pool":2}}`, nil
	case strings.Contains(a, "df"):
		return `{"stats":{"total_avail_bytes":4294967296},"pools":[
			{"name":"meta","id":2,"stats":{"max_avail":104857600}},
			{"name":"bardfs-data","id":3,"stats":{"max_avail":1073741824}},
			{"name":"other","id":9,"stats":{"max_avail":2147483648}}]}`, nil
	}
	return "", nil
}

func TestCephFSGetCapacityScopedToDataPools(t *testing.T) {
	b := chBackend(fsScopedDfRunner{})
	resp, err := b.GetCapacity(context.Background(), &bardplugin.GetCapacityRequest{Instance: "east"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.AvailableBytes != 1073741824 {
		t.Fatalf("available bytes = %d, want the fs data pool's 1073741824", resp.AvailableBytes)
	}
}

func TestCephFSGetVolumeHealth(t *testing.T) {
	vol := bardplugin.VolumeRef{Instance: "east", Location: "cephfs", Name: "bard-x"}

	healthy := chBackend(&dfHealthRunner{})
	resp, err := healthy.GetVolumeHealth(context.Background(), &bardplugin.GetVolumeHealthRequest{Volume: vol})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Abnormal {
		t.Fatalf("present subvolume should be healthy: %+v", resp)
	}

	gone := chBackend(&dfHealthRunner{missing: true})
	resp, err = gone.GetVolumeHealth(context.Background(), &bardplugin.GetVolumeHealthRequest{Volume: vol})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Abnormal {
		t.Fatalf("missing subvolume should be abnormal: %+v", resp)
	}
}

// The plugin must satisfy the optional reporter interfaces so Bard advertises the
// capabilities (the server auto-detects them by type assertion).
func TestCephFSImplementsOptionalReporters(t *testing.T) {
	var b any = New(nil, "", &dfHealthRunner{})
	if _, ok := b.(bardplugin.CapacityReporter); !ok {
		t.Fatal("cephfs backend must implement CapacityReporter")
	}
	if _, ok := b.(bardplugin.HealthReporter); !ok {
		t.Fatal("cephfs backend must implement HealthReporter")
	}
}
