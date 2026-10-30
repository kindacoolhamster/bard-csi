package cephplugin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// fenceRunner simulates just enough of rbd/ceph for the single-writer fence
// path: rbd status returns a seeded watcher set, rbd map yields a device that
// then reports a non-zero size, and `ceph osd blocklist add` is recorded.
type fenceRunner struct {
	calls    [][]string
	watchers string // JSON body returned for `rbd status`
	mapped   map[string]bool
	zeroSize bool // a mapped device never reports a size (waitForDevice fails)
	// showmapped/nbdMapped are the `rbd showmapped` / `rbd-nbd list-mapped`
	// JSON bodies (local maps that predate this NodeStage). "" means "[]".
	showmapped string
	nbdMapped  string
	mountedDev map[string]string // findmnt --source <dev> -> target
}

func newFenceRunner(watchers string) *fenceRunner {
	return &fenceRunner{watchers: watchers, mapped: map[string]bool{}}
}

func (r *fenceRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	switch name {
	case "rbd", "rbd-nbd":
		switch {
		case has(args, "showmapped"):
			return orEmptyList(r.showmapped), nil
		case has(args, "list-mapped"):
			return orEmptyList(r.nbdMapped), nil
		case has(args, "status"):
			return r.watchers, nil
		case has(args, "map"):
			dev := "/dev/nbd7"
			r.mapped[dev] = true
			return dev, nil
		case has(args, "unmap"):
			delete(r.mapped, args[1])
			return "", nil
		}
		return "", nil
	case "blockdev":
		if r.mapped[args[len(args)-1]] && !r.zeroSize {
			return "1073741824", nil
		}
		return "0", nil
	case "findmnt":
		if has(args, "--source") {
			return r.mountedDev[args[len(args)-1]], nil
		}
		return "", nil
	default: // ceph (blocklist), blkid, mkfs, mount, umount...
		return "", nil
	}
}

func orEmptyList(s string) string {
	if s == "" {
		return "[]"
	}
	return s
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

func (r *fenceRunner) unmapped() []string {
	var out []string
	for _, c := range r.calls {
		if (c[0] == "rbd" || c[0] == "rbd-nbd") && has(c, "unmap") {
			out = append(out, c[2])
		}
	}
	return out
}

func (r *fenceRunner) mapCalls() int {
	n := 0
	for _, c := range r.calls {
		if (c[0] == "rbd" || c[0] == "rbd-nbd") && has(c, "map") {
			n++
		}
	}
	return n
}

// newFenceBackend gives the node no krbd clients, so no seeded watcher is
// accidentally "local" on whatever host runs the tests.
func newFenceBackend(dir string, run Runner) *Backend {
	b := New(map[string]ClusterConfig{"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin"}}, "", filepath.Join(dir, "state"), run)
	b.localClients = func() (map[string]bool, error) { return map[string]bool{}, nil }
	return b
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

// The node's own krbd client is never the stale foreign writer the fence exists
// for, and krbd shares it across every volume this node has mapped for the user,
// so blocklisting it cuts off the node's own I/O. Only that exact client is
// skipped: another client at the same IP (a local rbd-nbd process, or a foreign
// writer behind the same NAT/VIP address) is still fenced.
func TestFenceSkipsOwnKrbdClient(t *testing.T) {
	dir := t.TempDir()
	run := newFenceRunner(`{"watchers":[{"address":"192.168.1.50:0/111"},{"address":"192.168.1.50:0/999"},{"address":"192.168.1.99:0/222"}]}`)
	b := newFenceBackend(dir, run)
	b.localClients = func() (map[string]bool, error) { return map[string]bool{"192.168.1.50:0/111": true}, nil }

	err := b.NodeStage(context.Background(), &bardplugin.NodeStageRequest{
		Volume:      bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img"},
		StagingPath: filepath.Join(dir, "staging"),
		Block:       true,
		Exclusive:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := run.blocklisted()
	if len(got) != 2 || got[0] != "192.168.1.50:0/999" || got[1] != "192.168.1.99:0/222" {
		t.Fatalf("expected every watcher but this node's krbd client fenced, got %v", got)
	}
}

// krbdClientAddrs reads each mapped device's client_addr; no rbd sysfs dir (module
// not loaded) means no clients, not an error.
func TestKrbdClientAddrs(t *testing.T) {
	dir := t.TempDir()
	old := sysBusRbdDir
	t.Cleanup(func() { sysBusRbdDir = old })
	sysBusRbdDir = filepath.Join(dir, "absent")
	if got, err := krbdClientAddrs(); err != nil || len(got) != 0 {
		t.Fatalf("absent sysfs: got %v, %v", got, err)
	}
	sysBusRbdDir = filepath.Join(dir, "devices")
	for id, addr := range map[string]string{"0": "192.168.1.234:0/1429837703\n", "1": "192.168.1.234:0/1429837703\n", "2": ""} {
		if err := os.MkdirAll(filepath.Join(sysBusRbdDir, id), 0o755); err != nil {
			t.Fatal(err)
		}
		if addr != "" {
			if err := os.WriteFile(filepath.Join(sysBusRbdDir, id, "client_addr"), []byte(addr), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	got, err := krbdClientAddrs()
	if err != nil || len(got) != 1 || !got["192.168.1.234:0/1429837703"] {
		t.Fatalf("got %v, %v", got, err)
	}
}

// If this node's krbd clients cannot be listed, the fence cannot tell its own
// watcher from a foreign one: fail the stage (kubelet retries) rather than risk
// fencing ourselves or skipping a real stale writer.
func TestFenceFailsClosedWhenLocalAddrsUnknown(t *testing.T) {
	dir := t.TempDir()
	run := newFenceRunner(`{"watchers":[{"address":"192.168.1.99:0/222"}]}`)
	b := newFenceBackend(dir, run)
	b.localClients = func() (map[string]bool, error) { return nil, errors.New("sysfs unreadable") }

	err := b.NodeStage(context.Background(), &bardplugin.NodeStageRequest{
		Volume:      bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img"},
		StagingPath: filepath.Join(dir, "staging"),
		Block:       true,
		Exclusive:   true,
	})
	if err == nil {
		t.Fatal("expected NodeStage to fail when local addresses are unknown")
	}
	if got := run.blocklisted(); len(got) != 0 {
		t.Fatalf("must not blocklist anyone, got %v", got)
	}
	if n := run.mapCalls(); n != 0 {
		t.Fatalf("must not map before the fence succeeds, got %d map calls", n)
	}
}

// A map whose device never becomes ready must be unmapped before NodeStage
// returns -- even when the request context is what expired -- so the retry
// starts clean instead of finding (and fencing) this node's own leftover watcher.
func TestStageUnmapsWhenDeviceNeverReady(t *testing.T) {
	dir := t.TempDir()
	run := newFenceRunner(`{"watchers":[]}`)
	run.zeroSize = true
	b := newFenceBackend(dir, run)
	staging := filepath.Join(dir, "staging")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := b.NodeStage(ctx, &bardplugin.NodeStageRequest{
		Volume:      bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img"},
		StagingPath: staging,
		Block:       true,
		Exclusive:   true,
	})
	if err == nil {
		t.Fatal("expected NodeStage to fail when the device never sizes")
	}
	if got := run.unmapped(); len(got) != 1 || got[0] != "/dev/nbd7" {
		t.Fatalf("expected the half-staged device to be unmapped, got %v\ncalls: %v", got, run.calls)
	}
	if len(run.mapped) != 0 {
		t.Fatalf("device still mapped after failed stage: %v", run.mapped)
	}
	if dev := b.lookupDevice(staging); dev != "" {
		t.Fatalf("no device record should remain after a failed stage, got %q", dev)
	}
	if b.mapPending(staging) {
		t.Fatal("the pending marker must be cleared once the half-staged device is released")
	}
}

// A local map of this image that no staging record owns is an orphan of ours (a
// map that succeeded but was never recorded). A fresh NodeStage must unmap it
// before mapping again -- otherwise its rbd watcher outlives the volume and
// blocks DeleteVolume. Snapshot maps, other images, and a device another staging
// path has recorded are left alone.
func TestStageUnmapsOrphanedLocalMap(t *testing.T) {
	dir := t.TempDir()
	run := newFenceRunner(`{"watchers":[]}`)
	run.showmapped = `[{"pool":"replicapool","namespace":"","name":"img","snap":"-","device":"/dev/rbd5"},` +
		`{"pool":"replicapool","namespace":"","name":"img","snap":"s1","device":"/dev/rbd6"},` +
		`{"pool":"replicapool","namespace":"","name":"other","snap":"-","device":"/dev/rbd7"},` +
		`{"pool":"replicapool","namespace":"","name":"img","snap":"-","device":"/dev/rbd8"}]`
	run.nbdMapped = `[{"pool":"replicapool","namespace":"","image":"img","snap":"-","device":"/dev/nbd3"}]`
	b := newFenceBackend(dir, run)
	staging := filepath.Join(dir, "staging")
	// /dev/rbd8 belongs to another staging path's record: not ours to touch.
	if err := b.recordDevice(filepath.Join(dir, "other-staging"), deviceRecord{Device: "/dev/rbd8"}); err != nil {
		t.Fatal(err)
	}
	// An earlier attempt at this staging path started mapping and never recorded.
	if err := b.markMapPending(staging); err != nil {
		t.Fatal(err)
	}

	err := b.NodeStage(context.Background(), &bardplugin.NodeStageRequest{
		Volume:      bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img"},
		StagingPath: staging,
		Block:       true,
		Exclusive:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, d := range run.unmapped() {
		got[d] = true
	}
	if len(got) != 2 || !got["/dev/rbd5"] || !got["/dev/nbd3"] {
		t.Fatalf("expected only the orphans /dev/rbd5 and /dev/nbd3 unmapped, got %v", run.unmapped())
	}
	// The orphan cleanup runs before the fresh map.
	var unmapAt, mapAt int
	for i, c := range run.calls {
		if (c[0] == "rbd" || c[0] == "rbd-nbd") && has(c, "unmap") && unmapAt == 0 {
			unmapAt = i + 1
		}
		if c[0] == "rbd" && has(c, "map") {
			mapAt = i + 1
		}
	}
	if unmapAt == 0 || mapAt == 0 || unmapAt > mapAt {
		t.Fatalf("orphan unmap must precede the map (unmap@%d map@%d)", unmapAt, mapAt)
	}
	if b.mapPending(staging) {
		t.Fatal("the pending marker must be cleared once the device is recorded")
	}
}

// A rados-namespaced volume (Location "pool/ns") matches maps by pool+namespace.
func TestStageOrphanMatchHonorsNamespace(t *testing.T) {
	dir := t.TempDir()
	run := newFenceRunner(`{"watchers":[]}`)
	run.showmapped = `[{"pool":"replicapool","namespace":"tenant","name":"img","snap":"-","device":"/dev/rbd5"},` +
		`{"pool":"replicapool","namespace":"","name":"img","snap":"-","device":"/dev/rbd6"}]`
	b := newFenceBackend(dir, run)
	if err := b.markMapPending(filepath.Join(dir, "staging")); err != nil {
		t.Fatal(err)
	}

	err := b.NodeStage(context.Background(), &bardplugin.NodeStageRequest{
		Volume:      bardplugin.VolumeRef{Instance: "east", Location: "replicapool/tenant", Name: "img"},
		StagingPath: filepath.Join(dir, "staging"),
		Block:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := run.unmapped(); len(got) != 1 || got[0] != "/dev/rbd5" {
		t.Fatalf("expected only the namespaced orphan /dev/rbd5 unmapped, got %v", got)
	}
}

// An unrecorded map that is still in use (mounted, or held by a dm-crypt mapper)
// must never be unmapped -- rbd-nbd would disconnect it from under the mount.
func TestStageLeavesInUseUnrecordedMap(t *testing.T) {
	dir := t.TempDir()
	sys := filepath.Join(dir, "sys")
	old := sysBlockDir
	sysBlockDir = sys
	t.Cleanup(func() { sysBlockDir = old })
	if err := os.MkdirAll(filepath.Join(sys, "rbd6", "holders", "dm-3"), 0o755); err != nil {
		t.Fatal(err)
	}
	run := newFenceRunner(`{"watchers":[]}`)
	run.showmapped = `[{"pool":"replicapool","namespace":"","name":"img","snap":"-","device":"/dev/rbd5"},` +
		`{"pool":"replicapool","namespace":"","name":"img","snap":"-","device":"/dev/rbd6"}]`
	run.mountedDev = map[string]string{"/dev/rbd5": "/var/lib/kubelet/old-staging"}
	b := newFenceBackend(dir, run)
	if err := b.markMapPending(filepath.Join(dir, "staging")); err != nil {
		t.Fatal(err)
	}

	err := b.NodeStage(context.Background(), &bardplugin.NodeStageRequest{
		Volume:      bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img"},
		StagingPath: filepath.Join(dir, "staging"),
		Block:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := run.unmapped(); len(got) != 0 {
		t.Fatalf("in-use maps (mounted rbd5, dm-held rbd6) must not be unmapped, got %v", got)
	}
}

// Without a pending marker an unrecorded map is NOT ours to judge -- e.g. a volume
// staged before device records existed, whose raw-block publish (a bind of the
// device node) no mount or holder check can see -- so nothing is unmapped. And a
// device this staging path itself records is never treated as an orphan.
func TestStageLeavesUnrecordedMapWithoutMarker(t *testing.T) {
	for _, tc := range []struct {
		name   string
		marker bool
		record string
	}{
		{"no-marker", false, ""},
		{"marker-but-self-recorded", true, "/dev/rbd5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			run := newFenceRunner(`{"watchers":[]}`)
			run.showmapped = `[{"pool":"replicapool","namespace":"","name":"img","snap":"-","device":"/dev/rbd5"}]`
			b := newFenceBackend(dir, run)
			staging := filepath.Join(dir, "staging")
			if tc.record != "" { // recorded, but its size probe fails -> fresh-map path
				if err := b.recordDevice(staging, deviceRecord{Device: tc.record}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.marker {
				if err := b.markMapPending(staging); err != nil {
					t.Fatal(err)
				}
			}
			err := b.NodeStage(context.Background(), &bardplugin.NodeStageRequest{
				Volume:      bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img"},
				StagingPath: staging,
				Block:       true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := run.unmapped(); len(got) != 0 {
				t.Fatalf("must not unmap %v", got)
			}
		})
	}
}

// Overlapping NodeStage calls for one staging path (a retry that core sent after
// giving up on the first, which the plugin is still running) are serialized.
func TestNodeStageSerializedPerStagingPath(t *testing.T) {
	b := newFenceBackend(t.TempDir(), newFenceRunner(`{"watchers":[]}`))
	release := b.lockStaging("/stage/a")
	entered := make(chan struct{})
	go func() {
		defer close(entered)
		b.lockStaging("/stage/a")()
	}()
	other := make(chan struct{})
	go func() { // a different staging path is not blocked
		defer close(other)
		b.lockStaging("/stage/b")()
	}()
	select {
	case <-other:
	case <-time.After(2 * time.Second):
		t.Fatal("a different staging path was blocked")
	}
	select {
	case <-entered:
		t.Fatal("second stage of the same path entered while the first held the lock")
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("second stage never entered after the first released")
	}
}
