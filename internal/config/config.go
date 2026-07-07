// Package config loads the driver's backend configuration.
//
// This file is the on-disk form of what will eventually be a BackendCluster
// CRD: it declares, per backend type, the set of instances (clusters/zones)
// the driver can provision into. It is mounted into the driver pods from a
// ConfigMap. Secrets (cephx keys) deliberately live elsewhere and arrive via
// CSI secrets, never in this file.
package config

import (
	"fmt"
	"os"

	"sigs.k8s.io/yaml"
)

// Config is the root configuration document.
type Config struct {
	// Backends maps backend type (e.g. "ceph-rbd") to its instances.
	Backends map[string]BackendConfig `json:"backends"`
	// Defaults maps backend type -> instance id used when a request has no
	// usable topology (Immediate binding).
	Defaults map[string]string `json:"defaults,omitempty"`
}

// BackendConfig holds the instances for one backend type.
type BackendConfig struct {
	// Plugin, when set, makes this an out-of-tree backend served by a bardplugin
	// process; Bard proxies all operations to it. When nil the backend type must
	// be one Bard has built in (e.g. ceph-rbd).
	Plugin *PluginConfig `json:"plugin,omitempty"`
	// Instances declares this backend's instances and the zone each serves; this
	// drives topology dispatch. For plugin backends, instance-specific details
	// (endpoints, credentials) are the plugin's own concern -- only Zone is read.
	Instances map[string]InstanceConfig `json:"instances"`
}

// PluginConfig points Bard at an out-of-tree backend plugin.
type PluginConfig struct {
	// Endpoint is the plugin's unix socket path (shared with Bard's pods).
	Endpoint string `json:"endpoint"`
}

// InstanceConfig is one backend instance as core sees it. Core is
// backend-agnostic, so it only needs the topology zone for dispatch; all
// backend-specific details (endpoints, pools, credentials) live in the plugin's
// own config.
type InstanceConfig struct {
	// Zone is the topology zone this instance serves. A volume created here is
	// pinned to this zone, and a node in this zone resolves here.
	Zone string `json:"zone"`
}

// Load reads and validates a config file (YAML or JSON).
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) validate() error {
	if len(c.Backends) == 0 {
		return fmt.Errorf("no backends configured")
	}
	for bt, bc := range c.Backends {
		if len(bc.Instances) == 0 {
			return fmt.Errorf("backend %q has no instances", bt)
		}
		for inst, ic := range bc.Instances {
			if ic.Zone == "" {
				return fmt.Errorf("backend %q instance %q has no zone", bt, inst)
			}
		}
	}
	return nil
}
