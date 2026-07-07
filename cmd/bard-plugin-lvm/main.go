// Command bard-plugin-lvm is the LVM backend as an out-of-tree Bard plugin.
// Deployed as a sidecar in Bard's controller + node pods; ships the LVM2 tools.
// See internal/lvmplugin for the locality model (shared-VG, not node-local).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"sigs.k8s.io/yaml"

	"github.com/kindacoolhamster/bard-csi/internal/lvmplugin"
	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

type config struct {
	Instances map[string]lvmplugin.InstanceConfig `json:"instances"`
}

func main() {
	socket := flag.String("socket", "/var/lib/bard/plugins/lvm.sock", "unix socket to serve on")
	cfgPath := flag.String("config", "/etc/bard-lvm/config.yaml", "path to instance config")
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	be := lvmplugin.New(cfg.Instances, nil)
	if err := bardplugin.Serve(ctx, *socket, be); err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		os.Exit(1)
	}
}
