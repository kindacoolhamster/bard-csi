package iscsiplugin

import (
	"context"
	"errors"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// iSCSI opts into capacity/health/trim/listing (but not snapshots).
var (
	_ bardplugin.CapacityReporter   = (*Backend)(nil)
	_ bardplugin.HealthReporter     = (*Backend)(nil)
	_ bardplugin.NodeSpaceReclaimer = (*Backend)(nil)
	_ bardplugin.VolumeLister       = (*Backend)(nil)
)

func TestISCSIGetCapacity(t *testing.T) {
	fr := &fakeRunner{results: map[string]func([]string) (string, error){
		"vgs": func([]string) (string, error) { return "  8053063680\n", nil },
	}}
	b := New(eastInst(), "", "", "", "", fr)
	resp, err := b.GetCapacity(context.Background(), &bardplugin.GetCapacityRequest{Instance: "east"})
	if err != nil || resp.AvailableBytes != 8053063680 {
		t.Fatalf("want vg_free 8053063680, got %+v err=%v", resp, err)
	}
}

func TestISCSIVolumeHealth(t *testing.T) {
	gone := &fakeRunner{results: map[string]func([]string) (string, error){
		"lvs": func([]string) (string, error) { return "", errors.New("Failed to find logical volume") },
	}}
	b := New(eastInst(), "", "", "", "", gone)
	h, err := b.GetVolumeHealth(context.Background(), &bardplugin.GetVolumeHealthRequest{
		Volume: bardplugin.VolumeRef{Instance: "east", Location: "bard-vg", Name: "bard-x"}})
	if err != nil || !h.Abnormal {
		t.Fatalf("missing LV must be abnormal, got %+v err=%v", h, err)
	}
}

func TestISCSINodeReclaimSpace(t *testing.T) {
	fr := &fakeRunner{}
	b := New(eastInst(), "", "", "", "", fr)
	if _, err := b.NodeReclaimSpace(context.Background(), &bardplugin.NodeReclaimSpaceRequest{VolumePath: "/data"}); err != nil {
		t.Fatal(err)
	}
	if !fr.ran("fstrim") {
		t.Fatal("expected fstrim")
	}
}

func TestISCSIListVolumes(t *testing.T) {
	vol := lvName("a")
	lvsOut := vol + "|1073741824|-wi-a-----\n" + "bard-thin|4294967296|twi-aotz--\n"
	fr := &fakeRunner{results: map[string]func([]string) (string, error){
		"lvs": func([]string) (string, error) { return lvsOut, nil },
	}}
	b := New(eastInst(), "", "", "", "", fr)
	vols, err := b.ListVolumes(context.Background(), &bardplugin.ListVolumesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(vols.Entries) != 1 || vols.Entries[0].Volume.Name != vol {
		t.Fatalf("expected only the volume LV (not the thin pool), got %+v", vols.Entries)
	}
}
