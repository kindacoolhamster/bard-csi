package config

import (
	"strings"
	"testing"
)

func bcl(name, btype, instance, zone, endpoint string, def bool) BackendCluster {
	var c BackendCluster
	c.Metadata.Name = name
	c.Spec = BackendClusterSpec{
		BackendType: btype, Instance: instance, Zone: zone, Default: def,
		Plugin: PluginSpec{Endpoint: endpoint},
	}
	return c
}

func TestFromBackendClusters(t *testing.T) {
	cfg, err := FromBackendClusters([]BackendCluster{
		bcl("galileo-rbd", "ceph-rbd", "galileo", "zone-east", "/var/lib/bard/plugins/ceph-rbd.sock", true),
		bcl("kepler-rbd", "ceph-rbd", "kepler", "zone-west", "/var/lib/bard/plugins/ceph-rbd.sock", false),
		bcl("galileo-fs", "cephfs", "galileo", "zone-east", "/var/lib/bard/plugins/cephfs.sock", true),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Two backend types, the rbd one with two instances.
	if len(cfg.Backends) != 2 {
		t.Fatalf("want 2 backends, got %d", len(cfg.Backends))
	}
	rbd := cfg.Backends["ceph-rbd"]
	if rbd.Plugin == nil || rbd.Plugin.Endpoint != "/var/lib/bard/plugins/ceph-rbd.sock" {
		t.Fatalf("rbd endpoint: %+v", rbd.Plugin)
	}
	if rbd.Instances["galileo"].Zone != "zone-east" || rbd.Instances["kepler"].Zone != "zone-west" {
		t.Fatalf("rbd instances/zones: %+v", rbd.Instances)
	}
	if cfg.Defaults["ceph-rbd"] != "galileo" || cfg.Defaults["cephfs"] != "galileo" {
		t.Fatalf("defaults: %+v", cfg.Defaults)
	}
}

func TestInstanceDefaultsToName(t *testing.T) {
	cfg, err := FromBackendClusters([]BackendCluster{
		bcl("galileo", "ceph-rbd", "", "zone-east", "/sock", false),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Backends["ceph-rbd"].Instances["galileo"]; !ok {
		t.Fatalf("instance should default to metadata.name: %+v", cfg.Backends["ceph-rbd"].Instances)
	}
}

func TestConflictingEndpoints(t *testing.T) {
	_, err := FromBackendClusters([]BackendCluster{
		bcl("a", "ceph-rbd", "a", "z1", "/sock-a", false),
		bcl("b", "ceph-rbd", "b", "z2", "/sock-b", false),
	})
	if err == nil || !strings.Contains(err.Error(), "conflicting plugin endpoints") {
		t.Fatalf("want conflicting-endpoint error, got %v", err)
	}
}

func TestDuplicateInstance(t *testing.T) {
	_, err := FromBackendClusters([]BackendCluster{
		bcl("a", "ceph-rbd", "galileo", "z1", "/sock", false),
		bcl("b", "ceph-rbd", "galileo", "z2", "/sock", false),
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate instance") {
		t.Fatalf("want duplicate-instance error, got %v", err)
	}
}

func TestTwoDefaults(t *testing.T) {
	_, err := FromBackendClusters([]BackendCluster{
		bcl("a", "ceph-rbd", "galileo", "z1", "/sock", true),
		bcl("b", "ceph-rbd", "kepler", "z2", "/sock", true),
	})
	if err == nil || !strings.Contains(err.Error(), "more than one default") {
		t.Fatalf("want two-defaults error, got %v", err)
	}
}

func TestMissingEndpoint(t *testing.T) {
	_, err := FromBackendClusters([]BackendCluster{
		bcl("a", "ceph-rbd", "galileo", "z1", "", false),
	})
	if err == nil || !strings.Contains(err.Error(), "empty plugin.endpoint") {
		t.Fatalf("want missing-endpoint error, got %v", err)
	}
}
