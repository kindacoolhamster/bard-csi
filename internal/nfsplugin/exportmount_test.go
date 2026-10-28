package nfsplugin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// Most fake runners in this package model the export mount as a no-op, so the
// files an operation writes land in the temp mountpoint itself -- which
// withExportMounted (rightly) refuses to delete recursively. Keep those
// leftovers in a scratch TMPDIR that the test binary removes on exit.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "nfsplugin-test-")
	if err != nil {
		panic(err)
	}
	os.Setenv("TMPDIR", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// mountRunner models the export mount faithfully enough to catch a recursive
// delete through it: mount moves the "server's" files into the mountpoint, a
// successful umount moves them back out, and -- like exec.CommandContext -- no
// command runs once its ctx is done.
type mountRunner struct {
	server     string             // stands in for the export's contents on the NFS server
	cancel     context.CancelFunc // non-nil: tar cancels the request mid-op
	umountFail bool
	umounts    int
}

func (r *mountRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	switch name {
	case "mount":
		return "", moveEntries(r.server, args[len(args)-1])
	case "umount":
		r.umounts++
		if r.umountFail {
			return "", errors.New("umount: target is busy")
		}
		return "", moveEntries(args[len(args)-1], r.server)
	case "tar":
		if r.cancel != nil {
			r.cancel()
			return "", context.Canceled
		}
	}
	return "", nil
}

func moveEntries(from, to string) error {
	entries, err := os.ReadDir(from)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.Rename(filepath.Join(from, e.Name()), filepath.Join(to, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// seedExport gives the fake server an unrelated volume whose survival is the
// assertion, plus the snapshot source.
func seedExport(t *testing.T) string {
	t.Helper()
	server := t.TempDir()
	for _, f := range []string{"bard-victim/data.txt", "bard-src/file.txt"} {
		p := filepath.Join(server, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return server
}

// A request cancelled mid-operation (deadline, client disconnect, plugin
// shutdown) must still unmount the export before cleanup -- the cleanup used to
// RemoveAll the temp mountpoint after a umount that never ran on the cancelled
// ctx, deleting every volume on the export.
func TestNFSCancelledOpDoesNotWipeExport(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	server := seedExport(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := &mountRunner{server: server, cancel: cancel}
	b := opBackend(run, InstanceConfig{Server: "10.0.0.9", Export: "/srv/nfs"})

	_, err := b.CreateSnapshot(ctx, &bardplugin.CreateSnapshotRequest{
		Name:         "snap1",
		SourceVolume: bardplugin.VolumeRef{Instance: "east", Location: "/srv/nfs", Name: "bard-src"},
	})
	if err == nil {
		t.Fatal("CreateSnapshot should fail when its request is cancelled mid-archive")
	}
	if run.umounts != 1 {
		t.Fatalf("export should be unmounted once despite the cancelled ctx, got %d umount(s)", run.umounts)
	}
	if _, err := os.Stat(filepath.Join(server, "bard-victim", "data.txt")); err != nil {
		t.Fatalf("unrelated volume on the export was destroyed: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(os.TempDir(), "bard-nfs-*")); len(left) != 0 {
		t.Fatalf("unmounted temp dir should be removed, found %v", left)
	}
}

// When the unmount itself fails, the export is still attached: the operation
// must report the failure and leave the mountpoint (and so the export) alone.
func TestNFSUnmountFailureLeavesExportIntact(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	server := seedExport(t)
	run := &mountRunner{server: server, umountFail: true}
	b := opBackend(run, InstanceConfig{Server: "10.0.0.9", Export: "/srv/nfs"})

	_, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{Name: "pvc-1", Instance: "east"})
	var se *bardplugin.StatusError
	if !errors.As(err, &se) || se.Code != bardplugin.CodeInternal {
		t.Fatalf("a failed unmount must fail the call as Internal, got %v", err)
	}
	left, _ := filepath.Glob(filepath.Join(os.TempDir(), "bard-nfs-*"))
	if len(left) != 1 {
		t.Fatalf("still-mounted temp dir must be left in place, found %v", left)
	}
	if _, err := os.Stat(filepath.Join(left[0], "bard-victim", "data.txt")); err != nil {
		t.Fatalf("export contents behind a still-attached mount were deleted: %v", err)
	}
}
