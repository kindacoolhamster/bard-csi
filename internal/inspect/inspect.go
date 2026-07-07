// Package inspect implements Bard's consistency scanner: it joins what
// Kubernetes believes (PVs, VolumeSnapshotContents, VolumeAttachments, node
// topology) against backend truth (what actually exists on each backend, via
// the plugins' ListVolumes/ListSnapshots) and the driver's own config, and
// reports the drift as findings — ghost PVs whose backing volume is gone,
// orphaned backend volumes no PV references, snapshot mismatches, and
// topology misconfiguration.
//
// The scanner is read-only: every finding carries a remediation hint, and it
// never mutates cluster or backend state itself. It runs where the plugin
// sockets are reachable — the controller pod — as `bard-csi --inspect`;
// `kubectl bard inspect` is a thin wrapper around that.
package inspect

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/kindacoolhamster/bard-csi/internal/backend"
	"github.com/kindacoolhamster/bard-csi/internal/config"
)

// Severity classifies a finding. ERROR means data is (or is about to be)
// unreachable; WARN is drift that needs an operator decision; INFO is
// context worth knowing; SKIP records a check that could not run.
type Severity string

const (
	SeverityError Severity = "ERROR"
	SeverityWarn  Severity = "WARN"
	SeverityInfo  Severity = "INFO"
	SeveritySkip  Severity = "SKIP"
)

// rank orders severities for display, most severe first.
func (s Severity) rank() int {
	switch s {
	case SeverityError:
		return 0
	case SeverityWarn:
		return 1
	case SeverityInfo:
		return 2
	default:
		return 3
	}
}

// Finding is one inconsistency (or skipped check). Resource identifies what
// it is about (e.g. "pv/pvc-123", "node/worker-1", or a backend-side object).
type Finding struct {
	Check    string   `json:"check"`
	Severity Severity `json:"severity"`
	Resource string   `json:"resource"`
	Detail   string   `json:"detail"`
	Hint     string   `json:"hint,omitempty"`
}

// Stats counts what the scan actually looked at, so an empty findings list is
// distinguishable from a scan that saw nothing.
type Stats struct {
	PVs              int `json:"pvs"`
	BackendVolumes   int `json:"backendVolumes"`
	SnapshotContents int `json:"snapshotContents"`
	BackendSnapshots int `json:"backendSnapshots"`
	Nodes            int `json:"nodes"`
	Attachments      int `json:"attachments"`
}

// Report is the scan result.
type Report struct {
	Findings []Finding `json:"findings"`
	Stats    Stats     `json:"stats"`
}

// HasErrors reports whether any finding is ERROR severity (the exit-code
// contract: errors mean data is at risk, not just untidy).
func (r Report) HasErrors() bool {
	for _, f := range r.Findings {
		if f.Severity == SeverityError {
			return true
		}
	}
	return false
}

// WriteJSON writes the report as indented JSON.
func (r Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// WriteTable writes the human-readable report.
func (r Report) WriteTable(w io.Writer) {
	fmt.Fprintf(w, "scanned %d PVs / %d backend volumes, %d snapshot contents / %d backend snapshots, %d nodes, %d attachments\n",
		r.Stats.PVs, r.Stats.BackendVolumes, r.Stats.SnapshotContents, r.Stats.BackendSnapshots, r.Stats.Nodes, r.Stats.Attachments)
	if len(r.Findings) == 0 {
		fmt.Fprintln(w, "\nno findings: Kubernetes and backend state are consistent")
		return
	}
	counts := map[Severity]int{}
	for _, f := range r.Findings {
		counts[f.Severity]++
		fmt.Fprintf(w, "\n[%s] %s  %s\n", f.Severity, f.Check, f.Resource)
		fmt.Fprintf(w, "    %s\n", f.Detail)
		if f.Hint != "" {
			fmt.Fprintf(w, "    hint: %s\n", f.Hint)
		}
	}
	fmt.Fprintf(w, "\n%d error(s), %d warning(s), %d info, %d skipped\n",
		counts[SeverityError], counts[SeverityWarn], counts[SeverityInfo], counts[SeveritySkip])
}

// State is everything a scan compares, gathered by Collect (or constructed
// directly in tests). Check is a pure function over it.
type State struct {
	Driver    string // CSI driver name Kubernetes objects are matched on
	ZoneLabel string // node label carrying the desired zone (the dispatch input)
	Config    *config.Config

	PVs              []PV
	SnapshotContents []SnapshotContent
	Nodes            []Node
	CSINodes         []CSINode
	Attachments      []VolumeAttachment

	// Backend truth per backend type. A type present in BackendVolumes was
	// listed successfully (possibly empty); a type in UnlistedVolumes does not
	// implement listing; a type in neither had a list failure, recorded in
	// Skips. Same split for snapshots.
	BackendVolumes    map[string][]backend.VolumeListEntry
	BackendSnapshots  map[string][]backend.SnapshotListEntry
	UnlistedVolumes   map[string]bool
	UnlistedSnapshots map[string]bool

	// HealthProbes double-checks ghost candidates, keyed by encoded handle:
	// a plugin that has lost an instance's config lists zero volumes for it,
	// which would otherwise report every one of that instance's PVs as a
	// ghost. Populated by Collect via GetVolumeHealth where supported.
	HealthProbes map[string]HealthProbe

	// Collection gaps (RBAC denied, API absent, plugin list failure),
	// pre-recorded as SKIP findings so a degraded scan is never silent.
	Skips []Finding

	// Which Kubernetes collections loaded; checks needing an unloaded one are
	// skipped (the corresponding SKIP finding is already in Skips).
	HaveSnapshotContents bool
	HaveNodes            bool
	HaveCSINodes         bool
	HaveAttachments      bool
}

// PV is the slice of a PersistentVolume the scanner needs.
type PV struct {
	Name         string
	Driver       string // spec.csi.driver; "" for non-CSI PVs
	VolumeHandle string
	Phase        string
	StorageClass string
	Claim        string // "namespace/name" of the bound claim, for messages
}

// SnapshotContent is the slice of a VolumeSnapshotContent the scanner needs.
type SnapshotContent struct {
	Name           string
	Driver         string
	SnapshotHandle string // status.snapshotHandle (or the pre-provisioned source handle)
}

// Node is the slice of a Kubernetes Node the scanner needs.
type Node struct {
	Name   string
	Labels map[string]string
}

// CSINode records whether the Bard driver is registered on a node and with
// which topology keys.
type CSINode struct {
	Name         string
	HasDriver    bool
	TopologyKeys []string
}

// VolumeAttachment is the slice of a VolumeAttachment the scanner needs.
type VolumeAttachment struct {
	Name     string
	Attacher string
	NodeName string
	PV       string // spec.source.persistentVolumeName
	Attached bool
}

// HealthProbe is the outcome of double-checking one ghost candidate with the
// backend's GetVolumeHealth. At most one of the bools is set.
type HealthProbe struct {
	Confirmed       bool   // the backend confirms the volume is gone
	Healthy         bool   // the volume exists — the plugin's list was inconsistent
	InstanceUnknown bool   // the plugin has no config for the handle's instance
	Err             string // probe detail (set with InstanceUnknown, or alone when inconclusive)
}

// Check runs every consistency check over the collected state and returns the
// report, findings ordered most severe first.
func Check(st *State) Report {
	// Instances a probe proved the plugin cannot see (key backend|instance ->
	// probe error). Their ghost findings are suppressed in favour of one
	// plugin-missing-instance finding each: the volumes are invisible to the
	// plugin, not gone.
	missing := missingInstances(st)

	var findings []Finding
	findings = append(findings, missingInstanceFindings(missing)...)
	findings = append(findings, checkVolumes(st, missing)...)
	findings = append(findings, checkSnapshots(st, missing)...)
	findings = append(findings, checkTopology(st)...)
	findings = append(findings, checkAttachments(st)...)
	findings = append(findings, st.Skips...)
	sort.SliceStable(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		if a.Severity.rank() != b.Severity.rank() {
			return a.Severity.rank() < b.Severity.rank()
		}
		if a.Check != b.Check {
			return a.Check < b.Check
		}
		return a.Resource < b.Resource
	})
	return Report{Findings: findings, Stats: st.stats()}
}

func (st *State) stats() Stats {
	s := Stats{Nodes: len(st.Nodes)}
	for _, pv := range st.PVs {
		if pv.Driver == st.Driver {
			s.PVs++
		}
	}
	for _, sc := range st.SnapshotContents {
		if sc.Driver == st.Driver && sc.SnapshotHandle != "" {
			s.SnapshotContents++
		}
	}
	for _, va := range st.Attachments {
		if va.Attacher == st.Driver {
			s.Attachments++
		}
	}
	for _, entries := range st.BackendVolumes {
		s.BackendVolumes += len(entries)
	}
	for _, entries := range st.BackendSnapshots {
		s.BackendSnapshots += len(entries)
	}
	return s
}
