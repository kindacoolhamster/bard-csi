package driver

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kindacoolhamster/bard-csi/internal/backend"
	"github.com/kindacoolhamster/bard-csi/internal/volumeid"
)

func vh(name string) volumeid.Handle {
	return volumeid.Handle{Backend: "ceph-rbd", Instance: "east", Location: "replicapool", Name: name}
}

// ListVolumes aggregates a backend's entries and sorts them by volume id.
func TestListVolumesSorted(t *testing.T) {
	cs, _ := controllerWith(t, &fakeBackend{
		listVolumes: func() ([]backend.VolumeListEntry, error) {
			return []backend.VolumeListEntry{
				{Handle: vh("csi-vol-c"), CapacityBytes: 3},
				{Handle: vh("csi-vol-a"), CapacityBytes: 1},
				{Handle: vh("csi-vol-b"), CapacityBytes: 2},
			}, nil
		},
	})
	resp, err := cs.ListVolumes(context.Background(), &csi.ListVolumesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetEntries()) != 3 {
		t.Fatalf("want 3 entries, got %d", len(resp.GetEntries()))
	}
	prev := ""
	for _, e := range resp.GetEntries() {
		if id := e.GetVolume().GetVolumeId(); id <= prev {
			t.Fatalf("entries not sorted: %q after %q", id, prev)
		} else {
			prev = id
		}
	}
}

// A backend that doesn't list is skipped and yields an empty list, never an error.
func TestListVolumesUnsupportedIsEmpty(t *testing.T) {
	cs, _ := controllerWith(t, &fakeBackend{}) // listVolumes nil -> ErrUnsupported
	resp, err := cs.ListVolumes(context.Background(), &csi.ListVolumesRequest{})
	if err != nil {
		t.Fatalf("listing with no lister backend must be empty success, got %v", err)
	}
	if len(resp.GetEntries()) != 0 {
		t.Fatalf("want empty, got %d", len(resp.GetEntries()))
	}
}

func TestListVolumesPagination(t *testing.T) {
	mk := func() *fakeBackend {
		return &fakeBackend{listVolumes: func() ([]backend.VolumeListEntry, error) {
			return []backend.VolumeListEntry{
				{Handle: vh("csi-vol-a")}, {Handle: vh("csi-vol-b")},
				{Handle: vh("csi-vol-c")}, {Handle: vh("csi-vol-d")},
			}, nil
		}}
	}
	cs, _ := controllerWith(t, mk())
	first, err := cs.ListVolumes(context.Background(), &csi.ListVolumesRequest{MaxEntries: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.GetEntries()) != 3 || first.GetNextToken() == "" {
		t.Fatalf("want 3 entries + a next token, got %d/%q", len(first.GetEntries()), first.GetNextToken())
	}
	second, err := cs.ListVolumes(context.Background(), &csi.ListVolumesRequest{MaxEntries: 3, StartingToken: first.GetNextToken()})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.GetEntries()) != 1 || second.GetNextToken() != "" {
		t.Fatalf("want 1 final entry + no token, got %d/%q", len(second.GetEntries()), second.GetNextToken())
	}
}

func TestListVolumesInvalidToken(t *testing.T) {
	cs, _ := controllerWith(t, &fakeBackend{
		listVolumes: func() ([]backend.VolumeListEntry, error) { return nil, nil },
	})
	_, err := cs.ListVolumes(context.Background(), &csi.ListVolumesRequest{StartingToken: "not-a-number"})
	if status.Code(err) != codes.Aborted {
		t.Fatalf("invalid starting_token must be Aborted, got %v", err)
	}
}

// List* capabilities are advertised only when a backend supports them.
func TestListCapabilitiesGated(t *testing.T) {
	has := func(fb *fakeBackend, want csi.ControllerServiceCapability_RPC_Type) bool {
		cs, _ := controllerWith(t, fb)
		resp, _ := cs.ControllerGetCapabilities(context.Background(), &csi.ControllerGetCapabilitiesRequest{})
		for _, c := range resp.GetCapabilities() {
			if c.GetRpc().GetType() == want {
				return true
			}
		}
		return false
	}
	if has(&fakeBackend{}, csi.ControllerServiceCapability_RPC_LIST_VOLUMES) {
		t.Fatal("must NOT advertise LIST_VOLUMES without a listing backend")
	}
	lister := &fakeBackend{listVolumes: func() ([]backend.VolumeListEntry, error) { return nil, nil }}
	if !has(lister, csi.ControllerServiceCapability_RPC_LIST_VOLUMES) {
		t.Fatal("must advertise LIST_VOLUMES when a backend lists")
	}
}

func TestListSnapshotsFilter(t *testing.T) {
	entries := []backend.SnapshotListEntry{
		{Handle: vh("csi-vol-a@snap1"), SourceVolume: vh("csi-vol-a"), ReadyToUse: true},
		{Handle: vh("csi-vol-b@snap2"), SourceVolume: vh("csi-vol-b"), ReadyToUse: true},
	}
	cs, _ := controllerWith(t, &fakeBackend{
		listSnapshots: func() ([]backend.SnapshotListEntry, error) { return entries, nil },
	})
	srcA := vh("csi-vol-a").String()

	// filter by source volume id -> only snap1
	bySrc, err := cs.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{SourceVolumeId: srcA})
	if err != nil {
		t.Fatal(err)
	}
	if len(bySrc.GetEntries()) != 1 || bySrc.GetEntries()[0].GetSnapshot().GetSourceVolumeId() != srcA {
		t.Fatalf("source-volume filter wrong: %+v", bySrc.GetEntries())
	}

	// filter by a snapshot id that exists -> exactly that one
	snapID := vh("csi-vol-b@snap2").String()
	byID, err := cs.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{SnapshotId: snapID})
	if err != nil {
		t.Fatal(err)
	}
	if len(byID.GetEntries()) != 1 || byID.GetEntries()[0].GetSnapshot().GetSnapshotId() != snapID {
		t.Fatalf("snapshot-id filter wrong: %+v", byID.GetEntries())
	}

	// filter by a non-existent id -> empty (not error)
	none, err := cs.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{SnapshotId: "no-such-id"})
	if err != nil || len(none.GetEntries()) != 0 {
		t.Fatalf("unknown filter must be empty success, got %d/%v", len(none.GetEntries()), err)
	}
}
