package cephplugin

import (
	"context"
	"encoding/json"
	"testing"
)

// healRunner models the node-local rbd-nbd state: which devices have a live map
// (list-mapped), and which dead devices are still held by a mount (blockdev size
// > 0). It records `rbd-nbd attach` calls so a test can assert what got healed.
type healRunner struct {
	calls [][]string
	live  []string        // devices with a live rbd-nbd map
	held  map[string]bool // dead devices still held (non-zero size)
}

func (r *healRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	switch {
	case name == "rbd-nbd" && has(args, "list-mapped"):
		rows := make([]map[string]string, 0, len(r.live))
		for _, d := range r.live {
			rows = append(rows, map[string]string{"device": d})
		}
		b, _ := json.Marshal(rows)
		return string(b), nil
	case name == "blockdev":
		if r.held[args[len(args)-1]] {
			return "1073741824", nil
		}
		return "0", nil
	default: // rbd-nbd attach, etc.
		return "", nil
	}
}

func (r *healRunner) attached(dev string) bool {
	for _, c := range r.calls {
		if c[0] == "rbd-nbd" && has(c, "attach") && has(c, dev) {
			return true
		}
	}
	return false
}

// Heal must reattach a dead-but-held rbd-nbd device, leave a healthy one and a
// krbd one alone, and drop the record of a cleanly-released device.
func TestHealReattachesDeadNBD(t *testing.T) {
	state := t.TempDir()
	run := &healRunner{
		live: []string{"/dev/nbd0"},              // healthy: live map
		held: map[string]bool{"/dev/nbd1": true}, // dead but still mounted
		// /dev/nbd2: dead and released (size 0) -> stale record, drop it
	}
	b := New(map[string]ClusterConfig{"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin"}}, "", state, run)

	rec := func(staging, dev, image, mounter string) {
		if err := b.recordDevice(staging, deviceRecord{
			Device: dev, Mounter: mounter, Instance: "east", Pool: "replicapool", Image: image,
		}); err != nil {
			t.Fatal(err)
		}
	}
	rec("/s/live", "/dev/nbd0", "live", mounterNBD)
	rec("/s/dead", "/dev/nbd1", "dead", mounterNBD)
	rec("/s/gone", "/dev/nbd2", "gone", mounterNBD)
	rec("/s/krbd", "/dev/rbd0", "krbd", "") // krbd: kernel-held, never reattach

	b.Heal(context.Background())

	if !run.attached("/dev/nbd1") {
		t.Errorf("dead-but-held nbd must be reattached\ncalls: %v", run.calls)
	}
	if run.attached("/dev/nbd0") {
		t.Error("a healthy nbd map must not be reattached")
	}
	if run.attached("/dev/rbd0") {
		t.Error("krbd must never be touched by the healer")
	}
	if b.lookupDevice("/s/gone") != "" {
		t.Error("a cleanly-released nbd record must be dropped")
	}
	if b.lookupDevice("/s/dead") != "/dev/nbd1" {
		t.Error("a reattached record must be kept")
	}
	if b.lookupDevice("/s/live") != "/dev/nbd0" {
		t.Error("a healthy record must be kept")
	}
}

// A node with no rbd-nbd records (krbd-only, or fresh) must not invoke the
// rbd-nbd tooling at all.
func TestHealNoNBDRecordsIsNoOp(t *testing.T) {
	state := t.TempDir()
	run := &healRunner{}
	b := New(map[string]ClusterConfig{"east": {Pool: "replicapool", UserID: "admin"}}, "", state, run)
	if err := b.recordDevice("/s/krbd", deviceRecord{Device: "/dev/rbd0", Instance: "east", Pool: "replicapool", Image: "k"}); err != nil {
		t.Fatal(err)
	}

	b.Heal(context.Background())

	for _, c := range run.calls {
		if c[0] == "rbd-nbd" {
			t.Fatalf("krbd-only node must not invoke rbd-nbd, got %v", c)
		}
	}
}
