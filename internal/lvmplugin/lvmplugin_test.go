package lvmplugin

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// fakeRunner returns canned results keyed by the command's first arg, recording
// every invocation. A result function receives the full args.
type fakeRunner struct {
	calls   [][]string
	results map[string]func(args []string) (string, error)
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if fn := f.results[name]; fn != nil {
		return fn(args)
	}
	return "", nil
}

func (f *fakeRunner) ran(name string) bool {
	for _, c := range f.calls {
		if c[0] == name {
			return true
		}
	}
	return false
}

func TestLVName(t *testing.T) {
	if lvName("pvc-1") != lvName("pvc-1") {
		t.Fatal("not deterministic")
	}
	if lvName("a") == lvName("b") {
		t.Fatal("collision")
	}
	if n := lvName("some-very-long-pvc-name-from-kubernetes"); len(n) != len("bard-")+16 {
		t.Fatalf("unexpected length %d: %q", len(n), n)
	}
}

func TestVGResolve(t *testing.T) {
	b := New(map[string]InstanceConfig{"east": {VG: "bard-vg"}}, &fakeRunner{})
	if _, err := b.vg("west"); err == nil {
		t.Fatal("expected error for unknown instance")
	}
	if vg, err := b.vg("east"); err != nil || vg != "bard-vg" {
		t.Fatalf("east: %q %v", vg, err)
	}
}

func TestCreateVolumeProvisions(t *testing.T) {
	notFound := errors.New("Failed to find logical volume")
	created := false
	fr := &fakeRunner{results: map[string]func([]string) (string, error){
		"lvs": func([]string) (string, error) {
			if !created {
				return "", notFound
			}
			return "  1073741824\n", nil
		},
		"lvcreate": func([]string) (string, error) { created = true; return "", nil },
	}}
	b := New(map[string]InstanceConfig{"east": {VG: "bard-vg"}}, fr)

	resp, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "pvc-1", Instance: "east", CapacityBytes: 1 << 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !fr.ran("lvcreate") {
		t.Fatal("expected lvcreate")
	}
	if resp.Location != "bard-vg" || resp.Name != lvName("pvc-1") {
		t.Fatalf("bad identity: %+v", resp)
	}
	if resp.CapacityBytes != 1<<30 {
		t.Fatalf("bad capacity: %d", resp.CapacityBytes)
	}
	if resp.Context[ctxDevPath] != "/dev/bard-vg/"+lvName("pvc-1") {
		t.Fatalf("bad devPath: %q", resp.Context[ctxDevPath])
	}
}

func TestCreateVolumeIdempotent(t *testing.T) {
	fr := &fakeRunner{results: map[string]func([]string) (string, error){
		"lvs": func([]string) (string, error) { return "  1073741824\n", nil }, // already exists
	}}
	b := New(map[string]InstanceConfig{"east": {VG: "bard-vg"}}, fr)

	if _, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "pvc-1", Instance: "east", CapacityBytes: 1 << 30,
	}); err != nil {
		t.Fatal(err)
	}
	if fr.ran("lvcreate") {
		t.Fatal("must not lvcreate when the LV already exists at the requested size")
	}
}

func TestCreateVolumeConflictSmaller(t *testing.T) {
	fr := &fakeRunner{results: map[string]func([]string) (string, error){
		"lvs": func([]string) (string, error) { return "  536870912\n", nil }, // 512MiB exists
	}}
	b := New(map[string]InstanceConfig{"east": {VG: "bard-vg"}}, fr)

	_, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "pvc-1", Instance: "east", CapacityBytes: 1 << 30, // want 1GiB
	})
	var se *bardplugin.StatusError
	if !errors.As(err, &se) || se.Code != bardplugin.CodeAlreadyExists {
		t.Fatalf("expected AlreadyExists, got %v", err)
	}
}

func TestExpandVolumeIdempotent(t *testing.T) {
	fr := &fakeRunner{results: map[string]func([]string) (string, error){
		"lvextend": func([]string) (string, error) {
			return "", errors.New("New size (256 extents) matches existing size")
		},
		"lvs": func([]string) (string, error) { return "  2147483648\n", nil },
	}}
	b := New(map[string]InstanceConfig{"east": {VG: "bard-vg"}}, fr)

	resp, err := b.ExpandVolume(context.Background(), &bardplugin.ExpandVolumeRequest{
		Volume:       bardplugin.VolumeRef{Instance: "east", Location: "bard-vg", Name: "bard-x"},
		NewSizeBytes: 2 << 30,
	})
	if err != nil {
		t.Fatalf("idempotent lvextend should be swallowed: %v", err)
	}
	if !resp.NodeExpansionRequired {
		t.Fatal("block grow requires node fs grow")
	}
}

func TestNodeUnstageOnlyUnmounts(t *testing.T) {
	fr := &fakeRunner{}
	b := New(map[string]InstanceConfig{"east": {VG: "bard-vg"}}, fr)
	if err := b.NodeUnstage(context.Background(), &bardplugin.NodeUnstageRequest{
		Volume: bardplugin.VolumeRef{Instance: "east", Location: "bard-vg", Name: "bard-x"}, StagingPath: "/stage",
	}); err != nil {
		t.Fatal(err)
	}
	// Must not deactivate/remove the LV on unstage -- data must survive a restart.
	for _, c := range fr.calls {
		if strings.HasPrefix(c[0], "lv") || c[0] == "dmsetup" {
			t.Fatalf("unexpected LVM mutation on unstage: %v", c)
		}
	}
	if !fr.ran("umount") {
		t.Fatal("expected umount")
	}
}
