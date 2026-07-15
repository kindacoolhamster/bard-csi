package iscsiplugin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// Snapshots opt the plugin into listing them too.
var _ bardplugin.SnapshotLister = (*Backend)(nil)

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

func thinInst() map[string]InstanceConfig {
	return map[string]InstanceConfig{"east": {VG: "bard-vg", Portal: "10.0.0.9:3260", ThinPool: "bard-thin"}}
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
	b := New(thinInst(), "", "", "", "", fr)
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
	// The LIO export must still happen for a thin volume.
	if !fr.ran("targetcli", "/iscsi", "create") {
		t.Fatal("thin volume must still be exported through its own target")
	}
}

// The thinPool StorageClass parameter selects thin on an otherwise-thick
// instance, overriding the instance default.
func TestThinPoolStorageClassParam(t *testing.T) {
	fr := provisionRunner()
	b := New(eastInst(), "", "", "", "", fr) // no instance default
	if _, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "v", Instance: "east", CapacityBytes: 1 << 30,
		Parameters: map[string]string{paramThinPool: "sc-pool"},
	}); err != nil {
		t.Fatal(err)
	}
	if c := fr.callArgs("lvcreate"); !hasArg(c, "-T") || !hasArg(c, "bard-vg/sc-pool") {
		t.Fatalf("the StorageClass thinPool param must select that pool; got %v", c)
	}
}

// Thin snapshot create (read-only CoW, NO LIO export) + delete.
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
	b := New(thinInst(), "", "", "", "", fr)
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
	if !hasArg(c, "--addtag") || !hasArg(c, srcTagPrefix+"bard-x") {
		t.Fatalf("snapshot create must record its source in a %s tag; got %v", srcTagPrefix, c)
	}
	if resp.Name != snapName("snap1") || !resp.ReadyToUse {
		t.Fatalf("unexpected snapshot response %+v", resp)
	}
	// A snapshot is a control-plane object: it must NOT get a backstore/target.
	if fr.ran("targetcli") {
		t.Fatalf("snapshot must not be exported through LIO; calls %v", fr.calls)
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
	b := New(eastInst(), "", "", "", "", thick)
	if _, err := b.CreateSnapshot(context.Background(), &bardplugin.CreateSnapshotRequest{
		Name: "s", SourceVolume: bardplugin.VolumeRef{Instance: "east", Location: "bard-vg", Name: "bard-x"},
	}); err == nil {
		t.Fatal("snapshot of a thick volume must be rejected")
	}
	if _, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "c", Instance: "east",
		SourceSnapshot: &bardplugin.VolumeRef{Instance: "east", Location: "bard-vg", Name: "snap-abc"},
	}); err == nil {
		t.Fatal("clone/restore from a thick source must be rejected")
	}
}

// Restore/clone makes an activated writable thin snapshot of the source AND
// exports it through its own LIO target like any other volume -- the iSCSI twist
// the LVM plugin doesn't have.
func TestThinCloneRestoreExports(t *testing.T) {
	fr := provisionRunner()
	b := New(thinInst(), "", "", "", "", fr)
	if _, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "restored", Instance: "east", CapacityBytes: 1 << 30,
		SourceSnapshot: &bardplugin.VolumeRef{Instance: "east", Location: "bard-vg", Name: "snap-abc"},
	}); err != nil {
		t.Fatal(err)
	}
	c := fr.callArgs("lvcreate")
	if !hasArg(c, "-s") || !hasArg(c, "bard-vg/snap-abc") {
		t.Fatalf("restore must snapshot the source; got %v", c)
	}
	if !fr.ran("lvchange") {
		t.Fatalf("restore must activate the clone (lvchange -ay -Ky); calls %v", fr.calls)
	}
	lv := lvName("restored")
	if !fr.ran("targetcli", "create", "name="+lv) || !fr.ran("targetcli", "/iscsi", "create", targetIQN(defaultIQNBase, lv)) {
		t.Fatalf("the clone must be exported through its own backstore + target; calls %v", fr.calls)
	}
}

// ListSnapshots reports the snap- LVs with their origins; ListVolumes excludes them.
func TestISCSIListSnapshots(t *testing.T) {
	vol, snap := lvName("a"), snapName("s")
	lvsOut := vol + "|1073741824|Vwi-a-tz--|\n" +
		snap + "|1073741824|Vri---tz-k|" + vol + "\n" +
		"bard-thin|4294967296|twi-aotz--|\n"
	fr := &fakeRunner{results: map[string]func([]string) (string, error){
		"lvs": func([]string) (string, error) { return lvsOut, nil },
	}}
	b := New(thinInst(), "", "", "", "", fr)
	snaps, err := b.ListSnapshots(context.Background(), &bardplugin.ListSnapshotsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps.Entries) != 1 || snaps.Entries[0].Snapshot.Name != snap || snaps.Entries[0].SourceVolume.Name != vol {
		t.Fatalf("expected the one snapshot with its origin, got %+v", snaps.Entries)
	}
	vols, err := b.ListVolumes(context.Background(), &bardplugin.ListVolumesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(vols.Entries) != 1 || vols.Entries[0].Volume.Name != vol {
		t.Fatalf("volumes must exclude snapshots + thin pools, got %+v", vols.Entries)
	}
}

// ---- CHAP ------------------------------------------------------------------

// chapSetup builds a CHAP-enforcing instance with a credentials file.
func chapSetup(t *testing.T, lines string) (map[string]InstanceConfig, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "east"), []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	return map[string]InstanceConfig{
		"east": {VG: "bard-vg", Portal: "10.0.0.9:3260", ThinPool: "bard-thin", CHAPAuth: true},
	}, dir
}

// A CHAP instance's targets require authentication (authentication=1).
func TestCreateVolumeChapRequiresAuthentication(t *testing.T) {
	inst, dir := chapSetup(t, "bard\nsecretpass\n")
	fr := provisionRunner()
	b := New(inst, "", "", dir, "", fr)
	if _, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "v", Instance: "east", CapacityBytes: 1 << 30,
	}); err != nil {
		t.Fatal(err)
	}
	if !fr.ran("targetcli", "set", "attribute", "authentication=1") {
		t.Fatalf("CHAP instance must set authentication=1 on the TPG; calls %v", fr.calls)
	}
}

// ControllerPublish puts the credentials on the node's ACL -- and NOT in the
// PublishContext (which lands in the API-visible VolumeAttachment).
func TestControllerPublishSetsChapOnACL(t *testing.T) {
	inst, dir := chapSetup(t, "bard\nsecretpass\nmutualuser\nmutualpass\n")
	fr := &fakeRunner{}
	b := New(inst, "", "", dir, "", fr)
	lv := lvName("pvc-1")
	resp, err := b.ControllerPublish(context.Background(), &bardplugin.ControllerPublishRequest{
		Volume: bardplugin.VolumeRef{Instance: "east", Location: "bard-vg", Name: lv}, NodeID: "n1",
	})
	if err != nil {
		t.Fatal(err)
	}
	acl := tpgPath(targetIQN(defaultIQNBase, lv)) + "/acls/" + initiatorIQN(defaultIQNBase, "n1")
	if !fr.ran("targetcli", acl, "set", "auth", "userid=bard", "password=secretpass") {
		t.Fatalf("expected chap auth set on the ACL; calls %v", fr.calls)
	}
	if !fr.ran("targetcli", "mutual_userid=mutualuser", "mutual_password=mutualpass") {
		t.Fatalf("expected the mutual pair set too; calls %v", fr.calls)
	}
	for k, v := range resp.PublishContext {
		if strings.Contains(v, "secretpass") || strings.Contains(v, "mutualpass") {
			t.Fatalf("credentials leaked into PublishContext (%s=%s)", k, v)
		}
	}
}

// NodeStage writes the credentials onto the node record BEFORE the login.
func TestNodeStageSetsChapBeforeLogin(t *testing.T) {
	inst, dir := chapSetup(t, "bard\nsecretpass\n")
	fr := &fakeRunner{results: map[string]func([]string) (string, error){
		"blockdev": func([]string) (string, error) { return "1073741824\n", nil },
		"findmnt":  func([]string) (string, error) { return "", errors.New("not found") },
		"blkid":    func([]string) (string, error) { return "", errors.New("not a filesystem") },
	}}
	b := New(inst, "n1", t.TempDir(), dir, "", fr)
	if err := b.NodeStage(context.Background(), &bardplugin.NodeStageRequest{
		Volume:      bardplugin.VolumeRef{Instance: "east", Location: "bard-vg", Name: lvName("pvc-1")},
		StagingPath: t.TempDir() + "/stage",
	}); err != nil {
		t.Fatal(err)
	}
	credsAt, loginAt := -1, -1
	for i, c := range fr.calls {
		joined := strings.Join(c, " ")
		if c[0] == "iscsiadm" && strings.Contains(joined, "node.session.auth.password") && credsAt < 0 {
			credsAt = i
		}
		if c[0] == "iscsiadm" && strings.Contains(joined, "--login") && loginAt < 0 {
			loginAt = i
		}
	}
	if credsAt < 0 || loginAt < 0 || credsAt > loginAt {
		t.Fatalf("chap credentials must be set on the node record before login (creds@%d login@%d); calls %v",
			credsAt, loginAt, fr.calls)
	}
	if !fr.ran("iscsiadm", "node.session.auth.authmethod", "CHAP") {
		t.Fatalf("expected authmethod CHAP on the node record; calls %v", fr.calls)
	}
}

// CHAP on without readable/well-formed credentials is an error, not silent
// unauthenticated access -- and it fails before any ACL is created.
func TestChapMissingOrMalformedCreds(t *testing.T) {
	inst := map[string]InstanceConfig{
		"east": {VG: "bard-vg", Portal: "10.0.0.9:3260", CHAPAuth: true},
	}
	fr := &fakeRunner{}
	b := New(inst, "", "", t.TempDir(), "", fr) // no credentials file
	if _, err := b.ControllerPublish(context.Background(), &bardplugin.ControllerPublishRequest{
		Volume: bardplugin.VolumeRef{Instance: "east", Location: "bard-vg", Name: "bard-x"}, NodeID: "n1",
	}); err == nil {
		t.Fatal("publish with chapAuth on and no credentials must fail")
	}
	if fr.ran("targetcli", "acls", "create") {
		t.Fatal("no ACL may be created when the credentials are unavailable")
	}

	instBad, dir := chapSetup(t, "only-a-user\nsecret\ndangling-mutual-user\n") // 3 lines
	b2 := New(instBad, "", "", dir, "", &fakeRunner{})
	if _, err := b2.chapFor("east"); err == nil {
		t.Fatal("3-line credentials file must be rejected (mutual needs both lines)")
	}

	// targetcli re-parses its argv through configshell, so whitespace or quotes
	// in a credential would split the `set auth` command at publish time --
	// reject at load instead.
	instWS, dirWS := chapSetup(t, "bard\npass word\n")
	b3 := New(instWS, "", "", dirWS, "", &fakeRunner{})
	if _, err := b3.chapFor("east"); err == nil {
		t.Fatal("credentials containing whitespace must be rejected")
	}
}

// A missing source LV must surface as NotFound (CSI: restore/snapshot of a
// deleted source), not a generic lvs failure.
func TestSnapshotAndCloneMissingSourceNotFound(t *testing.T) {
	gone := &fakeRunner{results: map[string]func([]string) (string, error){
		"lvs": func(args []string) (string, error) {
			return "", errors.New("Failed to find logical volume \"bard-vg/bard-x\"")
		},
	}}
	b := New(thinInst(), "", "", "", "", gone)
	wantNotFound := func(err error, op string) {
		t.Helper()
		var se *bardplugin.StatusError
		if err == nil || !errors.As(err, &se) || se.Code != bardplugin.CodeNotFound {
			t.Fatalf("%s of a missing source must be NotFound, got %v", op, err)
		}
	}
	_, err := b.CreateSnapshot(context.Background(), &bardplugin.CreateSnapshotRequest{
		Name: "s", SourceVolume: bardplugin.VolumeRef{Instance: "east", Location: "bard-vg", Name: "bard-x"},
	})
	wantNotFound(err, "snapshot")
	_, err = b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "c", Instance: "east",
		SourceSnapshot: &bardplugin.VolumeRef{Instance: "east", Location: "bard-vg", Name: "snap-abc"},
	})
	wantNotFound(err, "clone/restore")
}

// A clone source in a different VG than the instance's is a routing error,
// rejected fail-fast (thin snapshots cannot cross VGs).
func TestCloneSourceWrongVGRejected(t *testing.T) {
	fr := &fakeRunner{results: map[string]func([]string) (string, error){
		"lvs": func([]string) (string, error) { return "", errors.New("Failed to find logical volume") },
	}}
	b := New(thinInst(), "", "", "", "", fr)
	_, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "c", Instance: "east",
		SourceSnapshot: &bardplugin.VolumeRef{Instance: "east", Location: "other-vg", Name: "snap-abc"},
	})
	var se *bardplugin.StatusError
	if err == nil || !errors.As(err, &se) || se.Code != bardplugin.CodeInvalidArg {
		t.Fatalf("cross-VG clone source must be InvalidArgument, got %v", err)
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
	b := New(thinInst(), "", "", "", "", fr)
	snaps, err := b.ListSnapshots(context.Background(), &bardplugin.ListSnapshotsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps.Entries) != 1 || snaps.Entries[0].Snapshot.Name != snap || snaps.Entries[0].SourceVolume.Name != vol {
		t.Fatalf("expected the tagged snapshot with its recorded source, got %+v", snaps.Entries)
	}
}

// NodePublish of a raw Block volume must not wedge on a restart-lost stage
// record: the device is derived exactly as NodeUnstage derives it.
func TestNodePublishBlockDerivedDevice(t *testing.T) {
	fr := &fakeRunner{}
	b := New(eastInst(), "n1", t.TempDir(), "", "", fr) // fresh stateDir: no records
	lv := lvName("pvc-1")
	target := t.TempDir() + "/block-target"
	if err := b.NodePublish(context.Background(), &bardplugin.NodePublishRequest{
		Volume:      bardplugin.VolumeRef{Instance: "east", Location: "bard-vg", Name: lv},
		StagingPath: t.TempDir() + "/stage",
		TargetPath:  target,
		Block:       true,
	}); err != nil {
		t.Fatalf("block publish with a lost record must derive the device: %v", err)
	}
	dev := byPath("10.0.0.9:3260", targetIQN(defaultIQNBase, lv), "0")
	if !fr.ran("mount", "--bind", dev, target) {
		t.Fatalf("expected bind mount of the derived by-path device; calls %v", fr.calls)
	}
}

// Staging a SECOND volume on a node must not touch the iface: iscsiadm refuses
// create/update on an iface a live session is using (exit 15), so ensureIface
// must take the read-only fast path when the iface already carries our IQN --
// the live-found multi-volume bug.
func TestSecondStageLeavesBusyIfaceAlone(t *testing.T) {
	initIQN := initiatorIQN(defaultIQNBase, "n1")
	fr := &fakeRunner{results: map[string]func([]string) (string, error){
		"iscsiadm": func(args []string) (string, error) {
			joined := strings.Join(args, " ")
			if strings.Contains(joined, "--op new") || strings.Contains(joined, "--op update") {
				return "", errors.New("iscsiadm: Could not create new interface bard.")
			}
			if strings.Contains(joined, "-m iface") {
				return "iface.iscsi_ifacename = bard\niface.initiatorname = " + initIQN + "\n", nil
			}
			return "", nil
		},
		"blockdev": func([]string) (string, error) { return "1073741824\n", nil },
		"findmnt":  func([]string) (string, error) { return "", errors.New("not found") },
		"blkid":    func([]string) (string, error) { return "ext4\n", nil },
	}}
	b := New(eastInst(), "n1", t.TempDir(), "", "", fr)
	if err := b.NodeStage(context.Background(), &bardplugin.NodeStageRequest{
		Volume:      bardplugin.VolumeRef{Instance: "east", Location: "bard-vg", Name: lvName("pvc-2")},
		StagingPath: t.TempDir() + "/stage",
	}); err != nil {
		t.Fatalf("second stage must not fail on the busy iface: %v", err)
	}
	if fr.ran("iscsiadm", "--op", "new") || fr.ran("iscsiadm", "--op", "update") {
		t.Fatalf("iface already carries our IQN; it must not be created or updated: %v", fr.calls)
	}
}

// A repeated DeleteVolume must converge: targetcli's absent-backstore phrasing
// ("No storage object named <x>" -- no generic not-found marker, like its
// create-side sibling) broke delete idempotency until isNotFound learned it.
// Found by the conformance tool's double-delete check.
func TestDeleteVolumeAbsentBackstorePhrasing(t *testing.T) {
	fr := &fakeRunner{results: map[string]func([]string) (string, error){
		"targetcli": func(args []string) (string, error) {
			if hasArg(args, "/backstores/block") {
				return "", errors.New("No storage object named bard-x")
			}
			return "", errors.New("No such Target in configfs")
		},
		"lvremove": func([]string) (string, error) { return "", errors.New("Failed to find logical volume") },
	}}
	b := New(eastInst(), "", "", "", "", fr)
	if err := b.DeleteVolume(context.Background(), &bardplugin.DeleteVolumeRequest{
		Volume: bardplugin.VolumeRef{Instance: "east", Location: "bard-vg", Name: "bard-x"},
	}); err != nil {
		t.Fatalf("delete of an absent volume must be idempotent, got %v", err)
	}
}

// NodeUnstage with NO session record (the plugin container restarted since
// stage) must still log the session out with derived IQN/portal -- silently
// returning success leaked the session past volume deletion, found live
// in-cluster after a mid-lifetime pod restart.
func TestNodeUnstageWithoutStateStillLogsOut(t *testing.T) {
	fr := &fakeRunner{results: map[string]func([]string) (string, error){
		"blockdev": func([]string) (string, error) { return "0\n", nil }, // gone after logout
		// The derived logout on an already-logged-out node answers with
		// iscsiadm's real phrasing (exit 21) -- must classify as idempotent.
		"iscsiadm": func(args []string) (string, error) {
			if hasArg(args, "--logout") {
				return "", errors.New("iscsiadm: No matching sessions found")
			}
			return "", nil
		},
	}}
	b := New(eastInst(), "n1", t.TempDir(), "", "", fr) // fresh stateDir: no records
	lv := lvName("pvc-1")
	if err := b.NodeUnstage(context.Background(), &bardplugin.NodeUnstageRequest{
		Volume:      bardplugin.VolumeRef{Instance: "east", Location: "bard-vg", Name: lv},
		StagingPath: t.TempDir() + "/stage",
	}); err != nil {
		t.Fatal(err)
	}
	if !fr.ran("iscsiadm", "--logout", targetIQN(defaultIQNBase, lv), "10.0.0.9:3260") {
		t.Fatalf("expected a derived-identity logout; calls %v", fr.calls)
	}
}

// State-changing lvm commands must carry the self-managed-/dev config: in a
// container no udev serves activation, so an inactive thin pool (first volume
// after reboot / after the last LV was removed) fails to activate without it.
// Reads (lvs) stay plain.
func TestLvmSelfManagedDevNodes(t *testing.T) {
	fr := provisionRunner()
	b := New(thinInst(), "", "", "", "", fr)
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

// A failed CHAP command must never leak the credentials: command errors embed
// the full argv (as ExecRunner does), and a plugin error becomes a CSI error --
// sidecar logs, VolumeAttachment status, kubelet events. Both secret-carrying
// call sites (targetcli set auth; iscsiadm node-record update) must redact.
func TestChapErrorsNeverLeakSecrets(t *testing.T) {
	const pass, mutual = "supersecretpw", "mutualsecretpw"
	inst, dir := chapSetup(t, "bard\n"+pass+"\nmuser\n"+mutual+"\n")
	echo := func(name string) func([]string) (string, error) {
		return func(args []string) (string, error) {
			return "", errors.New(name + " " + strings.Join(args, " ") + ": exit status 1: boom")
		}
	}

	// Controller: targetcli set auth fails, echoing argv (incl. both passwords).
	frPub := &fakeRunner{results: map[string]func([]string) (string, error){
		"targetcli": func(args []string) (string, error) {
			if hasArg(args, "auth") {
				return echo("targetcli")(args)
			}
			return "", nil
		},
	}}
	b := New(inst, "", "", dir, "", frPub)
	_, err := b.ControllerPublish(context.Background(), &bardplugin.ControllerPublishRequest{
		Volume: bardplugin.VolumeRef{Instance: "east", Location: "bard-vg", Name: "bard-x"}, NodeID: "n1",
	})
	if err == nil {
		t.Fatal("expected the auth failure to surface")
	}
	if strings.Contains(err.Error(), pass) || strings.Contains(err.Error(), mutual) {
		t.Fatalf("CHAP password leaked into the publish error: %v", err)
	}
	if !strings.Contains(err.Error(), "***") {
		t.Fatalf("expected redaction marker in %v", err)
	}

	// Node: the password node-record update fails, echoing argv.
	frStage := &fakeRunner{results: map[string]func([]string) (string, error){
		"iscsiadm": func(args []string) (string, error) {
			if strings.Contains(strings.Join(args, " "), "node.session.auth.password") {
				return echo("iscsiadm")(args)
			}
			return "", nil
		},
	}}
	b2 := New(inst, "n1", t.TempDir(), dir, "", frStage)
	err = b2.NodeStage(context.Background(), &bardplugin.NodeStageRequest{
		Volume:      bardplugin.VolumeRef{Instance: "east", Location: "bard-vg", Name: "bard-x"},
		StagingPath: t.TempDir() + "/stage",
	})
	if err == nil {
		t.Fatal("expected the node-record failure to surface")
	}
	if strings.Contains(err.Error(), pass) {
		t.Fatalf("CHAP password leaked into the stage error: %v", err)
	}
}

// Unpublish must actually revoke the ACL even when the instance is no longer
// configured -- both IQNs derive from the volume name + node id (like
// DeleteVolume), and success-without-revocation would leave the node with
// standing access to the LUN.
func TestControllerUnpublishUnknownInstanceStillRevokes(t *testing.T) {
	fr := &fakeRunner{}
	b := New(map[string]InstanceConfig{}, "", "", "", "", fr) // nothing configured
	if err := b.ControllerUnpublish(context.Background(), &bardplugin.ControllerUnpublishRequest{
		Volume: bardplugin.VolumeRef{Instance: "gone", Location: "bard-vg", Name: "bard-x"}, NodeID: "n1",
	}); err != nil {
		t.Fatal(err)
	}
	if !fr.ran("targetcli", "acls", "delete", initiatorIQN(defaultIQNBase, "n1")) {
		t.Fatalf("expected the ACL delete to be attempted with derived IQNs; calls %v", fr.calls)
	}
}

// With --iscsiadm-chroot set, every iscsiadm runs through chroot into the host
// root (the host's matched iscsiadm+DB+iscsid stack); other tools stay direct.
func TestIscsiadmChroot(t *testing.T) {
	fr := &fakeRunner{results: map[string]func([]string) (string, error){
		"chroot":   func([]string) (string, error) { return "", nil },
		"blockdev": func([]string) (string, error) { return "1073741824\n", nil },
		"findmnt":  func([]string) (string, error) { return "", errors.New("not found") },
		"blkid":    func([]string) (string, error) { return "ext4\n", nil },
	}}
	b := New(eastInst(), "n1", t.TempDir(), "", "/host", fr)
	if err := b.NodeStage(context.Background(), &bardplugin.NodeStageRequest{
		Volume:      bardplugin.VolumeRef{Instance: "east", Location: "bard-vg", Name: lvName("pvc-1")},
		StagingPath: t.TempDir() + "/stage",
	}); err != nil {
		t.Fatal(err)
	}
	if fr.ran("iscsiadm") {
		t.Fatalf("iscsiadm must not run directly when chrooted; calls %v", fr.calls)
	}
	if !fr.ran("chroot", "/host", "iscsiadm", "--login") {
		t.Fatalf("expected chroot /host iscsiadm ... --login; calls %v", fr.calls)
	}
	if fr.ran("chroot", "mount") || fr.ran("chroot", "blkid") {
		t.Fatalf("only iscsiadm is chrooted; calls %v", fr.calls)
	}
}

// Without chapAuth nothing changes: no auth on the ACL, authentication=0.
func TestNoChapByDefault(t *testing.T) {
	fr := provisionRunner()
	b := New(eastInst(), "", "", "", "", fr)
	if _, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "v", Instance: "east", CapacityBytes: 1 << 30,
	}); err != nil {
		t.Fatal(err)
	}
	if !fr.ran("targetcli", "set", "attribute", "authentication=0") {
		t.Fatalf("non-CHAP instance must keep authentication=0; calls %v", fr.calls)
	}
	fr2 := &fakeRunner{}
	b2 := New(eastInst(), "", "", "", "", fr2)
	if _, err := b2.ControllerPublish(context.Background(), &bardplugin.ControllerPublishRequest{
		Volume: bardplugin.VolumeRef{Instance: "east", Location: "bard-vg", Name: "bard-x"}, NodeID: "n1",
	}); err != nil {
		t.Fatal(err)
	}
	if fr2.ran("targetcli", "set", "auth") {
		t.Fatalf("no auth must be set without chapAuth; calls %v", fr2.calls)
	}
}
