package lvmplugin

import (
	"context"
	"errors"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

func (f *fakeRunner) callArgs(name string) []string {
	for _, c := range f.calls {
		if c[0] == name {
			return c
		}
	}
	return nil
}

func hasArg(c []string, want string) bool {
	for _, a := range c {
		if a == want {
			return true
		}
	}
	return false
}

// provisionRunner reports an LV missing until lvcreate runs, then a fixed size,
// and answers lv_attr queries as a thin volume -- the create-then-size flow
// CreateVolume drives, plus the thin detection snapshot/clone use.
func provisionRunner() *fakeRunner {
	created := false
	return &fakeRunner{results: map[string]func([]string) (string, error){
		"lvs": func(args []string) (string, error) {
			if hasArg(args, "lv_attr") {
				return "Vwi-a-tz--\n", nil // a thin volume
			}
			if !created {
				return "", errors.New("Failed to find logical volume")
			}
			return "1073741824\n", nil
		},
		"lvcreate": func([]string) (string, error) { created = true; return "", nil },
	}}
}

// A thin instance provisions from the pool with -T/-V, never -L.
func TestThinProvisioning(t *testing.T) {
	fr := provisionRunner()
	b := New(map[string]InstanceConfig{"east": {VG: "bard-vg", ThinPool: "bard-thin"}}, fr)
	if _, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "v", Instance: "east", CapacityBytes: 1 << 30,
	}); err != nil {
		t.Fatal(err)
	}
	c := fr.callArgs("lvcreate")
	if !hasArg(c, "-T") || !hasArg(c, "bard-vg/bard-thin") || !hasArg(c, "-V") {
		t.Fatalf("thin create must use -T pool -V; got %v", c)
	}
	if hasArg(c, "-L") {
		t.Fatalf("thin create must not fully allocate with -L; got %v", c)
	}
}

// The thinPool StorageClass parameter selects thin on an otherwise-thick
// instance (per-PVC choice), overriding the instance default.
func TestThinPoolStorageClassParam(t *testing.T) {
	fr := provisionRunner()
	b := New(map[string]InstanceConfig{"east": {VG: "bard-vg"}}, fr) // no instance default
	if _, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "v", Instance: "east", CapacityBytes: 1 << 30,
		Parameters: map[string]string{paramThinPool: "sc-pool"},
	}); err != nil {
		t.Fatal(err)
	}
	c := fr.callArgs("lvcreate")
	if !hasArg(c, "-T") || !hasArg(c, "bard-vg/sc-pool") {
		t.Fatalf("the StorageClass thinPool param must select that pool; got %v", c)
	}
}

// A thick instance (no pool) still fully allocates with -L.
func TestThickProvisioningUnchanged(t *testing.T) {
	fr := provisionRunner()
	b := New(map[string]InstanceConfig{"east": {VG: "bard-vg"}}, fr)
	if _, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "v", Instance: "east", CapacityBytes: 1 << 30,
	}); err != nil {
		t.Fatal(err)
	}
	c := fr.callArgs("lvcreate")
	if !hasArg(c, "-L") || hasArg(c, "-T") {
		t.Fatalf("thick create must use -L and no -T; got %v", c)
	}
}

// Thin snapshot create (read-only CoW) + delete.
func TestThinSnapshot(t *testing.T) {
	fr := &fakeRunner{results: map[string]func([]string) (string, error){
		"lvs": func(args []string) (string, error) {
			if hasArg(args, "lv_attr") {
				return "Vwi-a-tz--\n", nil
			}
			if hasArg(args, "origin") {
				// The snapshot LV does not exist yet (fresh create).
				return "", errors.New("Failed to find logical volume")
			}
			return "1073741824\n", nil
		},
	}}
	b := New(map[string]InstanceConfig{"east": {VG: "bard-vg", ThinPool: "bard-thin"}}, fr)
	resp, err := b.CreateSnapshot(context.Background(), &bardplugin.CreateSnapshotRequest{
		Name:         "snap1",
		SourceVolume: bardplugin.VolumeRef{Instance: "east", Location: "bard-vg", Name: "bard-x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := fr.callArgs("lvcreate")
	if !hasArg(c, "-s") || !hasArg(c, "-pr") || !hasArg(c, "bard-vg/bard-x") {
		t.Fatalf("snapshot must be a read-only thin snapshot of the origin; got %v", c)
	}
	if resp.Name != snapName("snap1") || !resp.ReadyToUse {
		t.Fatalf("unexpected snapshot response %+v", resp)
	}
	if err := b.DeleteSnapshot(context.Background(), &bardplugin.DeleteSnapshotRequest{
		Snapshot: bardplugin.VolumeRef{Instance: "east", Location: "bard-vg", Name: resp.Name},
	}); err != nil {
		t.Fatal(err)
	}
	if !fr.ran("lvremove") {
		t.Fatalf("delete must lvremove the snapshot; calls %v", fr.calls)
	}
}

// Snapshots and clone/restore are rejected when the source is a thick LV.
func TestThinRequiredForSnapshotAndClone(t *testing.T) {
	thick := &fakeRunner{results: map[string]func([]string) (string, error){
		"lvs": func(args []string) (string, error) {
			if hasArg(args, "lv_attr") {
				return "-wi-a-----\n", nil // a thick (linear) volume
			}
			return "", errors.New("Failed to find logical volume")
		},
	}}
	b := New(map[string]InstanceConfig{"east": {VG: "bard-vg"}}, thick)
	if _, err := b.CreateSnapshot(context.Background(), &bardplugin.CreateSnapshotRequest{
		Name: "s", SourceVolume: bardplugin.VolumeRef{Instance: "east", Location: "bard-vg", Name: "bard-x"},
	}); err == nil {
		t.Fatal("snapshot on a thick instance must be rejected")
	}
	if _, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "c", Instance: "east",
		SourceSnapshot: &bardplugin.VolumeRef{Instance: "east", Location: "bard-vg", Name: "snap-abc"},
	}); err == nil {
		t.Fatal("clone/restore on a thick instance must be rejected")
	}
}

// Restore/clone makes an activated writable thin snapshot of the source.
func TestThinCloneRestore(t *testing.T) {
	fr := provisionRunner()
	b := New(map[string]InstanceConfig{"east": {VG: "bard-vg", ThinPool: "bard-thin"}}, fr)
	if _, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "restored", Instance: "east",
		SourceSnapshot: &bardplugin.VolumeRef{Instance: "east", Location: "bard-vg", Name: "snap-abc"},
	}); err != nil {
		t.Fatal(err)
	}
	c := fr.callArgs("lvcreate")
	if !hasArg(c, "-s") || !hasArg(c, "bard-vg/snap-abc") {
		t.Fatalf("restore must snapshot the source; got %v", c)
	}
	if !fr.ran("lvchange") {
		t.Fatalf("restore must activate the clone (lvchange -ay); calls %v", fr.calls)
	}
}
