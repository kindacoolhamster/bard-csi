package driver

import (
	"context"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/kindacoolhamster/bard-csi/internal/backend"
	"github.com/kindacoolhamster/bard-csi/internal/dispatch"
	"github.com/kindacoolhamster/bard-csi/internal/volumeid"
)

// contentSourceBackend reuses the controller test double while allowing these
// tests to observe whether CreateVolume reached the backend at all.
type contentSourceBackend struct {
	*fakeBackend
	createVolume func(context.Context, *backend.CreateVolumeRequest) (*backend.Volume, error)
}

func (b *contentSourceBackend) CreateVolume(ctx context.Context, req *backend.CreateVolumeRequest) (*backend.Volume, error) {
	if b.createVolume == nil {
		return nil, backend.ErrUnsupported
	}
	return b.createVolume(ctx, req)
}

func contentSourceServer(t *testing.T, be backend.Backend) *controllerServer {
	t.Helper()
	reg := backend.NewRegistry()
	reg.Register(be)
	disp, err := dispatch.New(dispatch.Config{
		Instances: map[string]map[string]string{
			"ceph-rbd": {"east": "zone-a", "west": "zone-b"},
		},
		Defaults: map[string]string{"ceph-rbd": "east"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &controllerServer{driver: New(Options{Registry: reg, Dispatch: disp})}
}

func contentSourceHandle(backendType, instance, name string) string {
	h := volumeid.Handle{
		Backend:  backendType,
		Instance: instance,
		Location: "source-pool",
		Name:     name,
	}
	return h.String()
}

func contentSourceVolume(backendType, instance string) *csi.VolumeContentSource {
	return &csi.VolumeContentSource{
		Type: &csi.VolumeContentSource_Volume{
			Volume: &csi.VolumeContentSource_VolumeSource{
				VolumeId: contentSourceHandle(backendType, instance, "source-volume"),
			},
		},
	}
}

func contentSourceSnapshot(backendType, instance string) *csi.VolumeContentSource {
	return &csi.VolumeContentSource{
		Type: &csi.VolumeContentSource_Snapshot{
			Snapshot: &csi.VolumeContentSource_SnapshotSource{
				SnapshotId: contentSourceHandle(backendType, instance, "source-snapshot"),
			},
		},
	}
}

func contentSourceRequest(source *csi.VolumeContentSource) *csi.CreateVolumeRequest {
	return &csi.CreateVolumeRequest{
		Name:                "content-source-target",
		Parameters:          map[string]string{dispatch.BackendParamKey: "ceph-rbd"},
		VolumeCapabilities:  []*csi.VolumeCapability{mountCap(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)},
		VolumeContentSource: source,
	}
}

func TestCreateVolumeRejectsContentSourceOutsideTarget(t *testing.T) {
	tests := []struct {
		name       string
		source     *csi.VolumeContentSource
		wantDetail string
	}{
		{
			name:       "volume backend mismatch",
			source:     contentSourceVolume("nfs", "east"),
			wantDetail: `source volume backend "nfs"`,
		},
		{
			name:       "volume instance mismatch",
			source:     contentSourceVolume("ceph-rbd", "west"),
			wantDetail: `source volume instance "west"`,
		},
		{
			name:       "snapshot backend mismatch",
			source:     contentSourceSnapshot("nfs", "east"),
			wantDetail: `source snapshot backend "nfs"`,
		},
		{
			name:       "snapshot instance mismatch",
			source:     contentSourceSnapshot("ceph-rbd", "west"),
			wantDetail: `source snapshot instance "west"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			be := &contentSourceBackend{
				fakeBackend: &fakeBackend{},
				createVolume: func(context.Context, *backend.CreateVolumeRequest) (*backend.Volume, error) {
					calls++
					return nil, backend.ErrUnsupported
				},
			}
			cs := contentSourceServer(t, be)

			_, err := cs.CreateVolume(context.Background(), contentSourceRequest(tt.source))
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("CreateVolume status = %v, want InvalidArgument: %v", status.Code(err), err)
			}
			if !strings.Contains(err.Error(), tt.wantDetail) || !strings.Contains(err.Error(), `target`) {
				t.Fatalf("error %q does not identify the source/target mismatch", err)
			}
			if calls != 0 {
				t.Fatalf("backend CreateVolume called %d times for rejected source", calls)
			}
		})
	}
}

func TestCreateVolumeAcceptsContentSourceInTargetInstance(t *testing.T) {
	tests := []struct {
		name   string
		source *csi.VolumeContentSource
	}{
		{name: "volume", source: contentSourceVolume("ceph-rbd", "east")},
		{name: "snapshot", source: contentSourceSnapshot("ceph-rbd", "east")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			var got *backend.CreateVolumeRequest
			be := &contentSourceBackend{
				fakeBackend: &fakeBackend{},
				createVolume: func(_ context.Context, req *backend.CreateVolumeRequest) (*backend.Volume, error) {
					calls++
					got = req
					return &backend.Volume{Handle: volumeid.Handle{
						Backend:  "ceph-rbd",
						Instance: req.Instance,
						Location: "target-pool",
						Name:     "created-volume",
					}}, nil
				},
			}
			cs := contentSourceServer(t, be)

			resp, err := cs.CreateVolume(context.Background(), contentSourceRequest(tt.source))
			if err != nil {
				t.Fatalf("CreateVolume failed for same-instance source: %v", err)
			}
			if calls != 1 || got == nil {
				t.Fatalf("backend CreateVolume calls = %d, request = %+v; want one call", calls, got)
			}
			if got.Instance != "east" {
				t.Fatalf("target instance = %q, want east", got.Instance)
			}
			switch {
			case tt.source.GetVolume() != nil:
				if got.SourceVolume == nil || got.SourceVolume.String() != tt.source.GetVolume().GetVolumeId() {
					t.Fatalf("source volume forwarded as %v, want exact handle %q", got.SourceVolume, tt.source.GetVolume().GetVolumeId())
				}
				if got.SourceSnapshot != nil {
					t.Fatalf("unexpected source snapshot in volume clone request: %v", got.SourceSnapshot)
				}
			case tt.source.GetSnapshot() != nil:
				if got.SourceSnapshot == nil || got.SourceSnapshot.String() != tt.source.GetSnapshot().GetSnapshotId() {
					t.Fatalf("source snapshot forwarded as %v, want exact handle %q", got.SourceSnapshot, tt.source.GetSnapshot().GetSnapshotId())
				}
				if got.SourceVolume != nil {
					t.Fatalf("unexpected source volume in snapshot restore request: %v", got.SourceVolume)
				}
			default:
				t.Fatal("test source has neither a volume nor a snapshot")
			}
			if resp.GetVolume().GetVolumeId() == "" {
				t.Fatal("successful CreateVolume returned an empty volume id")
			}
		})
	}
}
