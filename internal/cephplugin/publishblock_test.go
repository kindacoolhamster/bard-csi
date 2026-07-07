package cephplugin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

type blockRunner struct{ calls [][]string }

func (r *blockRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	return "", nil
}
func (r *blockRunner) ran(parts ...string) bool {
	want := strings.Join(parts, " ")
	for _, c := range r.calls {
		if strings.Join(c, " ") == want {
			return true
		}
	}
	return false
}

func TestPublishBlock(t *testing.T) {
	dir := t.TempDir()
	run := &blockRunner{}
	b := New(map[string]ClusterConfig{"east": {}}, "", filepath.Join(dir, "state"), run)

	staging := filepath.Join(dir, "staging")
	if err := b.recordDevice(staging, deviceRecord{Device: "/dev/nbd0"}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "pod", "vol") // parent dir does not exist yet

	err := b.NodePublish(context.Background(), &bardplugin.NodePublishRequest{
		Volume:      bardplugin.VolumeRef{Instance: "east"},
		StagingPath: staging,
		TargetPath:  target,
		Block:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("block target file not created: %v", err)
	}
	if !run.ran("mount", "--bind", "/dev/nbd0", target) {
		t.Fatalf("expected the mapped device to be bind-mounted; calls: %v", run.calls)
	}
}

// publishBlock must NOT attempt a read-only bind remount: it is a no-op for a
// bind-mounted block device. Read-only for a block volume is enforced at the map
// (rbd --read-only, see TestReadOnlyAccessMapsReadOnly), so the publish path just
// binds the device and leaves read-only to the map / kubelet device cgroup.
func TestPublishBlockNoReadonlyRemount(t *testing.T) {
	dir := t.TempDir()
	run := &blockRunner{}
	b := New(map[string]ClusterConfig{"east": {}}, "", filepath.Join(dir, "state"), run)
	staging := filepath.Join(dir, "staging")
	if err := b.recordDevice(staging, deviceRecord{Device: "/dev/rbd0"}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "pod", "vol")
	if err := b.NodePublish(context.Background(), &bardplugin.NodePublishRequest{
		Volume:      bardplugin.VolumeRef{Instance: "east"},
		StagingPath: staging,
		TargetPath:  target,
		Block:       true,
		Readonly:    true,
	}); err != nil {
		t.Fatal(err)
	}
	if !run.ran("mount", "--bind", "/dev/rbd0", target) {
		t.Fatalf("expected a plain device bind mount; calls: %v", run.calls)
	}
	if run.ran("mount", "-o", "remount,ro,bind", "/dev/rbd0", target) {
		t.Fatalf("read-only bind remount is ineffective for block and must not be used; calls: %v", run.calls)
	}
}

func TestPublishBlockNoDevice(t *testing.T) {
	dir := t.TempDir()
	b := New(map[string]ClusterConfig{"east": {}}, "", filepath.Join(dir, "state"), &blockRunner{})
	err := b.NodePublish(context.Background(), &bardplugin.NodePublishRequest{
		StagingPath: filepath.Join(dir, "staging"), // nothing recorded
		TargetPath:  filepath.Join(dir, "vol"),
		Block:       true,
	})
	if err == nil {
		t.Fatal("expected error when no device was recorded for the staging path")
	}
}
