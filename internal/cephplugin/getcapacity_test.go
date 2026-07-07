package cephplugin

import (
	"context"
	"strings"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// cephDfRunner returns canned `ceph df` JSON and records the ceph args.
type cephDfRunner struct{ cephArgs []string }

func (r *cephDfRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	if name == "ceph" {
		r.cephArgs = args
		return `{"pools":[{"name":"other","stats":{"max_avail":1}},{"name":"replicapool","stats":{"max_avail":5368709120}}]}`, nil
	}
	return "", nil
}

func TestGetCapacity(t *testing.T) {
	run := &cephDfRunner{}
	b := New(map[string]ClusterConfig{
		"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin"},
	}, "", "", run)

	resp, err := b.GetCapacity(context.Background(), &bardplugin.GetCapacityRequest{Instance: "east"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.AvailableBytes != 5368709120 {
		t.Fatalf("available = %d, want pool replicapool's max_avail", resp.AvailableBytes)
	}
	if got := strings.Join(run.cephArgs, " "); !strings.Contains(got, "df") || !strings.Contains(got, "--conf") {
		t.Fatalf("ceph df not invoked with a conf: %q", got)
	}
}

func TestGetCapacityPoolParamOverridesConfig(t *testing.T) {
	run := &cephDfRunner{}
	b := New(map[string]ClusterConfig{"east": {Monitors: []string{"m"}, Pool: "replicapool", UserID: "admin"}}, "", "", run)
	resp, err := b.GetCapacity(context.Background(), &bardplugin.GetCapacityRequest{
		Instance: "east", Parameters: map[string]string{paramPool: "other"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.AvailableBytes != 1 {
		t.Fatalf("pool param should select 'other' (max_avail 1), got %d", resp.AvailableBytes)
	}
}

func TestGetCapacityUnknownInstance(t *testing.T) {
	b := New(map[string]ClusterConfig{"east": {Pool: "p"}}, "", "", &cephDfRunner{})
	if _, err := b.GetCapacity(context.Background(), &bardplugin.GetCapacityRequest{Instance: "west"}); err == nil {
		t.Fatal("expected error for unknown instance")
	}
}
