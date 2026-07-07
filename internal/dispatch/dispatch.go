// Package dispatch resolves a provisioning request to a concrete backend
// instance using CSI topology.
//
// This is the heart of the "one StorageClass, many backend instances" design.
// Existing CSIs (e.g. Ceph RBD) bake the target cluster into the StorageClass,
// so a class can only ever talk to one cluster. Here the StorageClass names a
// *backend type* and a logical pool; the concrete instance (which Ceph cluster,
// in which zone) is chosen per-volume from the scheduled node's topology. With
// volumeBindingMode: WaitForFirstConsumer the external-provisioner passes the
// chosen node's topology into CreateVolume, and we map zone -> instance here.
package dispatch

import (
	"fmt"
	"sort"
)

// TopologyKeyZone is the topology segment key the node plugin advertises and
// the dispatcher keys instance selection on.
const TopologyKeyZone = "topology.csi.bard.io/zone"

// BackendParamKey is the StorageClass parameter naming the backend type.
const BackendParamKey = "backend"

// Config declares, per backend type, which instances exist and where they
// live. In production this is materialised from a BackendCluster CRD; here it
// is a plain value so it can be loaded from a config file or constructed in
// tests.
type Config struct {
	// Instances maps backend type -> instance id -> the zone that instance
	// serves. instance id is what gets encoded into the volume handle and is
	// what the backend uses to pick a connection.
	Instances map[string]map[string]string `json:"instances"`
	// Defaults maps backend type -> instance id to use when a request carries
	// no usable topology (e.g. Immediate binding). Optional.
	Defaults map[string]string `json:"defaults"`
}

// Dispatcher resolves requests to (backend type, instance).
type Dispatcher struct {
	cfg Config
	// zoneIndex[backendType][zone] = instance, derived from cfg.Instances.
	zoneIndex map[string]map[string]string
}

// New builds a Dispatcher from config, validating that no two instances of the
// same backend type claim the same zone (which would make selection ambiguous).
func New(cfg Config) (*Dispatcher, error) {
	idx := make(map[string]map[string]string, len(cfg.Instances))
	for bt, instances := range cfg.Instances {
		zi := make(map[string]string, len(instances))
		for inst, zone := range instances {
			if existing, dup := zi[zone]; dup {
				return nil, fmt.Errorf("backend %q: instances %q and %q both claim zone %q", bt, existing, inst, zone)
			}
			zi[zone] = inst
		}
		idx[bt] = zi
	}
	return &Dispatcher{cfg: cfg, zoneIndex: idx}, nil
}

// Resolution is the outcome of dispatching a request.
type Resolution struct {
	Backend  string // backend type
	Instance string // concrete instance id
	Zone     string // zone the instance serves (for AccessibleTopology)
}

// Resolve picks a backend instance for a CreateVolume request.
//
// params are the StorageClass parameters (must name a backend). preferred and
// requisite are the zone values pulled from the CSI accessibility requirements
// (preferred first). When no topology is supplied it falls back to the
// configured default instance for the backend type.
func (d *Dispatcher) Resolve(params map[string]string, preferred, requisite []string) (Resolution, error) {
	bt := params[BackendParamKey]
	if bt == "" {
		return Resolution{}, fmt.Errorf("StorageClass parameter %q is required", BackendParamKey)
	}
	zi, ok := d.zoneIndex[bt]
	if !ok || len(zi) == 0 {
		return Resolution{}, fmt.Errorf("no instances configured for backend %q", bt)
	}

	// Prefer the scheduler's preferred topology, then any requisite zone.
	for _, zone := range append(append([]string{}, preferred...), requisite...) {
		if inst, ok := zi[zone]; ok {
			return Resolution{Backend: bt, Instance: inst, Zone: zone}, nil
		}
	}

	// No usable topology: fall back to the configured default.
	if inst := d.cfg.Defaults[bt]; inst != "" {
		zone := d.cfg.Instances[bt][inst]
		return Resolution{Backend: bt, Instance: inst, Zone: zone}, nil
	}

	// As a last resort with a single instance, the choice is unambiguous.
	if len(zi) == 1 {
		for zone, inst := range zi {
			return Resolution{Backend: bt, Instance: inst, Zone: zone}, nil
		}
	}

	return Resolution{}, fmt.Errorf("backend %q has %d instances but the request carried no matching topology and no default is set", bt, len(zi))
}

// InstancesForBackend returns the configured instance ids for a backend type,
// sorted, for diagnostics and capacity reporting.
func (d *Dispatcher) InstancesForBackend(bt string) []string {
	out := make([]string, 0, len(d.cfg.Instances[bt]))
	for inst := range d.cfg.Instances[bt] {
		out = append(out, inst)
	}
	sort.Strings(out)
	return out
}
