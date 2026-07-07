package nfsplugin

import (
	"context"
	"errors"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// nodeRunner records calls and tracks mounted paths so findmnt --mountpoint
// reflects prior mounts (the node-plane idempotency check). umount of an
// unmounted target returns a "not mounted" error, like the real tool.
type nodeRunner struct {
	calls   [][]string
	mounted map[string]bool
}

func newNodeRunner() *nodeRunner { return &nodeRunner{mounted: map[string]bool{}} }

func (r *nodeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	switch name {
	case "findmnt": // findmnt -n -o SOURCE --mountpoint <path>
		if r.mounted[args[len(args)-1]] {
			return "src\n", nil
		}
		return "", errors.New("findmnt: not found")
	case "mount":
		r.mounted[mountTarget(args)] = true
		return "", nil
	case "umount":
		t := args[len(args)-1]
		if !r.mounted[t] {
			return "", errors.New("umount: " + t + ": not mounted")
		}
		delete(r.mounted, t)
		return "", nil
	}
	return "", nil
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

// mountTarget finds the mountpoint: the arg before "-o" (`mount -t nfs -o opts src MNT`
// has MNT last, but `mount -o opts src MNT` too), else the last arg.
func mountTarget(args []string) string {
	// NFS mount/bind both end with <src> <target>, so the target is the last arg.
	return args[len(args)-1]
}

func newNodeBackend(run Runner) *Backend {
	return New(map[string]InstanceConfig{"east": {Server: "10.0.0.9", Export: "/srv/nfs"}}, run)
}

// NodeStage/NodePublish mount once across two calls (idempotent); Unstage/Unpublish
// tolerate an already-unmounted target.
func TestNFSNodeIdempotency(t *testing.T) {
	run := newNodeRunner()
	b := newNodeBackend(run)
	ctx := context.Background()
	dir := t.TempDir()
	stage, pub := dir+"/stage", dir+"/pub"
	vol := bardplugin.VolumeRef{Instance: "east", Location: "/srv/nfs", Name: "bard-x"}

	for i := 0; i < 2; i++ {
		if err := b.NodeStage(ctx, &bardplugin.NodeStageRequest{Volume: vol, StagingPath: stage}); err != nil {
			t.Fatalf("stage %d: %v", i, err)
		}
	}
	if n := run.count("mount", "-t"); n != 1 {
		t.Fatalf("nfs mount must run once across two NodeStage calls, ran %d", n)
	}
	for i := 0; i < 2; i++ {
		if err := b.NodePublish(ctx, &bardplugin.NodePublishRequest{Volume: vol, StagingPath: stage, TargetPath: pub}); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	if n := run.count("mount", "--bind"); n != 1 {
		t.Fatalf("bind mount must run once across two NodePublish calls, ran %d", n)
	}
	// Second unpublish/unstage hit an already-unmounted target -> must not error.
	for i := 0; i < 2; i++ {
		if err := b.NodeUnpublish(ctx, &bardplugin.NodeUnpublishRequest{Volume: vol, TargetPath: pub}); err != nil {
			t.Fatalf("unpublish %d: %v", i, err)
		}
		if err := b.NodeUnstage(ctx, &bardplugin.NodeUnstageRequest{Volume: vol, StagingPath: stage}); err != nil {
			t.Fatalf("unstage %d: %v", i, err)
		}
	}
}

// The staging idempotency check must use --mountpoint, not --target.
func TestNFSUsesMountpoint(t *testing.T) {
	run := newNodeRunner()
	b := newNodeBackend(run)
	if err := b.NodeStage(context.Background(), &bardplugin.NodeStageRequest{
		Volume: bardplugin.VolumeRef{Instance: "east", Location: "/srv/nfs", Name: "v"}, StagingPath: t.TempDir() + "/s",
	}); err != nil {
		t.Fatal(err)
	}
	var saw bool
	for _, c := range run.calls {
		if c[0] != "findmnt" {
			continue
		}
		saw = true
		if !has(c, "--mountpoint") || has(c, "--target") {
			t.Fatalf("idempotency check must use --mountpoint: %v", c)
		}
	}
	if !saw {
		t.Fatal("NodeStage made no findmnt idempotency check")
	}
}

func has(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
