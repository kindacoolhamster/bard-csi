// Command bard-plugin-nfs is an out-of-tree Bard CSI backend for NFS, and a
// worked example of the bardplugin SDK. It needs no Bard code changes: build it
// into a container, run it as a sidecar in Bard's controller and node pods, and
// add a `plugin` backend entry to Bard's config. The backend logic lives in
// internal/nfsplugin (testable); this is just the binary's entry point.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"sigs.k8s.io/yaml"

	"github.com/kindacoolhamster/bard-csi/internal/nfsplugin"
	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// config maps instance ids (as known to Bard's dispatch) to NFS endpoints.
type config struct {
	Instances map[string]nfsplugin.InstanceConfig `json:"instances"`
}

func main() {
	socket := flag.String("socket", "/var/lib/bard/plugins/nfs.sock", "unix socket to serve on")
	cfgPath := flag.String("config", "/etc/bard-nfs/config.yaml", "path to NFS instance config")
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

	if err := bardplugin.Serve(ctx, *socket, nfsplugin.New(cfg.Instances, nil)); err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		os.Exit(1)
	}
}
