package cephfsplugin

import (
	"context"
	"strings"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

var _ bardplugin.VolumeModifier = (*Backend)(nil)

// ModifyVolume pins a live subvolume's MDS placement via VolumeAttributesClass
// parameters, addressing the subvolume in its group.
func TestModifyVolumePinsSubvolume(t *testing.T) {
	b := sgBackend()
	run := b.run.(*cephRunner)
	ctx := context.Background()

	vol, err := b.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: "pvc-1", CapacityBytes: 1 << 30, Instance: "east",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.ModifyVolume(ctx, &bardplugin.ModifyVolumeRequest{
		Volume:        bardplugin.VolumeRef{Instance: "east", Location: vol.Location, Name: vol.Name},
		MutableParams: map[string]string{mutablePinType: "distributed", mutablePinSetting: "1"},
	}); err != nil {
		t.Fatal(err)
	}
	if !run.ran("fs", "subvolume", "pin", "cephfs", vol.Name, "distributed", "1", "--group-name", "csi") {
		t.Fatalf("expected the pin in group csi; calls: %v", run.calls)
	}
}

// A create-time VolumeAttributesClass applies the pin as part of provisioning.
func TestCreateVolumeWithPin(t *testing.T) {
	b := sgBackend()
	run := b.run.(*cephRunner)
	vol, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "pvc-2", CapacityBytes: 1 << 30, Instance: "east",
		MutableParams: map[string]string{mutablePinType: "export", mutablePinSetting: "0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !run.ran("fs", "subvolume", "pin", "cephfs", vol.Name, "export", "0") {
		t.Fatalf("expected the create-time pin; calls: %v", run.calls)
	}
}

// The validation matrix: unknown keys, incomplete pairs, bad per-type settings.
func TestValidateMutableParams(t *testing.T) {
	valid := []map[string]string{
		nil,
		{mutablePinType: "export", mutablePinSetting: "-1"},
		{mutablePinType: "distributed", mutablePinSetting: "0"},
		{mutablePinType: "random", mutablePinSetting: "0.5"},
	}
	for _, p := range valid {
		if err := validateMutableParams(p); err != nil {
			t.Fatalf("params %v must validate: %v", p, err)
		}
	}
	invalid := []map[string]string{
		{"qosIopsLimit": "100"},                                 // rbd's key, not cephfs's
		{mutablePinType: "export"},                              // setting missing
		{mutablePinSetting: "1"},                                // type missing
		{mutablePinType: "exports", mutablePinSetting: "1"},     // bad type
		{mutablePinType: "export", mutablePinSetting: "one"},    // rank not an int
		{mutablePinType: "distributed", mutablePinSetting: "2"}, // not 0/1
		{mutablePinType: "random", mutablePinSetting: "1.5"},    // > 1.0
		{mutablePinType: "random", mutablePinSetting: "-0.1"},   // < 0.0
		{mutablePinType: "random", mutablePinSetting: "half"},   // not a float
	}
	for _, p := range invalid {
		if err := validateMutableParams(p); err == nil {
			t.Fatalf("params %v must be rejected", p)
		}
	}
}

// Modifying a missing subvolume is NotFound; a bad VAC never reaches ceph.
func TestModifyVolumeErrors(t *testing.T) {
	b := sgBackend()
	run := b.run.(*cephRunner)
	ctx := context.Background()

	_, err := b.ModifyVolume(ctx, &bardplugin.ModifyVolumeRequest{
		Volume:        bardplugin.VolumeRef{Instance: "east", Location: "cephfs/csi", Name: "bard-0011223344556677"},
		MutableParams: map[string]string{mutablePinType: "bogus", mutablePinSetting: "1"},
	})
	if err == nil || !strings.Contains(err.Error(), "pinType") {
		t.Fatalf("invalid pinType must be rejected, got %v", err)
	}
	if run.ran("fs", "subvolume", "pin") {
		t.Fatalf("a rejected VAC must not reach ceph; calls: %v", run.calls)
	}
}
