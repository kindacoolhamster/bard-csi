package driver

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"

	"github.com/kindacoolhamster/bard-csi/internal/backend"
)

// The always-on plugin capabilities, plus the one that is conditional: with no
// registered backend able to snapshot volumes together, GROUP_CONTROLLER_SERVICE
// must stay off. (Its positive case is TestGroupSnapshotCapabilityGating.)
func TestIdentityPluginCapabilities(t *testing.T) {
	d := New(Options{Registry: backend.NewRegistry(), Dispatch: mustDisp(t), Mode: Mode{Controller: true}})
	resp, err := (&identityServer{driver: d}).GetPluginCapabilities(context.Background(), &csi.GetPluginCapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[csi.PluginCapability_Service_Type]bool{}
	for _, capability := range resp.GetCapabilities() {
		got[capability.GetService().GetType()] = true
	}
	for _, want := range []csi.PluginCapability_Service_Type{
		csi.PluginCapability_Service_CONTROLLER_SERVICE,
		csi.PluginCapability_Service_VOLUME_ACCESSIBILITY_CONSTRAINTS,
	} {
		if !got[want] {
			t.Errorf("expected %v to always be advertised, got %v", want, got)
		}
	}
	if got[csi.PluginCapability_Service_GROUP_CONTROLLER_SERVICE] {
		t.Error("GROUP_CONTROLLER_SERVICE must not be advertised without a backend that can honour it")
	}
}
