package cephplugin

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// mounterRunner models the node map path with a controllable krbd outcome: `rbd map`
// (krbd) can be made to fail (e.g. an image feature the node's krbd lacks) while
// `rbd-nbd map` succeeds. It tracks mapped devices so blockdev/unmap behave.
type mounterRunner struct {
	calls        [][]string
	krbdMapFails bool
	mapped       map[string]bool
}

func newMounterRunner(krbdMapFails bool) *mounterRunner {
	return &mounterRunner{krbdMapFails: krbdMapFails, mapped: map[string]bool{}}
}

func (r *mounterRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	devArg := func() string {
		for _, a := range args {
			if len(a) > 5 && a[:5] == "/dev/" {
				return a
			}
		}
		return ""
	}
	switch name {
	case "rbd":
		if has(args, "map") {
			if r.krbdMapFails {
				return "", fmt.Errorf("rbd: sysfs write failed: RBD image feature set mismatch, you may want to set tryOtherMounters")
			}
			r.mapped["/dev/rbd0"] = true
			return "/dev/rbd0", nil
		}
		if has(args, "unmap") {
			delete(r.mapped, devArg())
		}
		return "", nil
	case "rbd-nbd":
		if has(args, "map") {
			r.mapped["/dev/nbd7"] = true
			return "/dev/nbd7", nil
		}
		if has(args, "unmap") {
			delete(r.mapped, devArg())
		}
		return "", nil
	case "blockdev":
		if r.mapped[args[len(args)-1]] {
			return "1073741824", nil
		}
		return "0", nil
	default: // blkid, mkfs, mount, findmnt, umount, cryptsetup...
		return "", nil
	}
}

func (r *mounterRunner) ranBin(bin string, tok string) bool {
	for _, c := range r.calls {
		if c[0] == bin && has(c, tok) {
			return true
		}
	}
	return false
}

func stageReq(dir string, ctx map[string]string) *bardplugin.NodeStageRequest {
	return &bardplugin.NodeStageRequest{
		Volume:      bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img"},
		StagingPath: filepath.Join(dir, "stage"),
		FsType:      "ext4",
		Context:     ctx,
	}
}

// tryOtherMounters: a failing krbd map falls back to rbd-nbd, and the device record
// captures the mounter that actually mapped (so NodeUnstage uses the right tool).
func TestTryOtherMountersFallback(t *testing.T) {
	dir := t.TempDir()
	run := newMounterRunner(true) // krbd map fails
	b := newFenceBackend(dir, run)
	if err := b.NodeStage(context.Background(), stageReq(dir, map[string]string{paramTryOtherMounters: "true"})); err != nil {
		t.Fatal(err)
	}
	if !run.ranBin("rbd", "map") {
		t.Fatalf("krbd map must be attempted first; calls: %v", run.calls)
	}
	if !run.ranBin("rbd-nbd", "map") {
		t.Fatalf("must fall back to rbd-nbd map; calls: %v", run.calls)
	}
	rec := b.readDeviceRecord(filepath.Join(dir, "stage"))
	if rec.Mounter != mounterNBD {
		t.Fatalf("recorded mounter = %q, want %q", rec.Mounter, mounterNBD)
	}
	if rec.Device != "/dev/nbd7" {
		t.Fatalf("recorded device = %q, want /dev/nbd7", rec.Device)
	}
}

// Without the opt-in, a krbd map failure fails the stage and does NOT fall back.
func TestTryOtherMountersDisabled(t *testing.T) {
	dir := t.TempDir()
	run := newMounterRunner(true)
	b := newFenceBackend(dir, run)
	if err := b.NodeStage(context.Background(), stageReq(dir, nil)); err == nil {
		t.Fatal("krbd map failure must fail the stage without tryOtherMounters")
	}
	if run.ranBin("rbd-nbd", "map") {
		t.Fatalf("must NOT fall back to rbd-nbd without the opt-in; calls: %v", run.calls)
	}
}

// When krbd succeeds, the opt-in is a no-op: no rbd-nbd, mounter recorded as krbd.
func TestTryOtherMountersKrbdSucceeds(t *testing.T) {
	dir := t.TempDir()
	run := newMounterRunner(false)
	b := newFenceBackend(dir, run)
	if err := b.NodeStage(context.Background(), stageReq(dir, map[string]string{paramTryOtherMounters: "true"})); err != nil {
		t.Fatal(err)
	}
	if run.ranBin("rbd-nbd", "map") {
		t.Fatalf("krbd succeeded, must not fall back; calls: %v", run.calls)
	}
	if rec := b.readDeviceRecord(filepath.Join(dir, "stage")); rec.Mounter != mounterKRBD {
		t.Fatalf("recorded mounter = %q, want %q", rec.Mounter, mounterKRBD)
	}
}

// A volume that fell back to rbd-nbd is unmapped with rbd-nbd (the recorded mounter),
// not krbd's rbd.
func TestNodeUnstageUsesRecordedMounter(t *testing.T) {
	dir := t.TempDir()
	run := newMounterRunner(true)
	b := newFenceBackend(dir, run)
	stage := filepath.Join(dir, "stage")
	if err := b.NodeStage(context.Background(), stageReq(dir, map[string]string{paramTryOtherMounters: "true"})); err != nil {
		t.Fatal(err)
	}
	if err := b.NodeUnstage(context.Background(), &bardplugin.NodeUnstageRequest{
		Volume:      bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img"},
		StagingPath: stage,
	}); err != nil {
		t.Fatal(err)
	}
	if !run.ranBin("rbd-nbd", "unmap") {
		t.Fatalf("unstage must unmap with rbd-nbd (the recorded mounter); calls: %v", run.calls)
	}
	if run.ranBin("rbd", "unmap") {
		t.Fatalf("must not use krbd unmap for an rbd-nbd-mapped volume; calls: %v", run.calls)
	}
}
