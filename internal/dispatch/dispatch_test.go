package dispatch

import "testing"

func newTestDispatcher(t *testing.T) *Dispatcher {
	t.Helper()
	d, err := New(Config{
		Instances: map[string]map[string]string{
			"ceph-rbd": {
				"east": "zone-east",
				"west": "zone-west",
			},
		},
		Defaults: map[string]string{"ceph-rbd": "east"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

// The headline requirement: one StorageClass (same params) resolves to
// different backend instances depending on the scheduled node's zone.
func TestResolve_TopologySelectsInstance(t *testing.T) {
	d := newTestDispatcher(t)
	params := map[string]string{BackendParamKey: "ceph-rbd"}

	got, err := d.Resolve(params, []string{"zone-west"}, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Instance != "west" || got.Zone != "zone-west" {
		t.Fatalf("got instance=%q zone=%q, want west/zone-west", got.Instance, got.Zone)
	}

	got, err = d.Resolve(params, []string{"zone-east"}, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Instance != "east" {
		t.Fatalf("got instance=%q, want east", got.Instance)
	}
}

func TestResolve_NoTopologyUsesDefault(t *testing.T) {
	d := newTestDispatcher(t)
	got, err := d.Resolve(map[string]string{BackendParamKey: "ceph-rbd"}, nil, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Instance != "east" {
		t.Fatalf("got %q, want default east", got.Instance)
	}
}

func TestResolve_UnknownBackend(t *testing.T) {
	d := newTestDispatcher(t)
	if _, err := d.Resolve(map[string]string{BackendParamKey: "nfs"}, nil, nil); err == nil {
		t.Fatal("expected error for unconfigured backend")
	}
}

func TestNew_RejectsAmbiguousZones(t *testing.T) {
	_, err := New(Config{Instances: map[string]map[string]string{
		"ceph-rbd": {"a": "z", "b": "z"},
	}})
	if err == nil {
		t.Fatal("expected error for two instances claiming the same zone")
	}
}
