package iscsiplugin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// targetdInst returns a single fully-valid targetd-managed instance ("remote").
func targetdInst() map[string]InstanceConfig {
	return map[string]InstanceConfig{
		"remote": {
			Management:      "targetd",
			TargetdEndpoint: "http://10.0.0.5:18700/targetrpc",
			TargetdPool:     "vg-targetd",
			TargetIQN:       "iqn.2025-01.io.bard:remote",
			Portal:          "10.0.0.5:3260",
		},
	}
}

// A targetd instance with chapAuth: true must be rejected at config load
// (inst()), not silently accepted: targetd's export_create hardcodes the
// shared target's TPG authentication attribute to "0" on every export, with
// no API to override it, so CHAP credentials set via initiator_set_auth are
// never actually enforced (live-verified against targetd 0.10.4: the login
// response still advertises AuthMethod=CHAP, but the kernel initiator aborts
// before the actual challenge, for both a correct and a missing password). A
// StorageClass carrying chapAuth: true must never silently protect nothing.
func TestTargetdInstanceRejectsCHAPAuth(t *testing.T) {
	inst := targetdInst()
	ic := inst["remote"]
	ic.CHAPAuth = true
	inst["remote"] = ic
	b := New(inst, "", "", "", "", "", &fakeRunner{})
	_, err := b.inst("remote")
	if err == nil {
		t.Fatal("targetd instance with chapAuth: true must be rejected at config load")
	}
	if !strings.Contains(err.Error(), "chapAuth") {
		t.Fatalf("error should explain the chapAuth rejection, got: %v", err)
	}
}

// A targetd instance rejects CreateSnapshot outright: targetd's vol_copy is a
// synchronous full copy, unsafe under provisioner retries. The rejection must
// carry CodeUnsupported (a terminal, non-retried CSI failure) and the message
// must say why AND that local-management instances support it.
func TestCreateSnapshotTargetdRejected(t *testing.T) {
	b := New(targetdInst(), "", "", "", "", "", &fakeRunner{})
	_, err := b.CreateSnapshot(context.Background(), &bardplugin.CreateSnapshotRequest{
		Name:         "snap1",
		SourceVolume: bardplugin.VolumeRef{Instance: "remote", Location: "vg-targetd", Name: "bard-x"},
	})
	var se *bardplugin.StatusError
	if err == nil || !errors.As(err, &se) || se.Code != bardplugin.CodeUnsupported {
		t.Fatalf("CreateSnapshot on a targetd instance must fail with CodeUnsupported, got %v", err)
	}
	if !strings.Contains(se.Message, "targetd") {
		t.Fatalf("error must say why (targetd), got %q", se.Message)
	}
	if !strings.Contains(se.Message, "local") {
		t.Fatalf("error must say local-management instances support it, got %q", se.Message)
	}
}

// A targetd instance rejects creating a volume from a source (snapshot
// restore OR volume clone) for the same underlying reason as CreateSnapshot,
// but with a DIFFERENT code: CSI mandates INVALID_ARGUMENT when a plugin
// cannot create a volume from the requested source, which overrides the
// general mode-of-operation rule that lets CreateSnapshot return Unsupported.
// Bard's conformance runner fails a clone rejected as anything else.
func TestCreateVolumeFromSourceTargetdRejected(t *testing.T) {
	b := New(targetdInst(), "", "", "", "", "", &fakeRunner{})

	t.Run("from snapshot", func(t *testing.T) {
		_, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
			Name: "restored", Instance: "remote", CapacityBytes: 1 << 30,
			SourceSnapshot: &bardplugin.VolumeRef{Instance: "remote", Location: "vg-targetd", Name: "snap-abc"},
		})
		var se *bardplugin.StatusError
		if err == nil || !errors.As(err, &se) || se.Code != bardplugin.CodeInvalidArg {
			t.Fatalf("restore-from-snapshot on a targetd instance must fail with CodeInvalidArg (CSI mandates INVALID_ARGUMENT for an unsupported source), got %v", err)
		}
		if !strings.Contains(se.Message, "targetd") || !strings.Contains(se.Message, "local") {
			t.Fatalf("error must say why (targetd) and that local-management instances support it, got %q", se.Message)
		}
	})

	t.Run("from volume clone", func(t *testing.T) {
		_, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
			Name: "cloned", Instance: "remote", CapacityBytes: 1 << 30,
			SourceVolume: &bardplugin.VolumeRef{Instance: "remote", Location: "vg-targetd", Name: "bard-src"},
		})
		var se *bardplugin.StatusError
		if err == nil || !errors.As(err, &se) || se.Code != bardplugin.CodeInvalidArg {
			t.Fatalf("clone on a targetd instance must fail with CodeInvalidArg (CSI mandates INVALID_ARGUMENT for an unsupported source), got %v", err)
		}
		if !strings.Contains(se.Message, "targetd") || !strings.Contains(se.Message, "local") {
			t.Fatalf("error must say why (targetd) and that local-management instances support it, got %q", se.Message)
		}
	})
}

// targetdCredsSetup mirrors chapSetup: writes a 2-line credentials file for
// instance "remote" and returns a fully-valid targetd instance map + the dir.
func targetdCredsSetup(t *testing.T, lines string) (map[string]InstanceConfig, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "remote"), []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	return targetdInst(), dir
}

// targetdCredsFor loads and parses valid 2-line (username, password)
// credentials for a targetd-managed instance.
func TestTargetdCredsForLoadsValid(t *testing.T) {
	inst, dir := targetdCredsSetup(t, "svcuser\nsvcpass\n")
	b := New(inst, "", "", "", dir, "", &fakeRunner{})
	creds, err := b.targetdCredsFor("remote")
	if err != nil {
		t.Fatal(err)
	}
	if creds == nil || creds.User != "svcuser" || creds.Password != "svcpass" {
		t.Fatalf("unexpected creds: %+v", creds)
	}
}

// A non-targetd (local-management) instance has no targetd credentials to
// load: targetdCredsFor must return (nil, nil), never an error, mirroring
// chapFor's not-applicable case.
func TestTargetdCredsForNotApplicableForLocal(t *testing.T) {
	b := New(eastInst(), "", "", "", "/nonexistent-dir", "", &fakeRunner{})
	creds, err := b.targetdCredsFor("east")
	if err != nil {
		t.Fatalf("local-management instance must not error looking up targetd creds, got %v", err)
	}
	if creds != nil {
		t.Fatalf("local-management instance must have no targetd creds, got %+v", creds)
	}
}

// A missing or malformed credentials file is an error, not a silent nil --
// and the error must name the file PATH only, never leak file contents.
func TestTargetdCredsForMissingOrMalformed(t *testing.T) {
	dir := t.TempDir()
	b := New(targetdInst(), "", "", "", dir, "", &fakeRunner{})
	if _, err := b.targetdCredsFor("remote"); err == nil {
		t.Fatal("targetd instance with no credentials file must error")
	}

	// Wrong line count (targetd creds are exactly 2 lines, no mutual-pair mode).
	instBad, dirBad := targetdCredsSetup(t, "only-a-user\n")
	b2 := New(instBad, "", "", "", dirBad, "", &fakeRunner{})
	if _, err := b2.targetdCredsFor("remote"); err == nil {
		t.Fatal("1-line targetd credentials file must be rejected")
	}

	// Whitespace/quotes rejected at load, same discipline as chapFor.
	instWS, dirWS := targetdCredsSetup(t, "svc\npass word\n")
	b3 := New(instWS, "", "", "", dirWS, "", &fakeRunner{})
	_, err := b3.targetdCredsFor("remote")
	if err == nil {
		t.Fatal("targetd credentials containing whitespace must be rejected")
	}
	if strings.Contains(err.Error(), "pass word") || strings.Contains(err.Error(), "svc") {
		t.Fatalf("targetd credential errors must never leak file contents, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), dirWS) {
		t.Fatalf("targetd credential errors must name the file path, got %q", err.Error())
	}
}
