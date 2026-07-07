package cephenc

import (
	"crypto/hkdf"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/unix"
)

// fscrypt is an alternative to block-level LUKS. Instead of layering dm-crypt over a
// device, the volume is a normal ext4 filesystem with the kernel's native fscrypt
// feature, and the pod's data lives in an encrypted directory on it. File contents and
// names are encrypted per-file with a key derived from the SAME KMS passphrase the LUKS
// path uses -- so fscrypt composes with every KMS provider -- while filesystem metadata
// and free space are NOT encrypted (the LUKS trade-off). The master key is added to the
// filesystem's keyring on every stage and the policy is set once on the data directory;
// unmounting the filesystem drops the key. Block-device-agnostic: it operates on a
// staging mount path, so any backend with an ext4 mount can use it.

// FscryptDirName is the encrypted subdirectory of the staging mount holding the
// volume's data; NodePublish bind-mounts it as the pod's volume root, so the
// unencrypted filesystem root (lost+found, FS metadata) is never exposed.
const FscryptDirName = "bard-fscrypt"

// FscryptDataDir is the encrypted data directory under a staging mount.
func FscryptDataDir(stagingPath string) string {
	return filepath.Join(stagingPath, FscryptDirName)
}

// deriveFscryptKey turns the volume's KMS passphrase into a 64-byte fscrypt master key.
// Deterministic, so every restage re-derives the same key and the kernel re-computes
// the same key identifier for the existing policy.
func deriveFscryptKey(passphrase string) ([]byte, error) {
	key, err := hkdf.Key(sha256.New, []byte(passphrase), nil, "bard-fscrypt-master-key", 64)
	if err != nil {
		return nil, fmt.Errorf("cephenc: derive fscrypt key: %w", err)
	}
	return key, nil
}

// SetupFscrypt makes <stagingPath>/<FscryptDirName> an fscrypt-encrypted directory
// keyed by the volume's passphrase: it adds the master key to the mounted filesystem's
// keyring and sets a v2 encryption policy on the (empty) data directory. Idempotent
// across restages -- adding an existing key and setting an existing policy are both
// success.
func SetupFscrypt(stagingPath, passphrase string) error {
	key, err := deriveFscryptKey(passphrase)
	if err != nil {
		return err
	}
	id, err := fscryptAddKey(stagingPath, key)
	if err != nil {
		return err
	}
	dir := FscryptDataDir(stagingPath)
	if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
		return fmt.Errorf("cephenc: fscrypt mkdir %s: %w", dir, err)
	}
	return fscryptSetPolicy(dir, id)
}

// fscryptAddKey adds a raw master key to the keyring of the filesystem containing path
// and returns the kernel-computed key identifier used to reference it in a policy. The
// key bytes follow the fixed-size arg header in one buffer. Re-adding the same key is
// idempotent.
func fscryptAddKey(path string, key []byte) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cephenc: fscrypt open %s: %w", path, err)
	}
	defer f.Close()

	hdr := unsafe.Sizeof(unix.FscryptAddKeyArg{})
	buf := make([]byte, int(hdr)+len(key))
	arg := (*unix.FscryptAddKeyArg)(unsafe.Pointer(&buf[0]))
	arg.Key_spec.Type = unix.FSCRYPT_KEY_SPEC_TYPE_IDENTIFIER
	arg.Raw_size = uint32(len(key))
	copy(buf[hdr:], key)

	if err := ioctlPtr(f.Fd(), unix.FS_IOC_ADD_ENCRYPTION_KEY, unsafe.Pointer(&buf[0])); err != nil {
		return nil, fmt.Errorf("cephenc: fscrypt add key: %w", err)
	}
	id := make([]byte, unix.FSCRYPT_KEY_IDENTIFIER_SIZE)
	copy(id, arg.Key_spec.U[:unix.FSCRYPT_KEY_IDENTIFIER_SIZE])
	return id, nil
}

// fscryptSetPolicy sets a v2 fscrypt policy (AES-256-XTS contents, AES-256-CTS-16
// filenames) referencing the key identifier on an empty directory. An existing policy
// (EEXIST, on restage) is success.
func fscryptSetPolicy(dir string, identifier []byte) error {
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("cephenc: fscrypt open %s: %w", dir, err)
	}
	defer f.Close()
	pol := unix.FscryptPolicyV2{
		Version:                   unix.FSCRYPT_POLICY_V2,
		Contents_encryption_mode:  unix.FSCRYPT_MODE_AES_256_XTS,
		Filenames_encryption_mode: unix.FSCRYPT_MODE_AES_256_CTS,
		Flags:                     unix.FSCRYPT_POLICY_FLAGS_PAD_16,
	}
	copy(pol.Master_key_identifier[:], identifier)
	if err := ioctlPtr(f.Fd(), unix.FS_IOC_SET_ENCRYPTION_POLICY, unsafe.Pointer(&pol)); err != nil {
		if err == unix.EEXIST {
			return nil // already encrypted with this policy (restage)
		}
		return fmt.Errorf("cephenc: fscrypt set policy on %s: %w", dir, err)
	}
	return nil
}

func ioctlPtr(fd uintptr, req uint, arg unsafe.Pointer) error {
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, uintptr(req), uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}
