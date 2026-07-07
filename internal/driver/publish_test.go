package driver

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

var rwoCap = &csi.VolumeCapability{
	AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
}

// A node-mapped backend (Ceph RBD, LVM) doesn't attach: ControllerPublish must
// succeed as an immediate no-op with an empty publish context, because the driver
// is one CSIDriver and the external-attacher calls publish for EVERY volume once
// attachRequired is on. (The non-no-op attach path is exercised by the iSCSI
// plugin's own tests.)
func TestControllerPublishNoOp(t *testing.T) {
	cs, id := controllerWith(t, &fakeBackend{}) // publish func nil -> returns (nil, nil)
	resp, err := cs.ControllerPublishVolume(context.Background(), &csi.ControllerPublishVolumeRequest{
		VolumeId: id, NodeId: "node-a", VolumeCapability: rwoCap,
	})
	if err != nil {
		t.Fatalf("no-op publish must succeed, got %v", err)
	}
	if len(resp.GetPublishContext()) != 0 {
		t.Fatalf("node-mapped backend must return an empty publish context, got %v", resp.GetPublishContext())
	}
}

// An attach backend's publish context is threaded back verbatim, and the node id
// reaches the backend.
func TestControllerPublishAttach(t *testing.T) {
	var gotNode string
	cs, id := controllerWith(t, &fakeBackend{
		publish: func(nodeID string) (map[string]string, error) {
			gotNode = nodeID
			return map[string]string{"portal": "192.168.1.225:3260", "lun": "0"}, nil
		},
	})
	resp, err := cs.ControllerPublishVolume(context.Background(), &csi.ControllerPublishVolumeRequest{
		VolumeId: id, NodeId: "node-b", VolumeCapability: rwoCap,
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotNode != "node-b" {
		t.Fatalf("node id not passed through, got %q", gotNode)
	}
	if resp.GetPublishContext()["portal"] != "192.168.1.225:3260" || resp.GetPublishContext()["lun"] != "0" {
		t.Fatalf("publish context not echoed, got %v", resp.GetPublishContext())
	}
}

func TestControllerPublishBadInput(t *testing.T) {
	cs, id := controllerWith(t, &fakeBackend{})
	cases := map[string]struct {
		req  *csi.ControllerPublishVolumeRequest
		code codes.Code
	}{
		"empty volume id": {&csi.ControllerPublishVolumeRequest{NodeId: "n", VolumeCapability: rwoCap}, codes.InvalidArgument},
		"empty node id":   {&csi.ControllerPublishVolumeRequest{VolumeId: id, VolumeCapability: rwoCap}, codes.InvalidArgument},
		"nil capability":  {&csi.ControllerPublishVolumeRequest{VolumeId: id, NodeId: "n"}, codes.InvalidArgument},
		"unknown id":      {&csi.ControllerPublishVolumeRequest{VolumeId: "not-a-handle", NodeId: "n", VolumeCapability: rwoCap}, codes.NotFound},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := cs.ControllerPublishVolume(context.Background(), tc.req); status.Code(err) != tc.code {
				t.Fatalf("want %v, got %v", tc.code, err)
			}
		})
	}
}

// Unpublish is idempotent: an unknown/unparseable id is a successful no-op, and a
// known id reaches the backend with the node id.
func TestControllerUnpublishIdempotent(t *testing.T) {
	fb := &fakeBackend{}
	cs, id := controllerWith(t, fb)

	if _, err := cs.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{VolumeId: "not-a-handle", NodeId: "n"}); err != nil {
		t.Fatalf("unpublish of an unknown id must be a no-op success, got %v", err)
	}
	if _, err := cs.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{VolumeId: id, NodeId: "node-c"}); err != nil {
		t.Fatal(err)
	}
	if len(fb.unpublished) != 1 || fb.unpublished[0] != "node-c" {
		t.Fatalf("expected one unpublish for node-c, got %v", fb.unpublished)
	}
}

// The capability is gated on a registered backend actually attaching: advertised
// when one does (iSCSI-shaped), absent when none do (RBD-only) so the
// external-attacher isn't pulled in needlessly.
func TestControllerPublishCapabilityIsGated(t *testing.T) {
	advertises := func(fb *fakeBackend) bool {
		cs, _ := controllerWith(t, fb)
		resp, err := cs.ControllerGetCapabilities(context.Background(), &csi.ControllerGetCapabilitiesRequest{})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range resp.GetCapabilities() {
			if c.GetRpc().GetType() == csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME {
				return true
			}
		}
		return false
	}
	if advertises(&fakeBackend{}) {
		t.Fatal("must NOT advertise publish when no backend attaches")
	}
	if !advertises(&fakeBackend{requiresPublish: true}) {
		t.Fatal("must advertise publish when a backend attaches")
	}
}
