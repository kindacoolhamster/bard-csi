package driver

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"

	"github.com/csi-addons/spec/lib/go/encryptionkeyrotation"

	"github.com/kindacoolhamster/bard-csi/internal/backend"
	"github.com/kindacoolhamster/bard-csi/internal/volumeid"
)

// encryptionKeyRotationServer implements the csi-addons EncryptionKeyRotation service
// by dispatching to the backend that owns the volume. It is node-plane: rotation
// needs the staged dm-crypt device, so only ceph-rbd (LUKS) implements it. The
// csi-addons controller does not send a volume_path, so the backend locates the
// staged device from the volume id. Volume-scoped, like ReclaimSpace.
type encryptionKeyRotationServer struct {
	encryptionkeyrotation.UnimplementedEncryptionKeyRotationControllerServer
	driver *Driver
}

func (s *encryptionKeyRotationServer) EncryptionKeyRotate(ctx context.Context, req *encryptionkeyrotation.EncryptionKeyRotateRequest) (*encryptionkeyrotation.EncryptionKeyRotateResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	// volume_path is optional: the csi-addons EncryptionKeyRotation controller does
	// not populate it (unlike NodeReclaimSpace), so the backend resolves the staged
	// device from the volume id when it is empty.
	h, err := volumeid.Parse(req.GetVolumeId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "unknown volume id: %v", err)
	}
	be, err := s.driver.snapshot().registry.Get(h.Backend)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	kr, ok := be.(backend.EncryptionKeyRotator)
	if !ok || !be.Capabilities().EncryptionKeyRotation {
		return nil, status.Errorf(codes.Unimplemented, "backend %q does not support encryption key rotation", h.Backend)
	}
	if err := kr.RotateEncryptionKey(ctx, h, req.GetVolumePath(), req.GetParameters(), req.GetSecrets()); err != nil {
		return nil, toStatus(err, "rotate encryption key")
	}
	klog.V(2).Infof("rotated encryption key for %s", req.GetVolumeId())
	return &encryptionkeyrotation.EncryptionKeyRotateResponse{}, nil
}
