package lvmplugin

import (
	"context"
	"errors"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// Compile-time proof the LVM backend opts into the optional capabilities.
var (
	_ bardplugin.CapacityReporter   = (*Backend)(nil)
	_ bardplugin.HealthReporter     = (*Backend)(nil)
	_ bardplugin.NodeSpaceReclaimer = (*Backend)(nil)
	_ bardplugin.VolumeLister       = (*Backend)(nil)
	_ bardplugin.SnapshotLister     = (*Backend)(nil)
)

func TestLVMGetCapacity(t *testing.T) {
	fr := &fakeRunner{results: map[string]func([]string) (string, error){
		"vgs": func([]string) (string, error) { return "  16106127360\n", nil },
	}}
	b := New(map[string]InstanceConfig{"east": {VG: "bard-vg"}}, fr)
	resp, err := b.GetCapacity(context.Background(), &bardplugin.GetCapacityRequest{Instance: "east"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.AvailableBytes != 16106127360 {
		t.Fatalf("want vg_free 16106127360, got %d", resp.AvailableBytes)
	}
}

func TestLVMVolumeHealth(t *testing.T) {
	// missing LV -> abnormal
	gone := &fakeRunner{results: map[string]func([]string) (string, error){
		"lvs": func([]string) (string, error) { return "", errors.New("Failed to find logical volume") },
	}}
	b := New(map[string]InstanceConfig{"east": {VG: "bard-vg"}}, gone)
	h, err := b.GetVolumeHealth(context.Background(), &bardplugin.GetVolumeHealthRequest{
		Volume: bardplugin.VolumeRef{Instance: "east", Location: "bard-vg", Name: "bard-x"}})
	if err != nil || !h.Abnormal {
		t.Fatalf("missing LV must be abnormal, got %+v err=%v", h, err)
	}
	// present LV -> healthy
	ok := &fakeRunner{results: map[string]func([]string) (string, error){
		"lvs": func([]string) (string, error) { return "  1073741824\n", nil },
	}}
	b2 := New(map[string]InstanceConfig{"east": {VG: "bard-vg"}}, ok)
	h2, _ := b2.GetVolumeHealth(context.Background(), &bardplugin.GetVolumeHealthRequest{
		Volume: bardplugin.VolumeRef{Instance: "east", Location: "bard-vg", Name: "bard-x"}})
	if h2.Abnormal {
		t.Fatalf("present LV must be healthy, got %+v", h2)
	}
}

func TestLVMNodeReclaimSpace(t *testing.T) {
	fr := &fakeRunner{}
	b := New(map[string]InstanceConfig{"east": {VG: "bard-vg"}}, fr)
	if _, err := b.NodeReclaimSpace(context.Background(), &bardplugin.NodeReclaimSpaceRequest{VolumePath: "/data"}); err != nil {
		t.Fatal(err)
	}
	if !fr.ran("fstrim") {
		t.Fatal("expected fstrim on the mounted path")
	}
	// raw block: nothing to trim
	frb := &fakeRunner{}
	b2 := New(map[string]InstanceConfig{"east": {VG: "bard-vg"}}, frb)
	if _, err := b2.NodeReclaimSpace(context.Background(), &bardplugin.NodeReclaimSpaceRequest{Block: true, VolumePath: "/dev/x"}); err != nil {
		t.Fatal(err)
	}
	if frb.ran("fstrim") {
		t.Fatal("raw block must not be trimmed (no filesystem)")
	}
}

// Listing filters to Bard objects: a volume is enumerated, the thin pool and the
// snapshot are not (the pool shares the bard- prefix but is excluded by lv_attr).
func TestLVMListVolumesAndSnapshots(t *testing.T) {
	vol, snap := lvName("a"), snapName("s")
	lvsOut := vol + "|1073741824|-wi-a-----|\n" +
		"bard-thin|4294967296|twi-aotz--|\n" +
		snap + "|1073741824|sri-a-s---|" + vol + "\n"
	fr := &fakeRunner{results: map[string]func([]string) (string, error){
		"lvs": func([]string) (string, error) { return lvsOut, nil },
	}}
	b := New(map[string]InstanceConfig{"east": {VG: "bard-vg"}}, fr)

	vols, err := b.ListVolumes(context.Background(), &bardplugin.ListVolumesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(vols.Entries) != 1 || vols.Entries[0].Volume.Name != vol || vols.Entries[0].CapacityBytes != 1073741824 {
		t.Fatalf("expected only the volume LV, got %+v", vols.Entries)
	}

	snaps, err := b.ListSnapshots(context.Background(), &bardplugin.ListSnapshotsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps.Entries) != 1 || snaps.Entries[0].Snapshot.Name != snap || snaps.Entries[0].SourceVolume.Name != vol {
		t.Fatalf("expected the snapshot with its origin, got %+v", snaps.Entries)
	}
}
