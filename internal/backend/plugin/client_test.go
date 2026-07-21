package plugin_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kindacoolhamster/bard-csi/internal/backend"
	"github.com/kindacoolhamster/bard-csi/internal/backend/plugin"
	"github.com/kindacoolhamster/bard-csi/internal/volumeid"
	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// fakeBackend is a minimal in-memory bardplugin.Backend for the roundtrip test.
type fakeBackend struct {
	info       bardplugin.Info
	lastCreate *bardplugin.CreateVolumeRequest
	lastStage  *bardplugin.NodeStageRequest
	createErr  error
}

func (f *fakeBackend) Info() bardplugin.Info { return f.info }

func (f *fakeBackend) CreateVolume(_ context.Context, req *bardplugin.CreateVolumeRequest) (*bardplugin.CreateVolumeResponse, error) {
	f.lastCreate = req
	if f.createErr != nil {
		return nil, f.createErr
	}
	return &bardplugin.CreateVolumeResponse{
		Location:      "exports",
		Name:          "obj-" + req.Name,
		CapacityBytes: req.CapacityBytes,
		Context:       map[string]string{"server": "nfs1"},
	}, nil
}

func (f *fakeBackend) DeleteVolume(context.Context, *bardplugin.DeleteVolumeRequest) error {
	return nil
}
func (f *fakeBackend) ExpandVolume(_ context.Context, req *bardplugin.ExpandVolumeRequest) (*bardplugin.ExpandVolumeResponse, error) {
	return &bardplugin.ExpandVolumeResponse{CapacityBytes: req.NewSizeBytes, NodeExpansionRequired: true}, nil
}
func (f *fakeBackend) CreateSnapshot(context.Context, *bardplugin.CreateSnapshotRequest) (*bardplugin.CreateSnapshotResponse, error) {
	return &bardplugin.CreateSnapshotResponse{Name: "snap", ReadyToUse: true}, nil
}
func (f *fakeBackend) DeleteSnapshot(context.Context, *bardplugin.DeleteSnapshotRequest) error {
	return nil
}
func (f *fakeBackend) NodeStage(_ context.Context, req *bardplugin.NodeStageRequest) error {
	f.lastStage = req
	return nil
}
func (f *fakeBackend) NodeUnstage(context.Context, *bardplugin.NodeUnstageRequest) error { return nil }
func (f *fakeBackend) NodePublish(context.Context, *bardplugin.NodePublishRequest) error { return nil }
func (f *fakeBackend) NodeUnpublish(context.Context, *bardplugin.NodeUnpublishRequest) error {
	return nil
}
func (f *fakeBackend) NodeExpand(context.Context, *bardplugin.NodeExpandRequest) (*bardplugin.NodeExpandResponse, error) {
	return &bardplugin.NodeExpandResponse{}, nil
}

func TestClientServerRoundtrip(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "p.sock")
	fb := &fakeBackend{info: bardplugin.Info{
		Type:         "fake",
		Capabilities: bardplugin.Capabilities{BlockDevice: true, Snapshots: true, Expand: true},
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bardplugin.Serve(ctx, sock, fb) }()
	waitForSocket(t, sock)

	cl, err := plugin.Dial(ctx, "fake", sock)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	// Identity + capabilities come from /info.
	if cl.Type() != "fake" || !cl.Capabilities().BlockDevice || !cl.Capabilities().Snapshots {
		t.Fatalf("unexpected type/caps: %s %+v", cl.Type(), cl.Capabilities())
	}

	// CreateVolume: fields pass through and the handle is built from the response.
	vol, err := cl.CreateVolume(ctx, &backend.CreateVolumeRequest{
		Name: "pvc-1", CapacityBytes: 1 << 20, Instance: "east",
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if fb.lastCreate.Name != "pvc-1" || fb.lastCreate.Instance != "east" {
		t.Fatalf("plugin saw wrong request: %+v", fb.lastCreate)
	}
	want := volumeid.Handle{Backend: "fake", Instance: "east", Location: "exports", Name: "obj-pvc-1"}
	if vol.Handle != want {
		t.Fatalf("handle: got %+v want %+v", vol.Handle, want)
	}
	if vol.Context["server"] != "nfs1" {
		t.Fatalf("context not propagated: %+v", vol.Context)
	}

	// NodeStage: the handle is serialized to a VolumeRef and fields pass through.
	if err := cl.NodeStage(ctx, &backend.NodeStageRequest{
		Handle: vol.Handle, StagingPath: "/stage", FsType: "nfs", MountFlags: []string{"noatime"},
	}); err != nil {
		t.Fatalf("NodeStage: %v", err)
	}
	if fb.lastStage.Volume.Name != "obj-pvc-1" || fb.lastStage.StagingPath != "/stage" || fb.lastStage.FsType != "nfs" {
		t.Fatalf("stage passthrough wrong: %+v", fb.lastStage)
	}

	// Error code mapping: AlreadyExists -> backend.ErrAlreadyExists.
	fb.createErr = bardplugin.Errorf(bardplugin.CodeAlreadyExists, "already there")
	_, err = cl.CreateVolume(ctx, &backend.CreateVolumeRequest{Name: "x", Instance: "east"})
	if !errors.Is(err, backend.ErrAlreadyExists) {
		t.Fatalf("want ErrAlreadyExists, got %v", err)
	}

	// Error code mapping: Unsupported -> backend.ErrUnsupported, which the
	// driver's toStatus maps to codes.Unimplemented -- a terminal, non-retried
	// CSI failure (see internal/driver/controller.go toStatus).
	fb.createErr = bardplugin.Errorf(bardplugin.CodeUnsupported, "not supported on this instance")
	_, err = cl.CreateVolume(ctx, &backend.CreateVolumeRequest{Name: "y", Instance: "east"})
	if !errors.Is(err, backend.ErrUnsupported) {
		t.Fatalf("want ErrUnsupported, got %v", err)
	}

	// An UNKNOWN code (what an older Bard sees when a newer-minor plugin uses
	// vocabulary it predates) must degrade to a generic error -- NOT to any
	// sentinel, and above all not silently to ErrUnsupported. The driver maps a
	// generic error to codes.Internal, which the CO reconciles with backoff,
	// whereas ErrUnsupported becomes a terminal codes.Unimplemented. Conflating
	// them would either retry a permanent failure forever or abandon a transient
	// one. This is exactly the mistranslation ContractMinor's gate now prevents.
	fb.createErr = bardplugin.Errorf(bardplugin.ErrorCode("SomeFutureCode"), "from a newer plugin")
	_, err = cl.CreateVolume(ctx, &backend.CreateVolumeRequest{Name: "z", Instance: "east"})
	switch {
	case err == nil:
		t.Fatal("an unknown error code must still be an error")
	case errors.Is(err, backend.ErrUnsupported),
		errors.Is(err, backend.ErrNotFound),
		errors.Is(err, backend.ErrAlreadyExists),
		errors.Is(err, backend.ErrInvalidArgument):
		t.Fatalf("unknown code must not map to a sentinel, got %v", err)
	case !strings.Contains(err.Error(), "from a newer plugin"):
		t.Fatalf("unknown code must preserve the plugin's message, got %v", err)
	}
}

// TestDialContractVersion verifies Dial's wire-contract gate. The gate is
// asymmetric within a major: an older or equal minor is accepted, a NEWER minor
// is refused (it may use vocabulary on an existing route that this Bard would
// mistranslate -- see ContractMinor), as is a foreign major or garbage.
func TestDialContractVersion(t *testing.T) {
	cases := []struct {
		version string
		wantErr bool
	}{
		{"", false}, // SDK fills in the current version; raw plugins may omit it (= 1.0)
		{bardplugin.ContractVersion, false},
		{"1.0", false}, // older minor: we understand everything it can say
		{"1.99", true}, // newer minor: refused, we cannot interpret its additions
		{"2.0", true},  // foreign major
		{"banana", true},
	}
	for _, c := range cases {
		t.Run("v="+c.version, func(t *testing.T) {
			dir := t.TempDir()
			sock := filepath.Join(dir, "p.sock")
			fb := &fakeBackend{info: bardplugin.Info{Type: "fake", ContractVersion: c.version}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() { _ = bardplugin.Serve(ctx, sock, fb) }()
			waitForSocket(t, sock)
			_, err := plugin.Dial(ctx, "fake", sock)
			if c.wantErr != (err != nil) {
				t.Fatalf("Dial with contractVersion %q: err = %v, wantErr = %v", c.version, err, c.wantErr)
			}
		})
	}
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
	t.Fatalf("socket %s never appeared", path)
}
