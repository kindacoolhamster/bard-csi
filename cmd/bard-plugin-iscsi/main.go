// Command bard-plugin-iscsi is the iSCSI backend as an out-of-tree Bard plugin --
// the reference ATTACH-style backend (control-plane LUN masking via
// ControllerPublish). Deployed as a sidecar in Bard's controller + node pods;
// ships LVM2 + targetcli (control plane) and open-iscsi (node plane).
// See internal/iscsiplugin for the model.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"sigs.k8s.io/yaml"

	"github.com/kindacoolhamster/bard-csi/internal/iscsiplugin"
	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

type config struct {
	Instances map[string]iscsiplugin.InstanceConfig `json:"instances"`
}

func main() {
	socket := flag.String("socket", "/var/lib/bard/plugins/iscsi.sock", "unix socket to serve on")
	cfgPath := flag.String("config", "/etc/bard-iscsi/config.yaml", "path to instance config")
	// Node plane only: this node's CSI id (== its node name), the source of this
	// node's derived initiator IQN, and a dir to record per-staging session state.
	nodeID := flag.String("node-id", "", "CSI node id (node plane); source of the initiator IQN")
	stateDir := flag.String("state-dir", "/var/lib/bard/iscsi", "dir to record node session state")
	// Both planes: per-instance CHAP credential files (a mounted Secret), read
	// only for instances with chapAuth: true.
	chapDir := flag.String("chap-dir", "/etc/bard-iscsi-chap", "dir of per-instance CHAP credential files")
	// Node plane, in-cluster: chroot for iscsiadm so the HOST's matched
	// iscsiadm+DB+iscsid stack is used (a container iscsiadm mis-pairs with the
	// host iscsid -- see internal/iscsiplugin). Mount the host root and pass it
	// here (e.g. --iscsiadm-chroot=/host). Empty runs iscsiadm directly.
	iscsiadmChroot := flag.String("iscsiadm-chroot", "", "chroot dir (host root mount) for iscsiadm; empty = run directly")
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

	be := iscsiplugin.New(cfg.Instances, *nodeID, *stateDir, *chapDir, *iscsiadmChroot, nil)
	if err := bardplugin.Serve(ctx, *socket, be); err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		os.Exit(1)
	}
}
