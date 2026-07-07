package inspect

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kindacoolhamster/bard-csi/internal/config"
	"github.com/kindacoolhamster/bard-csi/internal/dispatch"
	"github.com/kindacoolhamster/bard-csi/internal/volumeid"
)

// missingInstances collects the (backend, instance) pairs a health probe
// proved the plugin cannot see, keyed "backend|instance" -> probe error.
func missingInstances(st *State) map[string]string {
	out := map[string]string{}
	for hs, probe := range st.HealthProbes {
		if !probe.InstanceUnknown {
			continue
		}
		h, err := volumeid.Parse(hs)
		if err != nil {
			continue
		}
		key := h.Backend + "|" + h.Instance
		if _, dup := out[key]; !dup {
			out[key] = probe.Err
		}
	}
	return out
}

// missingInstanceFindings emits one ERROR per instance the plugin cannot
// serve: core dispatches to it (its BackendCluster exists) but the plugin has
// no config for it, so it silently lists nothing there and every operation on
// its volumes fails.
func missingInstanceFindings(missing map[string]string) []Finding {
	var out []Finding
	for _, key := range sortedKeys(missing) {
		bt, inst, _ := strings.Cut(key, "|")
		out = append(out, Finding{
			Check: "plugin-missing-instance", Severity: SeverityError,
			Resource: fmt.Sprintf("backend/%s instance/%s", bt, inst),
			Detail:   fmt.Sprintf("core dispatches to this instance but its plugin cannot serve it (probe: %s); its volumes are invisible to the plugin and every operation on them fails", missing[key]),
			Hint:     "add the instance back to the plugin's own config and restart the plugin sidecar — plugins read their per-instance config at startup",
		})
	}
	return out
}

// checkVolumes joins Bard PVs against backend volume truth, both directions:
// ghost-pv (PV whose backing volume is gone), orphan-volume (backend volume no
// PV references), plus handle validity and config-membership checks.
func checkVolumes(st *State, missing map[string]string) []Finding {
	var out []Finding

	backendVols := map[string]map[string]bool{}
	for bt, entries := range st.BackendVolumes {
		set := make(map[string]bool, len(entries))
		for _, e := range entries {
			set[e.Handle.String()] = true
		}
		backendVols[bt] = set
	}

	pvHandles := map[string]bool{}
	unverifiable := map[string]int{}

	for i := range st.PVs {
		pv := &st.PVs[i]
		if pv.Driver != st.Driver {
			continue
		}
		h, err := volumeid.Parse(pv.VolumeHandle)
		if err != nil {
			out = append(out, Finding{
				Check: "unparseable-handle", Severity: SeverityWarn,
				Resource: "pv/" + pv.Name,
				Detail:   fmt.Sprintf("volumeHandle %q is not a Bard handle: %v", pv.VolumeHandle, err),
				Hint:     "no Bard operation can address this volume; if the PV was hand-written (static provisioning), fix its volumeHandle format",
			})
			continue
		}
		pvHandles[pv.VolumeHandle] = true

		if f, known := checkHandleConfig(st, h, "pv/"+pv.Name); !known {
			out = append(out, f)
			// Still verifiable against the backend list below: an image can
			// outlive its instance's config entry.
		}

		switch {
		case st.UnlistedVolumes[h.Backend]:
			unverifiable[h.Backend]++
		case backendVols[h.Backend] != nil && !backendVols[h.Backend][pv.VolumeHandle]:
			if _, bad := missing[h.Backend+"|"+h.Instance]; bad {
				break // not a ghost: the plugin can't see the whole instance
			}
			probe, probed := st.HealthProbes[pv.VolumeHandle]
			if probed && probe.Healthy {
				out = append(out, Finding{
					Check: "list-inconsistent", Severity: SeverityWarn,
					Resource: "pv/" + pv.Name,
					Detail:   fmt.Sprintf("volume %s/%s is missing from the %s plugin's list but a direct probe says it exists", h.Location, h.Name, h.Backend),
					Hint:     "re-run to rule out a race; if it persists, the plugin's ListVolumes is dropping entries — check its logs",
				})
				break
			}
			detail := fmt.Sprintf("backing volume %s/%s not found on backend %q instance %q (PV phase %s", h.Location, h.Name, h.Backend, h.Instance, pv.Phase)
			if pv.Claim != "" {
				detail += ", bound to " + pv.Claim
			}
			detail += ")"
			switch {
			case probed && probe.Confirmed:
				detail += "; confirmed by a direct probe"
			case probed && probe.Err != "":
				detail += "; a direct probe also failed: " + probe.Err
			}
			out = append(out, Finding{
				Check: "ghost-pv", Severity: SeverityError,
				Resource: "pv/" + pv.Name,
				Detail:   detail,
				Hint:     "the data this PV promises is gone; verify directly on the backend before removing the PV/PVC (a Released PV mid-deletion can appear here transiently — re-run to confirm)",
			})
		}
	}

	// Backend volumes nothing in Kubernetes references.
	for _, bt := range sortedKeys(st.BackendVolumes) {
		for _, e := range st.BackendVolumes[bt] {
			if pvHandles[e.Handle.String()] {
				continue
			}
			out = append(out, Finding{
				Check: "orphan-volume", Severity: SeverityWarn,
				Resource: fmt.Sprintf("%s:%s %s/%s", bt, e.Handle.Instance, e.Handle.Location, e.Handle.Name),
				Detail:   "volume exists on the backend but no PV references it",
				Hint:     "re-run to rule out an in-flight provision, and if the pool is shared with another Kubernetes cluster check that cluster's PVs too; a persistent true orphan is leaked storage (e.g. a failed delete) — remove it with the backend's own tooling, Bard never deletes data on its own",
			})
		}
	}

	for _, bt := range sortedKeys(unverifiable) {
		out = append(out, Finding{
			Check: "unverifiable", Severity: SeverityInfo,
			Resource: "backend/" + bt,
			Detail:   fmt.Sprintf("%d PV(s) cannot be checked against backend truth: the %s plugin does not implement ListVolumes", unverifiable[bt], bt),
		})
	}
	return out
}

// checkSnapshots is checkVolumes for VolumeSnapshotContents vs backend
// snapshot truth.
func checkSnapshots(st *State, missing map[string]string) []Finding {
	if !st.HaveSnapshotContents {
		return nil
	}
	var out []Finding

	backendSnaps := map[string]map[string]bool{}
	for bt, entries := range st.BackendSnapshots {
		set := make(map[string]bool, len(entries))
		for _, e := range entries {
			set[e.Handle.String()] = true
		}
		backendSnaps[bt] = set
	}

	scHandles := map[string]bool{}
	unverifiable := map[string]int{}

	for i := range st.SnapshotContents {
		sc := &st.SnapshotContents[i]
		if sc.Driver != st.Driver || sc.SnapshotHandle == "" {
			continue
		}
		h, err := volumeid.Parse(sc.SnapshotHandle)
		if err != nil {
			out = append(out, Finding{
				Check: "unparseable-handle", Severity: SeverityWarn,
				Resource: "volumesnapshotcontent/" + sc.Name,
				Detail:   fmt.Sprintf("snapshotHandle %q is not a Bard handle: %v", sc.SnapshotHandle, err),
				Hint:     "no Bard operation can address this snapshot; if it was hand-written (pre-provisioned), fix its snapshotHandle format",
			})
			continue
		}
		scHandles[sc.SnapshotHandle] = true

		if f, known := checkHandleConfig(st, h, "volumesnapshotcontent/"+sc.Name); !known {
			out = append(out, f)
		}

		switch {
		case st.UnlistedSnapshots[h.Backend]:
			unverifiable[h.Backend]++
		case backendSnaps[h.Backend] != nil && !backendSnaps[h.Backend][sc.SnapshotHandle]:
			if _, bad := missing[h.Backend+"|"+h.Instance]; bad {
				break // not a ghost: the plugin can't see the whole instance
			}
			out = append(out, Finding{
				Check: "ghost-snapshot", Severity: SeverityError,
				Resource: "volumesnapshotcontent/" + sc.Name,
				Detail:   fmt.Sprintf("backing snapshot %s/%s not found on backend %q instance %q", h.Location, h.Name, h.Backend, h.Instance),
				Hint:     "restores from this snapshot will fail; verify on the backend before deleting the VolumeSnapshot/Content",
			})
		}
	}

	for _, bt := range sortedKeys(st.BackendSnapshots) {
		for _, e := range st.BackendSnapshots[bt] {
			if scHandles[e.Handle.String()] {
				continue
			}
			out = append(out, Finding{
				Check: "orphan-snapshot", Severity: SeverityWarn,
				Resource: fmt.Sprintf("%s:%s %s/%s", bt, e.Handle.Instance, e.Handle.Location, e.Handle.Name),
				Detail:   "snapshot exists on the backend but no VolumeSnapshotContent references it",
				Hint:     "re-run to rule out an in-flight snapshot; if it persists it is leaked space on the backend — remove it with the backend's own tooling",
			})
		}
	}

	for _, bt := range sortedKeys(unverifiable) {
		out = append(out, Finding{
			Check: "unverifiable", Severity: SeverityInfo,
			Resource: "backend/" + bt,
			Detail:   fmt.Sprintf("%d snapshot content(s) cannot be checked against backend truth: the %s plugin does not implement ListSnapshots", unverifiable[bt], bt),
		})
	}
	return out
}

// checkHandleConfig verifies a parsed handle's backend type + instance exist in
// the current config. known=false returns the finding to report.
func checkHandleConfig(st *State, h volumeid.Handle, resource string) (Finding, bool) {
	bc, ok := st.Config.Backends[h.Backend]
	if !ok {
		return Finding{
			Check: "unknown-instance", Severity: SeverityError,
			Resource: resource,
			Detail:   fmt.Sprintf("handle references backend type %q which is not configured; delete/expand/snapshot for it will fail", h.Backend),
			Hint:     "restore the BackendCluster(s) for this backend type, or migrate the data off it before retiring the config",
		}, false
	}
	if _, ok := bc.Instances[h.Instance]; !ok {
		return Finding{
			Check: "unknown-instance", Severity: SeverityError,
			Resource: resource,
			Detail:   fmt.Sprintf("handle references instance %q of backend %q which is not in the current config; delete/expand/snapshot for it will fail", h.Instance, h.Backend),
			Hint:     "restore the BackendCluster for this instance (its plugin config too), or migrate the data before retiring it",
		}, false
	}
	return Finding{}, true
}

// checkTopology validates the zone plumbing end to end: node labels, what the
// driver registered on each node, and whether config serves each side.
func checkTopology(st *State) []Finding {
	if !st.HaveNodes {
		return nil
	}
	var out []Finding

	csiByName := map[string]*CSINode{}
	if st.HaveCSINodes {
		for i := range st.CSINodes {
			csiByName[st.CSINodes[i].Name] = &st.CSINodes[i]
		}
	}

	nodeZones := map[string]bool{}
	for _, n := range st.Nodes {
		zone := n.Labels[st.ZoneLabel]
		if zone != "" {
			nodeZones[zone] = true
		}
		reported := n.Labels[dispatch.TopologyKeyZone]

		var registered, hasZoneKey bool
		if cn := csiByName[n.Name]; cn != nil && cn.HasDriver {
			registered = true
			for _, k := range cn.TopologyKeys {
				if k == dispatch.TopologyKeyZone {
					hasZoneKey = true
				}
			}
		}

		switch {
		case registered && !hasZoneKey:
			// The quickstart trap: the node had no zone label when the plugin
			// registered, so it advertises no topology at all.
			out = append(out, Finding{
				Check: "node-no-zone", Severity: SeverityWarn,
				Resource: "node/" + n.Name,
				Detail:   fmt.Sprintf("the Bard node plugin is registered here without topology keys (the node had no %q label at registration); provisioning for pods on this node fails with 'no topology key found'", st.ZoneLabel),
				Hint:     fmt.Sprintf("kubectl label node %s %s=<zone>, then delete the bard-csi node pod on it so it re-registers", n.Name, st.ZoneLabel),
			})
		case !registered && st.HaveCSINodes && zone == "":
			out = append(out, Finding{
				Check: "node-no-zone", Severity: SeverityInfo,
				Resource: "node/" + n.Name,
				Detail:   fmt.Sprintf("node has no %q label; if the Bard node plugin schedules here it will register without topology and provisioning on this node will fail", st.ZoneLabel),
				Hint:     "label the node before the node plugin starts (zones are deliberately required: topology selects the backend cluster)",
			})
		}

		if zone != "" && reported != "" && zone != reported {
			out = append(out, Finding{
				Check: "zone-collision", Severity: SeverityWarn,
				Resource: "node/" + n.Name,
				Detail:   fmt.Sprintf("node label %s=%q but the registered Bard topology %s=%q; kubelet refuses to change a registered topology value, so the node-driver-registrar will crashloop when the node pod restarts", st.ZoneLabel, zone, dispatch.TopologyKeyZone, reported),
				Hint:     fmt.Sprintf("one-time recovery for a re-zoned node: kubectl label node %s %s- && delete the bard-csi node pod so it re-registers", n.Name, dispatch.TopologyKeyZone),
			})
		}

		if registered && zone != "" {
			var unresolvable []string
			for bt := range st.Config.Backends {
				if !zoneResolvable(st.Config, bt, zone) {
					unresolvable = append(unresolvable, bt)
				}
			}
			if len(unresolvable) > 0 {
				sort.Strings(unresolvable)
				out = append(out, Finding{
					Check: "zone-no-backend", Severity: SeverityWarn,
					Resource: "node/" + n.Name,
					Detail:   fmt.Sprintf("zone %q is not served by any instance of backend(s) %s and no default instance is set; PVCs for those backends cannot provision for pods on this node", zone, strings.Join(unresolvable, ", ")),
					Hint:     fmt.Sprintf("add a BackendCluster with zone %q, or set a default instance for the backend type", zone),
				})
			}
		}
	}

	// Instances whose zone no node carries: their volumes are currently
	// unmountable and no new pods will land there.
	for _, bt := range sortedKeys(st.Config.Backends) {
		bc := st.Config.Backends[bt]
		for _, inst := range sortedKeys(bc.Instances) {
			if zone := bc.Instances[inst].Zone; !nodeZones[zone] {
				out = append(out, Finding{
					Check: "backend-no-nodes", Severity: SeverityInfo,
					Resource: fmt.Sprintf("backend/%s instance/%s", bt, inst),
					Detail:   fmt.Sprintf("no node carries %s=%q, the zone this instance serves; its volumes cannot be mounted and nothing will provision into it", st.ZoneLabel, zone),
					Hint:     "expected mid-migration or before nodes join; otherwise check the zone value for a typo",
				})
			}
		}
	}
	return out
}

// zoneResolvable mirrors dispatch.Resolve's fallbacks: a zone is served if an
// instance claims it, a default exists, or the type has exactly one instance.
func zoneResolvable(cfg *config.Config, bt, zone string) bool {
	bc := cfg.Backends[bt]
	for _, ic := range bc.Instances {
		if ic.Zone == zone {
			return true
		}
	}
	if cfg.Defaults[bt] != "" {
		return true
	}
	return len(bc.Instances) == 1
}

// checkAttachments flags VolumeAttachments pointing at things that no longer
// exist — the debris a force-deleted node or failover leaves behind.
func checkAttachments(st *State) []Finding {
	if !st.HaveAttachments {
		return nil
	}
	var out []Finding

	nodeSet := map[string]bool{}
	for _, n := range st.Nodes {
		nodeSet[n.Name] = true
	}
	pvSet := map[string]bool{}
	for _, pv := range st.PVs {
		pvSet[pv.Name] = true
	}

	for _, va := range st.Attachments {
		if va.Attacher != st.Driver {
			continue
		}
		if st.HaveNodes && va.NodeName != "" && !nodeSet[va.NodeName] {
			out = append(out, Finding{
				Check: "stale-attachment", Severity: SeverityWarn,
				Resource: "volumeattachment/" + va.Name,
				Detail:   fmt.Sprintf("attached to node %q which no longer exists", va.NodeName),
				Hint:     "left behind by a deleted node (e.g. the failover path); once sure the node is gone for good, kubectl delete volumeattachment " + va.Name,
			})
		}
		if va.PV != "" && !pvSet[va.PV] {
			out = append(out, Finding{
				Check: "stale-attachment", Severity: SeverityWarn,
				Resource: "volumeattachment/" + va.Name,
				Detail:   fmt.Sprintf("references PV %q which no longer exists", va.PV),
				Hint:     "kubectl delete volumeattachment " + va.Name,
			})
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
