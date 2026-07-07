package conformance

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// refBackend is an in-memory, contract-compliant backend: idempotent create
// and delete, AlreadyExists on an incompatible re-create, snapshots with
// restore and clone, expand, capacity, health, and both listers. The broken*
// knobs turn specific violations on so the test can prove the checks catch
// them.
type refBackend struct {
	mu    sync.Mutex
	vols  map[string]int64  // name -> size
	snaps map[string]string // snap name -> source volume name

	brokenRecreate       bool // identical re-create returns AlreadyExists
	brokenNotFoundDelete bool // deleting an absent volume returns NotFound
}

func newRefBackend() *refBackend {
	return &refBackend{vols: map[string]int64{}, snaps: map[string]string{}}
}

func (b *refBackend) Info() bardplugin.Info {
	return bardplugin.Info{Type: "ref", Capabilities: bardplugin.Capabilities{
		Snapshots: true,
		Expand:    true,
	}}
}

func (b *refBackend) CreateVolume(_ context.Context, req *bardplugin.CreateVolumeRequest) (*bardplugin.CreateVolumeResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if req.SourceSnapshot != nil {
		if _, ok := b.snaps[req.SourceSnapshot.Name]; !ok {
			return nil, bardplugin.Errorf(bardplugin.CodeNotFound, "snapshot %s", req.SourceSnapshot.Name)
		}
	}
	if req.SourceVolume != nil {
		if _, ok := b.vols[req.SourceVolume.Name]; !ok {
			return nil, bardplugin.Errorf(bardplugin.CodeNotFound, "volume %s", req.SourceVolume.Name)
		}
	}
	if size, ok := b.vols[req.Name]; ok {
		if b.brokenRecreate || size != req.CapacityBytes {
			return nil, bardplugin.Errorf(bardplugin.CodeAlreadyExists, "volume %s exists", req.Name)
		}
	}
	b.vols[req.Name] = req.CapacityBytes
	return &bardplugin.CreateVolumeResponse{Location: "pool0", Name: req.Name, CapacityBytes: req.CapacityBytes}, nil
}

func (b *refBackend) DeleteVolume(_ context.Context, req *bardplugin.DeleteVolumeRequest) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.vols[req.Volume.Name]; !ok && b.brokenNotFoundDelete {
		return bardplugin.Errorf(bardplugin.CodeNotFound, "volume %s", req.Volume.Name)
	}
	delete(b.vols, req.Volume.Name)
	return nil
}

func (b *refBackend) ExpandVolume(_ context.Context, req *bardplugin.ExpandVolumeRequest) (*bardplugin.ExpandVolumeResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	size, ok := b.vols[req.Volume.Name]
	if !ok {
		return nil, bardplugin.Errorf(bardplugin.CodeNotFound, "volume %s", req.Volume.Name)
	}
	if req.NewSizeBytes > size {
		size = req.NewSizeBytes
		b.vols[req.Volume.Name] = size
	}
	return &bardplugin.ExpandVolumeResponse{CapacityBytes: size, NodeExpansionRequired: false}, nil
}

func (b *refBackend) CreateSnapshot(_ context.Context, req *bardplugin.CreateSnapshotRequest) (*bardplugin.CreateSnapshotResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.vols[req.SourceVolume.Name]; !ok {
		return nil, bardplugin.Errorf(bardplugin.CodeNotFound, "volume %s", req.SourceVolume.Name)
	}
	if src, ok := b.snaps[req.Name]; ok && src != req.SourceVolume.Name {
		return nil, bardplugin.Errorf(bardplugin.CodeAlreadyExists, "snapshot %s has another source", req.Name)
	}
	b.snaps[req.Name] = req.SourceVolume.Name
	return &bardplugin.CreateSnapshotResponse{Location: "pool0", Name: req.Name, SizeBytes: b.vols[req.SourceVolume.Name], ReadyToUse: true}, nil
}

func (b *refBackend) DeleteSnapshot(_ context.Context, req *bardplugin.DeleteSnapshotRequest) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.snaps, req.Snapshot.Name)
	return nil
}

func (b *refBackend) NodeStage(context.Context, *bardplugin.NodeStageRequest) error     { return nil }
func (b *refBackend) NodeUnstage(context.Context, *bardplugin.NodeUnstageRequest) error { return nil }
func (b *refBackend) NodePublish(context.Context, *bardplugin.NodePublishRequest) error { return nil }
func (b *refBackend) NodeUnpublish(context.Context, *bardplugin.NodeUnpublishRequest) error {
	return nil
}
func (b *refBackend) NodeExpand(context.Context, *bardplugin.NodeExpandRequest) (*bardplugin.NodeExpandResponse, error) {
	return &bardplugin.NodeExpandResponse{}, nil
}

// Optional interfaces: capacity, health, both listers.
func (b *refBackend) GetCapacity(context.Context, *bardplugin.GetCapacityRequest) (*bardplugin.GetCapacityResponse, error) {
	return &bardplugin.GetCapacityResponse{AvailableBytes: 1 << 40}, nil
}

func (b *refBackend) GetVolumeHealth(_ context.Context, req *bardplugin.GetVolumeHealthRequest) (*bardplugin.GetVolumeHealthResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.vols[req.Volume.Name]; !ok {
		return &bardplugin.GetVolumeHealthResponse{Abnormal: true, Message: "volume gone"}, nil
	}
	return &bardplugin.GetVolumeHealthResponse{}, nil
}

func (b *refBackend) ListVolumes(context.Context, *bardplugin.ListVolumesRequest) (*bardplugin.ListVolumesResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out bardplugin.ListVolumesResponse
	for name, size := range b.vols {
		out.Entries = append(out.Entries, bardplugin.VolumeListEntry{
			Volume:        bardplugin.VolumeRef{Instance: "i1", Location: "pool0", Name: name},
			CapacityBytes: size,
		})
	}
	return &out, nil
}

func (b *refBackend) ListSnapshots(context.Context, *bardplugin.ListSnapshotsRequest) (*bardplugin.ListSnapshotsResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out bardplugin.ListSnapshotsResponse
	for name, src := range b.snaps {
		out.Entries = append(out.Entries, bardplugin.SnapshotListEntry{
			Snapshot:     bardplugin.VolumeRef{Instance: "i1", Location: "pool0", Name: name},
			SourceVolume: bardplugin.VolumeRef{Instance: "i1", Location: "pool0", Name: src},
			ReadyToUse:   true,
		})
	}
	return &out, nil
}

func startPlugin(t *testing.T, b bardplugin.Backend) string {
	t.Helper()
	// Not t.TempDir(): unix socket paths are capped (~108 bytes) and test names
	// can push a t.TempDir() path past it.
	dir, err := os.MkdirTemp("", "conf")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "p.sock")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = bardplugin.Serve(ctx, sock, b) }()
	return sock
}

func resultsByName(rs []Result) map[string]Result {
	out := map[string]Result{}
	for _, r := range rs {
		out[r.Name] = r
	}
	return out
}

// TestCompliantBackendPasses runs the full control-plane suite against the
// reference backend and requires zero failures -- and that the substantive
// checks actually ran (PASS, not SKIP).
func TestCompliantBackendPasses(t *testing.T) {
	sock := startPlugin(t, newRefBackend())
	results, err := Run(context.Background(), Config{
		Socket: sock, Instance: "i1", CapacityBytes: 1 << 20, Logf: t.Logf,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	byName := resultsByName(results)
	for _, r := range results {
		if r.Status == Fail {
			t.Errorf("unexpected FAIL: %s -- %s", r.Name, r.Detail)
		}
	}
	for _, name := range []string{
		"info", "info/stable",
		"volume/create", "volume/create-idempotent", "volume/create-conflict", "volume/unknown-field",
		"volume/expand", "capacity", "volume/health", "volume/list",
		"snapshot/create", "snapshot/create-idempotent", "snapshot/list", "snapshot/restore",
		"volume/clone",
		"snapshot/delete", "volume/delete", "volume/delete-absent",
	} {
		r, ok := byName[name]
		if !ok {
			t.Errorf("check %s never ran", name)
			continue
		}
		if r.Status != Pass {
			t.Errorf("check %s: %s -- %s (want PASS)", name, r.Status, r.Detail)
		}
	}
}

// TestViolationsAreCaught breaks re-create idempotency and delete idempotency
// and requires the matching checks to FAIL (and only sensible ones to fail).
func TestViolationsAreCaught(t *testing.T) {
	b := newRefBackend()
	b.brokenRecreate = true
	b.brokenNotFoundDelete = true
	sock := startPlugin(t, b)
	results, err := Run(context.Background(), Config{
		Socket: sock, Instance: "i1", CapacityBytes: 1 << 20, Logf: t.Logf,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	byName := resultsByName(results)
	for _, name := range []string{"volume/create-idempotent", "volume/delete", "volume/delete-absent"} {
		if byName[name].Status != Fail {
			t.Errorf("check %s: %s -- %s (want FAIL)", name, byName[name].Status, byName[name].Detail)
		}
	}
	if byName["volume/create"].Status != Pass {
		t.Errorf("volume/create should still PASS, got %s", byName["volume/create"].Status)
	}
}
