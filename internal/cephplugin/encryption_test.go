package cephplugin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// encRunner models the LUKS lifecycle statefully (format -> open -> close) plus
// the map/format/mount calls NodeStage needs, and records every call so a test
// can assert what ran and on which device.
type encRunner struct {
	calls     [][]string
	luks      map[string]bool   // device -> is LUKS
	open      map[string]string // mapper -> backing device
	mounted   map[string]bool
	formatdev map[string]bool
	mapped    map[string]bool // device -> currently mapped
}

func newEncRunner() *encRunner {
	return &encRunner{luks: map[string]bool{}, open: map[string]string{}, mounted: map[string]bool{}, formatdev: map[string]bool{}, mapped: map[string]bool{}}
}

func (r *encRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	switch name {
	case "rbd", "rbd-nbd":
		switch {
		case has(args, "info"):
			return "", os.ErrNotExist // not found -> CreateVolume provisions
		case has(args, "map"):
			r.mapped["/dev/nbd3"] = true
			return "/dev/nbd3", nil
		case has(args, "unmap"):
			delete(r.mapped, args[len(args)-1])
			return "", nil
		}
		return "", nil // create, rm, etc.
	case "blockdev":
		if r.mapped[args[len(args)-1]] {
			return "1073741824", nil
		}
		return "0", nil
	case "cryptsetup":
		return r.cryptsetup(args)
	case "blkid":
		if r.formatdev[args[len(args)-1]] {
			return "ext4", nil
		}
		return "", nil // unformatted -> ensureFormatted will mkfs
	case "mkfs.ext4":
		r.formatdev[args[len(args)-1]] = true
		return "", nil
	case "findmnt":
		return "", nil // not mounted -> NodeStage proceeds to mount
	case "mount", "umount":
		return "", nil
	}
	return "", nil
}

func (r *encRunner) cryptsetup(args []string) (string, error) {
	// positional, ignoring -q/--key-file/--type and their values.
	var pos []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--key-file", "--type":
			i++
			continue
		}
		if strings.HasPrefix(args[i], "-") {
			continue
		}
		pos = append(pos, args[i])
	}
	switch pos[0] {
	case "isLuks":
		if r.luks[pos[1]] {
			return "", nil
		}
		return "", os.ErrNotExist // not LUKS -> non-nil error
	case "luksFormat":
		r.luks[pos[1]] = true
		return "", nil
	case "open":
		r.open[pos[2]] = pos[1]
		return "", nil
	case "status":
		if _, ok := r.open[pos[1]]; ok {
			return "/dev/mapper/" + pos[1] + " is active.\n", nil
		}
		return "", os.ErrNotExist
	case "close":
		delete(r.open, pos[1])
		return "", nil
	}
	return "", nil
}

func (r *encRunner) find(name string, contains ...string) []string {
	for _, c := range r.calls {
		if c[0] != name {
			continue
		}
		joined := strings.Join(c, " ")
		ok := true
		for _, want := range contains {
			if !strings.Contains(joined, want) {
				ok = false
				break
			}
		}
		if ok {
			return c
		}
	}
	return nil
}

func (r *encRunner) count(name, sub string) int {
	n := 0
	for _, c := range r.calls {
		if c[0] == name && has(c, sub) {
			n++
		}
	}
	return n
}

func encBackend(t *testing.T, run Runner) (*Backend, string) {
	t.Helper()
	keyDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(keyDir, "east"), []byte("super-secret-master-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := New(map[string]ClusterConfig{"east": {Monitors: []string{"10.0.0.10:6789"}, Pool: "replicapool", UserID: "admin"}},
		"", filepath.Join(t.TempDir(), "state"), run).WithEncryption(keyDir)
	return b, t.TempDir()
}

// CreateVolume marks an encrypted volume in its context; NodeStage then LUKS-
// formats and opens the raw device and mounts the decrypted mapper; NodeUnstage
// closes the mapper. The passphrase is never passed on the command line.
func TestEncryptedStageLifecycle(t *testing.T) {
	run := newEncRunner()
	b, dir := encBackend(t, run)
	staging := filepath.Join(dir, "staging")
	vol := bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img"}

	create, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "v", CapacityBytes: 1 << 30, Instance: "east",
		Parameters: map[string]string{"pool": "replicapool", paramEncrypted: "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if create.Context[paramEncrypted] != "true" {
		t.Fatalf("CreateVolume should mark the volume encrypted in context, got %v", create.Context)
	}

	stage := &bardplugin.NodeStageRequest{Volume: vol, StagingPath: staging, FsType: "ext4", Context: create.Context}
	if err := b.NodeStage(context.Background(), stage); err != nil {
		t.Fatal(err)
	}

	mapper := luksMapperName(staging)
	// LUKS format + open happen on the RAW device (/dev/nbd3), not the mapper.
	if r := run.find("cryptsetup", "luksFormat", "/dev/nbd3"); r == nil {
		t.Fatalf("expected luksFormat on the raw device; calls: %v", run.calls)
	}
	if r := run.find("cryptsetup", "open", "/dev/nbd3", mapper); r == nil {
		t.Fatalf("expected cryptsetup open of the raw device to the mapper; calls: %v", run.calls)
	}
	// The filesystem is laid down and mounted from the decrypted mapper.
	if r := run.find("mkfs.ext4", "/dev/mapper/"+mapper); r == nil {
		t.Fatalf("expected mkfs on the mapper, not the ciphertext device; calls: %v", run.calls)
	}
	if r := run.find("mount", "/dev/mapper/"+mapper, staging); r == nil {
		t.Fatalf("expected mount of the mapper at staging; calls: %v", run.calls)
	}
	// The passphrase must never appear as a CLI argument (only via --key-file).
	for _, c := range run.calls {
		for _, a := range c {
			if a == "super-secret-master-key" || strings.HasPrefix(a, "super-secret") {
				t.Fatalf("master key leaked onto the command line: %v", c)
			}
		}
	}

	// Idempotent restage: the device is already LUKS and already open, so no
	// second format or open.
	if err := b.NodeStage(context.Background(), stage); err != nil {
		t.Fatal(err)
	}
	if n := run.count("cryptsetup", "luksFormat"); n != 1 {
		t.Fatalf("luksFormat must run once across two stages, ran %d", n)
	}
	if n := run.count("cryptsetup", "open"); n != 1 {
		t.Fatalf("cryptsetup open must run once across two stages, ran %d", n)
	}

	// NodeUnstage closes the mapper.
	if err := b.NodeUnstage(context.Background(), &bardplugin.NodeUnstageRequest{Volume: vol, StagingPath: staging}); err != nil {
		t.Fatal(err)
	}
	if r := run.find("cryptsetup", "close", mapper); r == nil {
		t.Fatalf("expected cryptsetup close of the mapper on unstage; calls: %v", run.calls)
	}
}

// encryptedDiscards: "true" on the StorageClass opens LUKS with --allow-discards
// so TRIM reaches the rbd image; it is off by default and only for encrypted
// volumes. Mirrors ceph-csi #3563.
func TestEncryptedDiscardsOption(t *testing.T) {
	vol := bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img"}

	// Off by default: encrypted but no discards param.
	run := newEncRunner()
	b, dir := encBackend(t, run)
	create, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "v", CapacityBytes: 1 << 30, Instance: "east",
		Parameters: map[string]string{"pool": "replicapool", paramEncrypted: "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.NodeStage(context.Background(), &bardplugin.NodeStageRequest{
		Volume: vol, StagingPath: filepath.Join(dir, "staging"), FsType: "ext4", Context: create.Context,
	}); err != nil {
		t.Fatal(err)
	}
	if r := run.find("cryptsetup", "open", "--allow-discards"); r != nil {
		t.Fatalf("discards must be off by default; calls: %v", run.calls)
	}

	// On: encryptedDiscards param flows to the volume context and to cryptsetup open.
	run = newEncRunner()
	b, dir = encBackend(t, run)
	create, err = b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "v", CapacityBytes: 1 << 30, Instance: "east",
		Parameters: map[string]string{"pool": "replicapool", paramEncrypted: "true", paramEncryptedDiscards: "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if create.Context[paramEncryptedDiscards] != "true" {
		t.Fatalf("discards decision must be carried in the volume context, got %v", create.Context)
	}
	if err := b.NodeStage(context.Background(), &bardplugin.NodeStageRequest{
		Volume: vol, StagingPath: filepath.Join(dir, "staging"), FsType: "ext4", Context: create.Context,
	}); err != nil {
		t.Fatal(err)
	}
	if r := run.find("cryptsetup", "open", "--allow-discards"); r == nil {
		t.Fatalf("expected cryptsetup open with --allow-discards; calls: %v", run.calls)
	}
}

// luksFormatArgs validates the LUKS tuning params and builds the cryptsetup flags;
// invalid values are rejected, empty values are skipped.
func TestLuksFormatArgs(t *testing.T) {
	join := func(a []string) string { return strings.Join(a, " ") }
	got, err := luksFormatArgs(map[string]string{
		paramEncryptionCipher:     "aes-xts-plain64",
		paramEncryptionKeySize:    "512",
		paramEncryptionSectorSize: "4096",
	})
	if err != nil {
		t.Fatal(err)
	}
	if join(got) != "--cipher aes-xts-plain64 --key-size 512 --sector-size 4096" {
		t.Fatalf("unexpected args: %q", join(got))
	}
	// A colon cipher spec (aes-cbc-essiv:sha256) is allowed.
	if _, err := luksFormatArgs(map[string]string{paramEncryptionCipher: "aes-cbc-essiv:sha256"}); err != nil {
		t.Fatalf("essiv cipher must be allowed: %v", err)
	}
	// integrityMode appends --integrity for an allowed mode.
	if got, err := luksFormatArgs(map[string]string{paramEncryptionCipher: "aes-xts-plain64", paramEncryptionIntegrity: "hmac-sha256"}); err != nil ||
		join(got) != "--cipher aes-xts-plain64 --integrity hmac-sha256" {
		t.Fatalf("integrity args wrong: %q %v", join(got), err)
	}
	// Empty params -> no args, no error.
	if got, err := luksFormatArgs(nil); err != nil || len(got) != 0 {
		t.Fatalf("empty params must yield no args: %v %v", got, err)
	}
	for name, p := range map[string]map[string]string{
		"bad cipher":      {paramEncryptionCipher: "aes xts"},        // space
		"cipher inject":   {paramEncryptionCipher: "aes;rm -rf"},     // metachar
		"bad key size":    {paramEncryptionKeySize: "513"},           // not a multiple of 8
		"nonnum key size": {paramEncryptionKeySize: "abc"},           //
		"bad sector":      {paramEncryptionSectorSize: "1000"},       // not a valid sector size
		"bad integrity":   {paramEncryptionIntegrity: "hmac-sha999"}, // not an allowed mode
	} {
		if _, err := luksFormatArgs(p); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}
}

// The LUKS tuning params flow from the StorageClass through the volume context to the
// node's cryptsetup luksFormat (cipher/key-size/sector-size), and only at format time.
func TestEncryptedStageLuksTuning(t *testing.T) {
	vol := bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img"}
	run := newEncRunner()
	b, dir := encBackend(t, run)
	create, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "v", CapacityBytes: 1 << 30, Instance: "east",
		Parameters: map[string]string{
			"pool": "replicapool", paramEncrypted: "true",
			paramEncryptionCipher: "aes-xts-plain64", paramEncryptionKeySize: "256", paramEncryptionSectorSize: "4096",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if create.Context[paramEncryptionCipher] != "aes-xts-plain64" {
		t.Fatalf("cipher must be carried in the volume context, got %v", create.Context)
	}
	if err := b.NodeStage(context.Background(), &bardplugin.NodeStageRequest{
		Volume: vol, StagingPath: filepath.Join(dir, "staging"), FsType: "ext4", Context: create.Context,
	}); err != nil {
		t.Fatal(err)
	}
	if r := run.find("cryptsetup", "luksFormat", "--cipher", "aes-xts-plain64", "--key-size", "256", "--sector-size", "4096"); r == nil {
		t.Fatalf("expected luksFormat to carry the tuning flags; calls: %v", run.calls)
	}
	// The flags must NOT be passed to open (a reopen reads them from the header).
	if r := run.find("cryptsetup", "open", "--cipher"); r != nil {
		t.Fatalf("cryptsetup open must not carry format tuning; call: %v", r)
	}
}

// integrityMode flows through to luksFormat's --integrity flag (dm-integrity
// authenticated encryption) and is carried in the volume context.
func TestEncryptedStageIntegrity(t *testing.T) {
	vol := bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img"}
	run := newEncRunner()
	b, dir := encBackend(t, run)
	create, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "v", CapacityBytes: 1 << 30, Instance: "east",
		Parameters: map[string]string{"pool": "replicapool", paramEncrypted: "true", paramEncryptionIntegrity: "hmac-sha256"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if create.Context[paramEncryptionIntegrity] != "hmac-sha256" {
		t.Fatalf("integrityMode must be carried in the volume context, got %v", create.Context)
	}
	if err := b.NodeStage(context.Background(), &bardplugin.NodeStageRequest{
		Volume: vol, StagingPath: filepath.Join(dir, "staging"), FsType: "ext4", Context: create.Context,
	}); err != nil {
		t.Fatal(err)
	}
	if r := run.find("cryptsetup", "luksFormat", "--integrity", "hmac-sha256"); r == nil {
		t.Fatalf("expected luksFormat with --integrity hmac-sha256; calls: %v", run.calls)
	}
}

// integrityMode is rejected at CreateVolume when combined with encryptedDiscards
// (dm-integrity has no discard) or with fscrypt (no LUKS header), before provisioning.
func TestIntegrityModeConflictsRejected(t *testing.T) {
	for name, params := range map[string]map[string]string{
		"integrity+discards": {"pool": "replicapool", paramEncrypted: "true", paramEncryptionIntegrity: "hmac-sha256", paramEncryptedDiscards: "true"},
		"integrity+fscrypt":  {"pool": "replicapool", paramEncrypted: "true", paramEncryptionIntegrity: "hmac-sha256", paramEncryptionType: encryptionTypeFile},
		"bad integrity mode": {"pool": "replicapool", paramEncrypted: "true", paramEncryptionIntegrity: "bogus"},
	} {
		run := newEncRunner()
		b, _ := encBackend(t, run)
		_, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
			Name: "v", CapacityBytes: 1 << 30, Instance: "east", Parameters: params,
		})
		var se *bardplugin.StatusError
		if !errors.As(err, &se) || se.Code != bardplugin.CodeInvalidArg {
			t.Fatalf("%s must be rejected with InvalidArgument, got %v", name, err)
		}
		for _, c := range run.calls {
			if c[0] == "rbd" && has(c, "create") {
				t.Fatalf("%s: a rejected PVC must not provision an image; call: %v", name, c)
			}
		}
	}
}

// A bad cipher fails the PVC at CreateVolume (InvalidArgument), not later at the node,
// and -- critically -- BEFORE any image is provisioned, so a rejected PVC leaks no
// orphan rbd image (the provisioner never retries/Deletes an InvalidArgument). This
// guards the ordering bug a live negative test caught.
func TestEncryptedLuksTuningRejectedAtCreate(t *testing.T) {
	run := newEncRunner()
	b, _ := encBackend(t, run)
	_, err := b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "v", CapacityBytes: 1 << 30, Instance: "east",
		Parameters: map[string]string{"pool": "replicapool", paramEncrypted: "true", paramEncryptionSectorSize: "777"},
	})
	var se *bardplugin.StatusError
	if !errors.As(err, &se) || se.Code != bardplugin.CodeInvalidArg {
		t.Fatalf("expected InvalidArgument for a bad sector size, got %v", err)
	}
	for _, c := range run.calls {
		if c[0] == "rbd" && has(c, "create") {
			t.Fatalf("a rejected PVC must not provision an image (orphan leak); call: %v", c)
		}
	}
	// fscrypt + LUKS tuning is a contradiction -> rejected.
	_, err = b.CreateVolume(context.Background(), &bardplugin.CreateVolumeRequest{
		Name: "v", CapacityBytes: 1 << 30, Instance: "east",
		Parameters: map[string]string{"pool": "replicapool", paramEncrypted: "true", paramEncryptionType: encryptionTypeFile, paramEncryptionCipher: "aes-xts-plain64"},
	})
	if !errors.As(err, &se) || se.Code != bardplugin.CodeInvalidArg {
		t.Fatalf("cipher options with fscrypt must be rejected, got %v", err)
	}
}

// An unencrypted volume never invokes cryptsetup, and unstage's unconditional
// close is a harmless no-op.
func TestUnencryptedStageSkipsLuks(t *testing.T) {
	run := newEncRunner()
	b, dir := encBackend(t, run)
	staging := filepath.Join(dir, "staging")
	vol := bardplugin.VolumeRef{Instance: "east", Location: "replicapool", Name: "img"}
	stage := &bardplugin.NodeStageRequest{Volume: vol, StagingPath: staging, FsType: "ext4"} // no encrypted context
	if err := b.NodeStage(context.Background(), stage); err != nil {
		t.Fatal(err)
	}
	for _, c := range run.calls {
		if c[0] == "cryptsetup" && (has(c, "luksFormat") || has(c, "open")) {
			t.Fatalf("unencrypted volume must not touch LUKS; call: %v", c)
		}
	}
	if err := b.NodeUnstage(context.Background(), &bardplugin.NodeUnstageRequest{Volume: vol, StagingPath: staging}); err != nil {
		t.Fatal(err)
	}
}

// The derived passphrase is deterministic per volume, distinct across volumes,
// and overridden by an explicit CSI secret.
func TestEncryptionPassphraseDerivation(t *testing.T) {
	run := newEncRunner()
	b, _ := encBackend(t, run)

	ctx := context.Background()
	p1, err := b.encryptionPassphrase(ctx, "east", "", "replicapool/img-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	p1again, _ := b.encryptionPassphrase(ctx, "east", "", "replicapool/img-a", nil)
	p2, _ := b.encryptionPassphrase(ctx, "east", "", "replicapool/img-b", nil)
	if p1 == "" || p1 != p1again {
		t.Fatalf("passphrase must be deterministic per volume: %q vs %q", p1, p1again)
	}
	if p1 == p2 {
		t.Fatalf("distinct volumes must derive distinct passphrases")
	}
	override, _ := b.encryptionPassphrase(ctx, "east", "", "replicapool/img-a", map[string]string{secretEncryptionPassphrase: "explicit"})
	if override != "explicit" {
		t.Fatalf("an explicit CSI passphrase must win, got %q", override)
	}
}

// Without a master key dir and without a secret, an encrypted stage fails clearly
// rather than silently mounting plaintext.
func TestEncryptionRequiresKeySource(t *testing.T) {
	b := New(map[string]ClusterConfig{"east": {}}, "", "", newEncRunner()) // no WithEncryption
	_, err := b.encryptionPassphrase(context.Background(), "east", "", "replicapool/img", nil)
	if err == nil {
		t.Fatal("expected an error when no key source is configured")
	}
}
