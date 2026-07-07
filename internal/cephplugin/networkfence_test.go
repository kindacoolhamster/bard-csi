package cephplugin

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// fenceCmdRunner records ceph invocations and returns a seeded `osd blocklist ls`.
type fenceCmdRunner struct {
	calls   [][]string
	listOut string
}

func (r *fenceCmdRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if name == "ceph" && has(args, "blocklist") && has(args, "ls") {
		return r.listOut, nil
	}
	if name == "ceph" && has(args, "fsid") {
		return "10ac4808-6530-11f1-8000-509a4c763a91\n", nil
	}
	return "", nil
}

func (r *fenceCmdRunner) cephCalls() [][]string {
	var out [][]string
	for _, c := range r.calls {
		if c[0] == "ceph" {
			out = append(out, c)
		}
	}
	return out
}

func newFenceNetBackend(run Runner) *Backend {
	return New(map[string]ClusterConfig{
		"galileo": {Monitors: []string{"192.168.1.225:3300"}, Pool: "k8s-csi-test", UserID: "k8s-csi-test"},
	}, "", "", run)
}

// FenceClusterNetwork must issue `osd blocklist range add <cidr>` for every CIDR,
// using the fence user supplied in the request secrets (not the instance user).
func TestFenceClusterNetworkAddsRanges(t *testing.T) {
	run := &fenceCmdRunner{}
	b := newFenceNetBackend(run)

	err := b.FenceClusterNetwork(context.Background(), &bardplugin.FenceClusterNetworkRequest{
		Instance: "galileo",
		CIDRs:    []string{"10.1.2.0/24", "10.1.3.5/32"},
		Secrets:  map[string]string{secretUserID: "k8s-fence", secretUserKey: "AQA-fencekey=="},
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := run.cephCalls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 ceph calls, got %d: %v", len(calls), calls)
	}
	for i, want := range []string{"10.1.2.0/24", "10.1.3.5/32"} {
		c := strings.Join(calls[i], " ")
		if !strings.Contains(c, "osd blocklist range add "+want) {
			t.Errorf("call %d missing `range add %s`: %s", i, want, c)
		}
		// Credentials must come from the request secrets (fence user), not the
		// instance's provisioning user -- the scoped user can't unfence.
		if !strings.Contains(c, "--id k8s-fence") {
			t.Errorf("call %d should use the fence user from secrets: %s", i, c)
		}
	}
}

// UnfenceClusterNetwork must issue `osd blocklist range rm <cidr>`.
func TestUnfenceClusterNetworkRemovesRanges(t *testing.T) {
	run := &fenceCmdRunner{}
	b := newFenceNetBackend(run)

	err := b.UnfenceClusterNetwork(context.Background(), &bardplugin.UnfenceClusterNetworkRequest{
		Instance: "galileo",
		CIDRs:    []string{"10.1.2.0/24"},
		Secrets:  map[string]string{secretUserID: "k8s-fence", secretUserKey: "k=="},
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := run.cephCalls()
	if len(calls) != 1 || !strings.Contains(strings.Join(calls[0], " "), "osd blocklist range rm 10.1.2.0/24") {
		t.Fatalf("expected `range rm 10.1.2.0/24`, got %v", calls)
	}
}

// ListClusterFence parses `osd blocklist ls`, returning only range (cidr:) entries
// normalised to plain CIDRs and skipping single-IP watcher blocklist entries.
func TestListClusterFenceParsesRanges(t *testing.T) {
	run := &fenceCmdRunner{listOut: `192.168.1.99:0/12345 2026-06-25T05:00:00.000000+0000
cidr:10.1.2.0:0/24 2026-06-25T05:00:00.000000+0000
cidr:10.1.3.0:0/24 2026-06-25T06:00:00.000000+0000
listed 3 entries`}
	b := newFenceNetBackend(run)

	resp, err := b.ListClusterFence(context.Background(), &bardplugin.ListClusterFenceRequest{
		Instance: "galileo",
		Secrets:  map[string]string{secretUserID: "k8s-fence", secretUserKey: "k=="},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.1.2.0/24", "10.1.3.0/24"}
	if len(resp.CIDRs) != len(want) {
		t.Fatalf("expected %v, got %v", want, resp.CIDRs)
	}
	for i := range want {
		if resp.CIDRs[i] != want[i] {
			t.Errorf("cidr %d: want %s got %s", i, want[i], resp.CIDRs[i])
		}
	}
}

// An empty CIDR list is a no-op (no ceph call), and an unknown instance errors.
func TestFenceEdgeCases(t *testing.T) {
	run := &fenceCmdRunner{}
	b := newFenceNetBackend(run)
	if err := b.FenceClusterNetwork(context.Background(), &bardplugin.FenceClusterNetworkRequest{Instance: "galileo"}); err != nil {
		t.Fatalf("empty cidrs should be a no-op, got %v", err)
	}
	if len(run.cephCalls()) != 0 {
		t.Fatalf("empty cidrs must issue no ceph calls, got %v", run.cephCalls())
	}
	if err := b.FenceClusterNetwork(context.Background(), &bardplugin.FenceClusterNetworkRequest{
		Instance: "nope", CIDRs: []string{"10.0.0.0/8"},
	}); err == nil {
		t.Fatal("unknown instance should error")
	}
}

// GetFenceClients returns one client: the cluster FSID as the id and this host's
// address toward the mon as a host CIDR (/32 for IPv4).
func TestGetFenceClients(t *testing.T) {
	run := &fenceCmdRunner{}
	b := newFenceNetBackend(run)
	resp, err := b.GetFenceClients(context.Background(), &bardplugin.GetFenceClientsRequest{
		Instance: "galileo",
		Secrets:  map[string]string{secretUserID: "k8s-fence", secretUserKey: "k=="},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Clients) != 1 {
		t.Fatalf("expected exactly one client, got %v", resp.Clients)
	}
	cl := resp.Clients[0]
	if cl.ID != "10ac4808-6530-11f1-8000-509a4c763a91" {
		t.Fatalf("client id must be the cluster FSID, got %q", cl.ID)
	}
	if len(cl.CIDRs) != 1 || !strings.HasSuffix(cl.CIDRs[0], "/32") {
		t.Fatalf("expected one /32 host CIDR, got %v", cl.CIDRs)
	}
	if ip := strings.TrimSuffix(cl.CIDRs[0], "/32"); net.ParseIP(ip) == nil {
		t.Fatalf("client CIDR %q is not a valid IP/32", cl.CIDRs[0])
	}
	// `ceph fsid` must have been issued.
	if !ranSeq(run.calls, "ceph") {
		t.Fatalf("expected a ceph call; got %v", run.calls)
	}
}

// clientAddrCIDR derives a host CIDR and errors with no monitors.
func TestClientAddrCIDR(t *testing.T) {
	cidr, err := clientAddrCIDR([]string{"192.168.1.225:3300"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := net.ParseCIDR(cidr); err != nil {
		t.Fatalf("clientAddrCIDR returned %q, not a valid CIDR: %v", cidr, err)
	}
	if _, err := clientAddrCIDR(nil); err == nil {
		t.Fatal("no monitors must error")
	}
}

func TestNormalizeCIDR(t *testing.T) {
	cases := map[string]string{
		"cidr:10.1.2.0:0/24":  "10.1.2.0/24",
		"cidr:192.0.2.0:5/16": "192.0.2.0/16",
		"10.0.0.5:0/32":       "10.0.0.5/32", // no cidr: prefix still normalises
		"cidr:bogus":          "",            // no prefix
		"cidr::/24":           "",            // empty addr
	}
	for in, want := range cases {
		if got := normalizeCIDR(in); got != want {
			t.Errorf("normalizeCIDR(%q) = %q, want %q", in, got, want)
		}
	}
}
