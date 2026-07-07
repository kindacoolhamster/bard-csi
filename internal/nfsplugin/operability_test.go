package nfsplugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// fsRunner models the export mount as a no-op and, at umount time, snapshots the
// mount directory so a test can assert what CreateVolume/DeleteVolume did to it.
// precreate seeds existing volume dirs (for delete tests).
type fsRunner struct {
	precreate []string
	seen      map[string]bool
	perms     map[string]os.FileMode
	umounts   int
}

func (r *fsRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	switch name {
	case "mount":
		target := args[len(args)-1]
		for _, d := range r.precreate {
			_ = os.MkdirAll(filepath.Join(target, d), 0o777)
		}
	case "umount":
		r.umounts++
		r.seen, r.perms = map[string]bool{}, map[string]os.FileMode{}
		entries, _ := os.ReadDir(args[len(args)-1])
		for _, e := range entries {
			r.seen[e.Name()] = true
			if fi, err := e.Info(); err == nil {
				r.perms[e.Name()] = fi.Mode().Perm()
			}
		}
	}
	return "", nil
}

func opBackend(run Runner, ic InstanceConfig) *Backend {
	return New(map[string]InstanceConfig{"east": ic}, run)
}

// subDir templates the directory from PVC/PV metadata; mountPermissions chmods it.
func TestNFSSubDirAndPermissions(t *testing.T) {
	run := &fsRunner{}
	b := opBackend(run, InstanceConfig{Server: "10.0.0.9", Export: "/srv/nfs"})
	resp, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name:     "pvc-123",
		Instance: "east",
		Parameters: map[string]string{
			paramSubDir:                        "${pvc.metadata.namespace}-${pvc.metadata.name}",
			paramMountPermissions:              "0770",
			"csi.storage.k8s.io/pvc/name":      "data",
			"csi.storage.k8s.io/pvc/namespace": "team-a",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Name != "team-a-data" {
		t.Fatalf("subDir should template to team-a-data, got %q", resp.Name)
	}
	if run.perms["team-a-data"] != 0o770 {
		t.Fatalf("mountPermissions 0770 not applied, got %o", run.perms["team-a-data"])
	}
}

// An unsubstituted subDir token falls back to the opaque hash, never a literal "${...}".
func TestNFSSubDirFallsBackWhenNoMetadata(t *testing.T) {
	run := &fsRunner{}
	b := opBackend(run, InstanceConfig{Server: "10.0.0.9", Export: "/srv/nfs"})
	resp, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "pvc-123", Instance: "east",
		Parameters: map[string]string{paramSubDir: "${pvc.metadata.name}"}, // no metadata supplied
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Name != volName("pvc-123") {
		t.Fatalf("expected fallback to the hashed name, got %q", resp.Name)
	}
}

// onDelete=retain leaves the data and does not even mount the export.
func TestNFSOnDeleteRetain(t *testing.T) {
	run := &fsRunner{}
	b := opBackend(run, InstanceConfig{Server: "10.0.0.9", Export: "/srv/nfs", OnDelete: "retain"})
	if err := b.DeleteVolume(context.Background(), &bardplugin.DeleteVolumeRequest{
		Volume: bardplugin.VolumeRef{Instance: "east", Location: "/srv/nfs", Name: "bard-x"},
	}); err != nil {
		t.Fatal(err)
	}
	if run.umounts != 0 {
		t.Fatalf("retain must not touch the export, but it mounted/unmounted")
	}
}

// onDelete=archive renames the directory instead of removing it; default removes it.
func TestNFSOnDeleteArchiveAndDefault(t *testing.T) {
	for _, tc := range []struct {
		policy      string
		wantArchive bool
	}{
		{"archive", true},
		{"", false}, // default delete
	} {
		run := &fsRunner{precreate: []string{"bard-x"}}
		b := opBackend(run, InstanceConfig{Server: "10.0.0.9", Export: "/srv/nfs", OnDelete: tc.policy})
		if err := b.DeleteVolume(context.Background(), &bardplugin.DeleteVolumeRequest{
			Volume: bardplugin.VolumeRef{Instance: "east", Location: "/srv/nfs", Name: "bard-x"},
		}); err != nil {
			t.Fatalf("%q: %v", tc.policy, err)
		}
		if run.seen["bard-x"] {
			t.Fatalf("%q: original dir must be gone", tc.policy)
		}
		if got := run.seen["archived-bard-x"]; got != tc.wantArchive {
			t.Fatalf("%q: archived dir present=%v, want %v", tc.policy, got, tc.wantArchive)
		}
	}
}
