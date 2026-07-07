// Package cephenc is the backend-agnostic encryption layer shared by Bard's Ceph
// plugins (ceph-rbd, cephfs): the pluggable KMS providers that resolve a per-volume
// passphrase, and the fscrypt key/policy helpers. It is deliberately independent of
// any one backend -- a provider needs only three things from its host: the master
// key directory, a Ceph connection to reuse for metadata ops, and a per-volume
// metadata store (rbd image-meta on the RBD plugin, subvolume metadata on CephFS).
// LUKS/dm-crypt orchestration is block-device specific and stays in the RBD plugin;
// this package provides the KMS passphrase source both encryption modes share.
package cephenc

import (
	"context"

	"github.com/kindacoolhamster/bard-csi/pkg/bardplugin"
)

// CSI-facing parameter / secret / volume-context keys shared by both Ceph backends.
const (
	// ParamEncrypted turns on encryption for a volume, and is echoed into the volume
	// context so the node applies it.
	ParamEncrypted = "encrypted"
	// ParamEncryptionKMSID selects a configured KMS provider; empty => the built-in
	// derived (per-instance master key) provider.
	ParamEncryptionKMSID = "encryptionKMSID"
	// ParamEncryptionType selects the mode: "block" (default, LUKS/dm-crypt; RBD only)
	// or "file" (fscrypt).
	ParamEncryptionType = "encryptionType"
	// EncryptionTypeFile is the fscrypt mode value for ParamEncryptionType.
	EncryptionTypeFile = "file"
	// SecretPassphrase is an optional CSI secret holding an explicit passphrase that
	// overrides any KMS provider.
	SecretPassphrase = "encryptionPassphrase"
	// CtxKeyID carries an encrypted volume's key identity to the node (for a clone,
	// the source's identity, so the derived provider re-derives the source key).
	CtxKeyID = "encryptionKeyID"
)

// Per-volume metadata keys recording an encrypted volume's KMS descriptor. The store
// is backend-defined (rbd image-meta / cephfs subvolume metadata) and reached through
// Host; the key names are shared so the descriptor is portable across backends.
const (
	// MetaKMSID records which KMS provider holds the volume's passphrase (read by
	// DeleteVolume, which carries no volume context).
	MetaKMSID = "bard.encryptionKMSID"
	// MetaKeyID records the volume's key identity (the value the derived provider
	// keys off); a clone inherits its source's so it re-derives the source key.
	MetaKeyID = "bard.encryptionKeyID"
	// MetaWrappedDEK holds a per-volume data key wrapped for the volume (secrets-
	// metadata's master-key-wrapped DEK, or AWS KMS's encrypted blob). Rides with the
	// volume, so deleting the volume reaps it.
	MetaWrappedDEK = "bard.encryptionDEK"
	// MetaKMIPUID records the KMIP object UID holding the passphrase (the secret lives
	// on the HSM, not the volume, so DeleteVolume must Destroy it explicitly).
	MetaKMIPUID = "bard.encryptionKMIPUID"
)

// IsEncrypted reports whether a volume context marks the volume for encryption.
func IsEncrypted(ctx map[string]string) bool {
	return ctx[ParamEncrypted] == "true"
}

// IsFsCrypt reports whether an encrypted volume uses fscrypt (file) vs LUKS (block).
func IsFsCrypt(ctx map[string]string) bool {
	return IsEncrypted(ctx) && ctx[ParamEncryptionType] == EncryptionTypeFile
}

// Host is what a backend supplies so the KMS providers can run against it: the master
// key directory, a Ceph connection to reuse, and a per-volume metadata store.
//
// ConnFor reuses the caller's connection (conn non-nil) rather than open a second one
// per operation; a nil conn opens a fresh one whose returned cleanup closes it, while
// a borrowed conn gets a no-op cleanup (the caller still owns it). MetaGet/MetaSet
// read/write a per-volume key/value identified by spec (the volume's pool/image or
// fs/subvolume); MetaGet returns "" for an absent key. Providers that keep key
// material outside the volume (Vault, Azure) ignore conn and never touch the store.
type Host interface {
	MasterKeyDir() string
	ConnFor(conn []string, instance string, secrets map[string]string) ([]string, func(), error)
	MetaGet(ctx context.Context, conn []string, spec, key string) string
	MetaSet(ctx context.Context, conn []string, spec, key, value string) error
}

// keyService resolves the passphrase for one volume. passphrase must be idempotent:
// the same volume always yields the same passphrase so it can be reopened on every
// restage. spec is the volume's own identity (where a stateful provider stores/reads
// its key, so each clone owns an independent entry); keyID is the encryption key
// identity, inherited from the source on a clone (the derived provider keys off it).
// For a fresh volume the two are equal. deleteKey removes any stored key material for
// the volume; it must be idempotent and a no-op for providers that store nothing.
type keyService interface {
	passphrase(ctx context.Context, conn []string, instance, spec, keyID string, secrets map[string]string) (string, error)
	deleteKey(ctx context.Context, conn []string, instance, spec string) error
}

// keyCloner is implemented by a provider that keeps key material outside the volume
// (Vault, Azure, KMIP), so an encrypted clone -- whose copied LUKS header needs the
// source's passphrase -- can be given its own independent copy. Providers that store
// their key in the volume metadata (secrets-metadata, aws-kms) need no hook: the
// descriptor copy carries the wrapped key with the clone.
type keyCloner interface {
	cloneKey(ctx context.Context, conn []string, instance, sourceSpec, cloneSpec string, secrets map[string]string) error
}

// keyRotator is implemented by a provider whose key material can be rotated in place
// (the stored-key providers: secrets-metadata, vault, aws-kms, azure-kv, kmip). It
// mints NEW material, hands the new passphrase to apply -- which the node uses to add
// a second LUKS keyslot while the old one is still valid -- and only persists the new
// material once apply succeeds, so any crash leaves a recoverable state (the old
// passphrase still resolves and still opens the volume). The derived provider does
// not implement this: its key is a deterministic function of the instance master key,
// so rotation means rotating that master key (which rekeys all its volumes).
type keyRotator interface {
	rotateKey(ctx context.Context, conn []string, instance, spec, keyID string, secrets map[string]string, apply func(newPassphrase string) error) error
}

// Registry holds the configured KMS providers for a backend and resolves volume
// passphrases through them. The empty (default) id is the built-in derived provider.
type Registry struct {
	host      Host
	providers map[string]keyService
}

// NewRegistry builds a registry bound to host with the given providers (which may be
// extended later via Register, e.g. when WithEncryption/WithKMS are chained).
func NewRegistry(host Host, cfgs map[string]KMSConfig) *Registry {
	r := &Registry{host: host, providers: map[string]keyService{}}
	r.Register(cfgs)
	return r
}

// resolve selects the provider for a KMS id: empty => the derived master-key
// provider; a known id => its provider; an unknown id is an error.
func (r *Registry) resolve(kmsID string) (keyService, error) {
	if kmsID == "" {
		return derivedKeyService{keyDir: r.host.MasterKeyDir()}, nil
	}
	ks, ok := r.providers[kmsID]
	if !ok {
		return nil, bardplugin.Errorf(bardplugin.CodeInvalidArg, "cephenc: unknown encryptionKMSID %q", kmsID)
	}
	return ks, nil
}

// Passphrase resolves a volume's passphrase: an explicit CSI secret always wins;
// otherwise the selected KMS provider supplies it. Idempotent per volume.
func (r *Registry) Passphrase(ctx context.Context, conn []string, instance, kmsID, spec, keyID string, secrets map[string]string) (string, error) {
	if p := secrets[SecretPassphrase]; p != "" {
		return p, nil
	}
	ks, err := r.resolve(kmsID)
	if err != nil {
		return "", err
	}
	return ks.passphrase(ctx, conn, instance, spec, keyID, secrets)
}

// DeleteKey removes a volume's stored key material from its KMS provider (called by
// DeleteVolume, which reads the recorded kmsID back from the volume metadata).
func (r *Registry) DeleteKey(ctx context.Context, conn []string, instance, kmsID, spec string) error {
	ks, err := r.resolve(kmsID)
	if err != nil {
		return err
	}
	return ks.deleteKey(ctx, conn, instance, spec)
}

// CloneKey gives an encrypted clone its own independent copy of the source's key, for
// a provider that stores key material outside the volume. A no-op for providers whose
// key rides in the volume metadata (the descriptor copy already carried it).
func (r *Registry) CloneKey(ctx context.Context, conn []string, instance, kmsID, sourceSpec, cloneSpec string, secrets map[string]string) error {
	ks, err := r.resolve(kmsID)
	if err != nil {
		return err
	}
	if kc, ok := ks.(keyCloner); ok {
		return kc.cloneKey(ctx, conn, instance, sourceSpec, cloneSpec, secrets)
	}
	return nil
}

// RotateKey rotates a volume's key (the csi-addons EncryptionKeyRotation operation).
// The selected provider mints new key material, calls apply(newPassphrase) -- which
// adds it as a second LUKS keyslot using the still-valid old passphrase -- and then
// persists the new material. An explicit CSI passphrase secret has no stored material
// to rotate, and the derived provider's key is deterministic, so both return
// InvalidArgument (rotate the instance master key for derived-key volumes instead).
func (r *Registry) RotateKey(ctx context.Context, conn []string, instance, kmsID, spec, keyID string, secrets map[string]string, apply func(newPassphrase string) error) error {
	if secrets[SecretPassphrase] != "" {
		return bardplugin.Errorf(bardplugin.CodeInvalidArg, "cephenc: cannot rotate an explicit encryptionPassphrase secret")
	}
	ks, err := r.resolve(kmsID)
	if err != nil {
		return err
	}
	kr, ok := ks.(keyRotator)
	if !ok {
		id := kmsID
		if id == "" {
			id = "derived"
		}
		return bardplugin.Errorf(bardplugin.CodeInvalidArg, "cephenc: provider %q does not support key rotation (rotate the instance master key instead)", id)
	}
	return kr.rotateKey(ctx, conn, instance, spec, keyID, secrets, apply)
}
