package driver

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kubernetes-csi/csi-test/v5/pkg/sanity"

	"github.com/kindacoolhamster/bard-csi/internal/backend"
	"github.com/kindacoolhamster/bard-csi/internal/backend/plugin"
	"github.com/kindacoolhamster/bard-csi/internal/cephplugin"
	"github.com/kindacoolhamster/bard-csi/internal/dispatch"
	"github.com/kindacoolhamster/bard-csi/internal/fakerun"
	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

const cephType = "ceph-rbd"

// TestSanity runs the upstream kubernetes-csi sanity suite against the full
// driver, exercising the entire plugin path: driver -> plugin.Client -> unix
// socket -> bardplugin.Serve(cephplugin backed by the fake runner). No real Ceph.
func TestSanity(t *testing.T) {
	dir := t.TempDir()
	endpoint := "unix://" + filepath.Join(dir, "csi.sock")

	// Stand up the Ceph plugin (fake runner) behind a socket, like in production.
	pluginSock := filepath.Join(dir, "ceph.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = bardplugin.Serve(ctx, pluginSock, cephplugin.New(
			map[string]cephplugin.ClusterConfig{"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin"}},
			"", filepath.Join(dir, "ceph-state"), fakerun.New(),
		))
	}()
	waitForSocket(t, pluginSock)

	cl, err := plugin.Dial(ctx, cephType, pluginSock)
	if err != nil {
		t.Fatalf("plugin.Dial: %v", err)
	}
	registry := backend.NewRegistry()
	registry.Register(cl)

	disp, err := dispatch.New(dispatch.Config{
		Instances: map[string]map[string]string{cephType: {"east": "zone-east"}},
		Defaults:  map[string]string{cephType: "east"},
	})
	if err != nil {
		t.Fatalf("dispatch.New: %v", err)
	}

	d := New(Options{
		Version:  "test",
		NodeID:   "test-node",
		Zone:     "zone-east",
		Mode:     Mode{Controller: true, Node: true},
		Registry: registry,
		Dispatch: disp,
	})

	go func() {
		if err := d.Run(ctx, endpoint, ""); err != nil && ctx.Err() == nil {
			t.Errorf("driver.Run: %v", err)
		}
	}()
	waitForSocket(t, filepath.Join(dir, "csi.sock"))

	cfg := sanity.NewTestConfig()
	cfg.Address = endpoint
	cfg.TargetPath = filepath.Join(dir, "target")
	cfg.StagingPath = filepath.Join(dir, "staging")
	cfg.TestVolumeParameters = map[string]string{
		dispatch.BackendParamKey: cephType,
		"pool":                   "replicapool",
	}
	// The default path management is os.Mkdir/os.Remove, which is brittle for a
	// driver that creates the mount point itself (a single leftover dir
	// cascades into "file exists" for every later spec). Use idempotent
	// create/remove instead.
	mkdir := func(p string) (string, error) { return p, os.MkdirAll(p, 0o755) }
	cfg.CreateTargetDir = mkdir
	cfg.CreateStagingDir = mkdir
	cfg.RemoveTargetPath = os.RemoveAll
	cfg.RemoveStagingPath = os.RemoveAll
	sanity.Test(t, cfg)
}

func waitForSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("driver socket %s never appeared", path)
}
