package cephfsplugin

import (
	"context"
	"strings"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

var (
	_ bardplugin.VolumeLister   = (*Backend)(nil)
	_ bardplugin.SnapshotLister = (*Backend)(nil)
)

// listRunner models the subvolume/snapshot ls + info commands. The ls results
// mix Bard-minted names (shortName shape: prefix + 16-hex hash, default and
// custom prefixes) with foreign names that must be filtered out.
type listRunner struct{}

func (listRunner) Run(_ context.Context, _ string, args ...string) (string, error) {
	a := strings.Join(args, " ")
	switch {
	case strings.Contains(a, "subvolume snapshot ls"):
		return `[{"name":"snap-00112233445566aa"},{"name":"manual-snap"}]`, nil
	case strings.Contains(a, "subvolume ls"):
		return `[{"name":"bard-1234567890abcdef"},{"name":"team-a-fedcba0987654321"},{"name":"stray"}]`, nil
	case strings.Contains(a, "subvolume info"):
		return `{"bytes_quota":1073741824}`, nil
	}
	return "", nil
}

func TestCephFSListVolumes(t *testing.T) {
	b := chBackend(listRunner{})
	resp, err := b.ListVolumes(context.Background(), &bardplugin.ListVolumesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// Both the default-prefix and the custom-volumeNamePrefix subvolume are
	// Bard-managed (shortName shape); "stray" is foreign and skipped.
	if len(resp.Entries) != 2 {
		t.Fatalf("expected the two shortName-shaped subvolumes, got %+v", resp.Entries)
	}
	names := map[string]bool{}
	for _, e := range resp.Entries {
		names[e.Volume.Name] = true
		if e.CapacityBytes != 1073741824 {
			t.Fatalf("expected the subvolume quota as capacity, got %d", e.CapacityBytes)
		}
	}
	if !names["bard-1234567890abcdef"] || !names["team-a-fedcba0987654321"] {
		t.Fatalf("wrong subvolumes listed: %v", names)
	}
}

func TestCephFSListSnapshots(t *testing.T) {
	b := chBackend(listRunner{})
	resp, err := b.ListSnapshots(context.Background(), &bardplugin.ListSnapshotsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// Each listed subvolume reports one shortName-shaped snapshot ("manual-snap"
	// is foreign and skipped), encoded subvolume@snapshot with the subvolume as
	// its source.
	if len(resp.Entries) != 2 {
		t.Fatalf("expected one snapshot per Bard subvolume, got %+v", resp.Entries)
	}
	for _, e := range resp.Entries {
		if !strings.HasSuffix(e.Snapshot.Name, "@snap-00112233445566aa") {
			t.Fatalf("bad snapshot encoding: %+v", e)
		}
		if e.SourceVolume.Name+"@snap-00112233445566aa" != e.Snapshot.Name {
			t.Fatalf("snapshot source mismatch: %+v", e)
		}
	}
}
