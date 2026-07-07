package cephfsplugin

import (
	"context"
	"fmt"
	"strings"

	"github.com/kindacoolhamster/bard-csi/internal/cephenc"
	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// Encryption for CephFS is filesystem-level (fscrypt) -- there is no block device to
// layer LUKS over, so the block/file split that the RBD plugin offers collapses to
// fscrypt only. A volume marked `encrypted: "true"` gets an fscrypt-encrypted data
// directory inside its subvolume mount, keyed by a passphrase from the same pluggable
// KMS the RBD plugin uses (derived master key, Vault, Azure, AWS, KMIP, secrets-
// metadata), so Ceph stores only ciphertext. The fscrypt key/policy machinery and the
// KMS providers live in the shared internal/cephenc package; this file wires CephFS in
// as a cephenc.Host (subvolume metadata is the per-volume key store).
//
// Restrictions enforced below: the kernel mounter only (the ceph kernel client carries
// the fscrypt ioctls; ceph-fuse and the NFS gateway do not), not for shallow
// (backingSnapshot) read-only volumes (they own no subvolume to host a policy), and NOT
// restore-from-snapshot / volume-clone. The last is a CephFS-layer limitation, verified
// live (2026-06-21): `ceph fs subvolume snapshot clone` copies an fscrypt-encrypted
// subvolume's ciphertext as opaque data without preserving the fscrypt context (the
// clone's file shows the padded ciphertext size, not the plaintext i_size), so the
// cloned tree is unmountable -- NodeStage blocks in the fscrypt ioctl. Unlike RBD, where
// block-level clone copies the LUKS header+ciphertext byte-for-byte, CephFS fscrypt and
// subvolume clone do not yet compose, so we reject the combination fail-fast rather than
// let the node hang. (Revisit when CephFS fscrypt+clone matures.)

// KMSConfig is the per-provider KMS config (re-exported from cephenc so the plugin's
// config loading and WithKMS keep a stable type).
type KMSConfig = cephenc.KMSConfig

// WithEncryption sets the directory of per-instance master key files, enabling fscrypt
// for volumes whose StorageClass sets `encrypted: "true"`. The KMS registry reads it
// lazily as cephenc.Host, so this may be chained before or after WithKMS.
func (b *Backend) WithEncryption(keyDir string) *Backend {
	b.encKeyDir = keyDir
	return b
}

// WithKMS registers named KMS providers (selected per-volume by encryptionKMSID).
func (b *Backend) WithKMS(cfgs map[string]KMSConfig) *Backend {
	b.kms.Register(cfgs)
	return b
}

// --- cephenc.Host: the KMS providers reach the master key dir, a reusable Ceph
// connection, and the per-volume metadata store (CephFS subvolume metadata) through
// these. A volume's spec is "<fs>/<subvolume>" (its handle Location/Name). ---

func (b *Backend) MasterKeyDir() string { return b.encKeyDir }

// ConnFor reuses the caller's Ceph connection (conn non-nil) rather than open a second
// one per operation; a nil conn opens a fresh one whose cleanup closes it.
func (b *Backend) ConnFor(conn []string, instance string, secrets map[string]string) ([]string, func(), error) {
	if conn != nil {
		return conn, func() {}, nil
	}
	cc, err := b.cluster(instance)
	if err != nil {
		return nil, nil, err
	}
	return b.cephConn(cc, instance, secrets)
}

// splitMetaSpec parses a volume spec into its parts: "<fs>/<group>/<subvolume>"
// (a grouped subvolume) or "<fs>/<subvolume>" (the _nogroup default, empty group).
func splitMetaSpec(spec string) (fs, group, sub string, ok bool) {
	parts := strings.Split(spec, "/")
	switch len(parts) {
	case 3:
		return parts[0], parts[1], parts[2], true
	case 2:
		return parts[0], "", parts[1], true
	default:
		return "", "", "", false
	}
}

// MetaGet reads a subvolume-metadata value, returning "" when the key (or subvolume) is
// absent -- the same "unset" semantics the RBD plugin's image-meta get has.
func (b *Backend) MetaGet(ctx context.Context, conn []string, spec, key string) string {
	fs, group, sub, ok := splitMetaSpec(spec)
	if !ok {
		return ""
	}
	out, err := b.run.Run(ctx, "ceph", withGroup(append(append([]string{}, conn...), "fs", "subvolume", "metadata", "get", fs, sub, key), group)...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// MetaSet records a key/value on a subvolume.
func (b *Backend) MetaSet(ctx context.Context, conn []string, spec, key, value string) error {
	fs, group, sub, ok := splitMetaSpec(spec)
	if !ok {
		return fmt.Errorf("cephfs: malformed metadata spec %q", spec)
	}
	if _, err := b.run.Run(ctx, "ceph", withGroup(append(append([]string{}, conn...), "fs", "subvolume", "metadata", "set", fs, sub, key, value), group)...); err != nil {
		return fmt.Errorf("cephfs: subvolume metadata set %s/%s %s: %w", fs, sub, key, err)
	}
	return nil
}

// validateEncryptionParams rejects encryption configurations CephFS cannot honour:
// block (LUKS) mode (no block device), a non-kernel mounter (no fscrypt ioctls), and
// shallow backing-snapshot volumes (no owned subvolume). Called from CreateVolume.
func (b *Backend) validateEncryptionParams(cc ClusterConfig, params map[string]string, isClone bool) error {
	if params[cephenc.ParamEncrypted] != "true" {
		return nil
	}
	if t := params[cephenc.ParamEncryptionType]; t != "" && t != cephenc.EncryptionTypeFile {
		return bardplugin.Errorf(bardplugin.CodeInvalidArg,
			"cephfs: only filesystem-level encryption is supported (encryptionType=file/fscrypt), got %q", t)
	}
	if cc.Mounter == mounterFuse || cc.Mounter == mounterNFS {
		return bardplugin.Errorf(bardplugin.CodeInvalidArg,
			"cephfs: encrypted volumes require the kernel mounter (mounter=%q does not support fscrypt)", cc.Mounter)
	}
	if params[paramBackingSnapshot] == "true" {
		return bardplugin.Errorf(bardplugin.CodeInvalidArg,
			"cephfs: a shallow (backingSnapshot) read-only volume cannot be encrypted")
	}
	if isClone {
		return bardplugin.Errorf(bardplugin.CodeInvalidArg,
			"cephfs: an encrypted volume cannot be restored from a snapshot or cloned "+
				"(CephFS subvolume clone does not preserve fscrypt context)")
	}
	return nil
}

// recordEncryption persists a fresh encrypted volume's KMS descriptor on its subvolume
// (the KMS id, so DeleteVolume can clean up the stored key; the derived provider records
// nothing). Encrypted clones are rejected at validation, so there is no inherit path.
func (b *Backend) recordEncryption(ctx context.Context, conn []string, req *bardplugin.CreateVolumeRequest, spec string) error {
	if req.Parameters[cephenc.ParamEncrypted] != "true" {
		return nil
	}
	if id := req.Parameters[cephenc.ParamEncryptionKMSID]; id != "" {
		return b.MetaSet(ctx, conn, spec, cephenc.MetaKMSID, id)
	}
	return nil
}

// encryptionVolumeContext returns the volume-context entries that carry the encryption
// decision to the node: the encrypted marker, the fixed fscrypt type, and the KMS id.
// CephFS encryption is always fscrypt and never a clone, so there is no inherited keyID.
func encryptionVolumeContext(params map[string]string) map[string]string {
	if params[cephenc.ParamEncrypted] != "true" {
		return nil
	}
	ctx := map[string]string{
		cephenc.ParamEncrypted:      "true",
		cephenc.ParamEncryptionType: cephenc.EncryptionTypeFile,
	}
	if id := params[cephenc.ParamEncryptionKMSID]; id != "" {
		ctx[cephenc.ParamEncryptionKMSID] = id
	}
	return ctx
}

// volumePassphrase resolves an encrypted volume's fscrypt passphrase via the KMS. The
// key identity is the volume's own spec (CephFS encryption has no clones).
func (b *Backend) volumePassphrase(ctx context.Context, conn []string, req *bardplugin.NodeStageRequest) (string, error) {
	spec := req.Volume.Location + "/" + req.Volume.Name
	return b.kms.Passphrase(ctx, conn, req.Volume.Instance, req.Context[cephenc.ParamEncryptionKMSID], spec, spec, req.Secrets)
}

// deleteEncryptionKey cleans up an encrypted subvolume's KMS-stored key material before
// the subvolume is removed (DeleteVolume carries no volume context, so the KMS id is
// read back from the subvolume metadata). A no-op for unencrypted volumes / the derived
// provider. Idempotent so a retried delete converges.
func (b *Backend) deleteEncryptionKey(ctx context.Context, conn []string, instance, spec string) error {
	kmsID := b.MetaGet(ctx, conn, spec, cephenc.MetaKMSID)
	if kmsID == "" {
		return nil
	}
	return b.kms.DeleteKey(ctx, conn, instance, kmsID, spec)
}
