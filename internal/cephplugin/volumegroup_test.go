package cephplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// groupRunner models `rbd group` state (groups + members) so the VolumeGroup plugin
// methods can be tested without a real Ceph cluster.
type groupRunner struct {
	calls  [][]string
	groups map[string]map[string]bool // "pool/group" -> set of "pool/image"
}

func newGroupRunner() *groupRunner { return &groupRunner{groups: map[string]map[string]bool{}} }

// groupTokens returns the positional tokens from "group" onward, dropping --format and
// its value, so a handler can read the rbd group subcommand regardless of conn args.
func groupTokens(args []string) []string {
	i := -1
	for j, a := range args {
		if a == "group" {
			i = j
			break
		}
	}
	if i < 0 {
		return nil
	}
	var out []string
	for j := i; j < len(args); j++ {
		if args[j] == "--format" {
			j++
			continue
		}
		out = append(out, args[j])
	}
	return out
}

func (r *groupRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if name != "rbd" {
		return "", nil
	}
	t := groupTokens(args)
	if len(t) < 2 {
		return "", nil
	}
	switch {
	case t[1] == "create": // group create <pool/group>
		g := t[2]
		if _, ok := r.groups[g]; ok {
			return "", fmt.Errorf("rbd: error: group already exists")
		}
		r.groups[g] = map[string]bool{}
		return "", nil
	case t[1] == "remove": // group remove <pool/group>
		g := t[2]
		if _, ok := r.groups[g]; !ok {
			return "", fmt.Errorf("rbd: error opening group: (2) No such file or directory")
		}
		delete(r.groups, g)
		return "", nil
	case t[1] == "list": // group list <pool>
		pool := t[2]
		var names []string
		for g := range r.groups {
			p, n, _ := strings.Cut(g, "/")
			if p == pool {
				names = append(names, n)
			}
		}
		b, _ := json.Marshal(names)
		return string(b), nil
	case t[1] == "image" && t[2] == "add": // group image add <group> <image>
		g, img := t[3], t[4]
		if _, ok := r.groups[g]; !ok {
			return "", fmt.Errorf("rbd: no such group")
		}
		if r.groups[g][img] {
			return "", fmt.Errorf("rbd: image already exists in group")
		}
		r.groups[g][img] = true
		return "", nil
	case t[1] == "image" && t[2] == "rm": // group image rm <group> <image>
		g, img := t[3], t[4]
		if !r.groups[g][img] {
			return "", fmt.Errorf("rbd: image does not exist in group")
		}
		delete(r.groups[g], img)
		return "", nil
	case t[1] == "image" && t[2] == "list": // group image list <group>
		g := t[3]
		type ent struct {
			Pool      string `json:"pool"`
			Namespace string `json:"namespace"`
			Image     string `json:"image"`
		}
		var out []ent
		for img := range r.groups[g] {
			p, n, _ := strings.Cut(img, "/")
			out = append(out, ent{Pool: p, Image: n})
		}
		b, _ := json.Marshal(out)
		return string(b), nil
	}
	return "", nil
}

func groupBackend(run Runner) *Backend {
	return New(map[string]ClusterConfig{
		"galileo": {Monitors: []string{"10.0.0.1:3300"}, Pool: "k8s-csi-test", UserID: "admin"},
	}, "", "", run)
}

func memberSet(members []bardplugin.VolumeRef) map[string]bool {
	s := map[string]bool{}
	for _, m := range members {
		s[m.Location+"/"+m.Name] = true
	}
	return s
}

// The VolumeGroup lifecycle: create with members, get, modify (add+remove), delete (the
// group goes but member images survive), and a deterministic group name.
func TestVolumeGroupLifecycle(t *testing.T) {
	run := newGroupRunner()
	b := groupBackend(run)
	ctx := context.Background()
	mk := func(img string) bardplugin.VolumeRef {
		return bardplugin.VolumeRef{Instance: "galileo", Location: "k8s-csi-test", Name: img}
	}

	// Create with two members.
	create, err := b.CreateVolumeGroup(ctx, &bardplugin.CreateVolumeGroupRequest{
		Instance: "galileo", Pool: "k8s-csi-test", Name: "my-group",
		Volumes: []bardplugin.VolumeRef{mk("csi-vol-a"), mk("csi-vol-b")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(create.Group.Name, groupNamePrefix) {
		t.Fatalf("group name must use the csi-group- prefix, got %q", create.Group.Name)
	}
	if got := memberSet(create.Volumes); len(got) != 2 || !got["k8s-csi-test/csi-vol-a"] || !got["k8s-csi-test/csi-vol-b"] {
		t.Fatalf("expected members a+b, got %v", create.Volumes)
	}
	// Member refs must carry the group's instance (so they form valid handles).
	for _, m := range create.Volumes {
		if m.Instance != "galileo" {
			t.Fatalf("member must carry the group instance, got %q", m.Instance)
		}
	}
	group := create.Group

	// Idempotent re-create yields the same group name + members.
	create2, err := b.CreateVolumeGroup(ctx, &bardplugin.CreateVolumeGroupRequest{
		Instance: "galileo", Pool: "k8s-csi-test", Name: "my-group",
		Volumes: []bardplugin.VolumeRef{mk("csi-vol-a"), mk("csi-vol-b")},
	})
	if err != nil || create2.Group.Name != group.Name {
		t.Fatalf("re-create must be idempotent: %v (%q vs %q)", err, create2.Group.Name, group.Name)
	}

	// Get returns the current members.
	got, err := b.GetVolumeGroup(ctx, &bardplugin.GetVolumeGroupRequest{Group: group})
	if err != nil || len(got.Volumes) != 2 {
		t.Fatalf("get: %v, members %v", err, got.Volumes)
	}

	// Modify to {b, c}: drops a, keeps b, adds c.
	mod, err := b.ModifyVolumeGroup(ctx, &bardplugin.ModifyVolumeGroupRequest{
		Group:   group,
		Volumes: []bardplugin.VolumeRef{mk("csi-vol-b"), mk("csi-vol-c")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if s := memberSet(mod.Volumes); len(s) != 2 || s["k8s-csi-test/csi-vol-a"] || !s["k8s-csi-test/csi-vol-b"] || !s["k8s-csi-test/csi-vol-c"] {
		t.Fatalf("modify membership wrong: %v", mod.Volumes)
	}

	// Delete the group: the group object is gone but the runner never deleted images.
	if err := b.DeleteVolumeGroup(ctx, &bardplugin.DeleteVolumeGroupRequest{Group: group}); err != nil {
		t.Fatal(err)
	}
	if _, ok := run.groups[group.Location+"/"+group.Name]; ok {
		t.Fatal("group must be removed on delete")
	}
	for _, c := range run.calls {
		if len(c) >= 3 && c[0] == "rbd" && has(c, "rm") && !has(c, "group") {
			t.Fatalf("DeleteVolumeGroup must not delete member images; call: %v", c)
		}
	}
	// Idempotent delete of a now-missing group succeeds.
	if err := b.DeleteVolumeGroup(ctx, &bardplugin.DeleteVolumeGroupRequest{Group: group}); err != nil {
		t.Fatalf("delete of a missing group must be a no-op: %v", err)
	}
}

// ListVolumeGroups enumerates only Bard-managed groups (csi-group- prefix) with members.
func TestListVolumeGroups(t *testing.T) {
	run := newGroupRunner()
	b := groupBackend(run)
	ctx := context.Background()
	// Seed: one Bard group + one foreign group directly in the runner state.
	run.groups["k8s-csi-test/"+shortName(groupNamePrefix, "g1")] = map[string]bool{"k8s-csi-test/csi-vol-x": true}
	run.groups["k8s-csi-test/someones-other-group"] = map[string]bool{"k8s-csi-test/other": true}

	resp, err := b.ListVolumeGroups(ctx, &bardplugin.ListVolumeGroupsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Groups) != 1 {
		t.Fatalf("expected only the one Bard-managed group, got %v", resp.Groups)
	}
	if resp.Groups[0].Volumes[0].Name != "csi-vol-x" || resp.Groups[0].Group.Instance != "galileo" {
		t.Fatalf("listed group wrong: %v", resp.Groups[0])
	}
}
