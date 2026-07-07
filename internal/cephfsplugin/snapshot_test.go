package cephfsplugin

import (
	"context"
	"strings"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// cephRunner models the `ceph fs subvolume/snapshot/clone` commands the control
// plane shells out to, and records calls for assertions.
type cephRunner struct {
	calls    [][]string
	notFound bool // snapshot rm reports not-found (idempotent delete path)
}

func (r *cephRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	a := strings.Join(args, " ")
	switch {
	case strings.Contains(a, "clone status"):
		return `{"status":{"state":"complete"}}`, nil
	case strings.Contains(a, "subvolume info"):
		return `{"bytes_quota": 1073741824}`, nil
	case strings.Contains(a, "subvolume getpath"):
		return "/volumes/_nogroup/sub/uuid\n", nil
	}
	return "", nil
}

func (r *cephRunner) ran(parts ...string) bool {
	want := strings.Join(parts, " ")
	for _, c := range r.calls {
		if strings.Contains(strings.Join(c, " "), want) {
			return true
		}
	}
	return false
}

func snapBackend() *Backend {
	return New(map[string]ClusterConfig{"east": {Monitors: []string{"10.0.0.10:6789"}, FSName: "cephfs", UserID: "admin"}}, "", &cephRunner{})
}

func TestCephFSCreateSnapshot(t *testing.T) {
	b := snapBackend()
	run := b.run.(*cephRunner)
	ctx := context.Background()
	src := bardplugin.VolumeRef{Instance: "east", Location: "cephfs", Name: "bard-x"}

	resp, err := b.CreateSnapshot(ctx, &bardplugin.CreateSnapshotRequest{Name: "snap1", SourceVolume: src})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resp.Name, "bard-x@snap-") {
		t.Fatalf("snapshot handle should encode subvol@snap, got %q", resp.Name)
	}
	if !resp.ReadyToUse || resp.SizeBytes != 1073741824 {
		t.Fatalf("ready=%v size=%d", resp.ReadyToUse, resp.SizeBytes)
	}
	if !run.ran("fs", "subvolume", "snapshot", "create", "cephfs", "bard-x") {
		t.Fatalf("expected a snapshot create; calls: %v", run.calls)
	}

	// Idempotent against the same source; AlreadyExists for a different source.
	if _, err := b.CreateSnapshot(ctx, &bardplugin.CreateSnapshotRequest{Name: "snap1", SourceVolume: src}); err != nil {
		t.Fatalf("same-source retry should be idempotent: %v", err)
	}
	_, err = b.CreateSnapshot(ctx, &bardplugin.CreateSnapshotRequest{Name: "snap1", SourceVolume: bardplugin.VolumeRef{Instance: "east", Location: "cephfs", Name: "bard-other"}})
	var se *bardplugin.StatusError
	if err == nil || !(strings.Contains(err.Error(), "different source")) {
		t.Fatalf("expected AlreadyExists for a different source, got %v", err)
	}
	_ = se
}

func TestCephFSDeleteSnapshot(t *testing.T) {
	b := snapBackend()
	run := b.run.(*cephRunner)
	err := b.DeleteSnapshot(context.Background(), &bardplugin.DeleteSnapshotRequest{
		Snapshot: bardplugin.VolumeRef{Instance: "east", Location: "cephfs", Name: "bard-x@snap-abc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !run.ran("fs", "subvolume", "snapshot", "rm", "cephfs", "bard-x", "snap-abc", "--force") {
		t.Fatalf("expected a forced snapshot rm; calls: %v", run.calls)
	}
}

func TestCephFSCloneFromVolume(t *testing.T) {
	b := snapBackend()
	run := b.run.(*cephRunner)
	resp, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name:         "cloned",
		Instance:     "east",
		SourceVolume: &bardplugin.VolumeRef{Instance: "east", Location: "cephfs", Name: "bard-src"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// snapshot the source, clone it to the target, poll status, then drop the temp snap.
	if !run.ran("fs", "subvolume", "snapshot", "create", "cephfs", "bard-src", "clonetmp-"+resp.Name) {
		t.Fatalf("expected a temp snapshot of the source; calls: %v", run.calls)
	}
	if !run.ran("fs", "subvolume", "snapshot", "clone", "cephfs", "bard-src", "clonetmp-"+resp.Name, resp.Name) {
		t.Fatalf("expected a clone of the temp snapshot; calls: %v", run.calls)
	}
	if !run.ran("fs", "clone", "status", "cephfs", resp.Name) {
		t.Fatalf("expected a clone status poll; calls: %v", run.calls)
	}
	if !run.ran("fs", "subvolume", "snapshot", "rm", "cephfs", "bard-src", "clonetmp-"+resp.Name, "--force") {
		t.Fatalf("expected the temp snapshot to be removed; calls: %v", run.calls)
	}
	if run.ran("fs", "subvolume", "create", "cephfs", resp.Name) {
		t.Fatalf("clone path must not also plain-create the subvolume; calls: %v", run.calls)
	}
}

func TestCephFSCloneFromSnapshot(t *testing.T) {
	b := snapBackend()
	run := b.run.(*cephRunner)
	resp, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name:     "restored",
		Instance: "east",
		SourceSnapshot: &bardplugin.VolumeRef{
			Instance: "east", Location: "cephfs", Name: "bard-x@snap-abc",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !run.ran("fs", "subvolume", "snapshot", "clone", "cephfs", "bard-x", "snap-abc") {
		t.Fatalf("expected a snapshot clone; calls: %v", run.calls)
	}
	if !run.ran("fs", "clone", "status", "cephfs", resp.Name) {
		t.Fatalf("expected a clone status poll for the target; calls: %v", run.calls)
	}
	// The clone must NOT also run a plain subvolume create (clone makes the subvol).
	if run.ran("fs", "subvolume", "create", "cephfs", resp.Name) {
		t.Fatalf("clone path must not also create the subvolume; calls: %v", run.calls)
	}
}
