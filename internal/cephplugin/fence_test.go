package cephplugin

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// fenceRunner simulates just enough of rbd/ceph for the single-writer fence
// path: rbd status returns a seeded watcher set, rbd map yields a device that
// then reports a non-zero size, and `ceph osd blocklist add` is recorded.
type fenceRunner struct {
	calls    [][]string
	watchers string // JSON body returned for `rbd status`
	mapped   map[string]bool
}

func newFenceRunner(watchers string) *fenceRunner {
	return &fenceRunner{watchers: watchers, mapped: map[string]bool{}}
}

func (r *fenceRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	switch name {
	case "rbd", "rbd-nbd":
		switch {
		case has(args, "status"):
			return r.watchers, nil
		case has(args, "map"):
			dev := "/dev/nbd7"
			r.mapped[dev] = true
			return dev, nil
		}
		return "", nil
	case "blockdev":
		if r.mapped[args[len(args)-1]] {
			return "1073741824", nil
		}
		return "0", nil
	default: // ceph (blocklist), blkid, mkfs, mount, findmnt, umount...
		return "", nil
	}
}

func has(args []string, tok string) bool {
	for _, a := range args {
		if a == tok {
			return true
		}
	}
	return false
}

func (r *fenceRunner) blocklisted() []string {
	var out []string
	for _, c := range r.calls {
		if c[0] == "ceph" && has(c, "blocklist") && has(c, "add") {
			out = append(out, c[len(c)-1])
		}
	}
	return out
}

func (r *fenceRunner) statusCalls() int {
	n := 0
	for _, c := range r.calls {
		if (c[0] == "rbd" || c[0] == "rbd-nbd") && has(c, "status") {
			n++
		}
	}
	return n
}

func newFenceBackend(dir string, run Runner) *Backend {
	return New(map[string]ClusterConfig{"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin"}}, "", filepath.Join(dir, "state"), run)
}

// A fresh stage of an exclusive volume with a foreign watcher must fence that
// watcher (blocklist its address) before taking the image over.
func TestFenceStaleWatcherOnExclusiveStage(t *testing.T) {
	dir := t.TempDir()
	run := newFenceRunner(`{"watchers":[{"address":"192.168.1.99:0/12345"}]}`)
	b := newFenceBackend(dir, run)

	err := b.NodeStage(context.Background(), &bardplugin.NodeStageRequest{
		Volume:      bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img"},
		StagingPath: filepath.Join(dir, "staging"),
		Block:       true, // skip format/mount; the fence happens before mapping either way
		Exclusive:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := run.blocklisted()
	if len(got) != 1 || got[0] != "192.168.1.99:0/12345" {
		t.Fatalf("expected the foreign watcher to be blocklisted, got %v\ncalls: %v", got, run.calls)
	}
}

// A multi-node (shared) volume must never fence: it does not even query status.
func TestNoFenceForSharedVolume(t *testing.T) {
	dir := t.TempDir()
	run := newFenceRunner(`{"watchers":[{"address":"192.168.1.99:0/12345"}]}`)
	b := newFenceBackend(dir, run)

	err := b.NodeStage(context.Background(), &bardplugin.NodeStageRequest{
		Volume:      bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img"},
		StagingPath: filepath.Join(dir, "staging"),
		Block:       true,
		Exclusive:   false, // shared writers -> no single-writer fencing
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := run.blocklisted(); len(got) != 0 {
		t.Fatalf("shared volume must not blocklist anyone, got %v", got)
	}
	if n := run.statusCalls(); n != 0 {
		t.Fatalf("shared volume must not query watchers, got %d status calls", n)
	}
}

// An idempotent re-stage on the *same* node reuses its recorded device and must
// not re-enter the fence path (it would otherwise fence its own client).
func TestNoFenceOnSameNodeRestage(t *testing.T) {
	dir := t.TempDir()
	run := newFenceRunner(`{"watchers":[]}`) // no foreign watcher on first map
	b := newFenceBackend(dir, run)
	staging := filepath.Join(dir, "staging")
	req := &bardplugin.NodeStageRequest{
		Volume:      bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img"},
		StagingPath: staging,
		Block:       true,
		Exclusive:   true,
	}
	if err := b.NodeStage(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if err := b.NodeStage(context.Background(), req); err != nil { // retry
		t.Fatal(err)
	}
	// First stage probes status once (empty -> no fence); the second reuses the
	// recorded device and never probes again.
	if n := run.statusCalls(); n != 1 {
		t.Fatalf("expected exactly one status probe across two stages, got %d", n)
	}
	if got := run.blocklisted(); len(got) != 0 {
		t.Fatalf("same-node re-stage must not blocklist, got %v", got)
	}
}
