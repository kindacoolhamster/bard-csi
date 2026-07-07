package cephplugin

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// rmRunner fails `rbd rm` with a given message and records calls; everything else
// (image-meta get, etc.) succeeds empty.
type rmRunner struct {
	calls   [][]string
	rmError string
}

func (r *rmRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if name == "rbd" && has(args, "rm") && r.rmError != "" {
		return "", errors.New(r.rmError)
	}
	return "", nil
}

func (r *rmRunner) rmCount() int {
	n := 0
	for _, c := range r.calls {
		if c[0] == "rbd" && has(c, "rm") {
			n++
		}
	}
	return n
}

// A watcher-blocked `rbd rm` must surface as an error from DeleteVolume, so the
// CSI sidecar keeps the PV and retries -- never reporting success while the image
// (and its data) still exist. This is the control-plane analogue of NodeUnstage
// refusing to report success while a device is still mapped. The exact Ceph text
// ("image still has watchers") must NOT be mistaken for not-found and swallowed.
func TestDeleteVolumeBlockedByWatcherErrors(t *testing.T) {
	// The realistic error string as ExecRunner produces it: the benign
	// "can't open ceph.conf: ... No such file or directory" warning lines are
	// interleaved with the real exit-16 cause. errString must drop the conf lines
	// so isNotFound does NOT misread this as not-found (the bug that orphaned a
	// live image: a watcher-blocked rm reported success and the PV was deleted).
	run := &rmRunner{rmError: "rbd -c /dev/null -m 10.0.0.10:6789 --id admin rm replicapool/img: " +
		"exit status 16: 2026-06-15T19:21:54 -1 can't open ceph.conf: (2) No such file or directory\n" +
		"2026-06-15T19:21:54 -1 can't open ceph.conf: (2) No such file or directory\n" +
		"librbd::image::PreRemoveRequest: check_image_watchers: image has watchers - not removing\n" +
		"rbd: error: image still has watchers"}
	b := New(map[string]ClusterConfig{"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin"}}, "", "", run)

	err := b.DeleteVolume(context.Background(), &bardplugin.DeleteVolumeRequest{
		Volume: bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img"},
	})
	if err == nil {
		t.Fatal("DeleteVolume must return an error when rbd rm is blocked by a watcher (otherwise the PV is removed and the image orphaned)")
	}
	if !strings.Contains(err.Error(), "watchers") {
		t.Fatalf("error should carry the underlying cause, got %v", err)
	}
	if run.rmCount() != 1 {
		t.Fatalf("expected exactly one rbd rm attempt, got %d", run.rmCount())
	}
}

// isNotFound must ignore the benign ceph.conf warning's "No such file" and
// classify only the real cause: a watcher error is NOT not-found; a genuine
// "error opening image" not-found still is.
func TestIsNotFoundIgnoresCephConfWarning(t *testing.T) {
	confWarn := "-1 can't open ceph.conf: (2) No such file or directory\n"
	if isNotFound(errors.New(confWarn + "rbd: error: image still has watchers")) {
		t.Fatal("watcher error must not be classified as not-found despite the ceph.conf warning")
	}
	if !isNotFound(errors.New(confWarn + "rbd: error opening image img: (2) No such file or directory")) {
		t.Fatal("a genuine image-not-found must still classify as not-found")
	}
}

// A genuinely-absent image (already removed on a prior retry) is idempotent
// success -- not-found must be swallowed so a retried DeleteVolume converges.
func TestDeleteVolumeNotFoundIsIdempotent(t *testing.T) {
	run := &rmRunner{rmError: "rbd: error opening image img: (2) No such file or directory"}
	b := New(map[string]ClusterConfig{"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin"}}, "", "", run)

	if err := b.DeleteVolume(context.Background(), &bardplugin.DeleteVolumeRequest{
		Volume: bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img"},
	}); err != nil {
		t.Fatalf("DeleteVolume on an already-absent image must be idempotent success, got %v", err)
	}
}

// staticRunner reports an image as statically provisioned via image-meta and
// records calls, so a test can assert DeleteVolume never reaches `rbd rm`.
type staticRunner struct{ calls [][]string }

func (r *staticRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if name == "rbd" && has(args, "image-meta") && has(args, "get") && has(args, imgMetaStatic) {
		return "true", nil
	}
	return "", nil
}

func (r *staticRunner) rmCount() int {
	n := 0
	for _, c := range r.calls {
		if c[0] == "rbd" && has(c, "rm") {
			n++
		}
	}
	return n
}

// A statically provisioned image (bard.static=true) must never be removed by
// DeleteVolume -- even under reclaimPolicy:Delete -- since the admin owns its
// lifecycle. DeleteVolume returns success without any `rbd rm`.
func TestDeleteVolumeStaticIsNoOp(t *testing.T) {
	run := &staticRunner{}
	b := New(map[string]ClusterConfig{"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin"}}, "", "", run)

	if err := b.DeleteVolume(context.Background(), &bardplugin.DeleteVolumeRequest{
		Volume: bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img"},
	}); err != nil {
		t.Fatalf("static DeleteVolume must succeed as a no-op, got %v", err)
	}
	if run.rmCount() != 0 {
		t.Fatalf("static DeleteVolume must NOT rbd rm a pre-existing image, got %d rm calls\ncalls: %v", run.rmCount(), run.calls)
	}
}
