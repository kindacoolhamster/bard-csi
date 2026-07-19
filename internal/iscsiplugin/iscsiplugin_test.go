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

// callIndex returns the position of the first call to name whose remaining args
// EXACTLY match wantArgs (length and order), or -1. Needed to pin the multi-portal
// delete-then-create SEQUENCE, where ran()'s substring-anywhere match can't tell
// order apart.
func callIndex(f *fakeRunner, name string, wantArgs ...string) int {
	for i, c := range f.calls {
		if c[0] != name || len(c)-1 != len(wantArgs) {
			continue
		}
		match := true
		for j, a := range wantArgs {
			if c[1+j] != a {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func eastInst() map[string]InstanceConfig {
	return map[string]InstanceConfig{"east": {VG: "bard-vg", Portal: "10.0.0.9:3260"}}
}

func TestInfoAdvertisesAttach(t *testing.T) {
	b := New(eastInst(), "", "", "", "", "", &fakeRunner{})
	caps := b.Info().Capabilities
	if !caps.RequiresControllerPublish {
		t.Fatal("iSCSI must advertise RequiresControllerPublish")
	}
	if !caps.BlockDevice || !caps.Snapshots {
		t.Fatalf("unexpected caps: %+v", caps)
	}
}

// TestPortalListFallback pins InstanceConfig.portalList()'s precedence: Portals
// wins when set (even alongside a legacy Portal), else the single Portal is
// wrapped into a one-element list, else empty.
func TestPortalListFallback(t *testing.T) {
	cases := []struct {
		name string
		ic   InstanceConfig
		want []string
	}{
		{"portal-only", InstanceConfig{Portal: "10.0.0.9:3260"}, []string{"10.0.0.9:3260"}},
		{"portals-only", InstanceConfig{Portals: []string{"10.0.0.9:3260", "10.0.0.10:3260"}}, []string{"10.0.0.9:3260", "10.0.0.10:3260"}},
		{"both-set-portals-wins", InstanceConfig{Portal: "10.0.0.1:3260", Portals: []string{"10.0.0.9:3260", "10.0.0.10:3260"}}, []string{"10.0.0.9:3260", "10.0.0.10:3260"}},
		{"neither", InstanceConfig{}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.ic.portalList()
			if !equalStrings(got, c.want) {
				t.Fatalf("portalList() = %v, want %v", got, c.want)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestInstPortalsOnlyValid: inst() must accept an instance configured with ONLY
// Portals (no legacy Portal), and still reject one with neither.
func TestInstPortalsOnlyValid(t *testing.T) {
	withPortals := New(map[string]InstanceConfig{
		"east": {VG: "bard-vg", Portals: []string{"10.0.0.9:3260", "10.0.0.10:3260"}},
	}, "", "", "", "", "", &fakeRunner{})
	if _, err := withPortals.inst("east"); err != nil {
		t.Fatalf("inst() must accept vg+portals (no single Portal), got %v", err)
	}

	withNeither := New(map[string]InstanceConfig{
		"east": {VG: "bard-vg"},
	}, "", "", "", "", "", &fakeRunner{})
	if _, err := withNeither.inst("east"); err == nil {
		t.Fatal("inst() must reject an instance with neither Portal nor Portals")
	}
}

func TestInstRejectsBracketedPortal(t *testing.T) {
	// Bracketed IPv6 portals are out of scope (splitPortal is proven only for
	// plain ip:port); inst() must reject them loudly, not mis-split silently.
	b := New(map[string]InstanceConfig{
		"east": {VG: "bard-vg", Portals: []string{"10.0.0.9:3260", "[::1]:3260"}},
	}, "", "", "", "", "", &fakeRunner{})
	if _, err := b.inst("east"); err == nil {
		t.Fatal("inst() must reject a bracketed IPv6 portal")
	}
}

// TestInstLocalManagementUnchanged pins that management absent/"local" keeps
// today's validation (vg required) BYTE-FOR-BYTE -- adding targetd support
// must not perturb the existing local-management error text that operators
// and any tooling already key off of.
func TestInstLocalManagementUnchanged(t *testing.T) {
	for _, mgmt := range []string{"", "local"} {
		t.Run("management="+mgmt, func(t *testing.T) {
			b := New(map[string]InstanceConfig{"east": {Management: mgmt}}, "", "", "", "", "", &fakeRunner{})
			_, err := b.inst("east")
			if err == nil {
				t.Fatal("local-management instance without vg/portal must be rejected")
			}
			want := `InvalidArgument: iscsi: instance "east" not configured (need vg + portal)`
			if err.Error() != want {
				t.Fatalf("local-management inst() error changed:\n got  %q\n want %q", err.Error(), want)
			}
		})
	}
}

// TestInstTargetdValid: a targetd instance is valid with
// endpoint+pool+targetIqn+portal and NO vg (targetd owns its own storage
// pool remotely; there is no local VG to carve from).
func TestInstTargetdValid(t *testing.T) {
	b := New(map[string]InstanceConfig{
		"remote": {
			Management:      "targetd",
			TargetdEndpoint: "http://10.0.0.5:18700/targetrpc",
			TargetdPool:     "vg-targetd",
			TargetIQN:       "iqn.2025-01.io.bard:remote",
			Portal:          "10.0.0.5:3260",
		},
	}, "", "", "", "", "", &fakeRunner{})
	ic, err := b.inst("remote")
	if err != nil {
		t.Fatalf("targetd instance with endpoint+pool+targetIqn+portal (no vg) must be valid, got %v", err)
	}
	if ic.VG != "" {
		t.Fatalf("targetd instance must not require/carry a vg, got %q", ic.VG)
	}
}

// TestInstTargetdMissingFieldsRejected: each of the four targetd-required
// fields is independently necessary.
func TestInstTargetdMissingFieldsRejected(t *testing.T) {
	base := InstanceConfig{
		Management:      "targetd",
		TargetdEndpoint: "http://10.0.0.5:18700/targetrpc",
		TargetdPool:     "vg-targetd",
		TargetIQN:       "iqn.2025-01.io.bard:remote",
		Portal:          "10.0.0.5:3260",
	}
	cases := []struct {
		name string
		mut  func(*InstanceConfig)
	}{
		{"no endpoint", func(ic *InstanceConfig) { ic.TargetdEndpoint = "" }},
		{"no pool", func(ic *InstanceConfig) { ic.TargetdPool = "" }},
		{"no targetIqn", func(ic *InstanceConfig) { ic.TargetIQN = "" }},
		{"no portal", func(ic *InstanceConfig) { ic.Portal = "" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ic := base
			c.mut(&ic)
			b := New(map[string]InstanceConfig{"remote": ic}, "", "", "", "", "", &fakeRunner{})
			if _, err := b.inst("remote"); err == nil {
				t.Fatalf("targetd instance missing a required field (%s) must be rejected", c.name)
			}
		})
	}
}

// TestInstUnknownManagementRejected: an unrecognized management value must
// fail clearly rather than silently falling into local-mode validation.
func TestInstUnknownManagementRejected(t *testing.T) {
	b := New(map[string]InstanceConfig{
		"east": {VG: "bard-vg", Portal: "10.0.0.9:3260", Management: "bogus"},
	}, "", "", "", "", "", &fakeRunner{})
	_, err := b.inst("east")
	if err == nil {
		t.Fatal("unknown management value must be rejected")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("error should name the bad management value, got %v", err)
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
	b := New(eastInst(), "", "", "", "", "", fr)

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
	b := New(eastInst(), "", "", "", "", "", fr)
	if _, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "pvc-1", Instance: "east", CapacityBytes: 1 << 30,
	}); err != nil {
		t.Fatalf("idempotent create must converge, got %v", err)
	}
	if fr.ran("lvcreate") {
		t.Fatal("must not lvcreate when the LV already exists at size")
	}
}

// TestCreateVolumeExplicitPortals: a 2+-portal instance gets explicit per-address
// LIO portals on its tpg -- the default targetcli-created portal must be torn
// down first. Substrate-verified 2026-07-19: modern targetcli auto-creates the
// default as dual-stack "::0:3260" (IPv6-any, which also holds port 3260 for
// v4), NOT "0.0.0.0:3260" -- so BOTH forms are attempted for delete, in that
// order, before the per-portal creates. Also proves the not-found/exists
// classifiers tolerate the live-observed phrasings ("No such NetworkPortal in
// configfs" on delete, "already exists in configFS" on create) instead of
// failing CreateVolume.
func TestCreateVolumeExplicitPortals(t *testing.T) {
	created := false
	fr := &fakeRunner{results: map[string]func([]string) (string, error){
		"lvs": func([]string) (string, error) {
			if !created {
				return "", errors.New("Failed to find logical volume")
			}
			return "  1073741824\n", nil
		},
		"lvcreate": func([]string) (string, error) { created = true; return "", nil },
		"targetcli": func(args []string) (string, error) {
			joined := strings.Join(args, " ")
			switch {
			case strings.Contains(joined, "/portals delete 0.0.0.0 3260"):
				// The live default is dual-stack ::0, so the legacy v4-only form is
				// absent -- a not-found that must be tolerated, not fatal.
				return "", errors.New("No such NetworkPortal in configfs: 0.0.0.0:3260")
			case strings.Contains(joined, "/portals delete ::0 3260"):
				return "", nil // the actual live default, removed cleanly
			case strings.Contains(joined, "/portals create 10.0.0.10 3260"):
				// One portal already present from a retried create -- tolerated.
				return "", errors.New("Network Portal 10.0.0.10:3260 already exists in configFS")
			default:
				return "", nil
			}
		},
	}}
	instances := map[string]InstanceConfig{"east": {VG: "bard-vg", Portals: []string{"10.0.0.9:3260", "10.0.0.10:3260"}}}
	b := New(instances, "", "", "", "", "", fr)
	lv := lvName("pvc-1")
	tpg := tpgPath(targetIQN(defaultIQNBase, lv))

	if _, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "pvc-1", Instance: "east", CapacityBytes: 1 << 30,
	}); err != nil {
		t.Fatalf("CreateVolume must converge despite tolerated not-found/exists on portal calls, got %v", err)
	}

	idxDel4 := callIndex(fr, "targetcli", tpg+"/portals", "delete", "0.0.0.0", "3260")
	idxDel6 := callIndex(fr, "targetcli", tpg+"/portals", "delete", "::0", "3260")
	idxC1 := callIndex(fr, "targetcli", tpg+"/portals", "create", "10.0.0.9", "3260")
	idxC2 := callIndex(fr, "targetcli", tpg+"/portals", "create", "10.0.0.10", "3260")
	if idxDel4 < 0 || idxDel6 < 0 || idxC1 < 0 || idxC2 < 0 {
		t.Fatalf("missing expected portal calls (del4=%d del6=%d c1=%d c2=%d); calls=%v", idxDel4, idxDel6, idxC1, idxC2, fr.calls)
	}
	if !(idxDel4 < idxDel6 && idxDel6 < idxC1 && idxC1 < idxC2) {
		t.Fatalf("expected order: delete 0.0.0.0 3260, delete ::0 3260, create per portal; got indices del4=%d del6=%d c1=%d c2=%d; calls=%v",
			idxDel4, idxDel6, idxC1, idxC2, fr.calls)
	}
}

// TestCreateVolumeSinglePortalNoPortalsCommands is the regression pin: a
// single-portal instance's CreateVolume must stay byte-identical to before this
// task -- no /portals delete/create calls at all.
func TestCreateVolumeSinglePortalNoPortalsCommands(t *testing.T) {
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
	b := New(eastInst(), "", "", "", "", "", fr)
	if _, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "pvc-1", Instance: "east", CapacityBytes: 1 << 30,
	}); err != nil {
		t.Fatal(err)
	}
	if fr.ran("targetcli", "/portals") {
		t.Fatalf("single-portal CreateVolume must not touch /portals at all; calls=%v", fr.calls)
	}
}

// ControllerPublish masks the LUN to the node and returns the connection context.
func TestControllerPublishMasksAndReturnsContext(t *testing.T) {
	fr := &fakeRunner{}
	b := New(eastInst(), "", "", "", "", "", fr)
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
	b := New(eastInst(), "", "", "", "", "", fr)
	if _, err := b.ControllerPublish(context.Background(), &bardplugin.ControllerPublishRequest{
		Volume: bardplugin.VolumeRef{Instance: "east", Name: lvName("pvc-1")}, NodeID: "n",
	}); err != nil {
		t.Fatalf("re-publish must be idempotent, got %v", err)
	}
}

// TestPublishContextCarriesPortals: a 2+-portal instance's PublishContext keeps
// `portal` as the first address (unchanged single-portal semantics for the
// node plane, which lands in task 2.2) and additively gains `portals` as the
// full comma-joined list. A single-portal instance must NOT carry the `portals`
// key at all -- the wire contract stays additive-only.
func TestPublishContextCarriesPortals(t *testing.T) {
	multi := New(map[string]InstanceConfig{
		"east": {VG: "bard-vg", Portals: []string{"10.0.0.9:3260", "10.0.0.10:3260"}},
	}, "", "", "", "", "", &fakeRunner{})
	resp, err := multi.ControllerPublish(context.Background(), &bardplugin.ControllerPublishRequest{
		Volume: bardplugin.VolumeRef{Instance: "east", Name: lvName("pvc-1")}, NodeID: "n",
	})
	if err != nil {
		t.Fatal(err)
	}
	pc := resp.PublishContext
	if pc[ctxPortal] != "10.0.0.9:3260" {
		t.Fatalf("expected ctxPortal = first portal, got %q", pc[ctxPortal])
	}
	if pc[ctxPortals] != "10.0.0.9:3260,10.0.0.10:3260" {
		t.Fatalf("expected ctxPortals = comma-joined full list, got %q", pc[ctxPortals])
	}

	single := New(eastInst(), "", "", "", "", "", &fakeRunner{})
	resp2, err := single.ControllerPublish(context.Background(), &bardplugin.ControllerPublishRequest{
		Volume: bardplugin.VolumeRef{Instance: "east", Name: lvName("pvc-1")}, NodeID: "n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := resp2.PublishContext[ctxPortals]; ok {
		t.Fatalf("single-portal publish must NOT carry a portals key (regression pin), got %+v", resp2.PublishContext)
	}
}

func TestControllerUnpublishRemovesACLAndIsIdempotent(t *testing.T) {
	// not-found on delete must be swallowed.
	fr := &fakeRunner{results: map[string]func([]string) (string, error){
		"targetcli": func([]string) (string, error) { return "", errors.New("No such path in configFS") },
	}}
	b := New(eastInst(), "", "", "", "", "", fr)
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
	b := New(eastInst(), "", "", "", "", "", fr)
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
	b := New(eastInst(), "", "", "", "", "", fr)
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
	b := New(eastInst(), "k3s-agent", stateDir, "", "", "", fr)
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
	b := New(eastInst(), "k3s-agent", stateDir, "", "", "", &fakeRunner{results: map[string]func([]string) (string, error){
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
	b := New(eastInst(), "k3s-agent", stateDir, "", "", "", &fakeRunner{results: map[string]func([]string) (string, error){
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
