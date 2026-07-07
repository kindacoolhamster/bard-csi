package cephplugin

import (
	"os"
	"testing"
)

func TestDeviceRecord(t *testing.T) {
	dir := t.TempDir()
	b := &Backend{stateDir: dir}
	staging := "/var/lib/kubelet/plugins/kubernetes.io/csi/csi.bard.io/abc/globalmount"

	if got := b.lookupDevice(staging); got != "" {
		t.Fatalf("before record: got %q, want empty", got)
	}
	if err := b.recordDevice(staging, deviceRecord{Device: "/dev/nbd7", UnmapOptions: "force"}); err != nil {
		t.Fatalf("recordDevice: %v", err)
	}
	if got := b.lookupDevice(staging); got != "/dev/nbd7" {
		t.Fatalf("lookup: got %q, want /dev/nbd7", got)
	}
	if rec := b.readDeviceRecord(staging); rec.UnmapOptions != "force" {
		t.Fatalf("unmap options: got %q, want force", rec.UnmapOptions)
	}
	// Different staging path is independent.
	if got := b.lookupDevice(staging + "x"); got != "" {
		t.Fatalf("other path: got %q, want empty", got)
	}
	b.clearDevice(staging)
	if got := b.lookupDevice(staging); got != "" {
		t.Fatalf("after clear: got %q, want empty", got)
	}

	// Backward compatibility: a record written in the old format (the bare device
	// path, not JSON) is still read back as the device, with no unmap options.
	if err := os.WriteFile(b.deviceRecordPath(staging), []byte("/dev/rbd3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rec := b.readDeviceRecord(staging); rec.Device != "/dev/rbd3" || rec.UnmapOptions != "" {
		t.Fatalf("legacy record: got %+v, want device=/dev/rbd3 no opts", rec)
	}

	// stateDir "" disables the record (no error, always empty -> findmnt fallback).
	nb := &Backend{stateDir: ""}
	if err := nb.recordDevice(staging, deviceRecord{Device: "/dev/nbd0"}); err != nil {
		t.Fatalf("recordDevice (disabled): %v", err)
	}
	if got := nb.lookupDevice(staging); got != "" {
		t.Fatalf("disabled lookup: got %q, want empty", got)
	}
}
