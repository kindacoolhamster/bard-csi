# Example: encryption key rotation via csi-addons (EncryptionKeyRotation)

Rotates an encrypted volume's key — the csi-addons **EncryptionKeyRotation**
operation. Bard changes the LUKS **passphrase** to fresh key material **without
re-encrypting the data**: a LUKS volume's master key (which encrypts the data) is
left untouched; only the keyslot that wraps it is replaced. So rotation is fast and
the volume stays online and readable throughout. Bard serves the real csi-addons
API, so a ceph-csi user's `EncryptionKeyRotationJob`/`CronJob` resources work
unchanged.

## How it works

Rotation is **node-side** (it needs the staged dm-crypt device), driven crash-safely:

1. resolve the volume's **current** passphrase via its KMS provider;
2. mint **new** key material and `cryptsetup luksAddKey` it as a second keyslot,
   authorised by the old passphrase — now both keys open the volume;
3. **persist** the new material in the KMS provider (overwriting the old);
4. `cryptsetup luksRemoveKey` the old slot.

A crash at any step leaves a state the old passphrase still opens (until step 3
commits), so a retry converges.

## Which volumes can rotate

Rotation needs a **stored-key** KMS provider — one that keeps independent random key
material per volume: **secrets-metadata, Vault, AWS KMS, Azure Key Vault, KMIP**. The
default **derived** provider computes its key deterministically from the instance
master key (no per-volume secret to rotate), so it is rejected with a clear error;
rotate the **instance master key** for those volumes instead (which rekeys all of
them). An explicit `encryptionPassphrase` secret likewise has nothing to rotate.

## Run it

```sh
# Prereqs: csi-addons CRDs + controller-manager (see ../reclaimspace/), Bard's
# csi-addons sidecar enabled (chart: sidecars.csiAddons.enabled=true), and an
# encrypted PVC that is currently mounted by a pod, on a stored-key KMS SC
# (e.g. bard-rbd-encrypted-k8s = secrets-metadata).
#
# IMPORTANT: enable attach (chart: attach.enabled=true). Rotation is a node
# operation, and the csi-addons controller-manager finds which node a volume is on
# via its VolumeAttachment object. With attach off (Bard's default for node-mapped
# backends) there is no VolumeAttachment, so the job fails fast with
# "unable to find nodeID for pv". This is the same reason ceph-csi runs rbd with
# the attacher. (Toggling attach on an existing install recreates the CSIDriver --
# its attachRequired field is immutable.)

kubectl apply -f deploy/examples/keyrotation/encryptionkeyrotationjob.yaml
kubectl get encryptionkeyrotationjob bard-rotate-key \
  -o jsonpath='{.status.result}{"\n"}'   # -> Succeeded
```

The volume keeps serving I/O the whole time; afterwards the old passphrase no longer
opens it (only the new key material does), while the data is byte-for-byte the same.
