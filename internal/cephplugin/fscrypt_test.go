package cephplugin

import (
	"context"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// stageDevice must NOT layer LUKS for an fscrypt (file) volume: fscrypt formats and
// mounts the raw device and applies encryption to a directory afterward, so stageDevice
// returns the raw device with no cryptsetup calls (the same as the unencrypted path),
// distinct from the block/LUKS path which returns a mapper. (The fscrypt key derivation
// and IsFsCrypt predicate are unit-tested in internal/cephenc.)
func TestStageDeviceFsCryptSkipsLuks(t *testing.T) {
	run := newEncRunner()
	b, _ := encBackend(t, run)
	req := &bardplugin.NodeStageRequest{
		Volume:      bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img"},
		StagingPath: "/run/bard/staging",
		Context:     map[string]string{paramEncrypted: "true", paramEncryptionType: "file"},
	}
	dev, err := b.stageDevice(context.Background(), nil, req, "/dev/rbd0")
	if err != nil {
		t.Fatal(err)
	}
	if dev != "/dev/rbd0" {
		t.Fatalf("fscrypt stageDevice must return the raw device, got %q", dev)
	}
	for _, c := range run.calls {
		if c[0] == "cryptsetup" {
			t.Fatalf("fscrypt volume must not touch LUKS; call: %v", c)
		}
	}
}
