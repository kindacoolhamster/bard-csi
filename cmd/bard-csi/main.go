// Command bard-csi is the unified CSI driver binary. The same binary runs as
// the controller (in a Deployment, alongside the CSI sidecars) and as the node
// plugin (in a DaemonSet), selected by --controller / --node.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"k8s.io/klog/v2"

	"github.com/kindacoolhamster/bard-csi/internal/backend"
	"github.com/kindacoolhamster/bard-csi/internal/backend/plugin"
	"github.com/kindacoolhamster/bard-csi/internal/config"
	"github.com/kindacoolhamster/bard-csi/internal/dispatch"
	"github.com/kindacoolhamster/bard-csi/internal/driver"
	"github.com/kindacoolhamster/bard-csi/internal/incluster"
	"github.com/kindacoolhamster/bard-csi/internal/inspect"
	"github.com/kindacoolhamster/bard-csi/internal/metrics"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	klog.InitFlags(nil)

	var (
		endpoint    = flag.String("endpoint", "unix:///csi/csi.sock", "CSI gRPC endpoint")
		csiAddons   = flag.String("csi-addons-endpoint", envOr("CSI_ADDONS_ENDPOINT", ""), "csi-addons gRPC endpoint (controller only; empty disables). Serves ReclaimSpace etc. for the csi-addons sidecar")
		nodeID      = flag.String("nodeid", envOr("NODE_ID", ""), "unique node identifier (node plugin)")
		zone        = flag.String("zone", envOr("NODE_ZONE", ""), "topology zone this node lives in (node plugin); fallback when --zone-label is unset or absent")
		zoneLabel   = flag.String("zone-label", envOr("ZONE_LABEL", "topology.kubernetes.io/zone"), "node label to read this node's zone from via the API (node plugin); empty disables")
		crushLabels = flag.String("crush-location-labels", envOr("CRUSH_LOCATION_LABELS", ""), "comma-separated node label keys to read this node's CRUSH location from (node plugin); each label's last path segment is the CRUSH bucket type. Enables RBD read-affinity when the backend opts in. Empty disables")
		configPath  = flag.String("config", "/etc/bard-csi/config.yaml", "path to backend config (when --config-source=file)")
		configSrc   = flag.String("config-source", envOr("CONFIG_SOURCE", "crd"), "where backend config comes from: crd (BackendCluster CRs) or file")
		controller  = flag.Bool("controller", false, "run the controller service")
		node        = flag.Bool("node", false, "run the node service")
		maxVolumes  = flag.Int64("max-volumes-per-node", envInt("MAX_VOLUMES_PER_NODE", 0), "cap on volumes the scheduler may place on this node (0 = unlimited); set where a mounter is device-limited, e.g. rbd-nbd's /dev/nbdN count")
		metricsAddr = flag.String("metrics-addr", envOr("METRICS_ADDR", ""), "address to serve the Prometheus /metrics endpoint on, e.g. :9809 (empty disables)")
		inspectRun  = flag.Bool("inspect", false, "run a one-shot consistency scan (Kubernetes state vs backend truth), print a report, and exit; run inside the controller pod — see docs/inspect.md")
		output      = flag.String("output", "table", "report format for --inspect: table or json")
		showVer     = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("bard-csi %s\n", version)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *inspectRun {
		os.Exit(runInspect(ctx, *configSrc, *configPath, *zoneLabel, *output))
	}
	if !*controller && !*node {
		klog.Fatal("at least one of --controller or --node must be set")
	}
	if *node && *nodeID == "" {
		klog.Fatal("--nodeid (or NODE_ID) is required when running the node service")
	}

	cfg, err := loadConfig(ctx, *configSrc, *configPath)
	if err != nil {
		klog.Fatalf("load config (%s): %v", *configSrc, err)
	}

	registry, disp, err := build(cfg)
	if err != nil {
		klog.Fatalf("build driver: %v", err)
	}

	// Resolve the node's topology zone from its Node label (the production
	// pattern), so one DaemonSet serves many zones. Falls back to the static
	// --zone if the label lookup fails or the label is absent.
	zoneVal := *zone
	if *node && *zoneLabel != "" && incluster.InCluster() {
		switch z, zerr := incluster.NodeZone(ctx, *nodeID, *zoneLabel); {
		case zerr != nil:
			klog.Warningf("read zone label %q from node %q: %v; falling back to --zone=%q", *zoneLabel, *nodeID, zerr, *zone)
		case z != "":
			zoneVal = z
			klog.Infof("node %q zone %q (from label %q)", *nodeID, z, *zoneLabel)
		default:
			klog.Warningf("node %q has no label %q; falling back to --zone=%q", *nodeID, *zoneLabel, *zone)
		}
	}

	// Derive this node's CRUSH location from its labels so a backend can place
	// reads near the data (RBD read-affinity). Node-level: read once at startup and
	// threaded into every NodeStage. Best-effort -- a lookup failure just disables
	// the locality hint, never the node plugin.
	var crushLocation string
	if *node && *crushLabels != "" && incluster.InCluster() {
		keys := splitAndTrim(*crushLabels)
		if loc, lerr := incluster.NodeCrushLocation(ctx, *nodeID, keys); lerr != nil {
			klog.Warningf("read crush-location labels %v from node %q: %v; read-affinity disabled", keys, *nodeID, lerr)
		} else if loc != "" {
			crushLocation = loc
			klog.Infof("node %q crush location %q (from labels %v)", *nodeID, loc, keys)
		}
	}

	d := driver.New(driver.Options{
		Version:       version,
		NodeID:        *nodeID,
		Zone:          zoneVal,
		CrushLocation: crushLocation,
		Mode:          driver.Mode{Controller: *controller, Node: *node},
		MaxVolumes:    *maxVolumes,
		Registry:      registry,
		Dispatch:      disp,
	})

	// With the CRD source, watch BackendClusters and hot-swap the driver's
	// registry+dispatcher on change -- no pod restart to re-point a zone, change
	// the default, or add/remove an instance of an already-running backend type.
	if *configSrc == "crd" {
		go func() {
			err := config.WatchFromAPI(ctx, func(c *config.Config) {
				reg, disp, err := build(c)
				if err != nil {
					klog.Warningf("reload: rebuild backends failed, keeping current config: %v", err)
					return
				}
				d.SetBackends(reg, disp)
			})
			if err != nil && ctx.Err() == nil {
				klog.Warningf("backendcluster watch stopped: %v", err)
			}
		}()
	}

	if *metricsAddr != "" {
		go func() {
			if err := metrics.Serve(ctx, *metricsAddr); err != nil && ctx.Err() == nil {
				klog.Warningf("metrics server stopped: %v", err)
			}
		}()
	}

	if err := d.Run(ctx, *endpoint, *csiAddons); err != nil {
		klog.Fatalf("run driver: %v", err)
	}
}

// loadConfig loads backend config from the selected source.
func loadConfig(ctx context.Context, configSrc, configPath string) (*config.Config, error) {
	switch configSrc {
	case "crd":
		return config.LoadFromAPI(ctx)
	case "file":
		return config.Load(configPath)
	default:
		return nil, fmt.Errorf("--config-source must be crd or file, got %q", configSrc)
	}
}

// runInspect is the --inspect mode: collect Kubernetes + backend state, run
// the consistency checks, print the report. Exit code 0 = no ERROR findings,
// 1 = ERROR findings present, 2 = the scan could not run.
func runInspect(ctx context.Context, configSrc, configPath, zoneLabel, output string) int {
	fail := func(format string, args ...any) int {
		fmt.Fprintf(os.Stderr, "bard-csi --inspect: "+format+"\n", args...)
		return 2
	}
	if output != "table" && output != "json" {
		return fail("--output must be table or json, got %q", output)
	}
	if zoneLabel == "" {
		zoneLabel = "topology.kubernetes.io/zone"
	}
	cfg, err := loadConfig(ctx, configSrc, configPath)
	if err != nil {
		return fail("load config (%s): %v", configSrc, err)
	}
	registry, _, err := build(cfg)
	if err != nil {
		return fail("connect backends: %v", err)
	}
	st, err := inspect.Collect(ctx, inspect.Options{
		Driver:    driver.DriverName,
		ZoneLabel: zoneLabel,
		Config:    cfg,
		Registry:  registry,
	})
	if err != nil {
		return fail("%v", err)
	}
	report := inspect.Check(st)
	if output == "json" {
		if err := report.WriteJSON(os.Stdout); err != nil {
			return fail("write report: %v", err)
		}
	} else {
		report.WriteTable(os.Stdout)
	}
	if report.HasErrors() {
		return 1
	}
	return 0
}

// build translates a Config into a backend registry and a dispatcher.
// This is where the binary learns which backend implementations exist; adding a
// new backend means registering it here.
func build(cfg *config.Config) (*backend.Registry, *dispatch.Dispatcher, error) {
	registry := backend.NewRegistry()
	dispCfg := dispatch.Config{
		Instances: make(map[string]map[string]string),
		Defaults:  cfg.Defaults,
	}

	for bt, bc := range cfg.Backends {
		zones := make(map[string]string, len(bc.Instances))
		for inst, ic := range bc.Instances {
			zones[inst] = ic.Zone
		}
		dispCfg.Instances[bt] = zones

		// Every backend is an out-of-tree plugin: Bard core is backend-agnostic
		// and proxies to the plugin over its unix socket.
		if bc.Plugin == nil {
			return nil, nil, fmt.Errorf("backend %q has no plugin endpoint", bt)
		}
		cl, err := plugin.Dial(context.Background(), bt, bc.Plugin.Endpoint)
		if err != nil {
			return nil, nil, fmt.Errorf("backend %q plugin: %w", bt, err)
		}
		registry.Register(cl)
	}

	disp, err := dispatch.New(dispCfg)
	if err != nil {
		return nil, nil, err
	}
	return registry, disp, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// splitAndTrim splits a comma-separated list and drops empty/blank entries.
func splitAndTrim(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envInt(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}
