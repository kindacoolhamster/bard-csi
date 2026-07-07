package inspect

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kindacoolhamster/bard-csi/internal/backend"
	"github.com/kindacoolhamster/bard-csi/internal/config"
	"github.com/kindacoolhamster/bard-csi/internal/volumeid"
)

const (
	testDriver = "csi.bard.io"
	zoneLabel  = "topology.kubernetes.io/zone"
	bardZone   = "topology.csi.bard.io/zone"
)

func h(name string) volumeid.Handle {
	return volumeid.Handle{Backend: "ceph-rbd", Instance: "east", Location: "pool", Name: name}
}

// cleanState returns a fully consistent state: one PV matching one backend
// volume, one snapshot content matching one backend snapshot, a labeled +
// registered node, and a live attachment. Tests mutate it to trip one check.
func cleanState() *State {
	vol := h("csi-vol-1")
	snap := h("csi-vol-1@csi-snap-1")
	return &State{
		Driver:    testDriver,
		ZoneLabel: zoneLabel,
		Config: &config.Config{
			Backends: map[string]config.BackendConfig{
				"ceph-rbd": {Instances: map[string]config.InstanceConfig{
					"east": {Zone: "zone-a"},
				}},
			},
		},
		PVs: []PV{{
			Name: "pv-1", Driver: testDriver, VolumeHandle: vol.String(),
			Phase: "Bound", Claim: "default/data",
		}},
		SnapshotContents: []SnapshotContent{{
			Name: "vsc-1", Driver: testDriver, SnapshotHandle: snap.String(),
		}},
		Nodes: []Node{{
			Name:   "n1",
			Labels: map[string]string{zoneLabel: "zone-a", bardZone: "zone-a"},
		}},
		CSINodes: []CSINode{{
			Name: "n1", HasDriver: true, TopologyKeys: []string{bardZone},
		}},
		Attachments: []VolumeAttachment{{
			Name: "va-1", Attacher: testDriver, NodeName: "n1", PV: "pv-1", Attached: true,
		}},
		BackendVolumes: map[string][]backend.VolumeListEntry{
			"ceph-rbd": {{Handle: vol, CapacityBytes: 1 << 30}},
		},
		BackendSnapshots: map[string][]backend.SnapshotListEntry{
			"ceph-rbd": {{Handle: snap, SourceVolume: vol}},
		},
		UnlistedVolumes:      map[string]bool{},
		UnlistedSnapshots:    map[string]bool{},
		HaveSnapshotContents: true,
		HaveNodes:            true,
		HaveCSINodes:         true,
		HaveAttachments:      true,
	}
}

// want asserts exactly one finding with the given check+severity whose detail
// contains substr, and no other findings.
func want(t *testing.T, rep Report, check string, sev Severity, substr string) {
	t.Helper()
	if len(rep.Findings) != 1 {
		t.Fatalf("want exactly 1 finding, got %d: %+v", len(rep.Findings), rep.Findings)
	}
	f := rep.Findings[0]
	if f.Check != check || f.Severity != sev {
		t.Fatalf("want %s/%s, got %s/%s (%s)", check, sev, f.Check, f.Severity, f.Detail)
	}
	if !strings.Contains(f.Detail, substr) {
		t.Fatalf("detail %q does not contain %q", f.Detail, substr)
	}
}

func TestCleanStateHasNoFindings(t *testing.T) {
	rep := Check(cleanState())
	if len(rep.Findings) != 0 {
		t.Fatalf("clean state produced findings: %+v", rep.Findings)
	}
	if rep.HasErrors() {
		t.Fatal("clean state reports errors")
	}
	if rep.Stats.PVs != 1 || rep.Stats.BackendVolumes != 1 || rep.Stats.SnapshotContents != 1 ||
		rep.Stats.BackendSnapshots != 1 || rep.Stats.Nodes != 1 || rep.Stats.Attachments != 1 {
		t.Fatalf("stats wrong: %+v", rep.Stats)
	}
}

func TestGhostPV(t *testing.T) {
	st := cleanState()
	st.BackendVolumes["ceph-rbd"] = nil // listed OK, but the volume is gone
	st.BackendSnapshots["ceph-rbd"] = nil
	rep := Check(st)
	// ghost-pv for the volume AND ghost-snapshot for its snapshot.
	if len(rep.Findings) != 2 {
		t.Fatalf("want 2 findings, got %+v", rep.Findings)
	}
	if rep.Findings[0].Check != "ghost-pv" || rep.Findings[0].Severity != SeverityError {
		t.Fatalf("got %+v", rep.Findings[0])
	}
	if !strings.Contains(rep.Findings[0].Detail, "bound to default/data") {
		t.Fatalf("ghost-pv detail should carry the claim: %q", rep.Findings[0].Detail)
	}
	if rep.Findings[1].Check != "ghost-snapshot" {
		t.Fatalf("got %+v", rep.Findings[1])
	}
	if !rep.HasErrors() {
		t.Fatal("ghosts must be errors")
	}
}

func TestOrphanVolumeAndSnapshot(t *testing.T) {
	st := cleanState()
	st.BackendVolumes["ceph-rbd"] = append(st.BackendVolumes["ceph-rbd"],
		backend.VolumeListEntry{Handle: h("csi-vol-leaked")})
	st.BackendSnapshots["ceph-rbd"] = append(st.BackendSnapshots["ceph-rbd"],
		backend.SnapshotListEntry{Handle: h("csi-vol-leaked@csi-snap-x")})
	rep := Check(st)
	if len(rep.Findings) != 2 {
		t.Fatalf("want 2 findings, got %+v", rep.Findings)
	}
	if rep.Findings[0].Check != "orphan-snapshot" && rep.Findings[1].Check != "orphan-snapshot" {
		t.Fatalf("missing orphan-snapshot: %+v", rep.Findings)
	}
	for _, f := range rep.Findings {
		if f.Severity != SeverityWarn {
			t.Fatalf("orphans are warnings, got %+v", f)
		}
		if !strings.Contains(f.Resource, "ceph-rbd:east") {
			t.Fatalf("orphan resource should name backend:instance, got %q", f.Resource)
		}
	}
	if rep.HasErrors() {
		t.Fatal("orphans alone must not flip the error exit code")
	}
}

func TestUnknownInstance(t *testing.T) {
	st := cleanState()
	bad := volumeid.Handle{Backend: "ceph-rbd", Instance: "gone", Location: "pool", Name: "csi-vol-2"}
	st.PVs = append(st.PVs, PV{Name: "pv-2", Driver: testDriver, VolumeHandle: bad.String(), Phase: "Bound"})
	// Keep the backend list consistent so only unknown-instance fires.
	st.BackendVolumes["ceph-rbd"] = append(st.BackendVolumes["ceph-rbd"], backend.VolumeListEntry{Handle: bad})
	rep := Check(st)
	want(t, rep, "unknown-instance", SeverityError, `instance "gone"`)
}

func TestUnknownBackendType(t *testing.T) {
	st := cleanState()
	bad := volumeid.Handle{Backend: "lvm", Instance: "vg1", Location: "", Name: "bard-x"}
	st.PVs = append(st.PVs, PV{Name: "pv-2", Driver: testDriver, VolumeHandle: bad.String(), Phase: "Bound"})
	rep := Check(st)
	// unknown-instance (backend type variant); the lvm PV is neither listed nor
	// unlisted (no such backend), so no ghost check fires. backend-no-nodes
	// doesn't either (lvm isn't in config).
	want(t, rep, "unknown-instance", SeverityError, `backend type "lvm"`)
}

func TestUnparseableHandle(t *testing.T) {
	st := cleanState()
	st.PVs = append(st.PVs, PV{Name: "pv-2", Driver: testDriver, VolumeHandle: "not-a-handle", Phase: "Bound"})
	rep := Check(st)
	want(t, rep, "unparseable-handle", SeverityWarn, "not-a-handle")
}

func TestUnverifiableBackend(t *testing.T) {
	st := cleanState()
	delete(st.BackendVolumes, "ceph-rbd")
	delete(st.BackendSnapshots, "ceph-rbd")
	st.UnlistedVolumes["ceph-rbd"] = true
	st.UnlistedSnapshots["ceph-rbd"] = true
	rep := Check(st)
	if len(rep.Findings) != 2 {
		t.Fatalf("want 2 unverifiable findings, got %+v", rep.Findings)
	}
	for _, f := range rep.Findings {
		if f.Check != "unverifiable" || f.Severity != SeverityInfo {
			t.Fatalf("got %+v", f)
		}
	}
}

func TestOtherDriversIgnored(t *testing.T) {
	st := cleanState()
	st.PVs = append(st.PVs, PV{Name: "pv-other", Driver: "other.csi.io", VolumeHandle: "whatever", Phase: "Bound"})
	st.SnapshotContents = append(st.SnapshotContents, SnapshotContent{Name: "vsc-other", Driver: "other.csi.io", SnapshotHandle: "x"})
	st.Attachments = append(st.Attachments, VolumeAttachment{Name: "va-other", Attacher: "other.csi.io", NodeName: "gone", PV: "nope"})
	rep := Check(st)
	if len(rep.Findings) != 0 {
		t.Fatalf("foreign drivers must be ignored: %+v", rep.Findings)
	}
	if rep.Stats.PVs != 1 {
		t.Fatalf("stats must count only Bard PVs: %+v", rep.Stats)
	}
}

func TestNodeNoZoneRegistered(t *testing.T) {
	st := cleanState()
	// Node registered the driver but with no topology keys (the quickstart trap).
	st.Nodes[0].Labels = map[string]string{}
	st.CSINodes[0].TopologyKeys = nil
	rep := Check(st)
	// node-no-zone WARN + backend-no-nodes INFO (no node carries zone-a anymore).
	if len(rep.Findings) != 2 {
		t.Fatalf("want 2 findings, got %+v", rep.Findings)
	}
	if rep.Findings[0].Check != "node-no-zone" || rep.Findings[0].Severity != SeverityWarn {
		t.Fatalf("got %+v", rep.Findings[0])
	}
	if !strings.Contains(rep.Findings[0].Detail, "no topology key found") {
		t.Fatalf("should quote the provisioner error: %q", rep.Findings[0].Detail)
	}
	if rep.Findings[1].Check != "backend-no-nodes" || rep.Findings[1].Severity != SeverityInfo {
		t.Fatalf("got %+v", rep.Findings[1])
	}
}

func TestNodeNoZoneUnregistered(t *testing.T) {
	st := cleanState()
	st.Nodes = append(st.Nodes, Node{Name: "n2", Labels: map[string]string{}})
	st.CSINodes = append(st.CSINodes, CSINode{Name: "n2"}) // driver not registered
	rep := Check(st)
	want(t, rep, "node-no-zone", SeverityInfo, "no "+`"`+zoneLabel+`"`+" label")
}

func TestZoneCollision(t *testing.T) {
	st := cleanState()
	st.Nodes[0].Labels[zoneLabel] = "zone-b" // re-zoned after registration
	rep := Check(st)
	// zone-collision + zone-no-backend (zone-b unserved) + backend-no-nodes
	// (zone-a now empty) all fire; find the collision.
	var found bool
	for _, f := range rep.Findings {
		if f.Check == "zone-collision" {
			found = true
			if f.Severity != SeverityWarn || !strings.Contains(f.Detail, "crashloop") {
				t.Fatalf("got %+v", f)
			}
		}
	}
	if !found {
		t.Fatalf("no zone-collision in %+v", rep.Findings)
	}
}

func TestZoneNoBackendRespectsDispatchFallbacks(t *testing.T) {
	st := cleanState()
	st.Nodes[0].Labels = map[string]string{zoneLabel: "zone-x", bardZone: "zone-x"}
	// Single instance => dispatch resolves anyway; only backend-no-nodes fires.
	rep := Check(st)
	for _, f := range rep.Findings {
		if f.Check == "zone-no-backend" {
			t.Fatalf("single-instance fallback must suppress zone-no-backend: %+v", f)
		}
	}

	// Two instances, no default: now zone-x really is unservable.
	st.Config.Backends["ceph-rbd"].Instances["west"] = config.InstanceConfig{Zone: "zone-w"}
	rep = Check(st)
	var found bool
	for _, f := range rep.Findings {
		if f.Check == "zone-no-backend" && strings.Contains(f.Detail, "ceph-rbd") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no zone-no-backend in %+v", rep.Findings)
	}

	// A default instance restores resolvability.
	st.Config.Defaults = map[string]string{"ceph-rbd": "east"}
	rep = Check(st)
	for _, f := range rep.Findings {
		if f.Check == "zone-no-backend" {
			t.Fatalf("default instance must suppress zone-no-backend: %+v", f)
		}
	}
}

func TestStaleAttachment(t *testing.T) {
	st := cleanState()
	st.Attachments = append(st.Attachments,
		VolumeAttachment{Name: "va-node-gone", Attacher: testDriver, NodeName: "gone", PV: "pv-1"},
		VolumeAttachment{Name: "va-pv-gone", Attacher: testDriver, NodeName: "n1", PV: "pv-gone"},
	)
	rep := Check(st)
	if len(rep.Findings) != 2 {
		t.Fatalf("want 2 findings, got %+v", rep.Findings)
	}
	for _, f := range rep.Findings {
		if f.Check != "stale-attachment" || f.Severity != SeverityWarn {
			t.Fatalf("got %+v", f)
		}
	}
}

func TestSkipsSurfaceInReport(t *testing.T) {
	st := cleanState()
	st.HaveSnapshotContents = false
	st.Skips = append(st.Skips, Finding{Check: "collect", Severity: SeveritySkip, Resource: "volumesnapshotcontents", Detail: "RBAC denies"})
	rep := Check(st)
	want(t, rep, "collect", SeveritySkip, "RBAC")
}

func TestFindingsSortedBySeverity(t *testing.T) {
	st := cleanState()
	st.BackendVolumes["ceph-rbd"] = append(st.BackendVolumes["ceph-rbd"],
		backend.VolumeListEntry{Handle: h("csi-vol-leaked")}) // WARN
	st.PVs = append(st.PVs, PV{Name: "pv-ghost", Driver: testDriver,
		VolumeHandle: h("csi-vol-gone").String(), Phase: "Bound"}) // ERROR
	st.Skips = append(st.Skips, Finding{Check: "collect", Severity: SeveritySkip, Resource: "x", Detail: "d"})
	rep := Check(st)
	if len(rep.Findings) != 3 {
		t.Fatalf("want 3 findings, got %+v", rep.Findings)
	}
	if rep.Findings[0].Severity != SeverityError || rep.Findings[1].Severity != SeverityWarn || rep.Findings[2].Severity != SeveritySkip {
		t.Fatalf("wrong order: %+v", rep.Findings)
	}
}

// ---- collector: backend truth trichotomy --------------------------------

type stubLister struct {
	vols    []backend.VolumeListEntry
	volErr  error
	snaps   []backend.SnapshotListEntry
	snapErr error
	health  func() (*backend.VolumeHealth, error)
}

func (s *stubLister) ListVolumes(context.Context) ([]backend.VolumeListEntry, error) {
	return s.vols, s.volErr
}
func (s *stubLister) ListSnapshots(context.Context) ([]backend.SnapshotListEntry, error) {
	return s.snaps, s.snapErr
}
func (s *stubLister) GetVolumeHealth(context.Context, volumeid.Handle, map[string]string) (*backend.VolumeHealth, error) {
	if s.health != nil {
		return s.health()
	}
	return nil, backend.ErrUnsupported
}

func TestCollectBackendTruth(t *testing.T) {
	st := &State{
		BackendVolumes:    map[string][]backend.VolumeListEntry{},
		BackendSnapshots:  map[string][]backend.SnapshotListEntry{},
		UnlistedVolumes:   map[string]bool{},
		UnlistedSnapshots: map[string]bool{},
	}
	collectBackendTruth(context.Background(), st, map[string]backendTruth{
		"ok":          &stubLister{vols: []backend.VolumeListEntry{{Handle: h("csi-vol-1")}}, snapErr: backend.ErrUnsupported},
		"unsupported": &stubLister{volErr: backend.ErrUnsupported, snapErr: backend.ErrUnsupported},
		"broken":      &stubLister{volErr: errors.New("socket down"), snapErr: errors.New("socket down")},
	})
	if len(st.BackendVolumes["ok"]) != 1 {
		t.Fatalf("ok backend not listed: %+v", st.BackendVolumes)
	}
	if !st.UnlistedSnapshots["ok"] || !st.UnlistedVolumes["unsupported"] {
		t.Fatalf("unsupported not recorded: vols=%v snaps=%v", st.UnlistedVolumes, st.UnlistedSnapshots)
	}
	if _, listed := st.BackendVolumes["broken"]; listed {
		t.Fatal("broken backend must not appear listed")
	}
	if len(st.Skips) != 2 {
		t.Fatalf("broken backend must record 2 SKIPs, got %+v", st.Skips)
	}
	// An empty successful list must be non-nil so checks can tell "listed,
	// empty" from "not listed".
	st2 := &State{
		BackendVolumes:    map[string][]backend.VolumeListEntry{},
		BackendSnapshots:  map[string][]backend.SnapshotListEntry{},
		UnlistedVolumes:   map[string]bool{},
		UnlistedSnapshots: map[string]bool{},
	}
	collectBackendTruth(context.Background(), st2, map[string]backendTruth{"empty": &stubLister{}})
	if st2.BackendVolumes["empty"] == nil || st2.BackendSnapshots["empty"] == nil {
		t.Fatal("successful empty list must be non-nil")
	}
}

// ---- ghost probes ---------------------------------------------------------

func TestGhostConfirmedByProbe(t *testing.T) {
	st := cleanState()
	st.BackendVolumes["ceph-rbd"] = nil
	st.BackendSnapshots["ceph-rbd"] = nil
	st.HealthProbes = map[string]HealthProbe{
		h("csi-vol-1").String(): {Confirmed: true},
	}
	rep := Check(st)
	if rep.Findings[0].Check != "ghost-pv" || !strings.Contains(rep.Findings[0].Detail, "confirmed by a direct probe") {
		t.Fatalf("got %+v", rep.Findings[0])
	}
}

func TestFalseGhostHealthyProbe(t *testing.T) {
	st := cleanState()
	st.BackendVolumes["ceph-rbd"] = nil // list says gone...
	st.HealthProbes = map[string]HealthProbe{
		h("csi-vol-1").String(): {Healthy: true}, // ...but the probe says alive
	}
	// Drop the snapshot side so only the volume path is under test.
	st.SnapshotContents = nil
	st.BackendSnapshots = map[string][]backend.SnapshotListEntry{}
	rep := Check(st)
	want(t, rep, "list-inconsistent", SeverityWarn, "a direct probe says it exists")
	if rep.HasErrors() {
		t.Fatal("a healthy probe must suppress the ghost error")
	}
}

func TestPluginMissingInstance(t *testing.T) {
	// The live-found failure mode: the plugin lost an instance's config, so it
	// lists zero volumes there — every PV and snapshot on the instance would
	// read as a ghost. One plugin-missing-instance ERROR must replace them all.
	st := cleanState()
	st.BackendVolumes["ceph-rbd"] = nil
	st.BackendSnapshots["ceph-rbd"] = nil
	st.PVs = append(st.PVs, PV{Name: "pv-2", Driver: testDriver,
		VolumeHandle: h("csi-vol-2").String(), Phase: "Bound"})
	st.HealthProbes = map[string]HealthProbe{
		h("csi-vol-1").String(): {InstanceUnknown: true, Err: `no cluster configured for instance "east"`},
		h("csi-vol-2").String(): {InstanceUnknown: true, Err: `no cluster configured for instance "east"`},
	}
	rep := Check(st)
	if len(rep.Findings) != 1 {
		t.Fatalf("want exactly 1 finding, got %+v", rep.Findings)
	}
	f := rep.Findings[0]
	if f.Check != "plugin-missing-instance" || f.Severity != SeverityError {
		t.Fatalf("got %+v", f)
	}
	if !strings.Contains(f.Resource, "instance/east") || !strings.Contains(f.Detail, "no cluster configured") {
		t.Fatalf("got %+v", f)
	}
}

func TestProbeGhostCandidates(t *testing.T) {
	mkState := func() *State {
		st := cleanState()
		st.BackendVolumes["ceph-rbd"] = nil // makes csi-vol-1 a candidate
		st.HealthProbes = map[string]HealthProbe{}
		return st
	}
	cases := []struct {
		name   string
		health func() (*backend.VolumeHealth, error)
		want   func(t *testing.T, probes map[string]HealthProbe)
	}{
		{"unsupported", func() (*backend.VolumeHealth, error) { return nil, backend.ErrUnsupported },
			func(t *testing.T, p map[string]HealthProbe) {
				if len(p) != 0 {
					t.Fatalf("unsupported probe must record nothing: %+v", p)
				}
			}},
		{"invalid-argument", func() (*backend.VolumeHealth, error) {
			return nil, fmt.Errorf("no cluster configured: %w", backend.ErrInvalidArgument)
		}, func(t *testing.T, p map[string]HealthProbe) {
			if pr := p[h("csi-vol-1").String()]; !pr.InstanceUnknown || pr.Err == "" {
				t.Fatalf("got %+v", p)
			}
		}},
		{"not-found", func() (*backend.VolumeHealth, error) { return nil, backend.ErrNotFound },
			func(t *testing.T, p map[string]HealthProbe) {
				if !p[h("csi-vol-1").String()].Confirmed {
					t.Fatalf("got %+v", p)
				}
			}},
		{"abnormal", func() (*backend.VolumeHealth, error) {
			return &backend.VolumeHealth{Abnormal: true, Message: "no longer exists"}, nil
		}, func(t *testing.T, p map[string]HealthProbe) {
			if !p[h("csi-vol-1").String()].Confirmed {
				t.Fatalf("got %+v", p)
			}
		}},
		{"healthy", func() (*backend.VolumeHealth, error) { return &backend.VolumeHealth{}, nil },
			func(t *testing.T, p map[string]HealthProbe) {
				if !p[h("csi-vol-1").String()].Healthy {
					t.Fatalf("got %+v", p)
				}
			}},
		{"other-error", func() (*backend.VolumeHealth, error) { return nil, errors.New("mon down") },
			func(t *testing.T, p map[string]HealthProbe) {
				if pr := p[h("csi-vol-1").String()]; pr.Err != "mon down" || pr.Confirmed || pr.Healthy || pr.InstanceUnknown {
					t.Fatalf("got %+v", p)
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := mkState()
			probeGhostCandidates(context.Background(), st,
				map[string]backendTruth{"ceph-rbd": &stubLister{health: tc.health}})
			tc.want(t, st.HealthProbes)
		})
	}
}

// ---- k8s decoders --------------------------------------------------------

func TestParsePVList(t *testing.T) {
	body := []byte(`{"items":[
	  {"metadata":{"name":"pv-1"},
	   "spec":{"csi":{"driver":"csi.bard.io","volumeHandle":"swsk|1|ceph-rbd|east|pool|csi-vol-1"},
	           "storageClassName":"bard-rbd",
	           "claimRef":{"namespace":"default","name":"data"}},
	   "status":{"phase":"Bound"}},
	  {"metadata":{"name":"pv-hostpath"},
	   "spec":{"storageClassName":"standard"},
	   "status":{"phase":"Bound"}}
	]}`)
	pvs, err := parsePVList(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(pvs) != 2 {
		t.Fatalf("got %+v", pvs)
	}
	if pvs[0].Driver != "csi.bard.io" || pvs[0].VolumeHandle != "swsk|1|ceph-rbd|east|pool|csi-vol-1" ||
		pvs[0].Claim != "default/data" || pvs[0].Phase != "Bound" || pvs[0].StorageClass != "bard-rbd" {
		t.Fatalf("got %+v", pvs[0])
	}
	if pvs[1].Driver != "" { // non-CSI PV decodes without a driver
		t.Fatalf("got %+v", pvs[1])
	}
}

func TestParseSnapshotContentList(t *testing.T) {
	body := []byte(`{"items":[
	  {"metadata":{"name":"vsc-ready"},
	   "spec":{"driver":"csi.bard.io","source":{"volumeHandle":"ignored"}},
	   "status":{"snapshotHandle":"swsk|1|ceph-rbd|east|pool|img@snap"}},
	  {"metadata":{"name":"vsc-preprov"},
	   "spec":{"driver":"csi.bard.io","source":{"snapshotHandle":"swsk|1|ceph-rbd|east|pool|img@pre"}}},
	  {"metadata":{"name":"vsc-pending"},
	   "spec":{"driver":"csi.bard.io","source":{"volumeHandle":"v"}}}
	]}`)
	scs, err := parseSnapshotContentList(body)
	if err != nil {
		t.Fatal(err)
	}
	if scs[0].SnapshotHandle != "swsk|1|ceph-rbd|east|pool|img@snap" {
		t.Fatalf("got %+v", scs[0])
	}
	if scs[1].SnapshotHandle != "swsk|1|ceph-rbd|east|pool|img@pre" {
		t.Fatalf("pre-provisioned source handle not used: %+v", scs[1])
	}
	if scs[2].SnapshotHandle != "" {
		t.Fatalf("pending content must have no handle: %+v", scs[2])
	}
}

func TestParseNodeAndCSINodeLists(t *testing.T) {
	nodes, err := parseNodeList([]byte(`{"items":[{"metadata":{"name":"n1","labels":{"topology.kubernetes.io/zone":"z"}}}]}`))
	if err != nil || len(nodes) != 1 || nodes[0].Labels[zoneLabel] != "z" {
		t.Fatalf("err=%v nodes=%+v", err, nodes)
	}
	csis, err := parseCSINodeList([]byte(`{"items":[
	  {"metadata":{"name":"n1"},"spec":{"drivers":[{"name":"csi.bard.io","topologyKeys":["topology.csi.bard.io/zone"]}]}},
	  {"metadata":{"name":"n2"},"spec":{"drivers":[{"name":"other.io"}]}},
	  {"metadata":{"name":"n3"},"spec":{"drivers":null}}
	]}`), testDriver)
	if err != nil || len(csis) != 3 {
		t.Fatalf("err=%v csis=%+v", err, csis)
	}
	if !csis[0].HasDriver || len(csis[0].TopologyKeys) != 1 {
		t.Fatalf("got %+v", csis[0])
	}
	if csis[1].HasDriver || csis[2].HasDriver {
		t.Fatalf("foreign/no driver must not count: %+v", csis[1:])
	}
}

func TestParseVolumeAttachmentList(t *testing.T) {
	vas, err := parseVolumeAttachmentList([]byte(`{"items":[
	  {"metadata":{"name":"va-1"},
	   "spec":{"attacher":"csi.bard.io","nodeName":"n1","source":{"persistentVolumeName":"pv-1"}},
	   "status":{"attached":true}}
	]}`))
	if err != nil || len(vas) != 1 {
		t.Fatalf("err=%v vas=%+v", err, vas)
	}
	if vas[0].Attacher != testDriver || vas[0].NodeName != "n1" || vas[0].PV != "pv-1" || !vas[0].Attached {
		t.Fatalf("got %+v", vas[0])
	}
}
