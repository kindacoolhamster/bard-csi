package lvmplugin

import (
	"context"
	"errors"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// State-changing lvm commands must carry the self-managed-/dev config: in a
// container no udev serves an activation, so an inactive thin pool (the first
// volume after a node reboot, or after the last thin LV was removed) fails to
// activate without it -- the bug the iSCSI plugin, which shares this thin-LV
// logic, hit live in-cluster. Reads (lvs) stay plain.
func TestLvmSelfManagedDevNodes(t *testing.T) {
	fr := provisionRunner()
	b := New(map[string]InstanceConfig{"east": {VG: "bard-vg", ThinPool: "bard-thin"}}, fr)
	if _, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "v", Instance: "east", CapacityBytes: 1 << 30,
	}); err != nil {
		t.Fatal(err)
	}
	c := fr.callArgs("lvcreate")
	if !hasArg(c, "--config") || !hasArg(c, lvmUdevConfig) {
		t.Fatalf("lvcreate must disable udev sync/rules (self-managed dev nodes); got %v", c)
	}
	if lvs := fr.callArgs("lvs"); hasArg(lvs, "--config") {
		t.Fatalf("plain reads must not carry the activation config; got %v", lvs)
	}
}

// CreateSnapshot records the source LV in a tag (bardsrc.<lv>): provenance
// that outlives the origin attribute, which lvm clears once the source LV is
// deleted.
func TestSnapshotRecordsSourceTag(t *testing.T) {
	fr := &fakeRunner{results: map[string]func([]string) (string, error){
		"lvs": func(args []string) (string, error) {
			if hasArg(args, "lv_attr") {
				return "Vwi-a-tz--\n", nil
			}
			if hasArg(args, "origin") {
				return "", errors.New("Failed to find logical volume")
			}
			return "1073741824\n", nil
		},
	}}
	b := New(map[string]InstanceConfig{"east": {VG: "bard-vg", ThinPool: "bard-thin"}}, fr)
	if _, err := b.CreateSnapshot(context.Background(), &bardplugin.CreateSnapshotRequest{
		Name:         "s",
		SourceVolume: bardplugin.VolumeRef{Instance: "east", Location: "bard-vg", Name: "bard-x"},
	}); err != nil {
		t.Fatal(err)
	}
	if c := fr.callArgs("lvcreate"); !hasArg(c, "--addtag") || !hasArg(c, srcTagPrefix+"bard-x") {
		t.Fatalf("snapshot create must record its source in a %s tag; got %v", srcTagPrefix, c)
	}
}

// ListSnapshots must keep reporting a snapshot after its source volume is
// deleted: the origin column is empty then and the create-time tag supplies
// the source. A pre-tag snapshot with neither stays dropped (no provenance).
func TestListSnapshotsSurvivesSourceDeletion(t *testing.T) {
	vol, snap := lvName("a"), snapName("s")
	lvsOut := snap + "|1073741824|Vri---tz-k||" + srcTagPrefix + vol + "\n" +
		"snap-feedfacefeedface|1073741824|Vri---tz-k||\n" // pre-tag orphan
	fr := &fakeRunner{results: map[string]func([]string) (string, error){
		"lvs": func([]string) (string, error) { return lvsOut, nil },
	}}
	b := New(map[string]InstanceConfig{"east": {VG: "bard-vg"}}, fr)
	snaps, err := b.ListSnapshots(context.Background(), &bardplugin.ListSnapshotsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps.Entries) != 1 || snaps.Entries[0].Snapshot.Name != snap || snaps.Entries[0].SourceVolume.Name != vol {
		t.Fatalf("expected the tagged snapshot with its recorded source, got %+v", snaps.Entries)
	}
}
