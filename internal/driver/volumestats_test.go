package driver

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestNodeGetVolumeStatsFilesystem(t *testing.T) {
	s := &nodeServer{driver: New(Options{})}
	resp, err := s.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "swsk|1|x|i|loc|n",
		VolumePath: t.TempDir(), // a real directory -> statfs succeeds
	})
	if err != nil {
		t.Fatal(err)
	}
	var gotBytes, gotInodes bool
	for _, u := range resp.GetUsage() {
		switch u.GetUnit() {
		case csi.VolumeUsage_BYTES:
			gotBytes = true
			if u.GetTotal() <= 0 {
				t.Errorf("bytes total should be positive, got %d", u.GetTotal())
			}
		case csi.VolumeUsage_INODES:
			gotInodes = true
		}
	}
	if !gotBytes || !gotInodes {
		t.Fatalf("want BYTES and INODES usage, got %+v", resp.GetUsage())
	}
	// VOLUME_CONDITION is advertised, so a healthy mount reports a non-abnormal
	// condition alongside usage.
	if cond := resp.GetVolumeCondition(); cond == nil || cond.GetAbnormal() {
		t.Fatalf("want a healthy volume condition, got %+v", cond)
	}
}

func TestNodeGetVolumeStatsErrors(t *testing.T) {
	s := &nodeServer{driver: New(Options{})}
	if _, err := s.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{VolumePath: "/x"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("missing volume id: want InvalidArgument, got %v", err)
	}
	if _, err := s.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{VolumeId: "v"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("missing volume path: want InvalidArgument, got %v", err)
	}
	missing := filepath.Join(t.TempDir(), "nope")
	if _, err := s.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{VolumeId: "v", VolumePath: missing}); status.Code(err) != codes.NotFound {
		t.Errorf("missing path: want NotFound, got %v", err)
	}
}
