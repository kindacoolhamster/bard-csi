// Command bard-plugin-ceph-rbd is the Ceph RBD backend as an out-of-tree Bard
// plugin. It is deployed as a sidecar in Bard's controller + node pods and ships
// its own rbd/rbd-nbd tooling -- Bard core has no Ceph dependency.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"sigs.k8s.io/yaml"

	"github.com/kindacoolhamster/bard-csi/internal/cephplugin"
	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// config maps instance ids (as known to Bard's dispatch) to Ceph clusters.
type config struct {
	Instances map[string]cephplugin.ClusterConfig `json:"instances"`
}

// kmsConfig is the optional KMS providers file: id -> provider config, selected
// per-volume by the encryptionKMSID StorageClass parameter.
type kmsConfig struct {
	Providers map[string]cephplugin.KMSConfig `json:"providers"`
}

func main() {
	socket := flag.String("socket", "/var/lib/bard/plugins/ceph-rbd.sock", "unix socket to serve on")
	cfgPath := flag.String("config", "/etc/bard-ceph/config.yaml", "path to cluster config")
	keyDir := flag.String("key-dir", "/etc/bard-ceph-keys", "directory of per-instance cephx key files")
	encKeyDir := flag.String("encryption-key-dir", "", "directory of per-instance LUKS master keys (enables encrypted volumes; empty disables)")
	kmsCfgPath := flag.String("kms-config", "", "optional path to a KMS providers config (selected per-volume by encryptionKMSID)")
	stateDir := flag.String("state-dir", "/var/lib/kubelet/plugins/csi.bard.io/ceph-rbd", "node-persistent dir for staging->device records")
	maxCloneDepth := flag.Int("max-clone-depth", 4, "flatten a cloned rbd image once its COW parent chain reaches this depth, so iterative snapshot/restore can't exceed rbd's hard limit (0 disables)")
	flag.Parse()

	raw, err := os.ReadFile(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read config: %v\n", err)
		os.Exit(1)
	}
	var cfg config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "parse config: %v\n", err)
		os.Exit(1)
	}

	var kms kmsConfig
	if *kmsCfgPath != "" {
		// The flag is optional and is commonly mounted from an optional ConfigMap, so
		// a missing file means "no KMS providers configured" -- not a fatal error.
		// (A present-but-unparseable file still fails loudly.) This keeps a deployment
		// that wires --kms-config but supplies no KMS config from crashlooping.
		raw, err := os.ReadFile(*kmsCfgPath)
		switch {
		case os.IsNotExist(err):
			// no providers
		case err != nil:
			fmt.Fprintf(os.Stderr, "read kms config: %v\n", err)
			os.Exit(1)
		default:
			if err := yaml.Unmarshal(raw, &kms); err != nil {
				fmt.Fprintf(os.Stderr, "parse kms config: %v\n", err)
				os.Exit(1)
			}
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	be := cephplugin.New(cfg.Instances, *keyDir, *stateDir, nil).
		WithEncryption(*encKeyDir).
		WithKMS(kms.Providers).
		WithCloneDepthLimit(*maxCloneDepth)
	// Reattach any rbd-nbd devices orphaned by a previous plugin process (a no-op
	// for krbd / a fresh node). Async so it never delays the socket coming up --
	// core probes /info at startup.
	go be.Heal(ctx)
	if err := bardplugin.Serve(ctx, *socket, be); err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		os.Exit(1)
	}
}
