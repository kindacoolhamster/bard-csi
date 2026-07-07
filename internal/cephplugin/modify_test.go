package cephplugin

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// modifyRunner records calls and reports whether an image exists (for rbd info).
type modifyRunner struct {
	calls  [][]string
	exists bool
}

func (r *modifyRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if name == "rbd" && has(args, "info") {
		if r.exists {
			return `{"size":1073741824}`, nil
		}
		return "", errors.New("rbd: error opening image: No such file or directory")
	}
	return "", nil
}

func (r *modifyRunner) ran(parts ...string) bool {
	want := strings.Join(parts, " ")
	for _, c := range r.calls {
		if strings.Contains(strings.Join(c, " "), want) {
			return true
		}
	}
	return false
}

func newModifyBackend(run Runner) *Backend {
	return New(map[string]ClusterConfig{"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin"}}, "", "", run)
}

// A supported QoS parameter is applied to the image via rbd config image set.
func TestModifyVolumeAppliesQoS(t *testing.T) {
	run := &modifyRunner{exists: true}
	b := newModifyBackend(run)
	_, err := b.ModifyVolume(context.Background(), &bardplugin.ModifyVolumeRequest{
		Volume:        bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img"},
		MutableParams: map[string]string{"qosIopsLimit": "100"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !run.ran("config", "image", "set", "replicapool/img", "rbd_qos_iops_limit", "100") {
		t.Fatalf("expected rbd config image set for the QoS limit; calls: %v", run.calls)
	}
}

// An unsupported mutable parameter is rejected with InvalidArgument and never
// touches the image.
func TestModifyVolumeRejectsUnknownParam(t *testing.T) {
	run := &modifyRunner{exists: true}
	b := newModifyBackend(run)
	_, err := b.ModifyVolume(context.Background(), &bardplugin.ModifyVolumeRequest{
		Volume:        bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img"},
		MutableParams: map[string]string{"bogusKey": "1"},
	})
	var se *bardplugin.StatusError
	if !errors.As(err, &se) || se.Code != bardplugin.CodeInvalidArg {
		t.Fatalf("expected InvalidArgument StatusError, got %v", err)
	}
	if run.ran("config", "image", "set") {
		t.Fatalf("must not touch the image when a param is invalid; calls: %v", run.calls)
	}
}

// Modifying a non-existent volume is NotFound.
func TestModifyVolumeMissingImage(t *testing.T) {
	run := &modifyRunner{exists: false}
	b := newModifyBackend(run)
	_, err := b.ModifyVolume(context.Background(), &bardplugin.ModifyVolumeRequest{
		Volume:        bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "gone"},
		MutableParams: map[string]string{"qosIopsLimit": "100"},
	})
	var se *bardplugin.StatusError
	if !errors.As(err, &se) || se.Code != bardplugin.CodeNotFound {
		t.Fatalf("expected NotFound StatusError, got %v", err)
	}
}

// The plugin must advertise VolumeModifier so Bard wires the /volume/modify route.
func TestVolumeModifierAdvertised(t *testing.T) {
	var _ bardplugin.VolumeModifier = New(nil, "", "", nil)
}
