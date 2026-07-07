package lvmplugin

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// A thin clone inherits its SOURCE's virtual size; CreateVolume must grow it to
// the request and report the real size.
func TestThinCloneGrowsToRequest(t *testing.T) {
	cloned, extended := false, false
	fr := &fakeRunner{results: map[string]func([]string) (string, error){
		"lvs": func(args []string) (string, error) {
			a := strings.Join(args, " ")
			if strings.Contains(a, "lv_attr") {
				return "  Vwi-aotz--\n", nil // the source is thin
			}
			// lv_size of the clone target
			switch {
			case !cloned:
				return "", errors.New("Failed to find logical volume")
			case extended:
				return "  2147483648\n", nil
			default:
				return "  1073741824\n", nil
			}
		},
		"lvcreate": func([]string) (string, error) { cloned = true; return "", nil },
		"lvextend": func([]string) (string, error) { extended = true; return "", nil },
	}}
	b := New(map[string]InstanceConfig{"east": {VG: "bard-vg", ThinPool: "bard-thin"}}, fr)

	src := bardplugin.VolumeRef{Instance: "east", Location: "bard-vg", Name: "bard-src"}
	resp, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "bigcopy", Instance: "east", CapacityBytes: 2 << 30, SourceVolume: &src,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !extended {
		t.Fatalf("clone must be lvextend'ed to the request; calls: %v", fr.calls)
	}
	if resp.CapacityBytes != 2<<30 {
		t.Fatalf("capacity = %d, want %d", resp.CapacityBytes, int64(2<<30))
	}
}

// NodeStage must grow the filesystem to the device (a clone into a larger volume
// carries the source's smaller filesystem).
func TestNodeStageGrowsFilesystem(t *testing.T) {
	fr := &fakeRunner{results: map[string]func([]string) (string, error){
		"blkid":   func([]string) (string, error) { return "ext4\n", nil }, // already formatted (a clone)
		"findmnt": func([]string) (string, error) { return "", errors.New("not found") },
	}}
	b := New(map[string]InstanceConfig{"east": {VG: "bard-vg"}}, fr)

	if err := b.NodeStage(context.Background(), &bardplugin.NodeStageRequest{
		Volume:      bardplugin.VolumeRef{Instance: "east", Location: "bard-vg", Name: "bard-x"},
		StagingPath: t.TempDir(),
		FsType:      "ext4",
	}); err != nil {
		t.Fatal(err)
	}
	if !fr.ran("resize2fs") {
		t.Fatalf("NodeStage must grow the filesystem to the device; calls: %v", fr.calls)
	}
}

// An equal-size lvextend is a no-op, whatever the running lvm2 calls it (the
// "No size change." variant was caught live by hack/lvm-plugin-test.sh).
func TestIsNotLargerVariants(t *testing.T) {
	for _, msg := range []string{
		"lvextend: New size (256 extents) matches existing size (256 extents)",
		"New size given (10 extents) not larger than existing size (256 extents)",
		"lvextend: exit status 5: No size change.",
	} {
		if !isNotLarger(errors.New(msg)) {
			t.Fatalf("message must classify as a no-op resize: %q", msg)
		}
	}
	if isNotLarger(errors.New("Insufficient free space")) {
		t.Fatal("a real lvextend failure must not classify as a no-op")
	}
}

// A snapshot LV name is derived from the CSI name in a VG-wide namespace: the
// same CSI name against a different source must be AlreadyExists, not a silent
// reuse of the other source's snapshot.
func TestSnapshotOriginConflict(t *testing.T) {
	mkRunner := func(existingOrigin string) *fakeRunner {
		return &fakeRunner{results: map[string]func([]string) (string, error){
			"lvs": func(args []string) (string, error) {
				a := strings.Join(args, " ")
				switch {
				case strings.Contains(a, "lv_attr"):
					return "  Vwi-aotz--\n", nil // source is thin
				case strings.Contains(a, "origin"):
					return "  " + existingOrigin + "\n", nil
				default: // lv_size (snapshot response size read)
					return "  1073741824\n", nil
				}
			},
		}}
	}
	src := bardplugin.VolumeRef{Instance: "east", Location: "bard-vg", Name: "bard-src"}

	// Existing snapshot from ANOTHER origin: refused.
	b := New(map[string]InstanceConfig{"east": {VG: "bard-vg"}}, mkRunner("bard-other"))
	if _, err := b.CreateSnapshot(context.Background(), &bardplugin.CreateSnapshotRequest{
		Name: "s1", SourceVolume: src,
	}); err == nil || !strings.Contains(err.Error(), "different source") {
		t.Fatalf("mismatched origin must be AlreadyExists, got %v", err)
	}

	// Existing snapshot from THIS origin: idempotent retry succeeds.
	b = New(map[string]InstanceConfig{"east": {VG: "bard-vg"}}, mkRunner("bard-src"))
	if _, err := b.CreateSnapshot(context.Background(), &bardplugin.CreateSnapshotRequest{
		Name: "s1", SourceVolume: src,
	}); err != nil {
		t.Fatalf("matching-origin retry must succeed, got %v", err)
	}
}
