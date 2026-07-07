package driver

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/csi-addons/spec/lib/go/fence"
	"github.com/csi-addons/spec/lib/go/identity"
	"github.com/csi-addons/spec/lib/go/reclaimspace"

	"github.com/kindacoolhamster/bard-csi/internal/backend"
	"github.com/kindacoolhamster/bard-csi/internal/volumeid"
)

func addonsWith(t *testing.T, fb *fakeBackend) (*reclaimSpaceControllerServer, string) {
	t.Helper()
	reg := backend.NewRegistry()
	reg.Register(fb)
	d := New(Options{Registry: reg, Dispatch: mustDisp(t)})
	h := volumeid.Handle{Backend: "ceph-rbd", Instance: "east", Location: "replicapool", Name: "csi-vol-abc"}
	if err := h.Validate(); err != nil {
		t.Fatal(err)
	}
	return &reclaimSpaceControllerServer{driver: d}, h.String()
}

// ControllerReclaimSpace dispatches to the owning backend and maps its pre/post
// usage into the csi-addons response.
func TestControllerReclaimSpaceDispatches(t *testing.T) {
	var called bool
	rs, id := addonsWith(t, &fakeBackend{
		reclaim: func() (*backend.SpaceUsage, error) {
			called = true
			return &backend.SpaceUsage{PreUsageBytes: 1000, PostUsageBytes: 400}, nil
		},
	})
	resp, err := rs.ControllerReclaimSpace(context.Background(), &reclaimspace.ControllerReclaimSpaceRequest{VolumeId: id})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("backend ReclaimSpace was not called")
	}
	if resp.GetPreUsage().GetUsageBytes() != 1000 || resp.GetPostUsage().GetUsageBytes() != 400 {
		t.Fatalf("usage not mapped: pre=%v post=%v", resp.GetPreUsage(), resp.GetPostUsage())
	}
}

// NodeReclaimSpace dispatches to the owning backend with the volume path.
func TestNodeReclaimSpaceDispatches(t *testing.T) {
	reg := backend.NewRegistry()
	var called bool
	reg.Register(&fakeBackend{nodeReclaim: func() (*backend.SpaceUsage, error) {
		called = true
		return &backend.SpaceUsage{PreUsageBytes: -1, PostUsageBytes: -1}, nil
	}})
	d := New(Options{Registry: reg, Dispatch: mustDisp(t), Mode: Mode{Node: true}})
	h := volumeid.Handle{Backend: "ceph-rbd", Instance: "east", Location: "replicapool", Name: "csi-vol-abc"}
	if err := h.Validate(); err != nil {
		t.Fatal(err)
	}
	ns := &reclaimSpaceNodeServer{driver: d}
	resp, err := ns.NodeReclaimSpace(context.Background(), &reclaimspace.NodeReclaimSpaceRequest{
		VolumeId: h.String(), VolumePath: "/var/lib/kubelet/pods/p/volumes/x/mount",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("backend NodeReclaimSpace was not called")
	}
	if resp.GetPreUsage() != nil || resp.GetPostUsage() != nil {
		t.Fatalf("unknown usage must be nil, got %v / %v", resp.GetPreUsage(), resp.GetPostUsage())
	}
}

// Unknown usage (a negative figure) is reported as a nil consumption, not 0.
func TestControllerReclaimSpaceUnknownUsage(t *testing.T) {
	rs, id := addonsWith(t, &fakeBackend{
		reclaim: func() (*backend.SpaceUsage, error) {
			return &backend.SpaceUsage{PreUsageBytes: -1, PostUsageBytes: -1}, nil
		},
	})
	resp, err := rs.ControllerReclaimSpace(context.Background(), &reclaimspace.ControllerReclaimSpaceRequest{VolumeId: id})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetPreUsage() != nil || resp.GetPostUsage() != nil {
		t.Fatalf("unknown usage must be nil, got pre=%v post=%v", resp.GetPreUsage(), resp.GetPostUsage())
	}
}

// A backend that doesn't support reclaim surfaces as Unimplemented; a bad id as
// InvalidArgument (empty) / NotFound (unparseable).
func TestControllerReclaimSpaceErrors(t *testing.T) {
	rs, id := addonsWith(t, &fakeBackend{}) // default reclaim -> ErrUnsupported
	_, err := rs.ControllerReclaimSpace(context.Background(), &reclaimspace.ControllerReclaimSpaceRequest{VolumeId: id})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("unsupported backend should be Unimplemented, got %v", err)
	}
	_, err = rs.ControllerReclaimSpace(context.Background(), &reclaimspace.ControllerReclaimSpaceRequest{VolumeId: ""})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty id should be InvalidArgument, got %v", err)
	}
	_, err = rs.ControllerReclaimSpace(context.Background(), &reclaimspace.ControllerReclaimSpaceRequest{VolumeId: "not-a-handle"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("bad id should be NotFound, got %v", err)
	}
}

// The csi-addons Identity advertises capabilities by mode: the controller plane
// CONTROLLER_SERVICE + ReclaimSpace OFFLINE, the node plane NODE_SERVICE + ONLINE.
func TestCSIAddonsIdentityCapabilities(t *testing.T) {
	caps := func(m Mode) (svcCtrl, svcNode, rsOff, rsOn bool) {
		d := New(Options{Registry: backend.NewRegistry(), Dispatch: mustDisp(t), Mode: m})
		is := &csiAddonsIdentityServer{driver: d}
		resp, err := is.GetCapabilities(context.Background(), &identity.GetCapabilitiesRequest{})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range resp.GetCapabilities() {
			switch {
			case c.GetService().GetType() == identity.Capability_Service_CONTROLLER_SERVICE:
				svcCtrl = true
			case c.GetService().GetType() == identity.Capability_Service_NODE_SERVICE:
				svcNode = true
			}
			switch c.GetReclaimSpace().GetType() {
			case identity.Capability_ReclaimSpace_OFFLINE:
				rsOff = true
			case identity.Capability_ReclaimSpace_ONLINE:
				rsOn = true
			}
		}
		return
	}
	if c, n, off, on := caps(Mode{Controller: true}); !c || !off || n || on {
		t.Fatalf("controller mode want CONTROLLER+OFFLINE only, got ctrl=%v off=%v node=%v on=%v", c, off, n, on)
	}
	if c, n, off, on := caps(Mode{Node: true}); !n || !on || c || off {
		t.Fatalf("node mode want NODE+ONLINE only, got ctrl=%v off=%v node=%v on=%v", c, off, n, on)
	}
}

func fenceWith(t *testing.T, fb *fakeBackend) *fenceControllerServer {
	t.Helper()
	reg := backend.NewRegistry()
	reg.Register(fb)
	return &fenceControllerServer{driver: New(Options{Registry: reg, Dispatch: mustDisp(t), Mode: Mode{Controller: true}})}
}

// FenceClusterNetwork resolves the target instance from the CR parameters
// (clusterID) and dispatches the CIDRs to the fencing backend.
func TestFenceClusterNetworkDispatches(t *testing.T) {
	fb := &fakeBackend{fence: func(string, []string) error { return nil }}
	fs := fenceWith(t, fb)
	_, err := fs.FenceClusterNetwork(context.Background(), &fence.FenceClusterNetworkRequest{
		Parameters: map[string]string{"clusterID": "east"},
		Cidrs:      []*fence.CIDR{{Cidr: "10.1.0.0/16"}, {Cidr: "10.2.0.0/16"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fb.fenced) != 1 || fb.fenced[0][0] != "fence" || fb.fenced[0][1] != "east" {
		t.Fatalf("expected one fence call on instance east, got %v", fb.fenced)
	}
	if len(fb.fenced[0]) != 4 { // fence, east, 2 cidrs
		t.Fatalf("expected 2 cidrs threaded, got %v", fb.fenced[0])
	}
	// Unfence + List round-trip.
	if _, err := fs.UnfenceClusterNetwork(context.Background(), &fence.UnfenceClusterNetworkRequest{
		Parameters: map[string]string{"instance": "east"}, Cidrs: []*fence.CIDR{{Cidr: "10.1.0.0/16"}},
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := fs.ListClusterFence(context.Background(), &fence.ListClusterFenceRequest{Parameters: map[string]string{"clusterID": "east"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetCidrs()) != 1 || resp.GetCidrs()[0].GetCidr() != "10.9.9.0/24" {
		t.Fatalf("list should surface the backend's fenced cidrs, got %v", resp.GetCidrs())
	}
	// GetFenceClients resolves the instance and maps the backend clients to the proto.
	cresp, err := fs.GetFenceClients(context.Background(), &fence.GetFenceClientsRequest{Parameters: map[string]string{"clusterID": "east"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(cresp.GetClients()) != 1 {
		t.Fatalf("expected one client, got %v", cresp.GetClients())
	}
	c0 := cresp.GetClients()[0]
	if c0.GetId() != "fsid-east" || len(c0.GetAddresses()) != 1 || c0.GetAddresses()[0].GetCidr() != "10.5.5.5/32" {
		t.Fatalf("GetFenceClients did not map the backend client correctly: %v", c0)
	}
}

// With no fencing backend registered, FenceClusterNetwork is Unimplemented and the
// Identity does not advertise NetworkFence; with one, both flip on.
func TestNetworkFenceCapabilityGating(t *testing.T) {
	hasFenceCap := func(fb *fakeBackend) bool {
		reg := backend.NewRegistry()
		if fb != nil {
			reg.Register(fb)
		}
		d := New(Options{Registry: reg, Dispatch: mustDisp(t), Mode: Mode{Controller: true}})
		resp, err := (&csiAddonsIdentityServer{driver: d}).GetCapabilities(context.Background(), &identity.GetCapabilitiesRequest{})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range resp.GetCapabilities() {
			if c.GetNetworkFence().GetType() == identity.Capability_NetworkFence_NETWORK_FENCE {
				return true
			}
		}
		return false
	}
	hasGetClientsCap := func(fb *fakeBackend) bool {
		reg := backend.NewRegistry()
		if fb != nil {
			reg.Register(fb)
		}
		d := New(Options{Registry: reg, Dispatch: mustDisp(t), Mode: Mode{Controller: true}})
		resp, _ := (&csiAddonsIdentityServer{driver: d}).GetCapabilities(context.Background(), &identity.GetCapabilitiesRequest{})
		for _, c := range resp.GetCapabilities() {
			if c.GetNetworkFence().GetType() == identity.Capability_NetworkFence_GET_CLIENTS_TO_FENCE {
				return true
			}
		}
		return false
	}
	if hasFenceCap(&fakeBackend{}) { // no fence func -> no NetworkFence cap
		t.Fatal("a non-fencing backend must not advertise NetworkFence")
	}
	if !hasFenceCap(&fakeBackend{fence: func(string, []string) error { return nil }}) {
		t.Fatal("a fencing backend must advertise NetworkFence")
	}
	// GET_CLIENTS_TO_FENCE rides with NetworkFence: advertised iff a fencer is registered.
	if hasGetClientsCap(&fakeBackend{}) {
		t.Fatal("a non-fencing backend must not advertise GET_CLIENTS_TO_FENCE")
	}
	if !hasGetClientsCap(&fakeBackend{fence: func(string, []string) error { return nil }}) {
		t.Fatal("a fencing backend must advertise GET_CLIENTS_TO_FENCE")
	}

	// No fencing backend -> the RPC is Unimplemented.
	fs := fenceWith(t, &fakeBackend{}) // fence cap false
	_, err := fs.FenceClusterNetwork(context.Background(), &fence.FenceClusterNetworkRequest{
		Parameters: map[string]string{"clusterID": "east"}, Cidrs: []*fence.CIDR{{Cidr: "10.0.0.0/8"}},
	})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("no fencer should be Unimplemented, got %v", err)
	}
}
