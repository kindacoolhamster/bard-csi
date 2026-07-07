package cephplugin

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

func TestResolveMounterOptions(t *testing.T) {
	cases := []struct {
		raw, mounter, want string
	}{
		{"", "krbd", ""},
		{"krbd:notrim;nbd:try-netlink", "", "notrim"},                   // empty mounter == krbd default
		{"notrim", "krbd", "notrim"},                                    // unprefixed -> any mounter
		{"notrim", mounterNBD, "notrim"},                                // same, nbd
		{"krbd:notrim;nbd:try-netlink", "krbd", "notrim"},               // krbd-scoped only
		{"krbd:notrim;nbd:try-netlink", mounterNBD, "try-netlink"},      // nbd-scoped only
		{"nbd:try-netlink", "krbd", ""},                                 // none apply to krbd
		{"ms_mode=secure;krbd:notrim", "krbd", "ms_mode=secure,notrim"}, // mix of shared + scoped
		{" krbd:notrim ;  ", "krbd", "notrim"},                          // whitespace tolerance
	}
	for _, c := range cases {
		if got := resolveMounterOptions(c.raw, c.mounter); got != c.want {
			t.Errorf("resolveMounterOptions(%q, %q) = %q, want %q", c.raw, c.mounter, got, c.want)
		}
	}
}

// mapRunner models map/unmap so a freed device stops reporting a size, letting
// NodeUnstage's "still mapped?" check pass; it records every call for assertions.
type mapRunner struct {
	calls  [][]string
	mapped bool
}

func (r *mapRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	switch name {
	case "rbd", "rbd-nbd":
		switch {
		case has(args, "map"):
			r.mapped = true
			return "/dev/rbd0", nil
		case has(args, "unmap"):
			r.mapped = false
		}
		return "", nil
	case "blockdev":
		if r.mapped {
			return "1073741824", nil
		}
		return "0", nil
	default:
		return "", nil
	}
}

func (r *mapRunner) optionsFor(verb string) string {
	for _, c := range r.calls {
		if (c[0] == "rbd" || c[0] == "rbd-nbd") && has(c, verb) {
			for i, a := range c {
				if a == "--options" && i+1 < len(c) {
					return c[i+1]
				}
			}
		}
	}
	return ""
}

func TestReadAffinityOptions(t *testing.T) {
	on := ClusterConfig{ReadAffinity: true}
	cases := []struct {
		cc        ClusterConfig
		loc, want string
	}{
		{on, "region:r1|zone:z1", "read_from_replica=localize,crush_location=region:r1|zone:z1"},
		{on, "", ""},                       // no crush location -> off
		{ClusterConfig{}, "region:r1", ""}, // not enabled
		{ClusterConfig{ReadAffinity: true, Mounter: mounterNBD}, "region:r1", ""}, // krbd-only
	}
	for _, c := range cases {
		if got := readAffinityOptions(c.cc, c.loc); got != c.want {
			t.Errorf("readAffinityOptions(%+v, %q) = %q, want %q", c.cc, c.loc, got, c.want)
		}
	}
}

// With read-affinity enabled, NodeStage must prepend the locality options to the
// `rbd map --options`, ahead of any user mapOptions, using the node's CRUSH
// location threaded in by core.
func TestReadAffinityThreadedToMap(t *testing.T) {
	dir := t.TempDir()
	run := &mapRunner{}
	b := New(map[string]ClusterConfig{"east": {
		Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin", ReadAffinity: true,
	}}, "", filepath.Join(dir, "state"), run)

	err := b.NodeStage(context.Background(), &bardplugin.NodeStageRequest{
		Volume:        bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img"},
		StagingPath:   filepath.Join(dir, "staging"),
		Block:         true,
		CrushLocation: "region:r1|zone:z1",
		Context:       map[string]string{paramMapOptions: "notrim"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := run.optionsFor("map")
	want := "read_from_replica=localize,crush_location=region:r1|zone:z1,notrim"
	if got != want {
		t.Fatalf("map options: got %q, want %q\ncalls: %v", got, want, run.calls)
	}
}

// mapOptions from the volume context must reach `rbd map --options`, and the
// unmapOptions persisted at stage time must reach `rbd unmap --options` even
// though NodeUnstage carries no volume context.
func TestMapUnmapOptionsThreaded(t *testing.T) {
	dir := t.TempDir()
	run := &mapRunner{}
	b := newFenceBackend(dir, run) // cluster "east", krbd (no mounter set)
	staging := filepath.Join(dir, "staging")

	stage := &bardplugin.NodeStageRequest{
		Volume:      bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img"},
		StagingPath: staging,
		Block:       true, // skip format/mount
		Context: map[string]string{
			paramMapOptions:   "krbd:notrim;nbd:try-netlink",
			paramUnmapOptions: "krbd:force;nbd:ignore",
		},
	}
	if err := b.NodeStage(context.Background(), stage); err != nil {
		t.Fatal(err)
	}
	if got := run.optionsFor("map"); got != "notrim" {
		t.Fatalf("map options: got %q, want notrim\ncalls: %v", got, run.calls)
	}

	if err := b.NodeUnstage(context.Background(), &bardplugin.NodeUnstageRequest{
		Volume:      stage.Volume,
		StagingPath: staging,
	}); err != nil {
		t.Fatal(err)
	}
	if got := run.optionsFor("unmap"); got != "force" {
		t.Fatalf("unmap options: got %q, want force\ncalls: %v", got, run.calls)
	}
}

// A read-only access mode (core sets NodeStageRequest.Readonly) must map the rbd
// image `--read-only`, so a ReadOnlyMany volume is write-protected at the Ceph
// client; a writable access mode must not.
func TestReadOnlyAccessMapsReadOnly(t *testing.T) {
	mapReadOnly := func(run *mapRunner) bool {
		for _, c := range run.calls {
			if (c[0] == "rbd" || c[0] == "rbd-nbd") && has(c, "map") && has(c, "--read-only") {
				return true
			}
		}
		return false
	}
	for _, ro := range []bool{true, false} {
		name := "readwrite"
		if ro {
			name = "readonly"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			run := &mapRunner{}
			b := newFenceBackend(dir, run)
			if err := b.NodeStage(context.Background(), &bardplugin.NodeStageRequest{
				Volume:      bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img"},
				StagingPath: filepath.Join(dir, "staging"),
				Block:       true, // skip format/mount
				Readonly:    ro,
			}); err != nil {
				t.Fatal(err)
			}
			if got := mapReadOnly(run); got != ro {
				t.Fatalf("rbd map --read-only present=%v, want %v\ncalls: %v", got, ro, run.calls)
			}
		})
	}
}
