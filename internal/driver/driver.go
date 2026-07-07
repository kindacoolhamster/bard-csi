// Package driver implements the CSI gRPC services (Identity, Controller, Node)
// on top of the backend abstraction. The services are deliberately thin: they
// translate CSI requests, use the dispatcher to pick a backend instance, and
// delegate the real work to a backend.Backend.
package driver

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync/atomic"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"k8s.io/klog/v2"

	"github.com/kindacoolhamster/bard-csi/internal/backend"
	"github.com/kindacoolhamster/bard-csi/internal/dispatch"
	"github.com/kindacoolhamster/bard-csi/internal/metrics"
)

const (
	// DriverName is the CSI driver name, reported by GetPluginInfo and used in
	// the CSIDriver object and StorageClass provisioner field.
	DriverName = "csi.bard.io"
)

// Mode selects which CSI services a process runs. The controller Deployment
// runs the controller service; the node DaemonSet runs the node service.
type Mode struct {
	Controller bool
	Node       bool
}

// backends is the hot-swappable pair the CSI services resolve through. It is
// stored behind an atomic pointer and never mutated in place, so a live config
// reload just swaps in a new value while in-flight calls keep the one they read.
type backends struct {
	registry *backend.Registry
	disp     *dispatch.Dispatcher
}

// Driver wires the registry, dispatcher and gRPC services together.
type Driver struct {
	name          string
	version       string
	nodeID        string
	zone          string
	crushLocation string // this node's CRUSH location ("region:r1|zone:z1"), or ""
	mode          Mode
	maxVolumes    int64 // per-node volume cap reported in NodeGetInfo (0 = unlimited)

	state atomic.Pointer[backends] // the current registry+dispatcher snapshot

	// ops refuses duplicate concurrent operations on the same object (Aborted),
	// so a CO retry racing a still-running original cannot interleave backend
	// commands. See inflight.go.
	ops *inflight

	srv *grpc.Server
}

// Options configures a Driver.
type Options struct {
	Version string
	NodeID  string
	Zone    string
	// CrushLocation is this node's topology as a Ceph CRUSH location
	// ("region:r1|zone:z1"), derived from node labels; "" disables read-affinity.
	CrushLocation string
	Mode          Mode
	Registry      *backend.Registry
	Dispatch      *dispatch.Dispatcher
	// MaxVolumes caps how many volumes the scheduler may place on this node
	// (reported via NodeGetInfo). 0 means unlimited. Use it where a node-side
	// mounter is device-limited -- e.g. rbd-nbd is bounded by the number of
	// /dev/nbdN devices (nbds_max).
	MaxVolumes int64
}

// New builds a Driver.
func New(o Options) *Driver {
	d := &Driver{
		name:          DriverName,
		version:       o.Version,
		nodeID:        o.NodeID,
		zone:          o.Zone,
		crushLocation: o.CrushLocation,
		mode:          o.Mode,
		maxVolumes:    o.MaxVolumes,
		ops:           newInflight(),
	}
	d.state.Store(&backends{registry: o.Registry, disp: o.Dispatch})
	return d
}

// SetBackends atomically swaps the registry+dispatcher used for subsequent
// calls (a live config reload). In-flight calls keep the snapshot they loaded.
func (d *Driver) SetBackends(registry *backend.Registry, disp *dispatch.Dispatcher) {
	d.state.Store(&backends{registry: registry, disp: disp})
}

// snapshot returns the current registry+dispatcher pair. A CSI method that needs
// both should call this once so it sees a consistent pair across a reload.
func (d *Driver) snapshot() *backends { return d.state.Load() }

// Run listens on the CSI endpoint (a unix:// socket) and serves until ctx is
// cancelled. If csiAddonsEndpoint is non-empty, the csi-addons API (ReclaimSpace,
// ...) is served on that second socket in parallel for the csi-addons sidecar to
// drive -- the controller plane serves the offline ops, the node plane the online.
func (d *Driver) Run(ctx context.Context, endpoint, csiAddonsEndpoint string) error {
	if csiAddonsEndpoint != "" && (d.mode.Controller || d.mode.Node) {
		go func() {
			if err := d.runCSIAddons(ctx, csiAddonsEndpoint); err != nil {
				klog.Errorf("csi-addons server exited: %v", err)
			}
		}()
	}
	network, addr, err := parseEndpoint(endpoint)
	if err != nil {
		return err
	}
	lis, err := listen(network, addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", endpoint, err)
	}

	d.srv = grpc.NewServer(grpc.ChainUnaryInterceptor(metrics.Interceptor, logInterceptor))

	// Identity is always served (Probe/GetPluginInfo are required everywhere).
	csi.RegisterIdentityServer(d.srv, &identityServer{driver: d})
	if d.mode.Controller {
		csi.RegisterControllerServer(d.srv, &controllerServer{driver: d})
		// VolumeGroupSnapshot lives in the separate GroupController service.
		csi.RegisterGroupControllerServer(d.srv, &groupControllerServer{driver: d})
	}
	if d.mode.Node {
		csi.RegisterNodeServer(d.srv, &nodeServer{driver: d})
	}

	go func() {
		<-ctx.Done()
		klog.Info("shutting down gRPC server")
		d.srv.GracefulStop()
	}()

	klog.Infof("%s %s serving on %s (controller=%v node=%v)", d.name, d.version, endpoint, d.mode.Controller, d.mode.Node)
	return d.srv.Serve(lis)
}

// runCSIAddons serves the csi-addons gRPC API on its own socket until ctx is
// cancelled. Called only in controller mode with a configured endpoint.
func (d *Driver) runCSIAddons(ctx context.Context, endpoint string) error {
	network, addr, err := parseEndpoint(endpoint)
	if err != nil {
		return err
	}
	lis, err := listen(network, addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", endpoint, err)
	}
	srv := grpc.NewServer(grpc.ChainUnaryInterceptor(metrics.Interceptor, logInterceptor))
	d.registerCSIAddons(srv)
	go func() {
		<-ctx.Done()
		srv.GracefulStop()
	}()
	klog.Infof("csi-addons serving on %s", endpoint)
	return srv.Serve(lis)
}

// listen opens a gRPC listener, clearing a stale unix socket from a prior run.
func listen(network, addr string) (net.Listener, error) {
	if network == "unix" {
		if _, statErr := os.Stat(addr); statErr == nil {
			if rmErr := os.Remove(addr); rmErr != nil {
				return nil, fmt.Errorf("remove stale socket %s: %w", addr, rmErr)
			}
		}
	}
	return net.Listen(network, addr)
}

func parseEndpoint(ep string) (network, addr string, err error) {
	if strings.HasPrefix(ep, "/") {
		return "unix", ep, nil
	}
	u, err := url.Parse(ep)
	if err != nil {
		return "", "", fmt.Errorf("invalid endpoint %q: %w", ep, err)
	}
	switch u.Scheme {
	case "unix":
		return "unix", u.Path, nil
	case "tcp":
		return "tcp", u.Host, nil
	default:
		return "", "", fmt.Errorf("unsupported endpoint scheme %q", u.Scheme)
	}
}

func logInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	klog.V(4).Infof("call: %s", info.FullMethod)
	resp, err := handler(ctx, req)
	if err != nil {
		klog.Errorf("call %s failed: %v", info.FullMethod, err)
	}
	return resp, err
}
