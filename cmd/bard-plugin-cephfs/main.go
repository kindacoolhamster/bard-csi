// Command bard-plugin-cephfs is the CephFS backend as an out-of-tree Bard plugin.
// Deployed as a sidecar in Bard's controller + node pods; ships its own ceph CLI
// and ceph-fuse tooling.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"sigs.k8s.io/yaml"

	"github.com/kindacoolhamster/bard-csi/internal/cephfsplugin"
	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

type config struct {
	Instances map[string]cephfsplugin.ClusterConfig `json:"instances"`
}

// kmsConfig is the optional KMS providers file: id -> provider config, selected
// per-volume by the encryptionKMSID StorageClass parameter (the fscrypt passphrase).
type kmsConfig struct {
	Providers map[string]cephfsplugin.KMSConfig `json:"providers"`
}

func main() {
	socket := flag.String("socket", "/var/lib/bard/plugins/cephfs.sock", "unix socket to serve on")
	cfgPath := flag.String("config", "/etc/bard-cephfs/config.yaml", "path to cluster config")
	keyDir := flag.String("key-dir", "/etc/bard-cephfs-keys", "directory of per-instance cephx key files")
	encKeyDir := flag.String("encryption-key-dir", "", "directory of per-instance master keys (enables fscrypt-encrypted volumes; empty disables)")
	kmsCfgPath := flag.String("kms-config", "", "optional path to a KMS providers config (selected per-volume by encryptionKMSID)")
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
		// Optional and commonly mounted from an optional ConfigMap, so a missing file
		// means "no KMS providers" -- not fatal. A present-but-unparseable file fails.
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

	be := cephfsplugin.New(cfg.Instances, *keyDir, nil).
		WithEncryption(*encKeyDir).
		WithKMS(kms.Providers)
	if err := bardplugin.Serve(ctx, *socket, be); err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		os.Exit(1)
	}
}
