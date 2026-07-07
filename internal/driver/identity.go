package driver

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type identityServer struct {
	csi.UnimplementedIdentityServer
	driver *Driver
}

func (s *identityServer) GetPluginInfo(_ context.Context, _ *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{
		Name:          s.driver.name,
		VendorVersion: s.driver.version,
	}, nil
}

func (s *identityServer) GetPluginCapabilities(_ context.Context, _ *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	return &csi.GetPluginCapabilitiesResponse{
		Capabilities: []*csi.PluginCapability{
			{
				Type: &csi.PluginCapability_Service_{
					Service: &csi.PluginCapability_Service{Type: csi.PluginCapability_Service_CONTROLLER_SERVICE},
				},
			},
			{
				// We make volumes topology-constrained: this is what lets a
				// single StorageClass resolve to per-zone backend instances.
				Type: &csi.PluginCapability_Service_{
					Service: &csi.PluginCapability_Service{Type: csi.PluginCapability_Service_VOLUME_ACCESSIBILITY_CONSTRAINTS},
				},
			},
			{
				// We serve the GroupController service (VolumeGroupSnapshot).
				Type: &csi.PluginCapability_Service_{
					Service: &csi.PluginCapability_Service{Type: csi.PluginCapability_Service_GROUP_CONTROLLER_SERVICE},
				},
			},
		},
	}, nil
}

func (s *identityServer) Probe(_ context.Context, _ *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{Ready: wrapperspb.Bool(true)}, nil
}
