package cephplugin

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// reclaimRunner models rbd info/du/sparsify. du reports a smaller used_size after
// a sparsify so the pre/post report is exercised; exists toggles the NotFound path.
type reclaimRunner struct {
	calls      [][]string
	exists     bool
	sparsified bool
}

func (r *reclaimRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	switch {
	case name == "rbd" && has(args, "info"):
		if r.exists {
			return `{"size":1073741824}`, nil
		}
		return "", errors.New("rbd: error opening image: No such file or directory")
	case name == "rbd" && has(args, "du"):
		if r.sparsified {
			return `{"images":[{"used_size":400}],"total_used_size":400}`, nil
		}
		return `{"images":[{"used_size":1000}],"total_used_size":1000}`, nil
	case name == "rbd" && has(args, "sparsify"):
		r.sparsified = true
		return "", nil
	}
	return "", nil
}

func (r *reclaimRunner) ran(parts ...string) bool {
	want := strings.Join(parts, " ")
	for _, c := range r.calls {
		if strings.Contains(strings.Join(c, " "), want) {
			return true
		}
	}
	return false
}

func reclaimBackend(run Runner) *Backend {
	return New(map[string]ClusterConfig{"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin"}}, "", "", run)
}

// ReclaimSpace sparsifies the image and reports used bytes before/after.
func TestReclaimSpaceSparsifies(t *testing.T) {
	run := &reclaimRunner{exists: true}
	b := reclaimBackend(run)
	resp, err := b.ReclaimSpace(context.Background(), &bardplugin.ReclaimSpaceRequest{
		Volume: bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !run.ran("sparsify", "replicapool/img") {
		t.Fatalf("expected rbd sparsify of the image; calls: %v", run.calls)
	}
	if resp.PreUsageBytes != 1000 || resp.PostUsageBytes != 400 {
		t.Fatalf("expected pre=1000 post=400 usage, got pre=%d post=%d", resp.PreUsageBytes, resp.PostUsageBytes)
	}
}

// A reclaim on a missing image is NotFound and never runs sparsify.
func TestReclaimSpaceMissingImage(t *testing.T) {
	run := &reclaimRunner{exists: false}
	b := reclaimBackend(run)
	_, err := b.ReclaimSpace(context.Background(), &bardplugin.ReclaimSpaceRequest{
		Volume: bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img"},
	})
	var se *bardplugin.StatusError
	if !errors.As(err, &se) || se.Code != bardplugin.CodeNotFound {
		t.Fatalf("expected NotFound for a missing image, got %v", err)
	}
	if run.ran("sparsify") {
		t.Fatalf("must not sparsify a missing image; calls: %v", run.calls)
	}
}

// The backend must satisfy SpaceReclaimer so the server advertises the capability.
func TestReclaimSpaceInterfaceSatisfied(t *testing.T) {
	var _ bardplugin.SpaceReclaimer = reclaimBackend(&reclaimRunner{})
	var _ bardplugin.NodeSpaceReclaimer = reclaimBackend(&reclaimRunner{})
}

// NodeReclaimSpace fstrims the mounted filesystem at the volume path.
func TestNodeReclaimSpaceFstrims(t *testing.T) {
	run := &reclaimRunner{}
	b := reclaimBackend(run)
	_, err := b.NodeReclaimSpace(context.Background(), &bardplugin.NodeReclaimSpaceRequest{
		Volume:     bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img"},
		VolumePath: "/var/lib/kubelet/pods/p/volumes/x/mount",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !run.ran("fstrim", "/var/lib/kubelet/pods/p/volumes/x/mount") {
		t.Fatalf("expected fstrim of the volume path; calls: %v", run.calls)
	}
}

// A raw block volume has no filesystem, so node reclaim is a no-op (never fstrims).
func TestNodeReclaimSpaceBlockNoop(t *testing.T) {
	run := &reclaimRunner{}
	b := reclaimBackend(run)
	_, err := b.NodeReclaimSpace(context.Background(), &bardplugin.NodeReclaimSpaceRequest{
		Volume:     bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img"},
		VolumePath: "/dev/some-block", Block: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.ran("fstrim") {
		t.Fatalf("block volume must not be fstrimmed; calls: %v", run.calls)
	}
}
