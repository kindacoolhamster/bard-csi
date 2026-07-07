package cephfsplugin

import (
	"context"
	"strings"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

func nfsMounterBackend(run Runner) *Backend {
	return New(map[string]ClusterConfig{"east": {
		Monitors:   []string{"10.0.0.10:6789"},
		FSName:     "cephfs",
		UserID:     "admin",
		Mounter:    mounterNFS,
		NFSCluster: "bard-nfs",
		NFSServer:  "10.0.0.9",
	}}, "", run)
}

// A mounter:nfs volume is still a CephFS subvolume, plus a Ganesha export; the
// controller creates both and carries the gateway endpoint + pseudo path to the node.
func TestCephFSNFSCreateExportsSubvolume(t *testing.T) {
	run := &cephRunner{}
	b := nfsMounterBackend(run)
	resp, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{Name: "v", Instance: "east"})
	if err != nil {
		t.Fatal(err)
	}
	pseudo := nfsPseudoPath(resp.Name)

	if !run.ran("fs", "subvolume", "create", "cephfs", resp.Name) {
		t.Fatalf("expected the subvolume to be created; calls: %v", run.calls)
	}
	if !run.ran("nfs", "export", "create", "cephfs", "--cluster-id", "bard-nfs", "--pseudo-path", pseudo, "--fsname", "cephfs", "--path") {
		t.Fatalf("expected an nfs export of the subvolume path; calls: %v", run.calls)
	}
	if resp.Context[ctxNFSServer] != "10.0.0.9" || resp.Context[ctxNFSPseudo] != pseudo {
		t.Fatalf("volume context must carry the gateway endpoint + pseudo path, got %v", resp.Context)
	}
}

// DeleteVolume drops the export before the subvolume (reconstructing the pseudo
// path from the subvolume name, since DeleteVolume carries no context).
func TestCephFSNFSDeleteRemovesExport(t *testing.T) {
	run := &cephRunner{}
	b := nfsMounterBackend(run)
	sub := subvolName(subvolNamePrefix, "v")
	if err := b.DeleteVolume(context.Background(), &bardplugin.DeleteVolumeRequest{
		Volume: bardplugin.VolumeRef{Instance: "east", Location: "cephfs", Name: sub},
	}); err != nil {
		t.Fatal(err)
	}
	pseudo := nfsPseudoPath(sub)
	rmExport, rmSub := -1, -1
	for i, c := range run.calls {
		j := strings.Join(c, " ")
		if strings.Contains(j, "nfs export rm bard-nfs "+pseudo) {
			rmExport = i
		}
		if strings.Contains(j, "fs subvolume rm cephfs "+sub) {
			rmSub = i
		}
	}
	if rmExport < 0 || rmSub < 0 {
		t.Fatalf("expected both export rm and subvolume rm; calls: %v", run.calls)
	}
	if rmExport > rmSub {
		t.Fatalf("export must be removed before the subvolume; calls: %v", run.calls)
	}
}

// NodeStage for mounter:nfs mounts the Ganesha export over NFS -- no ceph client,
// no cephx key on the node -- and is idempotent across retries.
func TestCephFSNFSNodeStageMountsNFS(t *testing.T) {
	run := newNodeRunner()
	b := nfsMounterBackend(run)
	ctx := context.Background()
	staging := t.TempDir() + "/stage"
	vol := bardplugin.VolumeRef{Instance: "east", Location: "cephfs", Name: "bard-x"}
	volCtx := map[string]string{ctxPath: "/volumes/_nogroup/x/uuid", ctxNFSServer: "10.0.0.9", ctxNFSPseudo: "/bard-x"}

	for i := 0; i < 2; i++ {
		if err := b.NodeStage(ctx, &bardplugin.NodeStageRequest{Volume: vol, StagingPath: staging, Context: volCtx}); err != nil {
			t.Fatalf("stage %d: %v", i, err)
		}
	}
	var nfsMounts int
	for _, c := range run.calls {
		j := strings.Join(c, " ")
		if c[0] == "mount" && strings.Contains(j, "-t nfs") && strings.Contains(j, "10.0.0.9:/bard-x") {
			nfsMounts++
		}
		if c[0] == "ceph-fuse" || (c[0] == "mount" && strings.Contains(j, "-t ceph")) {
			t.Fatalf("nfs mounter must not invoke the ceph client; call: %v", c)
		}
	}
	if nfsMounts != 1 {
		t.Fatalf("nfs mount must run exactly once across two NodeStage calls, ran %d", nfsMounts)
	}
}
