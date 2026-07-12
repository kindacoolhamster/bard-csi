package iscsiplugin

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// Compile-time proof this is a full backend AND an attach backend.
var (
	_ bardplugin.Backend             = (*Backend)(nil)
	_ bardplugin.ControllerPublisher = (*Backend)(nil)
)

// fakeRunner returns canned results keyed by the command name, recording every
// invocation (mirrors the LVM plugin's test harness).
type fakeRunner struct {
	calls   [][]string
	results map[string]func(args []string) (string, error)
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if fn := f.results[name]; fn != nil {
		return fn(args)
	}
	return "", nil
}

func (f *fakeRunner) ran(name string, mustContain ...string) bool {
	for _, c := range f.calls {
		if c[0] != name {
			continue
		}
		joined := strings.Join(c, " ")
		ok := true
		for _, m := range mustContain {
			if !strings.Contains(joined, m) {
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

// ranArg reports whether name was called with an exact positional arg (not just a
// substring) -- needed for targetcli, where /backstores/block (the create parent)
// vs /backstores/block/<obj> (the object path) is a real distinction.
func (f *fakeRunner) ranArg(name, exactArg string) bool {
	for _, c := range f.calls {
		if c[0] != name {
			continue
		}
		for _, a := range c[1:] {
			if a == exactArg {
				return true
			}
		}
	}
	return false
}

func eastInst() map[string]InstanceConfig {
	return map[string]InstanceConfig{"east": {VG: "bard-vg", Portal: "10.0.0.9:3260"}}
}

func TestInfoAdvertisesAttach(t *testing.T) {
	b := New(eastInst(), "", "", "", "", &fakeRunner{})
	caps := b.Info().Capabilities
	if !caps.RequiresControllerPublish {
		t.Fatal("iSCSI must advertise RequiresControllerPublish")
	}
	if !caps.BlockDevice || !caps.Snapshots {
		t.Fatalf("unexpected caps: %+v", caps)
	}
}

func TestInitiatorIQNDerivation(t *testing.T) {
	// Deterministic, sanitized, and distinct from the target namespace.
	a := initiatorIQN(defaultIQNBase, "k3s-agent")
	if a != initiatorIQN(defaultIQNBase, "k3s-agent") {
		t.Fatal("not deterministic")
	}
	if !strings.HasPrefix(a, defaultIQNBase+":init-") {
		t.Fatalf("bad initiator iqn: %q", a)
	}
	if strings.ContainsAny(initiatorIQN(defaultIQNBase, "Node_With/Weird@chars"), "_/@") {
		t.Fatal("initiator iqn not sanitized to the IQN charset")
	}
}

func TestCreateVolumeProvisionsAndExports(t *testing.T) {
	created := false
	fr := &fakeRunner{results: map[string]func([]string) (string, error){
		"lvs": func([]string) (string, error) {
			if !created {
				return "", errors.New("Failed to find logical volume")
			}
			return "  1073741824\n", nil
		},
		"lvcreate": func([]string) (string, error) { created = true; return "", nil },
	}}
	b := New(eastInst(), "", "", "", "", fr)

	resp, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "pvc-1", Instance: "east", CapacityBytes: 1 << 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	lv := lvName("pvc-1")
	iqn := targetIQN(defaultIQNBase, lv)
	if resp.Location != "bard-vg" || resp.Name != lv || resp.CapacityBytes != 1<<30 {
		t.Fatalf("bad identity: %+v", resp)
	}
	if !fr.ran("lvcreate") {
		t.Fatal("expected lvcreate")
	}
	// create must target the PARENT path with name=, not the (nonexistent) object
	// path -- the live bug that returned "No such path /backstores/block/<obj>".
	if !fr.ranArg("targetcli", "/backstores/block") || !fr.ran("targetcli", "create", "name="+lv, "dev=/dev/bard-vg/"+lv) {
		t.Fatal("expected block backstore created under the /backstores/block parent")
	}
	if fr.ranArg("targetcli", backstore(lv)+" create") {
		t.Fatal("backstore create must not address the object path directly")
	}
	if !fr.ran("targetcli", "/iscsi", "create", iqn) {
		t.Fatal("expected per-volume target create")
	}
	if !fr.ran("targetcli", "luns", "create") {
		t.Fatal("expected LUN mapping")
	}
	if !fr.ran("targetcli", "set", "attribute", "generate_node_acls=0") {
		t.Fatal("expected ACL enforcement (no demo mode)")
	}
}

func TestCreateVolumeIdempotent(t *testing.T) {
	// LV already exists at size, and every targetcli create reports its object as
	// existing -- with the REAL, per-path phrasings observed live (the backstore
	// one carries no "already", which broke retries until isExists learned it).
	fr := &fakeRunner{results: map[string]func([]string) (string, error){
		"lvs": func([]string) (string, error) { return "  1073741824\n", nil },
		"targetcli": func(args []string) (string, error) {
			if !contains(args, "create") {
				return "", nil
			}
			switch {
			case contains(args, "/backstores/block"):
				return "", errors.New("Storage object block/bard-x exists")
			case strings.Contains(strings.Join(args, " "), "/luns"):
				return "", errors.New("lun for storage object block/bard-x already exists")
			default:
				return "", errors.New("This Target already exists in configFS")
			}
		},
	}}
	b := New(eastInst(), "", "", "", "", fr)
	if _, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "pvc-1", Instance: "east", CapacityBytes: 1 << 30,
	}); err != nil {
		t.Fatalf("idempotent create must converge, got %v", err)
	}
	if fr.ran("lvcreate") {
		t.Fatal("must not lvcreate when the LV already exists at size")
	}
}

// ControllerPublish masks the LUN to the node and returns the connection context.
func TestControllerPublishMasksAndReturnsContext(t *testing.T) {
	fr := &fakeRunner{}
	b := New(eastInst(), "", "", "", "", fr)
	lv := lvName("pvc-1")
	resp, err := b.ControllerPublish(context.Background(), &bardplugin.ControllerPublishRequest{
		Volume: bardplugin.VolumeRef{Instance: "east", Location: "bard-vg", Name: lv}, NodeID: "k3s-agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantInit := initiatorIQN(defaultIQNBase, "k3s-agent")
	if !fr.ran("targetcli", "acls", "create", wantInit) {
		t.Fatalf("expected an ACL for the node initiator %q; calls=%v", wantInit, fr.calls)
	}
	pc := resp.PublishContext
	if pc[ctxPortal] != "10.0.0.9:3260" || pc[ctxIQN] != targetIQN(defaultIQNBase, lv) || pc[ctxLUN] != "0" {
		t.Fatalf("bad publish context: %+v", pc)
	}
}

func TestControllerPublishIdempotent(t *testing.T) {
	fr := &fakeRunner{results: map[string]func([]string) (string, error){
		"targetcli": func([]string) (string, error) { return "", errors.New("ACL already exists in configFS") },
	}}
	b := New(eastInst(), "", "", "", "", fr)
	if _, err := b.ControllerPublish(context.Background(), &bardplugin.ControllerPublishRequest{
		Volume: bardplugin.VolumeRef{Instance: "east", Name: lvName("pvc-1")}, NodeID: "n",
	}); err != nil {
		t.Fatalf("re-publish must be idempotent, got %v", err)
	}
}

func TestControllerUnpublishRemovesACLAndIsIdempotent(t *testing.T) {
	// not-found on delete must be swallowed.
	fr := &fakeRunner{results: map[string]func([]string) (string, error){
		"targetcli": func([]string) (string, error) { return "", errors.New("No such path in configFS") },
	}}
	b := New(eastInst(), "", "", "", "", fr)
	if err := b.ControllerUnpublish(context.Background(), &bardplugin.ControllerUnpublishRequest{
		Volume: bardplugin.VolumeRef{Instance: "east", Name: lvName("pvc-1")}, NodeID: "n",
	}); err != nil {
		t.Fatalf("unpublish of a missing ACL must be idempotent, got %v", err)
	}
	if !fr.ran("targetcli", "acls", "delete") {
		t.Fatal("expected an ACL delete attempt")
	}
}

// DeleteVolume tears down in order and surfaces a real failure (no silent orphan).
func TestDeleteVolumeOrderAndNoOrphan(t *testing.T) {
	fr := &fakeRunner{results: map[string]func([]string) (string, error){
		"lvremove": func([]string) (string, error) { return "", errors.New("LV is in use") },
	}}
	b := New(eastInst(), "", "", "", "", fr)
	lv := lvName("pvc-1")
	err := b.DeleteVolume(context.Background(), &bardplugin.DeleteVolumeRequest{
		Volume: bardplugin.VolumeRef{Instance: "east", Location: "bard-vg", Name: lv},
	})
	if err == nil {
		t.Fatal("a failed lvremove must surface (else the volume is reported deleted while data remains)")
	}
	if !fr.ran("targetcli", "/iscsi", "delete", targetIQN(defaultIQNBase, lv)) {
		t.Fatal("expected target teardown before backstore/LV")
	}
	if !fr.ran("targetcli", "/backstores/block", "delete", lv) {
		t.Fatal("expected backstore teardown")
	}
}

func TestDeleteVolumeIdempotent(t *testing.T) {
	// Everything already gone: all three steps report not-found, delete succeeds.
	notFound := func([]string) (string, error) { return "", errors.New("No such path / Failed to find") }
	fr := &fakeRunner{results: map[string]func([]string) (string, error){
		"targetcli": notFound, "lvremove": notFound,
	}}
	b := New(eastInst(), "", "", "", "", fr)
	if err := b.DeleteVolume(context.Background(), &bardplugin.DeleteVolumeRequest{
		Volume: bardplugin.VolumeRef{Instance: "east", Location: "bard-vg", Name: lvName("pvc-1")},
	}); err != nil {
		t.Fatalf("delete of an absent volume must be idempotent, got %v", err)
	}
}

// NodeStage logs in under the per-node iface, records session state, and mounts.
func TestNodeStageLogsInAndRecords(t *testing.T) {
	fr := &fakeRunner{results: map[string]func([]string) (string, error){
		// device present + sized
		"blockdev": func([]string) (string, error) { return "1073741824\n", nil },
		// not yet mounted -> proceed to mount
		"findmnt": func([]string) (string, error) { return "", errors.New("not found") },
		"blkid":   func([]string) (string, error) { return "", errors.New("not a filesystem") },
	}}
	stateDir := t.TempDir()
	b := New(eastInst(), "k3s-agent", stateDir, "", "", fr)
	staging := t.TempDir() + "/stage"

	err := b.NodeStage(context.Background(), &bardplugin.NodeStageRequest{
		Volume:         bardplugin.VolumeRef{Instance: "east", Location: "bard-vg", Name: lvName("pvc-1")},
		StagingPath:    staging,
		PublishContext: map[string]string{ctxPortal: "10.0.0.9:3260", ctxIQN: targetIQN(defaultIQNBase, lvName("pvc-1")), ctxLUN: "0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !fr.ran("iscsiadm", "iface", "iface.initiatorname", initiatorIQN(defaultIQNBase, "k3s-agent")) {
		t.Fatal("expected the per-node iface initiatorname to be set")
	}
	if !fr.ran("iscsiadm", "node", "--login") {
		t.Fatal("expected an iscsiadm login")
	}
	if !fr.ran("mkfs.ext4") || !fr.ran("mount") {
		t.Fatal("expected format + mount")
	}
	if st, ok := b.loadState(staging); !ok || st.IQN == "" || st.Portal != "10.0.0.9:3260" {
		t.Fatalf("expected recorded session state, got %+v ok=%v", st, ok)
	}
}

// NodeUnstage must refuse to report success while the device is still present.
func TestNodeUnstageRefusesWhileAttached(t *testing.T) {
	stateDir := t.TempDir()
	b := New(eastInst(), "k3s-agent", stateDir, "", "", &fakeRunner{results: map[string]func([]string) (string, error){
		"blockdev": func([]string) (string, error) { return "1073741824\n", nil }, // still there
	}})
	staging := t.TempDir() + "/stage"
	if err := b.recordState(staging, stagedState{Device: "/dev/disk/by-path/x", IQN: "iqn:tgt-x", Portal: "10.0.0.9:3260"}); err != nil {
		t.Fatal(err)
	}
	if err := b.NodeUnstage(context.Background(), &bardplugin.NodeUnstageRequest{StagingPath: staging}); err == nil {
		t.Fatal("NodeUnstage must error while the device is still attached (kubelet retries)")
	}
}

func TestNodeUnstageSucceedsWhenDetached(t *testing.T) {
	stateDir := t.TempDir()
	b := New(eastInst(), "k3s-agent", stateDir, "", "", &fakeRunner{results: map[string]func([]string) (string, error){
		"blockdev": func([]string) (string, error) { return "0\n", nil }, // device gone
	}})
	staging := t.TempDir() + "/stage"
	_ = b.recordState(staging, stagedState{Device: "/dev/disk/by-path/x", IQN: "iqn:tgt-x", Portal: "10.0.0.9:3260"})
	if err := b.NodeUnstage(context.Background(), &bardplugin.NodeUnstageRequest{StagingPath: staging}); err != nil {
		t.Fatalf("clean detach must succeed, got %v", err)
	}
	if _, ok := b.loadState(staging); ok {
		t.Fatal("state must be cleared after a successful unstage")
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
