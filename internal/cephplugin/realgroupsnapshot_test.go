package cephplugin

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// TestRealGroupSnapshot exercises CSI VolumeGroupSnapshot against a *real*
// cluster using the real `rbd` CLI: the exact path unit tests (against the fake
// runner) cannot prove, since a prior version of this code had a JSON field-name
// bug (`group_snap` vs Ceph's actual `"group snap"`, with a space) that the fake
// runner mirrored identically -- every unit test passed while the feature was
// completely non-functional against real Ceph. Skipped unless BARD_CEPH_TEST=1.
//
//	BARD_CEPH_TEST=1 CEPH_MON=192.168.1.225:3300 CEPH_POOL=k8s-csi-test \
//	CEPH_USER=k8s-csi-test CEPH_KEY=AQ...== \
//	go test ./internal/cephplugin/ -run TestRealGroupSnapshot -v
func TestRealGroupSnapshot(t *testing.T) {
	if os.Getenv("BARD_CEPH_TEST") != "1" {
		t.Skip("set BARD_CEPH_TEST=1 (and CEPH_MON/POOL/USER/KEY) to run against real Ceph")
	}
	mon, pool, user, key := os.Getenv("CEPH_MON"), os.Getenv("CEPH_POOL"), os.Getenv("CEPH_USER"), os.Getenv("CEPH_KEY")
	if mon == "" || pool == "" || user == "" || key == "" {
		t.Fatal("CEPH_MON, CEPH_POOL, CEPH_USER and CEPH_KEY must all be set")
	}

	const instance = "real"
	be := New(map[string]ClusterConfig{
		instance: {Monitors: []string{mon}, Pool: pool, UserID: user},
	}, "", "", nil)
	secrets := map[string]string{"userID": user, "userKey": key}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	suffix := time.Now().Format("150405")
	var volA, volB bardplugin.VolumeRef
	for _, name := range []string{"gs-real-a-" + suffix, "gs-real-b-" + suffix} {
		vol, err := be.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
			Name: name, CapacityBytes: 32 << 20, Instance: instance,
			Parameters: map[string]string{"pool": pool}, Secrets: secrets,
		})
		if err != nil {
			t.Fatalf("CreateVolume %s: %v", name, err)
		}
		ref := bardplugin.VolumeRef{Instance: instance, Location: vol.Location, Name: vol.Name}
		t.Logf("created volume %s/%s", ref.Location, ref.Name)
		defer func(r bardplugin.VolumeRef) {
			if err := be.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{Volume: r, Secrets: secrets}); err != nil {
				t.Errorf("DeleteVolume %s: %v", r.Name, err)
			}
		}(ref)
		if volA.Name == "" {
			volA = ref
		} else {
			volB = ref
		}
	}

	groupName := "gs-real-group-" + suffix
	created, err := be.CreateVolumeGroupSnapshot(ctx, &bardplugin.CreateVolumeGroupSnapshotRequest{
		Name:          groupName,
		SourceVolumes: []bardplugin.VolumeRef{volA, volB},
		Secrets:       secrets,
	})
	if err != nil {
		t.Fatalf("CreateVolumeGroupSnapshot: %v", err)
	}
	t.Logf("created group snapshot %s/%s with %d member(s)", created.GroupSnapshot.Location, created.GroupSnapshot.Name, len(created.Snapshots))
	if len(created.Snapshots) != 2 {
		t.Fatalf("got %d members, want 2 -- if this is 0, the group-snap/member matching (the exact JSON-tag bug this test targets) regressed", len(created.Snapshots))
	}
	if !created.ReadyToUse {
		t.Fatal("group snapshot not ReadyToUse")
	}
	for _, m := range created.Snapshots {
		if !strings.Contains(m.Snapshot.Name, groupMemberSigil) {
			t.Fatalf("member snapshot name %q does not carry the group-member sigil %q", m.Snapshot.Name, groupMemberSigil)
		}
	}
	groupRef := created.GroupSnapshot
	groupRef.Instance = instance

	// Idempotent re-create: same name, same members -> same group snapshot, no error.
	if again, err := be.CreateVolumeGroupSnapshot(ctx, &bardplugin.CreateVolumeGroupSnapshotRequest{
		Name: groupName, SourceVolumes: []bardplugin.VolumeRef{volA, volB}, Secrets: secrets,
	}); err != nil || again.GroupSnapshot.Name != created.GroupSnapshot.Name {
		t.Fatalf("CreateVolumeGroupSnapshot (idempotent retry): resp=%+v err=%v", again, err)
	}

	got, err := be.GetVolumeGroupSnapshot(ctx, &bardplugin.GetVolumeGroupSnapshotRequest{GroupSnapshot: groupRef, Secrets: secrets})
	if err != nil {
		t.Fatalf("GetVolumeGroupSnapshot: %v", err)
	}
	if len(got.Snapshots) != 2 || !got.ReadyToUse {
		t.Fatalf("GetVolumeGroupSnapshot = %+v, want 2 ready members", got)
	}

	// Restore ONE member -- the other half of the CSI contract, and the other call
	// site that depends on the same group-member snapshot matching.
	restored, err := be.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: "gs-real-restore-" + suffix, CapacityBytes: 32 << 20, Instance: instance,
		Parameters:     map[string]string{"pool": pool},
		Secrets:        secrets,
		SourceSnapshot: &created.Snapshots[0].Snapshot,
	})
	if err != nil {
		t.Fatalf("CreateVolume (restore from group member): %v", err)
	}
	restoredRef := bardplugin.VolumeRef{Instance: instance, Location: restored.Location, Name: restored.Name}
	t.Logf("restored member into volume %s/%s", restoredRef.Location, restoredRef.Name)
	defer func() {
		if err := be.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{Volume: restoredRef, Secrets: secrets}); err != nil {
			t.Errorf("DeleteVolume (restored): %v", err)
		}
	}()

	if err := be.DeleteVolumeGroupSnapshot(ctx, &bardplugin.DeleteVolumeGroupSnapshotRequest{GroupSnapshot: groupRef, Secrets: secrets}); err != nil {
		t.Fatalf("DeleteVolumeGroupSnapshot: %v", err)
	}
	t.Log("deleted group snapshot")

	// Idempotent re-delete: already gone, still succeeds.
	if err := be.DeleteVolumeGroupSnapshot(ctx, &bardplugin.DeleteVolumeGroupSnapshotRequest{GroupSnapshot: groupRef, Secrets: secrets}); err != nil {
		t.Fatalf("DeleteVolumeGroupSnapshot (idempotent retry): %v", err)
	}

	if _, err := be.GetVolumeGroupSnapshot(ctx, &bardplugin.GetVolumeGroupSnapshotRequest{GroupSnapshot: groupRef, Secrets: secrets}); err == nil {
		t.Fatal("GetVolumeGroupSnapshot after delete: got no error, want CodeNotFound")
	} else {
		var serr *bardplugin.StatusError
		if !errors.As(err, &serr) || serr.Code != bardplugin.CodeNotFound {
			t.Fatalf("GetVolumeGroupSnapshot after delete: err = %v, want a StatusError with CodeNotFound", err)
		}
	}

	// The restored clone must survive the group snapshot's deletion -- the live
	// proof that `rbd group snap rm` trashes (not destroys) a still-referenced
	// member and the restored volume stays independently readable/deletable.
	if info, err := be.GetVolumeHealth(ctx, &bardplugin.GetVolumeHealthRequest{Volume: restoredRef, Secrets: secrets}); err != nil || info.Abnormal {
		t.Fatalf("restored volume unhealthy after group snapshot delete: info=%+v err=%v", info, err)
	}
	t.Log("restored volume intact after group snapshot deletion")
}
