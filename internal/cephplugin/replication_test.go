package cephplugin

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// replRunner records rbd invocations and returns seeded mirror status / snap ls.
type replRunner struct {
	calls     [][]string
	statusOut string // `rbd mirror image status` JSON
	snapOut   string // `rbd snap ls --all` JSON
	enableErr error
}

func (r *replRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	switch {
	case has(args, "snap") && has(args, "ls"):
		return r.snapOut, nil
	case has(args, "status"):
		return r.statusOut, nil
	case has(args, "enable"):
		return "", r.enableErr
	}
	return "", nil
}

func (r *replRunner) rbd() [][]string {
	var out [][]string
	for _, c := range r.calls {
		if c[0] == "rbd" {
			out = append(out, c)
		}
	}
	return out
}

func newReplBackend(run Runner) *Backend {
	return New(map[string]ClusterConfig{
		"galileo": {Monitors: []string{"192.168.1.225:3300"}, Pool: "k8s-csi-test", UserID: "k8s-csi-test"},
	}, "", "", run)
}

func joined(calls [][]string) string {
	var b strings.Builder
	for _, c := range calls {
		b.WriteString(strings.Join(c, " "))
		b.WriteByte('\n')
	}
	return b.String()
}

// Enable issues `mirror image enable <spec> snapshot` and, when the class sets a
// schedulingInterval, adds a mirror-snapshot schedule for the pool/image.
func TestEnableVolumeReplicationWithSchedule(t *testing.T) {
	run := &replRunner{}
	b := newReplBackend(run)
	err := b.EnableVolumeReplication(context.Background(), &bardplugin.EnableReplicationRequest{
		Volume:     bardplugin.VolumeRef{Instance: "galileo", Location: "k8s-csi-test", Name: "csi-vol-abc"},
		Parameters: map[string]string{paramSchedulingInterval: "5m"},
	})
	if err != nil {
		t.Fatal(err)
	}
	all := joined(run.rbd())
	if !strings.Contains(all, "mirror image enable k8s-csi-test/csi-vol-abc snapshot") {
		t.Errorf("missing image enable:\n%s", all)
	}
	if !strings.Contains(all, "mirror snapshot schedule add --pool k8s-csi-test --image csi-vol-abc 5m") {
		t.Errorf("missing schedule add:\n%s", all)
	}
}

// Without a schedulingInterval, Enable does not add a schedule.
func TestEnableVolumeReplicationNoSchedule(t *testing.T) {
	run := &replRunner{}
	b := newReplBackend(run)
	if err := b.EnableVolumeReplication(context.Background(), &bardplugin.EnableReplicationRequest{
		Volume: bardplugin.VolumeRef{Instance: "galileo", Location: "k8s-csi-test", Name: "csi-vol-abc"},
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(joined(run.rbd()), "schedule add") {
		t.Fatalf("no schedulingInterval should mean no schedule add:\n%s", joined(run.rbd()))
	}
}

// Promote passes --force only when requested; demote never does.
func TestPromoteDemoteForce(t *testing.T) {
	run := &replRunner{}
	b := newReplBackend(run)
	vol := bardplugin.VolumeRef{Instance: "galileo", Location: "k8s-csi-test", Name: "img"}
	if err := b.PromoteVolume(context.Background(), &bardplugin.PromoteVolumeRequest{Volume: vol, Force: true}); err != nil {
		t.Fatal(err)
	}
	if err := b.DemoteVolume(context.Background(), &bardplugin.DemoteVolumeRequest{Volume: vol}); err != nil {
		t.Fatal(err)
	}
	all := joined(run.rbd())
	if !strings.Contains(all, "mirror image promote k8s-csi-test/img --force") {
		t.Errorf("promote should pass --force:\n%s", all)
	}
	if strings.Contains(all, "demote k8s-csi-test/img --force") {
		t.Errorf("demote must not pass --force:\n%s", all)
	}
}

// Disable removes the schedule then disables mirroring.
func TestDisableVolumeReplication(t *testing.T) {
	run := &replRunner{}
	b := newReplBackend(run)
	if err := b.DisableVolumeReplication(context.Background(), &bardplugin.DisableReplicationRequest{
		Volume: bardplugin.VolumeRef{Instance: "galileo", Location: "k8s-csi-test", Name: "img"},
	}); err != nil {
		t.Fatal(err)
	}
	all := joined(run.rbd())
	if !strings.Contains(all, "schedule remove --pool k8s-csi-test --image img") || !strings.Contains(all, "mirror image disable k8s-csi-test/img --force") {
		t.Fatalf("disable should remove schedule + force-disable image:\n%s", all)
	}
}

// GetVolumeReplicationInfo returns the latest COMPLETE mirror snapshot's time
// (the reliable primary+secondary RPO source), ignoring incomplete + non-mirror
// snapshots.
func TestReplicationInfoLastSync(t *testing.T) {
	run := &replRunner{snapOut: `[
		{"name":"user-snap","timestamp":"Thu Jun 25 23:12:00 2026","namespace":{"type":"user"}},
		{"name":".mirror.primary.x.a","timestamp":"Thu Jun 25 23:12:57 2026","namespace":{"type":"mirror","complete":true}},
		{"name":".mirror.primary.x.b","timestamp":"Thu Jun 25 23:13:30 2026","namespace":{"type":"mirror","complete":false}},
		{"name":".mirror.primary.x.c","timestamp":"Thu Jun 25 23:13:10 2026","namespace":{"type":"mirror","complete":true}}
	]`}
	b := newReplBackend(run)
	resp, err := b.GetVolumeReplicationInfo(context.Background(), &bardplugin.ReplicationInfoRequest{
		Volume: bardplugin.VolumeRef{Instance: "galileo", Location: "k8s-csi-test", Name: "img"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Latest COMPLETE mirror snapshot is 23:13:10 (the 23:13:30 one is incomplete,
	// the user snap is ignored).
	want := time.Date(2026, 6, 25, 23, 13, 10, 0, time.Local).Unix()
	if resp.LastSyncTimeUnix != want {
		t.Fatalf("want %d (23:13:10 local), got %d", want, resp.LastSyncTimeUnix)
	}
}

// Resync issues the resync and reports Ready from the post-status (up+replaying).
func TestResyncReady(t *testing.T) {
	run := &replRunner{statusOut: `{"state":"up+replaying","description":"replaying"}`}
	b := newReplBackend(run)
	resp, err := b.ResyncVolume(context.Background(), &bardplugin.ResyncVolumeRequest{
		Volume: bardplugin.VolumeRef{Instance: "galileo", Location: "k8s-csi-test", Name: "img"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Ready {
		t.Fatal("up+replaying status should report Ready")
	}
	if !strings.Contains(joined(run.rbd()), "mirror image resync k8s-csi-test/img") {
		t.Fatalf("resync command missing:\n%s", joined(run.rbd()))
	}
}

// A namespaced volume threads --namespace into the schedule command.
func TestEnableReplicationNamespaced(t *testing.T) {
	run := &replRunner{}
	b := newReplBackend(run)
	if err := b.EnableVolumeReplication(context.Background(), &bardplugin.EnableReplicationRequest{
		Volume:     bardplugin.VolumeRef{Instance: "galileo", Location: "k8s-csi-test/tenant-a", Name: "img"},
		Parameters: map[string]string{paramSchedulingInterval: "1h"},
	}); err != nil {
		t.Fatal(err)
	}
	all := joined(run.rbd())
	if !strings.Contains(all, "mirror image enable k8s-csi-test/tenant-a/img snapshot") {
		t.Errorf("namespaced enable spec wrong:\n%s", all)
	}
	if !strings.Contains(all, "--pool k8s-csi-test --image img 1h --namespace tenant-a") {
		t.Errorf("namespaced schedule should set --namespace:\n%s", all)
	}
}
