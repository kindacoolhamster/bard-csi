package lvmplugin

import (
	"context"
	"errors"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

func ranSeq(calls [][]string, want ...string) bool {
	for _, c := range calls {
		if len(c) < len(want) {
			continue
		}
		ok := true
		for i, w := range want {
			if c[i] != w {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// A btrfs LV is formatted with mkfs.btrfs and mounted -t btrfs.
func TestStageBtrfs(t *testing.T) {
	fr := &fakeRunner{}
	b := New(map[string]InstanceConfig{"east": {VG: "bard-vg"}}, fr)
	err := b.NodeStage(context.Background(), &bardplugin.NodeStageRequest{
		Volume:      bardplugin.VolumeRef{Instance: "east", Location: "bard-vg", Name: "lv"},
		StagingPath: t.TempDir(),
		FsType:      "btrfs",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ranSeq(fr.calls, "mkfs.btrfs") {
		t.Fatalf("expected mkfs.btrfs; calls: %v", fr.calls)
	}
	if !ranSeq(fr.calls, "mount", "-t", "btrfs") {
		t.Fatalf("expected mount -t btrfs; calls: %v", fr.calls)
	}
}

// An unsupported fsType is rejected with InvalidArgument before any mkfs.
func TestStageUnsupportedFsType(t *testing.T) {
	fr := &fakeRunner{}
	b := New(map[string]InstanceConfig{"east": {VG: "bard-vg"}}, fr)
	err := b.NodeStage(context.Background(), &bardplugin.NodeStageRequest{
		Volume:      bardplugin.VolumeRef{Instance: "east", Location: "bard-vg", Name: "lv"},
		StagingPath: t.TempDir(),
		FsType:      "zfs",
	})
	var se *bardplugin.StatusError
	if !errors.As(err, &se) || se.Code != bardplugin.CodeInvalidArg {
		t.Fatalf("expected InvalidArgument for unsupported fsType, got %v", err)
	}
	for _, c := range fr.calls {
		if len(c) > 0 && c[0] == "mkfs.zfs" {
			t.Fatalf("must not mkfs an unsupported fsType; calls: %v", fr.calls)
		}
	}
}

// NodeExpand grows btrfs by mountpoint, never resize2fs.
func TestNodeExpandBtrfs(t *testing.T) {
	fr := &fakeRunner{results: map[string]func([]string) (string, error){
		"findmnt": func(args []string) (string, error) {
			if hasArg(args, "FSTYPE") {
				return "btrfs", nil
			}
			return "/dev/bard-vg/lv", nil // SOURCE
		},
	}}
	b := New(map[string]InstanceConfig{"east": {VG: "bard-vg"}}, fr)
	if _, err := b.NodeExpand(context.Background(), &bardplugin.NodeExpandRequest{VolumePath: "/var/lib/kubelet/pods/x/vol"}); err != nil {
		t.Fatal(err)
	}
	if !ranSeq(fr.calls, "btrfs", "filesystem", "resize", "max", "/var/lib/kubelet/pods/x/vol") {
		t.Fatalf("expected btrfs filesystem resize max <mountpoint>; calls: %v", fr.calls)
	}
	if ranSeq(fr.calls, "resize2fs") {
		t.Fatalf("btrfs must not be grown with resize2fs; calls: %v", fr.calls)
	}
}
