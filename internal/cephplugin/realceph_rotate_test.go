package cephplugin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// realRotHeaderLen is the LUKS2 default payload offset (16 MiB): the region holding
// the binary headers, JSON metadata and keyslots. It is the only part of the image
// that rotation and restore touch, so it is the only part this test copies.
const realRotHeaderLen = 16 << 20

// realRotWriterScript writes the first N bytes of a local file into an rbd image at
// offset 0 through librbd, without mapping the image (no sudo, no block device).
// argv: mon user pool image localPath N. The cephx key arrives on stdin, never argv.
const realRotWriterScript = `
import sys
import rados
import rbd

mon, user, pool, image, path, n = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5], int(sys.argv[6])
key = sys.stdin.readline().strip()
with open(path, "rb") as f:
    data = f.read(n)
if len(data) != n:
    raise SystemExit("short local header: %d != %d" % (len(data), n))
cluster = rados.Rados(conffile="", conf={"mon_host": mon, "key": key}, rados_id=user)
cluster.connect(timeout=30)
try:
    ioctx = cluster.open_ioctx(pool)
    try:
        with rbd.Image(ioctx, image) as img:
            img.write(data, 0)
    finally:
        ioctx.close()
finally:
    cluster.shutdown()
`

// realRotRun runs an external command and returns its exit code and combined output.
// A non-zero exit is reported as a code, not an error; err is only for failing to
// start the process.
func realRotRun(ctx context.Context, stdin, name string, args ...string) (int, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out), nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), string(out), nil
	}
	return -1, string(out), err
}

// realRotMust fails the test unless a command exited 0.
func realRotMust(t *testing.T, what string, code int, out string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if code != 0 {
		t.Fatalf("%s: exit %d: %s", what, code, strings.TrimSpace(out))
	}
}

// realRotPassFile writes a passphrase to a 0600 file in dir and returns its path.
// Passphrases only ever travel through key files, never argv.
func realRotPassFile(t *testing.T, dir, name, pass string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(pass), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// realRotWriteHeader copies the first realRotHeaderLen bytes of localPath into the
// rbd image spec (pool/image) at offset 0, via the librbd script above.
func realRotWriteHeader(t *testing.T, ctx context.Context, mon, user, key, pool, image, localPath string) {
	t.Helper()
	code, out, err := realRotRun(ctx, key+"\n", "python3", "-I", "-c", realRotWriterScript,
		mon, user, pool, image, localPath, strconv.Itoa(realRotHeaderLen))
	realRotMust(t, "librbd header write to "+pool+"/"+image, code, out, err)
}

// realRotExport exports an image or snapshot spec (pool/image or pool/image@snap) to
// a local file through the rbd CLI with the same connection args the plugin uses.
func realRotExport(t *testing.T, ctx context.Context, conn []string, spec, dst string) {
	t.Helper()
	args := append(append([]string{}, conn...), "export", spec, dst)
	code, out, err := realRotRun(ctx, "", "rbd", args...)
	realRotMust(t, "rbd export "+spec, code, out, err)
}

// realRotOpens reports whether the LUKS container at path opens with pass, as a
// cryptsetup exit code. --test-passphrase checks the keyslot without activating a
// dm mapping, so it needs no root and no block device.
func realRotOpens(t *testing.T, ctx context.Context, path, pass string) int {
	t.Helper()
	kf := realRotPassFile(t, t.TempDir(), "probe.key", pass)
	code, out, err := realRotRun(ctx, "", "cryptsetup", "open", "--test-passphrase", "--disable-locks",
		"--type", "luks2", "--key-file", kf, path)
	if err != nil {
		t.Fatalf("cryptsetup open --test-passphrase: %v", err)
	}
	if code != 0 {
		t.Logf("cryptsetup open --test-passphrase exit %d: %s", code, strings.TrimSpace(out))
	}
	return code
}

// realRotPayloadOffsetCheck asserts the local LUKS container's payload starts at the
// 16 MiB boundary we copy, so the header write covers every keyslot.
func realRotPayloadOffsetCheck(t *testing.T, ctx context.Context, path string) {
	t.Helper()
	code, out, err := realRotRun(ctx, "", "cryptsetup", "luksDump", "--disable-locks", path)
	realRotMust(t, "luksDump", code, out, err)
	// The data segment's line is "offset: <bytes> [bytes]" (cryptsetup 2.8); the keyslot
	// area line is "Area offset:..." and is deliberately not matched.
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 3 && f[0] == "offset:" && f[2] == "[bytes]" {
			got, perr := strconv.ParseInt(f[1], 10, 64)
			if perr != nil {
				t.Fatalf("parse data offset %q: %v", line, perr)
			}
			if got != realRotHeaderLen {
				t.Fatalf("data offset = %d bytes, harness copies %d", got, realRotHeaderLen)
			}
			return
		}
	}
	t.Fatalf("luksDump printed no data-segment offset line:\n%s", out)
}

// TestRealCephRotateThenRestore proves, against a REAL Ceph cluster with REAL LUKS2
// headers, that restoring a snapshot taken BEFORE an encryption key rotation opens
// with the passphrase the plugin resolves for the restore -- the snapshot's own key
// copy (CreateSnapshot), not the source's rotated key, which this header never held.
// Restores used to inherit the rotated key and failed to open (cryptsetup exit 2).
// Skipped unless BARD_CEPH_TEST=1. Run from a host that reaches the mon:
//
//	BARD_CEPH_TEST=1 CEPH_MON=192.168.1.225:3300 CEPH_POOL=k8s-csi-test \
//	CEPH_USER=k8s-csi-test CEPH_KEY=AQ...== \
//	go test ./internal/cephplugin/ -run TestRealCephRotateThenRestore -v
//
// Sequence: encrypted source -> write LUKS header (old key) -> snapshot -> rotate
// (add new keyslot to the LIVE header, persist new key, drop old keyslot) -> restore
// clone from the snapshot -> open the restore's header with the plugin-resolved
// passphrase. Cleanup is deferred restore -> snapshot -> source.
func TestRealCephRotateThenRestore(t *testing.T) {
	if os.Getenv("BARD_CEPH_TEST") != "1" {
		t.Skip("set BARD_CEPH_TEST=1 (and CEPH_MON/POOL/USER/KEY) to run against real Ceph")
	}
	mon, pool, user, key := os.Getenv("CEPH_MON"), os.Getenv("CEPH_POOL"), os.Getenv("CEPH_USER"), os.Getenv("CEPH_KEY")
	if mon == "" || pool == "" || user == "" || key == "" {
		t.Fatal("CEPH_MON, CEPH_POOL, CEPH_USER and CEPH_KEY must all be set")
	}

	const instance, kmsID = "real", "k8s"
	work := t.TempDir()
	keyDir := filepath.Join(work, "keys")
	if err := os.MkdirAll(keyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keyDir, instance), []byte("rotrest-live-master-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	be := New(map[string]ClusterConfig{
		instance: {Monitors: []string{mon}, Pool: pool, UserID: user},
	}, "", "", nil). // nil => real ExecRunner (real rbd / cryptsetup CLIs)
				WithEncryption(keyDir).
				WithKMS(map[string]KMSConfig{kmsID: {Type: "secrets-metadata"}})

	secrets := map[string]string{"userID": user, "userKey": key}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	conn, connCleanup, err := be.connArgs(be.clusters[instance], instance, secrets)
	if err != nil {
		t.Fatal(err)
	}
	defer connCleanup()

	stamp := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().Unix())
	srcName := "rotrest-src-" + stamp
	encParams := func() map[string]string {
		return map[string]string{"pool": pool, paramEncrypted: "true", paramEncryptionKMSID: kmsID}
	}

	// 1. Encrypted source. Its DEK is minted by the first passphrase resolution.
	src, err := be.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: srcName, CapacityBytes: 64 << 20, Instance: instance,
		Parameters: encParams(), Secrets: secrets,
	})
	if err != nil {
		t.Fatalf("CreateVolume (source): %v", err)
	}
	srcRef := bardplugin.VolumeRef{Instance: instance, Location: src.Location, Name: src.Name}
	srcSpec := src.Location + "/" + src.Name
	defer func() {
		if err := be.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{Volume: srcRef, Secrets: secrets}); err != nil {
			t.Errorf("DeleteVolume (source %s): %v", srcSpec, err)
		}
	}()
	oldPass, err := be.encryptionPassphrase(ctx, instance, kmsID, srcSpec, secrets)
	if err != nil {
		t.Fatalf("resolve old passphrase: %v", err)
	}
	t.Logf("source %s created; old passphrase resolved", srcSpec)

	// 2. Real LUKS2 header (old key only) written into the source image.
	local := filepath.Join(work, "luks.img")
	f, err := os.Create(local)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(64 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	oldKF := realRotPassFile(t, work, "old.key", oldPass)
	code, out, err := realRotRun(ctx, "", "cryptsetup", "luksFormat", "-q", "--type", "luks2",
		"--pbkdf", "pbkdf2", "--pbkdf-force-iterations", "1000", "--disable-locks",
		"--key-file", oldKF, local)
	realRotMust(t, "cryptsetup luksFormat (old key)", code, out, err)
	realRotPayloadOffsetCheck(t, ctx, local)
	realRotWriteHeader(t, ctx, mon, user, key, pool, src.Name, local)
	hdrAtSnapshot, err := os.ReadFile(local)
	if err != nil {
		t.Fatal(err)
	}
	hdrAtSnapshot = hdrAtSnapshot[:realRotHeaderLen]
	t.Log("LUKS2 header (old keyslot) written into the source image")

	// 3. Snapshot of the source: point-in-time copy of that header.
	snap, err := be.CreateSnapshot(ctx, &bardplugin.CreateSnapshotRequest{
		Name: srcName + "-snap", SourceVolume: srcRef, Secrets: secrets,
	})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	snapRef := bardplugin.VolumeRef{Instance: instance, Location: snap.Location, Name: snap.Name}
	snapSpec := snap.Location + "/" + snap.Name
	defer func() {
		if err := be.DeleteSnapshot(ctx, &bardplugin.DeleteSnapshotRequest{Snapshot: snapRef, Secrets: secrets}); err != nil {
			t.Errorf("DeleteSnapshot (%s): %v", snapSpec, err)
		}
	}()
	// Control: the snapshot really holds the pre-rotation header (old keyslot).
	snapExport := filepath.Join(work, "snap.img")
	realRotExport(t, ctx, conn, snapSpec, snapExport)
	if got := realRotHeaderOf(t, snapExport); !bytes.Equal(got, hdrAtSnapshot) {
		t.Fatal("control: snapshot header differs from the pre-rotation header")
	}
	t.Logf("snapshot %s taken; its header matches the pre-rotation header", snapSpec)

	// 4. Rotate. RotateKey mints new material and calls apply(newPass) BEFORE it
	// persists it: apply adds newPass as a keyslot to the local header, then writes
	// the header back to the live source image. This mirrors RotateEncryptionKey,
	// except the backing device is the local file (no mapping, no node).
	var newPass string
	err = be.kms.RotateKey(ctx, nil, instance, kmsID, srcSpec, srcSpec, secrets, func(np string) error {
		newPass = np
		if err := be.luksAddKey(ctx, local, oldPass, np); err != nil {
			return err
		}
		realRotWriteHeader(t, ctx, mon, user, key, pool, src.Name, local)
		return nil
	})
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	if newPass == "" || newPass == oldPass {
		t.Fatal("rotation did not produce a distinct new passphrase")
	}
	// After the new key is persisted, drop the old keyslot (as RotateEncryptionKey does),
	// then write the final header back to the live source.
	if err := be.luksRemoveKey(ctx, local, oldPass); err != nil {
		t.Fatalf("luksRemoveKey (old): %v", err)
	}
	realRotWriteHeader(t, ctx, mon, user, key, pool, src.Name, local)
	t.Log("rotated: new keyslot added, persisted, old keyslot removed; header written back")

	// 7. Control: the LIVE source now opens with the new key and not the old one.
	srcExport := filepath.Join(work, "src-after-rotate.img")
	realRotExport(t, ctx, conn, srcSpec, srcExport)
	srcNew := realRotOpens(t, ctx, srcExport, newPass)
	srcOld := realRotOpens(t, ctx, srcExport, oldPass)
	t.Logf("control source after rotation: open(newPass)=exit %d, open(oldPass)=exit %d", srcNew, srcOld)
	if srcNew != 0 {
		t.Errorf("control: live source does not open with the rotated (new) passphrase (exit %d)", srcNew)
	}
	if srcOld == 0 {
		t.Errorf("control: live source still opens with the retired (old) passphrase")
	}

	// 5. Restore a clone from the pre-rotation snapshot.
	restore, err := be.CreateVolume(ctx, &bardplugin.CreateVolumeRequest{
		Name: srcName + "-restore", CapacityBytes: 64 << 20, Instance: instance,
		Parameters: encParams(), Secrets: secrets, SourceSnapshot: &snapRef,
	})
	if err != nil {
		t.Fatalf("CreateVolume (restore from pre-rotation snapshot): %v", err)
	}
	restoreRef := bardplugin.VolumeRef{Instance: instance, Location: restore.Location, Name: restore.Name}
	restoreSpec := restore.Location + "/" + restore.Name
	defer func() {
		if err := be.DeleteVolume(ctx, &bardplugin.DeleteVolumeRequest{Volume: restoreRef, Secrets: secrets}); err != nil {
			t.Errorf("DeleteVolume (restore %s): %v", restoreSpec, err)
		}
	}()
	restorePass, err := be.encryptionPassphraseFor(ctx, nil, instance, kmsID, restoreSpec, restore.Context[ctxEncryptionKeyID], secrets)
	if err != nil {
		t.Fatalf("resolve restore passphrase: %v", err)
	}

	// 6. Open the restore's header with the plugin-resolved passphrase, and record the
	// old-key result for diagnosis.
	restoreExport := filepath.Join(work, "restore.img")
	realRotExport(t, ctx, conn, restoreSpec, restoreExport)
	if got := realRotHeaderOf(t, restoreExport); !bytes.Equal(got, hdrAtSnapshot) {
		t.Fatal("control: restore header differs from the snapshot header (clone did not copy the snapshot)")
	}
	restoreWithResolved := realRotOpens(t, ctx, restoreExport, restorePass)
	restoreWithOld := realRotOpens(t, ctx, restoreExport, oldPass)
	t.Logf("restore %s: open(plugin-resolved)=exit %d, open(oldPass)=exit %d; resolved==oldPass: %v, resolved==newPass: %v",
		restoreSpec, restoreWithResolved, restoreWithOld, restorePass == oldPass, restorePass == newPass)

	if restoreWithResolved != 0 {
		t.Errorf("restore of a pre-rotation snapshot does not open with the passphrase the plugin resolves for it "+
			"(cryptsetup exit %d); its header holds only the old keyslot (open with oldPass exit %d)", restoreWithResolved, restoreWithOld)
	}
}

// realRotHeaderOf reads the first realRotHeaderLen bytes of an exported image.
func realRotHeaderOf(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < realRotHeaderLen {
		t.Fatalf("exported image %s is %d bytes, shorter than the header", path, len(b))
	}
	return b[:realRotHeaderLen]
}
