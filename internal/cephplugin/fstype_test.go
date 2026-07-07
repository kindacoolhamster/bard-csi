package cephplugin

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/kindacoolhamster/bard-csi/internal/cephenc"
	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// ranSeq reports whether some recorded call begins with the given tokens.
func ranSeq(calls [][]string, want ...string) bool {
	for _, c := range calls {
		if len(c) < len(want) {
			continue
		}
		ok := true
		for i, w := range want {
			if c[i] != w {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// A btrfs volume is formatted with mkfs.btrfs and mounted -t btrfs (proves a
// non-default, non-ext, non-xfs fsType flows through stage end to end).
func TestStageBtrfs(t *testing.T) {
	dir := t.TempDir()
	run := newFenceRunner(`{"watchers":[]}`)
	b := newFenceBackend(dir, run)
	err := b.NodeStage(context.Background(), &bardplugin.NodeStageRequest{
		Volume:      bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img"},
		StagingPath: filepath.Join(dir, "staging"),
		FsType:      "btrfs",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ranSeq(run.calls, "mkfs.btrfs") {
		t.Fatalf("expected mkfs.btrfs; calls: %v", run.calls)
	}
	if !ranSeq(run.calls, "mount", "-t", "btrfs") {
		t.Fatalf("expected mount -t btrfs; calls: %v", run.calls)
	}
}

// An unsupported fsType is rejected with InvalidArgument and never reaches mkfs.
func TestStageUnsupportedFsType(t *testing.T) {
	dir := t.TempDir()
	run := newFenceRunner(`{"watchers":[]}`)
	b := newFenceBackend(dir, run)
	err := b.NodeStage(context.Background(), &bardplugin.NodeStageRequest{
		Volume:      bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img"},
		StagingPath: filepath.Join(dir, "staging"),
		FsType:      "zfs",
	})
	var se *bardplugin.StatusError
	if !errors.As(err, &se) || se.Code != bardplugin.CodeInvalidArg {
		t.Fatalf("expected InvalidArgument for unsupported fsType, got %v", err)
	}
	for _, c := range run.calls {
		if len(c) > 0 && c[0] == "mkfs.zfs" {
			t.Fatalf("must not attempt mkfs for an unsupported fsType; calls: %v", run.calls)
		}
	}
}

// mountFlagsForFs adds nouuid for xfs only, and never duplicates it.
func TestMountFlagsForFs(t *testing.T) {
	if got := mountFlagsForFs("ext4", []string{"noatime"}); len(got) != 1 || got[0] != "noatime" {
		t.Fatalf("ext4 flags must be untouched, got %v", got)
	}
	got := mountFlagsForFs("xfs", nil)
	if len(got) != 1 || got[0] != "nouuid" {
		t.Fatalf("xfs must get nouuid, got %v", got)
	}
	got = mountFlagsForFs("xfs", []string{"noatime"})
	if len(got) != 2 || got[1] != "nouuid" {
		t.Fatalf("xfs must append nouuid to existing flags, got %v", got)
	}
	got = mountFlagsForFs("xfs", []string{"nouuid", "noatime"})
	if len(got) != 2 {
		t.Fatalf("xfs must not duplicate nouuid, got %v", got)
	}
}

// An xfs volume is mounted with nouuid auto-added (an rbd clone shares the source's
// xfs UUID, which xfs refuses to mount otherwise).
func TestStageXfsAddsNouuid(t *testing.T) {
	dir := t.TempDir()
	run := newFenceRunner(`{"watchers":[]}`)
	b := newFenceBackend(dir, run)
	err := b.NodeStage(context.Background(), &bardplugin.NodeStageRequest{
		Volume:      bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img"},
		StagingPath: filepath.Join(dir, "staging"),
		FsType:      "xfs",
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range run.calls {
		if len(c) >= 4 && c[0] == "mount" && c[1] == "-t" && c[2] == "xfs" {
			joined := ""
			for i, a := range c {
				if a == "-o" && i+1 < len(c) {
					joined = c[i+1]
				}
			}
			if joined != "nouuid" {
				t.Fatalf("xfs mount must carry -o nouuid, got options %q in %v", joined, c)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an xfs mount call; calls: %v", run.calls)
	}
}

// mkfsArgsForFs uses tuned thin-rbd defaults per filesystem, lets mkfsOptions replace
// them entirely, and always appends the fscrypt encrypt feature last.
func TestMkfsArgsForFs(t *testing.T) {
	eq := func(got, want []string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}
	if got := mkfsArgsForFs("ext4", "", false); !eq(got, []string{"-m0", "-Enodiscard,lazy_itable_init=1,lazy_journal_init=1"}) {
		t.Fatalf("ext4 default mkfs args wrong: %v", got)
	}
	if got := mkfsArgsForFs("ext2", "", false); !eq(got, []string{"-m0", "-Enodiscard,lazy_itable_init=1"}) {
		t.Fatalf("ext2 must drop lazy_journal_init (no journal): %v", got)
	}
	if got := mkfsArgsForFs("xfs", "", false); !eq(got, []string{"-K"}) {
		t.Fatalf("xfs default mkfs args wrong: %v", got)
	}
	if got := mkfsArgsForFs("btrfs", "", false); len(got) != 0 {
		t.Fatalf("btrfs must have no tuned default args, got %v", got)
	}
	// mkfsOptions replaces the tuned defaults verbatim.
	if got := mkfsArgsForFs("ext4", "-b 4096 -I 256", false); !eq(got, []string{"-b", "4096", "-I", "256"}) {
		t.Fatalf("mkfsOptions must replace defaults: %v", got)
	}
	// fscrypt's encrypt feature is appended even when mkfsOptions overrides.
	if got := mkfsArgsForFs("ext4", "-b 4096", true); !eq(got, []string{"-b", "4096", "-O", "encrypt"}) {
		t.Fatalf("fscrypt encrypt feature must survive an mkfsOptions override: %v", got)
	}
}

// A fresh ext4 volume is formatted with the tuned defaults; an explicit mkfsOptions in
// the volume context replaces them at the node.
func TestStageMkfsOptions(t *testing.T) {
	dir := t.TempDir()
	// Default (no mkfsOptions): tuned ext4 args present on the mkfs call.
	run := newFenceRunner(`{"watchers":[]}`)
	b := newFenceBackend(dir, run)
	if err := b.NodeStage(context.Background(), &bardplugin.NodeStageRequest{
		Volume:      bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img"},
		StagingPath: filepath.Join(dir, "staging"),
		FsType:      "ext4",
	}); err != nil {
		t.Fatal(err)
	}
	if !ranSeq(run.calls, "mkfs.ext4", "-m0", "-Enodiscard,lazy_itable_init=1,lazy_journal_init=1") {
		t.Fatalf("expected tuned default mkfs.ext4 args; calls: %v", run.calls)
	}

	// With mkfsOptions: the user's args replace the defaults.
	run2 := newFenceRunner(`{"watchers":[]}`)
	b2 := newFenceBackend(dir, run2)
	if err := b2.NodeStage(context.Background(), &bardplugin.NodeStageRequest{
		Volume:      bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img2"},
		StagingPath: filepath.Join(dir, "staging2"),
		FsType:      "ext4",
		Context:     map[string]string{paramMkfsOptions: "-b 1024 -N 8192"},
	}); err != nil {
		t.Fatal(err)
	}
	if !ranSeq(run2.calls, "mkfs.ext4", "-b", "1024", "-N", "8192") {
		t.Fatalf("expected mkfs.ext4 with the overriding mkfsOptions; calls: %v", run2.calls)
	}
	if ranSeq(run2.calls, "mkfs.ext4", "-m0") {
		t.Fatalf("mkfsOptions must replace, not append to, the tuned defaults; calls: %v", run2.calls)
	}
}

// fsTypeRunner answers findmnt with a chosen FSTYPE + SOURCE so NodeExpand picks
// the right grow tool.
type fsTypeRunner struct {
	calls  [][]string
	fsType string
	source string // findmnt SOURCE; defaults to /dev/nbd7
}

func (r *fsTypeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if name == "findmnt" {
		if has(args, "FSTYPE") {
			return r.fsType, nil
		}
		if r.source != "" {
			return r.source, nil
		}
		return "/dev/nbd7", nil // SOURCE
	}
	return "", nil
}

// NodeExpand grows btrfs online by mountpoint (btrfs filesystem resize max),
// never resize2fs.
func TestNodeExpandBtrfs(t *testing.T) {
	dir := t.TempDir()
	run := &fsTypeRunner{fsType: "btrfs"}
	b := newFenceBackend(dir, run)
	if _, err := b.NodeExpand(context.Background(), &bardplugin.NodeExpandRequest{VolumePath: "/var/lib/kubelet/pods/x/vol"}); err != nil {
		t.Fatal(err)
	}
	if !ranSeq(run.calls, "btrfs", "filesystem", "resize", "max", "/var/lib/kubelet/pods/x/vol") {
		t.Fatalf("expected btrfs filesystem resize max <mountpoint>; calls: %v", run.calls)
	}
	if ranSeq(run.calls, "resize2fs") {
		t.Fatalf("btrfs must not be grown with resize2fs; calls: %v", run.calls)
	}
}

// An fscrypt volume publishes the encrypted subdir, so findmnt reports the device
// with a bind subpath (`/dev/rbd0[/bard-fscrypt]`). NodeExpand must strip it to the
// bare device for resize2fs, and must not invoke cryptsetup (no LUKS mapper).
func TestNodeExpandFscryptStripsSubpath(t *testing.T) {
	dir := t.TempDir()
	run := &fsTypeRunner{fsType: "ext4", source: "/dev/rbd0[/" + cephenc.FscryptDirName + "]"}
	b := newFenceBackend(dir, run)
	if _, err := b.NodeExpand(context.Background(), &bardplugin.NodeExpandRequest{VolumePath: "/var/lib/kubelet/pods/x/vol"}); err != nil {
		t.Fatal(err)
	}
	if !ranSeq(run.calls, "resize2fs", "/dev/rbd0") {
		t.Fatalf("expected resize2fs on the bare device /dev/rbd0; calls: %v", run.calls)
	}
	for _, c := range run.calls {
		if c[0] == "cryptsetup" {
			t.Fatalf("fscrypt must not touch cryptsetup; call: %v", c)
		}
	}
}

// LUKS-mapper resize (cryptsetup resize the dm-crypt mapping before resize2fs) is
// proven live on the k3s tier, not unit-faked: it re-resolves the passphrase through
// the KMS (image-meta + cluster), so it needs the real encryption harness, the same
// way the LUKS open/format path is live-proven rather than run under the fake runner.
