package cephfsplugin

import (
	"context"
	"strings"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// nodeRunner records calls and tracks which paths are mounted, so findmnt
// --mountpoint reflects prior mounts (the idempotency check the node plane needs).
type nodeRunner struct {
	calls   [][]string
	mounted map[string]bool
}

func newNodeRunner() *nodeRunner { return &nodeRunner{mounted: map[string]bool{}} }

func (r *nodeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	switch name {
	case "findmnt": // findmnt ... --mountpoint <path>
		if r.mounted[args[len(args)-1]] {
			return "src\n", nil
		}
		return "", nil // not a mountpoint
	case "mount":
		r.mounted[mountTarget(args)] = true
		return "", nil
	case "ceph-fuse": // ceph-fuse <mountpoint> -m ...
		r.mounted[args[0]] = true
		return "", nil
	case "umount":
		delete(r.mounted, args[len(args)-1])
		return "", nil
	}
	return "", nil
}

// mountTarget finds the mountpoint in a mount(8) command: the arg before "-o"
// (e.g. `mount -t ceph src MNT -o opts`), else the last arg (`mount --bind src MNT`).
func mountTarget(args []string) string {
	for i, a := range args {
		if a == "-o" && i > 0 {
			return args[i-1]
		}
	}
	return args[len(args)-1]
}

func (r *nodeRunner) count(name, sub string) int {
	n := 0
	for _, c := range r.calls {
		if c[0] != name {
			continue
		}
		for _, a := range c {
			if a == sub {
				n++
				break
			}
		}
	}
	return n
}

// NodeStage and NodePublish must be idempotent: a retried call against an
// already-mounted path is a no-op, not a second mount.
func TestCephFSNodeIdempotency(t *testing.T) {
	run := newNodeRunner()
	b := New(map[string]ClusterConfig{"east": {Monitors: []string{"10.0.0.10:6789"}, FSName: "cephfs", UserID: "admin"}}, "", run)
	ctx := context.Background()
	dir := t.TempDir()
	stagePath := dir + "/stage"
	pubPath := dir + "/pub"
	vol := bardplugin.VolumeRef{Instance: "east", Name: "sub"}
	stage := &bardplugin.NodeStageRequest{Volume: vol, StagingPath: stagePath, Context: map[string]string{ctxPath: "/volumes/_x/sub"}}

	for i := 0; i < 2; i++ {
		if err := b.NodeStage(ctx, stage); err != nil {
			t.Fatalf("stage %d: %v", i, err)
		}
	}
	if n := run.count("mount", "-t"); n != 1 {
		t.Fatalf("kernel mount must run once across two NodeStage calls, ran %d", n)
	}

	pub := &bardplugin.NodePublishRequest{Volume: vol, StagingPath: stagePath, TargetPath: pubPath}
	for i := 0; i < 2; i++ {
		if err := b.NodePublish(ctx, pub); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	if n := run.count("mount", "--bind"); n != 1 {
		t.Fatalf("bind mount must run once across two NodePublish calls, ran %d", n)
	}

	// Unstage/unpublish tolerate an already-unmounted target (second call no-op).
	for i := 0; i < 2; i++ {
		if err := b.NodeUnpublish(ctx, &bardplugin.NodeUnpublishRequest{Volume: vol, TargetPath: pubPath}); err != nil {
			t.Fatalf("unpublish %d: %v", i, err)
		}
		if err := b.NodeUnstage(ctx, &bardplugin.NodeUnstageRequest{Volume: vol, StagingPath: stagePath}); err != nil {
			t.Fatalf("unstage %d: %v", i, err)
		}
	}
}

// Guard against a regression to findmnt --target (which matches any path on a
// mount, not just a mountpoint, and would wrongly skip the real mount).
func TestCephFSUsesMountpointNotTarget(t *testing.T) {
	run := newNodeRunner()
	b := New(map[string]ClusterConfig{"east": {Monitors: []string{"m"}, FSName: "fs", UserID: "admin"}}, "", run)
	if err := b.NodeStage(context.Background(), &bardplugin.NodeStageRequest{
		Volume: bardplugin.VolumeRef{Instance: "east", Name: "s"}, StagingPath: t.TempDir() + "/stage",
		Context: map[string]string{ctxPath: "/p"},
	}); err != nil {
		t.Fatal(err)
	}
	var sawFindmnt bool
	for _, c := range run.calls {
		if c[0] == "findmnt" {
			sawFindmnt = true
			if !contains(c, "--mountpoint") || contains(c, "--target") {
				t.Fatalf("NodeStage idempotency check must use --mountpoint, got %v", c)
			}
		}
	}
	if !sawFindmnt {
		t.Fatal("NodeStage made no findmnt idempotency check")
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want || strings.Contains(s, want) {
			return true
		}
	}
	return false
}
