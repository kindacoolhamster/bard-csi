package inspect

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"

	"github.com/kindacoolhamster/bard-csi/internal/backend"
	"github.com/kindacoolhamster/bard-csi/internal/config"
	"github.com/kindacoolhamster/bard-csi/internal/incluster"
	"github.com/kindacoolhamster/bard-csi/internal/volumeid"
)

// Options configures a scan.
type Options struct {
	Driver    string // CSI driver name (driver.DriverName)
	ZoneLabel string // node label carrying the desired zone
	Config    *config.Config
	Registry  *backend.Registry
}

// backendTruth is the slice of backend.Backend the collector needs; split out
// so tests can feed stub backends without implementing the full interface.
type backendTruth interface {
	ListVolumes(ctx context.Context) ([]backend.VolumeListEntry, error)
	ListSnapshots(ctx context.Context) ([]backend.SnapshotListEntry, error)
	GetVolumeHealth(ctx context.Context, h volumeid.Handle, secrets map[string]string) (*backend.VolumeHealth, error)
}

// Collect gathers everything Check compares: Kubernetes objects over the API
// and backend truth over the plugin sockets. PVs are the core join input, so
// an unreadable PV list fails the scan; every other collection degrades to a
// SKIP finding instead.
func Collect(ctx context.Context, opts Options) (*State, error) {
	st := &State{
		Driver:            opts.Driver,
		ZoneLabel:         opts.ZoneLabel,
		Config:            opts.Config,
		BackendVolumes:    map[string][]backend.VolumeListEntry{},
		BackendSnapshots:  map[string][]backend.SnapshotListEntry{},
		UnlistedVolumes:   map[string]bool{},
		UnlistedSnapshots: map[string]bool{},
		HealthProbes:      map[string]HealthProbe{},
	}

	code, body, err := incluster.GetCode(ctx, "/api/v1/persistentvolumes")
	if err != nil {
		return nil, fmt.Errorf("list PersistentVolumes: %w", err)
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("list PersistentVolumes: HTTP %d: %s", code, body)
	}
	if st.PVs, err = parsePVList(body); err != nil {
		return nil, err
	}

	st.HaveSnapshotContents = fetch(ctx, st,
		"/apis/snapshot.storage.k8s.io/v1/volumesnapshotcontents",
		"volumesnapshotcontents", "ghost-snapshot, orphan-snapshot",
		func(b []byte) error { var e error; st.SnapshotContents, e = parseSnapshotContentList(b); return e })
	st.HaveNodes = fetch(ctx, st,
		"/api/v1/nodes",
		"nodes", "topology checks",
		func(b []byte) error { var e error; st.Nodes, e = parseNodeList(b); return e })
	st.HaveCSINodes = fetch(ctx, st,
		"/apis/storage.k8s.io/v1/csinodes",
		"csinodes", "driver-registration checks",
		func(b []byte) error { var e error; st.CSINodes, e = parseCSINodeList(b, opts.Driver); return e })
	st.HaveAttachments = fetch(ctx, st,
		"/apis/storage.k8s.io/v1/volumeattachments",
		"volumeattachments", "stale-attachment",
		func(b []byte) error { var e error; st.Attachments, e = parseVolumeAttachmentList(b); return e })

	backends := map[string]backendTruth{}
	for _, bt := range opts.Registry.Types() {
		be, err := opts.Registry.Get(bt)
		if err != nil { // unreachable: Types() and Get() see the same map
			return nil, err
		}
		backends[bt] = be
	}
	collectBackendTruth(ctx, st, backends)
	probeGhostCandidates(ctx, st, backends)
	return st, nil
}

// fetch GETs one Kubernetes collection, parsing on 200 and recording a SKIP
// finding on 403 (RBAC), 404 (API not installed) or any other failure.
// Returns whether the collection loaded.
func fetch(ctx context.Context, st *State, path, what, affected string, parse func([]byte) error) bool {
	skip := func(detail, hint string) bool {
		st.Skips = append(st.Skips, Finding{
			Check: "collect", Severity: SeveritySkip,
			Resource: what,
			Detail:   detail + "; skipped: " + affected,
			Hint:     hint,
		})
		return false
	}
	code, body, err := incluster.GetCode(ctx, path)
	if err != nil {
		return skip(fmt.Sprintf("listing failed: %v", err), "")
	}
	switch code {
	case http.StatusOK:
		if err := parse(body); err != nil {
			return skip(err.Error(), "")
		}
		return true
	case http.StatusForbidden:
		return skip("RBAC denies reading "+path,
			"grant get,list on "+what+" to the controller ServiceAccount (the chart does this by default)")
	case http.StatusNotFound:
		return skip("API not present ("+path+" is 404)",
			"expected when the snapshot CRDs are not installed; otherwise check the API group")
	default:
		return skip(fmt.Sprintf("GET %s: HTTP %d: %s", path, code, body), "")
	}
}

// probeGhostCandidates double-checks every would-be ghost PV with a direct
// GetVolumeHealth call. A plugin that has lost an instance's config silently
// lists zero volumes for it, which would otherwise report every one of that
// instance's PVs as a ghost; the probe tells "volume really gone" from
// "plugin cannot see the instance" (surfaced live on the first real scan).
func probeGhostCandidates(ctx context.Context, st *State, backends map[string]backendTruth) {
	for _, h := range ghostCandidates(st) {
		be := backends[h.Backend]
		if be == nil {
			continue
		}
		var hp HealthProbe
		health, err := be.GetVolumeHealth(ctx, h, nil)
		switch {
		case errors.Is(err, backend.ErrUnsupported):
			continue // no probe available; the ghost stands unconfirmed
		case errors.Is(err, backend.ErrInvalidArgument):
			// The dominant cause for a parseable handle: the plugin has no
			// config for the instance. The raw error rides into the finding.
			hp.InstanceUnknown = true
			hp.Err = err.Error()
		case errors.Is(err, backend.ErrNotFound):
			hp.Confirmed = true
		case err != nil:
			hp.Err = err.Error()
		case health.Abnormal:
			hp.Confirmed = true
		default:
			hp.Healthy = true
		}
		st.HealthProbes[h.String()] = hp
	}
}

// ghostCandidates returns the handles of Bard PVs whose backend listed
// successfully but does not contain them — the inputs to the health probe.
func ghostCandidates(st *State) []volumeid.Handle {
	sets := map[string]map[string]bool{}
	for bt, entries := range st.BackendVolumes {
		set := make(map[string]bool, len(entries))
		for _, e := range entries {
			set[e.Handle.String()] = true
		}
		sets[bt] = set
	}
	var out []volumeid.Handle
	for _, pv := range st.PVs {
		if pv.Driver != st.Driver {
			continue
		}
		h, err := volumeid.Parse(pv.VolumeHandle)
		if err != nil {
			continue
		}
		if set, ok := sets[h.Backend]; ok && !set[pv.VolumeHandle] {
			out = append(out, h)
		}
	}
	return out
}

// collectBackendTruth asks each backend to enumerate its volumes and
// snapshots, sorting each type into listed / unsupported / failed (SKIP).
func collectBackendTruth(ctx context.Context, st *State, backends map[string]backendTruth) {
	skip := func(bt, what, affected string, err error) {
		st.Skips = append(st.Skips, Finding{
			Check: "collect", Severity: SeveritySkip,
			Resource: "backend/" + bt,
			Detail:   fmt.Sprintf("listing %s failed: %v; skipped: %s", what, err, affected),
			Hint:     "check the plugin sidecar's logs and its backend connectivity",
		})
	}
	types := make([]string, 0, len(backends))
	for bt := range backends {
		types = append(types, bt)
	}
	sort.Strings(types)
	for _, bt := range types {
		be := backends[bt]
		vols, err := be.ListVolumes(ctx)
		switch {
		case err == nil:
			if vols == nil {
				vols = []backend.VolumeListEntry{}
			}
			st.BackendVolumes[bt] = vols
		case errors.Is(err, backend.ErrUnsupported):
			st.UnlistedVolumes[bt] = true
		default:
			skip(bt, "volumes", "ghost-pv, orphan-volume", err)
		}

		snaps, err := be.ListSnapshots(ctx)
		switch {
		case err == nil:
			if snaps == nil {
				snaps = []backend.SnapshotListEntry{}
			}
			st.BackendSnapshots[bt] = snaps
		case errors.Is(err, backend.ErrUnsupported):
			st.UnlistedSnapshots[bt] = true
		default:
			skip(bt, "snapshots", "ghost-snapshot, orphan-snapshot", err)
		}
	}
}
