package cephplugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/kindacoolhamster/bard-csi/internal/cephenc"
	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// luksCipherPattern allows a cryptsetup cipher spec like `aes-xts-plain64` or
// `aes-cbc-essiv:sha256` -- letters, digits, dash, underscore, and a colon for the
// IV/ESSIV hash. cryptsetup itself is the final authority; this guards against junk.
var luksCipherPattern = regexp.MustCompile(`^[a-zA-Z0-9_:-]{1,64}$`)

// validateLuksTuning checks the LUKS tuning parameters at CreateVolume (before the
// image is provisioned) so a bad StorageClass fails fast without leaking an orphan
// image. It rejects malformed values and the fscrypt-plus-LUKS-tuning contradiction
// (file encryption has no LUKS header to tune).
func validateLuksTuning(p map[string]string) error {
	if _, err := luksFormatArgs(p); err != nil {
		return err
	}
	if p[paramEncryptionType] == encryptionTypeFile {
		for _, k := range []string{paramEncryptionCipher, paramEncryptionKeySize, paramEncryptionSectorSize, paramEncryptionIntegrity} {
			if p[k] != "" {
				return bardplugin.Errorf(bardplugin.CodeInvalidArg,
					"ceph-rbd: LUKS tuning (encryptionCipher/encryptionKeySize/encryptionSectorSize/integrityMode) is not supported with encryptionType=file (fscrypt)")
			}
		}
	}
	// dm-integrity (journal mode) does not support discard, so integrityMode and
	// encryptedDiscards are mutually exclusive -- reject the combination up front rather
	// than fail cryptically at `cryptsetup open` on the node.
	if strings.TrimSpace(p[paramEncryptionIntegrity]) != "" && p[paramEncryptedDiscards] == "true" {
		return bardplugin.Errorf(bardplugin.CodeInvalidArg,
			"ceph-rbd: integrityMode is incompatible with encryptedDiscards (dm-integrity does not support discard)")
	}
	return nil
}

// luksFormatArgs validates the optional LUKS tuning parameters in a volume/parameter
// map and returns the extra `cryptsetup luksFormat` arguments (cipher / key-size /
// sector-size). Empty values are skipped; an invalid value is a clear InvalidArgument
// (surfaced at CreateVolume so a bad StorageClass fails the PVC fast, not at NodeStage).
func luksFormatArgs(p map[string]string) ([]string, error) {
	var args []string
	if c := strings.TrimSpace(p[paramEncryptionCipher]); c != "" {
		if !luksCipherPattern.MatchString(c) {
			return nil, bardplugin.Errorf(bardplugin.CodeInvalidArg,
				"ceph-rbd: invalid encryptionCipher %q", c)
		}
		args = append(args, "--cipher", c)
	}
	if k := strings.TrimSpace(p[paramEncryptionKeySize]); k != "" {
		n, err := strconv.Atoi(k)
		if err != nil || n <= 0 || n%8 != 0 {
			return nil, bardplugin.Errorf(bardplugin.CodeInvalidArg,
				"ceph-rbd: invalid encryptionKeySize %q (want a positive multiple of 8, in bits)", k)
		}
		args = append(args, "--key-size", strconv.Itoa(n))
	}
	if s := strings.TrimSpace(p[paramEncryptionSectorSize]); s != "" {
		switch s {
		case "512", "1024", "2048", "4096":
			args = append(args, "--sector-size", s)
		default:
			return nil, bardplugin.Errorf(bardplugin.CodeInvalidArg,
				"ceph-rbd: invalid encryptionSectorSize %q (want 512, 1024, 2048, or 4096)", s)
		}
	}
	if m := strings.TrimSpace(p[paramEncryptionIntegrity]); m != "" {
		switch m {
		case "hmac-sha256", "hmac-sha512", "aead":
			args = append(args, "--integrity", m)
		default:
			return nil, bardplugin.Errorf(bardplugin.CodeInvalidArg,
				"ceph-rbd: invalid integrityMode %q (want hmac-sha256, hmac-sha512, or aead)", m)
		}
	}
	return args, nil
}

// Encryption: an rbd volume can be encrypted at rest with LUKS, opened on the
// node. The image itself is provisioned normally; the node maps it to a block
// device and layers a LUKS (dm-crypt) device on top, then formats/mounts the
// decrypted /dev/mapper device. So Ceph only ever sees ciphertext.
//
// Key handling follows this project's rule that credentials are plugin-resolved
// per instance (the StorageClass carries no secret, so it can address many
// clusters). Each instance has a master key mounted in the plugin; a per-volume
// passphrase is HKDF-derived from it and the volume's identity. Derivation means
// every volume gets a distinct key with no per-volume key storage -- the trade
// vs. ceph-csi's random-passphrase-stored-per-volume is that rotating an
// instance's master key rekeys all its volumes, in exchange for keeping zero key
// state. A CSI-provided `encryptionPassphrase` secret overrides derivation.

const (
	// paramEncrypted is the StorageClass parameter that turns on encryption, and the
	// volume-context key that carries that decision to the node. (Shared with cephenc.)
	paramEncrypted = cephenc.ParamEncrypted
	// paramEncryptionKMSID selects a configured KMS provider for an encrypted volume.
	paramEncryptionKMSID = cephenc.ParamEncryptionKMSID
	// paramEncryptionType selects the mode: block (default, LUKS) or file (fscrypt).
	paramEncryptionType = cephenc.ParamEncryptionType
	// encryptionTypeFile is the fscrypt mode value for paramEncryptionType.
	encryptionTypeFile = cephenc.EncryptionTypeFile
	// paramEncryptedDiscards, when "true" on an encrypted StorageClass, opens the
	// LUKS device with `--allow-discards` so filesystem TRIM/discard reaches the
	// rbd image and Ceph can reclaim freed space (thin provisioning). dm-crypt
	// blocks discards by default because they leak which blocks hold data, so this
	// is opt-in. Mirrors ceph-csi #3563. Carried to the node in the volume context.
	paramEncryptedDiscards = "encryptedDiscards"
	// LUKS format tuning, applied to `cryptsetup luksFormat` for block-mode encrypted
	// volumes (ceph-csi parity). All optional; cryptsetup defaults are used when unset.
	// Validated at CreateVolume and carried to the node in the volume context (the
	// luksFormat runs node-side). N/A for fscrypt (file) encryption.
	paramEncryptionCipher     = "encryptionCipher"     // e.g. aes-xts-plain64
	paramEncryptionKeySize    = "encryptionKeySize"    // key size in bits, e.g. 512
	paramEncryptionSectorSize = "encryptionSectorSize" // sector size in bytes (512/1024/2048/4096)
	// paramEncryptionIntegrity adds dm-integrity authenticated encryption to the LUKS2
	// volume (`cryptsetup luksFormat --integrity <mode>`): hmac-sha256/hmac-sha512 (with
	// an XTS cipher) or aead (for an AEAD cipher). It detects tampering, not just
	// confidentiality. NOTE: integrity format WIPES the device to initialise the
	// integrity tags (slower provision; de-sparsifies the image) and is incompatible
	// with encryptedDiscards. ceph-csi parity.
	paramEncryptionIntegrity = "integrityMode"
	// secretEncryptionPassphrase is an optional CSI secret holding an explicit
	// passphrase, taking precedence over the derived per-volume key. (Shared.)
	secretEncryptionPassphrase = cephenc.SecretPassphrase
	luksMapperPrefix           = "bard-luks-"
	// ctxEncryptionKeyID carries an encrypted volume's key identity to the node in
	// the volume context. For a clone it is the source's key id (the clone shares
	// the source's LUKS header), so the derived provider re-derives the source's
	// passphrase. Absent (fresh volume) the node falls back to the volume's own
	// pool/image -- identical to the value a fresh derived volume used before.
	ctxEncryptionKeyID = cephenc.CtxKeyID
)

// WithEncryption sets the directory of per-instance master key files (one file
// named for each instance), enabling encryption for volumes whose StorageClass sets
// `encrypted: "true"`. The KMS registry reads it lazily as cephenc.Host, so this may
// be chained either before or after WithKMS. Returns the backend for chaining.
func (b *Backend) WithEncryption(keyDir string) *Backend {
	b.encKeyDir = keyDir
	return b
}

// WithKMS registers named KMS providers (selected per-volume by encryptionKMSID) on
// the backend's KMS registry. Returns the backend for chaining.
func (b *Backend) WithKMS(cfgs map[string]KMSConfig) *Backend {
	b.kms.Register(cfgs)
	return b
}

// encryptionPassphrase resolves the LUKS passphrase for a volume: an explicit
// CSI secret always wins; otherwise the volume's selected KMS provider (by
// encryptionKMSID, default the derived master-key provider) supplies it. Every
// path is idempotent so the same volume yields the same passphrase on restage.
// This is the spec==keyID form with no caller connection (opens its own), for a
// freshly created volume / the test path.
func (b *Backend) encryptionPassphrase(ctx context.Context, instance, kmsID, spec string, secrets map[string]string) (string, error) {
	return b.encryptionPassphraseFor(ctx, nil, instance, kmsID, spec, spec, secrets)
}

// encryptionPassphraseFor is encryptionPassphrase with spec (the volume's own
// pool/image, for per-volume state lookup) and keyID (the key identity, inherited
// from the source on a clone) given separately, plus the caller's Ceph connection
// (conn) so an image-meta provider reuses it instead of opening a second one (nil =
// open its own). See keyService.
func (b *Backend) encryptionPassphraseFor(ctx context.Context, conn []string, instance, kmsID, spec, keyID string, secrets map[string]string) (string, error) {
	return b.kms.Passphrase(ctx, conn, instance, kmsID, spec, keyID, secrets)
}

// resizeLuks grows an active dm-crypt mapping to its underlying device's new size
// (NodeExpand, after the rbd image grew). cryptsetup resize needs the volume key;
// we re-resolve the passphrase through the KMS exactly as DeleteVolume does -- the
// KMS id and key id are recorded on the image, so this works for every provider and
// for clones -- and pass it via a temp key file (never on the command line).
func (b *Backend) resizeLuks(ctx context.Context, vol bardplugin.VolumeRef, mapper string) error {
	cc, err := b.cluster(vol.Instance)
	if err != nil {
		return err
	}
	conn, cleanup, err := b.connArgs(cc, vol.Instance, nil)
	if err != nil {
		return err
	}
	defer cleanup()
	spec := vol.Location + "/" + vol.Name
	kmsID := b.imageMetaGet(ctx, conn, spec, imgMetaKMSID)
	keyID := b.imageMetaGet(ctx, conn, spec, imgMetaKeyID)
	if keyID == "" {
		keyID = spec
	}
	pass, err := b.encryptionPassphraseFor(ctx, conn, vol.Instance, kmsID, spec, keyID, nil)
	if err != nil {
		return err
	}
	kf, err := cephenc.SecretTemp("bard-luks-key-")
	if err != nil {
		return fmt.Errorf("ceph-rbd: luks keyfile: %w", err)
	}
	defer os.Remove(kf.Name())
	if _, err := kf.WriteString(pass); err != nil {
		kf.Close()
		return fmt.Errorf("ceph-rbd: luks keyfile: %w", err)
	}
	kf.Close()
	if _, err := b.run.Run(ctx, "cryptsetup", "resize", mapper, "--key-file", kf.Name()); err != nil {
		return fmt.Errorf("ceph-rbd: grow LUKS mapping %s: %w", mapper, err)
	}
	return nil
}

// luksMapperName is the deterministic dm-crypt name for a staging path, so
// NodeUnstage can find and close the device without any recorded state.
func luksMapperName(stagingPath string) string {
	sum := sha256.Sum256([]byte(stagingPath))
	return luksMapperPrefix + hex.EncodeToString(sum[:8])
}

// isEncrypted reports whether a volume context marks the volume for LUKS.
func isEncrypted(ctx map[string]string) bool {
	return ctx[paramEncrypted] == "true"
}

// ensureLuksOpen makes /dev/mapper/<mapper> a decrypted view of dev: it formats
// dev as LUKS on first use, then opens it. Both steps are idempotent, so a
// retried NodeStage (same node) converges rather than failing. The passphrase is
// passed via a temp key file, never on the command line.
func (b *Backend) ensureLuksOpen(ctx context.Context, dev, mapper, passphrase string, allowDiscards bool, formatArgs []string) error {
	kf, err := cephenc.SecretTemp("bard-luks-key-")
	if err != nil {
		return fmt.Errorf("ceph-rbd: luks keyfile: %w", err)
	}
	defer os.Remove(kf.Name())
	if _, err := kf.WriteString(passphrase); err != nil {
		kf.Close()
		return fmt.Errorf("ceph-rbd: luks keyfile: %w", err)
	}
	kf.Close()

	// luksFormat only if the device is not already a LUKS container (isLuks exits
	// non-zero when it is not), so existing data is never reformatted. The format
	// tuning (cipher/key-size/sector-size) applies ONLY at format time -- a reopen
	// reads these from the on-disk header, so it must not pass them again.
	if _, err := b.run.Run(ctx, "cryptsetup", "isLuks", dev); err != nil {
		args := append([]string{"-q", "luksFormat", dev, "--key-file", kf.Name()}, formatArgs...)
		if _, err := b.run.Run(ctx, "cryptsetup", args...); err != nil {
			return fmt.Errorf("ceph-rbd: luksFormat %s: %w", dev, err)
		}
	}
	if b.luksActive(ctx, mapper) {
		return nil
	}
	openArgs := []string{"open", "--type", "luks", dev, mapper, "--key-file", kf.Name()}
	if allowDiscards {
		// Pass TRIM through to the rbd image so Ceph reclaims freed blocks. Off by
		// default: dm-crypt blocks discards because they reveal which sectors are used.
		openArgs = append(openArgs, "--allow-discards")
	}
	if _, err := b.run.Run(ctx, "cryptsetup", openArgs...); err != nil {
		return fmt.Errorf("ceph-rbd: luks open %s -> %s: %w", dev, mapper, err)
	}
	return nil
}

// luksActive reports whether a dm-crypt mapping is currently open.
func (b *Backend) luksActive(ctx context.Context, mapper string) bool {
	out, err := b.run.Run(ctx, "cryptsetup", "status", mapper)
	return err == nil && strings.Contains(out, "is active")
}

// tempKeyFile writes a passphrase to a private temp file for cryptsetup's --key-file
// (so it never appears on the command line). The returned cleanup removes it.
func tempKeyFile(pass string) (string, func(), error) {
	kf, err := cephenc.SecretTemp("bard-luks-key-")
	if err != nil {
		return "", func() {}, fmt.Errorf("ceph-rbd: luks keyfile: %w", err)
	}
	if _, err := kf.WriteString(pass); err != nil {
		kf.Close()
		os.Remove(kf.Name())
		return "", func() {}, fmt.Errorf("ceph-rbd: luks keyfile: %w", err)
	}
	kf.Close()
	return kf.Name(), func() { os.Remove(kf.Name()) }, nil
}

// luksBackingDevice returns the block device a dm-crypt mapping sits on (the LUKS
// container), which is what luksAddKey/luksRemoveKey operate on -- not the mapper.
func (b *Backend) luksBackingDevice(ctx context.Context, mapper string) (string, error) {
	out, err := b.run.Run(ctx, "cryptsetup", "status", mapper)
	if err != nil {
		return "", fmt.Errorf("ceph-rbd: cryptsetup status %s: %w", mapper, err)
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "device:" {
			return f[1], nil
		}
	}
	return "", fmt.Errorf("ceph-rbd: no backing device for dm-crypt %s", mapper)
}

// luksAddKey adds newPass as a new LUKS keyslot on dev, authorising with the still-
// valid oldPass. Both keyslots are usable afterwards, which is what makes rotation
// crash-safe (the old key keeps working until it is explicitly removed).
func (b *Backend) luksAddKey(ctx context.Context, dev, oldPass, newPass string) error {
	oldKf, rmOld, err := tempKeyFile(oldPass)
	if err != nil {
		return err
	}
	defer rmOld()
	newKf, rmNew, err := tempKeyFile(newPass)
	if err != nil {
		return err
	}
	defer rmNew()
	if _, err := b.run.Run(ctx, "cryptsetup", "luksAddKey", dev, newKf, "--key-file", oldKf); err != nil {
		return fmt.Errorf("ceph-rbd: luks add key on %s: %w", dev, err)
	}
	return nil
}

// luksRemoveKey removes the keyslot matching pass from dev. Idempotent: removing a
// passphrase that no longer has a slot is treated as success (a retried rotation).
func (b *Backend) luksRemoveKey(ctx context.Context, dev, pass string) error {
	kf, rm, err := tempKeyFile(pass)
	if err != nil {
		return err
	}
	defer rm()
	if _, err := b.run.Run(ctx, "cryptsetup", "luksRemoveKey", dev, kf); err != nil {
		if errContains(err, "no key available") {
			return nil
		}
		return fmt.Errorf("ceph-rbd: luks remove key on %s: %w", dev, err)
	}
	return nil
}

// RotateEncryptionKey rotates a block-encrypted (LUKS) volume's key in place (the
// csi-addons EncryptionKeyRotation operation): mint fresh key material via the
// volume's KMS provider, add it as a second LUKS keyslot while the old one still
// works, persist it, then drop the old keyslot. The data (encrypted under the LUKS
// master key) is never rewritten. Node-side: the volume must be staged here.
func (b *Backend) RotateEncryptionKey(ctx context.Context, req *bardplugin.RotateEncryptionKeyRequest) error {
	// The LUKS header lives on the rbd device that backs the dm-crypt mapping;
	// luksAddKey/luksRemoveKey operate on that backing device. Resolve it either
	// from the published path (if the CO sent one) or, since the csi-addons
	// EncryptionKeyRotation controller does not, from the staged device record.
	backing, err := b.rotationBackingDevice(ctx, req)
	if err != nil {
		return err
	}

	cc, err := b.cluster(req.Volume.Instance)
	if err != nil {
		return err
	}
	conn, cleanup, err := b.connArgs(cc, req.Volume.Instance, req.Secrets)
	if err != nil {
		return err
	}
	defer cleanup()
	spec := req.Volume.Location + "/" + req.Volume.Name
	kmsID := b.imageMetaGet(ctx, conn, spec, imgMetaKMSID)
	keyID := b.imageMetaGet(ctx, conn, spec, imgMetaKeyID)
	if keyID == "" {
		keyID = spec
	}
	oldPass, err := b.encryptionPassphraseFor(ctx, conn, req.Volume.Instance, kmsID, spec, keyID, req.Secrets)
	if err != nil {
		return err
	}
	// The provider mints + persists new material; we splice it into the LUKS keyslots
	// in the apply callback (added while the old key still authorises) so a crash at
	// any step leaves a state the old passphrase still opens.
	if err := b.kms.RotateKey(ctx, conn, req.Volume.Instance, kmsID, spec, keyID, req.Secrets, func(newPass string) error {
		return b.luksAddKey(ctx, backing, oldPass, newPass)
	}); err != nil {
		return err
	}
	return b.luksRemoveKey(ctx, backing, oldPass)
}

// rotationBackingDevice resolves the rbd device that holds the volume's LUKS header
// (the device key rotation operates on). With a published path it walks the mount to
// the dm-crypt mapper and reads the mapper's backing device. Without one (the
// csi-addons EncryptionKeyRotation controller sends no volume_path), it finds the
// volume's staged device record by handle -- the record stores the rbd device exactly
// as mapped, before LUKS was layered on -- and verifies it carries a LUKS header.
func (b *Backend) rotationBackingDevice(ctx context.Context, req *bardplugin.RotateEncryptionKeyRequest) (string, error) {
	if req.VolumePath != "" {
		src, err := b.run.Run(ctx, "findmnt", "-n", "-o", "SOURCE", "--target", req.VolumePath)
		if err != nil {
			return "", fmt.Errorf("ceph-rbd: resolve device for %s: %w", req.VolumePath, err)
		}
		dev := strings.TrimSpace(src)
		if i := strings.IndexByte(dev, '['); i >= 0 {
			dev = dev[:i]
		}
		mapper := strings.TrimPrefix(dev, "/dev/mapper/")
		if mapper == dev || !strings.HasPrefix(mapper, luksMapperPrefix) {
			return "", bardplugin.Errorf(bardplugin.CodeInvalidArg, "ceph-rbd: %s is not a LUKS-encrypted volume; key rotation applies to block (LUKS) encryption", req.VolumePath)
		}
		return b.luksBackingDevice(ctx, mapper)
	}
	rec, ok := b.findStagedDevice(req.Volume)
	if !ok || rec.Device == "" {
		return "", bardplugin.Errorf(bardplugin.CodeNotFound, "ceph-rbd: volume %s is not staged on this node; key rotation is a node operation and needs the volume mounted", refKey(req.Volume))
	}
	if !b.isLuksDevice(ctx, rec.Device) {
		return "", bardplugin.Errorf(bardplugin.CodeInvalidArg, "ceph-rbd: volume %s is not LUKS-encrypted; key rotation applies to block (LUKS) encryption", refKey(req.Volume))
	}
	return rec.Device, nil
}

// findStagedDevice scans the state dir for the device record of a volume (matched by
// its handle), since records are keyed by staging-path hash with no reverse index.
func (b *Backend) findStagedDevice(vol bardplugin.VolumeRef) (deviceRecord, bool) {
	if b.stateDir == "" {
		return deviceRecord{}, false
	}
	entries, err := os.ReadDir(b.stateDir)
	if err != nil {
		return deviceRecord{}, false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(b.stateDir, e.Name()))
		if err != nil {
			continue
		}
		var rec deviceRecord
		if json.Unmarshal(data, &rec) != nil {
			continue
		}
		if rec.Instance == vol.Instance && rec.Pool == vol.Location && rec.Image == vol.Name {
			return rec, true
		}
	}
	return deviceRecord{}, false
}

// isLuksDevice reports whether a device carries a LUKS header (`cryptsetup isLuks`).
func (b *Backend) isLuksDevice(ctx context.Context, dev string) bool {
	_, err := b.run.Run(ctx, "cryptsetup", "isLuks", dev)
	return err == nil
}

// closeLuks closes a dm-crypt mapping if it is open. It is a no-op for an
// unencrypted volume (whose mapper was never created), so NodeUnstage can call
// it unconditionally without knowing whether the volume was encrypted.
func (b *Backend) closeLuks(ctx context.Context, mapper string) error {
	if !b.luksActive(ctx, mapper) {
		return nil
	}
	if _, err := b.run.Run(ctx, "cryptsetup", "close", mapper); err != nil {
		return fmt.Errorf("ceph-rbd: luks close %s: %w", mapper, err)
	}
	return nil
}

// volumePassphrase resolves a volume's LUKS/fscrypt passphrase via the same KMS
// path for both encryption modes (so fscrypt composes with every KMS provider). A
// clone resolves its inherited key identity (encryptionKeyID).
func (b *Backend) volumePassphrase(ctx context.Context, conn []string, req *bardplugin.NodeStageRequest) (string, error) {
	spec := req.Volume.Location + "/" + req.Volume.Name
	keyID := req.Context[ctxEncryptionKeyID]
	if keyID == "" {
		keyID = spec // fresh volume: key identity is its own pool/image
	}
	return b.encryptionPassphraseFor(ctx, conn, req.Volume.Instance, req.Context[paramEncryptionKMSID], spec, keyID, req.Secrets)
}

// stageDevice is the device NodeStage should format/mount and NodePublish (block)
// should bind: the decrypted LUKS mapper for a block-encrypted volume, else the raw
// device. fscrypt (file) volumes are NOT layered here -- they format/mount the raw
// device and apply fscrypt to a directory on the mounted filesystem (setupFscrypt),
// so they take the raw-device path like an unencrypted volume.
func (b *Backend) stageDevice(ctx context.Context, conn []string, req *bardplugin.NodeStageRequest, rawDev string) (string, error) {
	if !isEncrypted(req.Context) || cephenc.IsFsCrypt(req.Context) {
		return rawDev, nil
	}
	pass, err := b.volumePassphrase(ctx, conn, req)
	if err != nil {
		return "", err
	}
	mapper := luksMapperName(req.StagingPath)
	allowDiscards := req.Context[paramEncryptedDiscards] == "true"
	formatArgs, err := luksFormatArgs(req.Context)
	if err != nil {
		return "", err
	}
	if err := b.ensureLuksOpen(ctx, rawDev, mapper, pass, allowDiscards, formatArgs); err != nil {
		return "", err
	}
	return "/dev/mapper/" + mapper, nil
}
