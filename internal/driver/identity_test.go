package driver

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

func TestIdentityDoesNotAdvertiseGroupController(t *testing.T) {
	resp, err := (&identityServer{}).GetPluginCapabilities(context.Background(), &csi.GetPluginCapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, capability := range resp.GetCapabilities() {
		if capability.GetService().GetType() == csi.PluginCapability_Service_GROUP_CONTROLLER_SERVICE {
			t.Fatal("identity advertised GROUP_CONTROLLER_SERVICE")
		}
	}
}
