# Migrating from ceph-csi to Bard CSI (in-place, no data copy)

A ceph-csi RBD volume **is** an rbd image in a pool. Bard's node plane maps
`<pool>/<image>` straight out of the volume handle (see
[../static/README.md](../static/README.md)), so the *same image* can be brought
under a Bard `PersistentVolume` with **zero bytes copied**. Migrating off ceph-csi
is therefore a metadata swap — delete the ceph-csi PV/PVC objects, create Bard
ones pointing at the same image — not a data migration. Downtime is a single pod
restart, and it is fully reversible (the image is never touched).

This is distinct from copy-based migration (a Job that `rsync`/`dd`s between two
PVCs), which you only need when moving across *different* backends (e.g. EBS →
Bard RBD). For ceph-csi → Bard, in-place adoption is strictly better: no copy, no
extra capacity, near-zero downtime.

## Prerequisites

- A Bard instance in `bard-ceph-config` that points at the **same Ceph cluster and
  pool** the ceph-csi volume lives in, with a cephx user that can map the image.
  (Bard addresses clusters by *instance id*; ceph-csi by *clusterID* — there is no
  automatic mapping, so you tell the tool which Bard instance corresponds.)
- `kubectl` + `jq`, and `rbd` access to the pool for the one `image-meta` step.
- The workload can tolerate a brief restart during cutover.

## Generate the Bard objects

[`hack/adopt-ceph-csi-rbd.sh`](../../../hack/adopt-ceph-csi-rbd.sh) reads a bound
ceph-csi PVC and prints the matching Bard static PV+PVC on **stdout**, plus the
ordered cutover runbook on **stderr**. It mutates nothing.

```sh
hack/adopt-ceph-csi-rbd.sh -n <namespace> -c <pvc-name> -i <bard-instance> \
  -o /tmp/adopt.yaml
```

It derives `(instance, pool, image)` as follows: the pool and image come from the
ceph-csi PV's `volumeAttributes` (`pool`, `imageName`); if `imageName` is absent
(older ceph-csi), the image is reconstructed as `csi-vol-<uuid>` from the trailing
36 chars of the ceph-csi handle. The generated PVC keeps the **same name and
namespace** as the source, so the workload reattaches by `claimName` with no edit.

## Cutover (the reclaim dance)

The image has exactly one writer, so the workload must be down for the swap. The
order matters: set the source PV to `Retain` **before** deleting the PVC, or
ceph-csi's `DeleteVolume` will reap the image.

```sh
# 1. Quiesce the workload (release the rbd mount). NOT a data copy -- just a stop.
kubectl -n <ns> scale <deploy|statefulset>/<name> --replicas=0

# 2. Make Kubernetes keep the image when the ceph-csi PVC goes away.
kubectl patch pv <ceph-csi-pv> \
  -p '{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}'

# 3. Defence in depth: a static-marked image makes even a stray Bard DeleteVolume
#    a no-op (no `rbd rm`). See ../static/README.md.
rbd -p <pool> image-meta set <image> bard.static true

# 4. Delete the ceph-csi PVC then its now-Released PV. The image is untouched.
kubectl -n <ns> delete pvc <pvc>
kubectl delete pv <ceph-csi-pv>

# 5. Bind the image to Bard.
kubectl apply -f /tmp/adopt.yaml

# 6. Bring the workload back -- kubelet NodeStage maps the SAME image via Bard.
kubectl -n <ns> scale <deploy|statefulset>/<name> --replicas=1
```

The script prints these same steps filled in with your concrete pool/image/PV
names.

## Verify

```sh
kubectl -n <ns> get pvc <pvc>            # Bound, to bard-<pvc>
kubectl get pv bard-<pvc>                # csi.driver: csi.bard.io, Retain
# the pod comes up and sees its old data; check your app, then e.g.:
rbd -p <pool> status <image>             # a single watcher = the new Bard node
```

## Encrypted volumes

A ceph-csi encrypted RBD volume is **LUKS-on-the-image**: ceph-csi maps the rbd
image and layers dm-crypt on top, so the LUKS header and all ciphertext live *in
the image itself*, at the front of the device. Bard encrypts exactly the same way
(node-plane LUKS-on-image), and its `cryptsetup open --type luks` reads both LUKS1
and LUKS2 — whatever ceph-csi wrote — so the header is already format-compatible.

The header therefore travels with the image at adoption with **zero data
transform**: this is the same property Bard's encrypted snapshot/clone support
relies on (a LUKS header is preserved byte-for-byte; opening it is purely a matter
of resolving the right passphrase). So an encrypted adoption is the *same* metadata
swap as an unencrypted one **plus one task — bridge the passphrase**, because
ceph-csi's passphrase comes from ceph-csi's KMS and Bard resolves its own.

Because a Bard StorageClass carries no CSI secret params (that is what lets one
class address many clusters), the passphrase is attached to the **adopted PV**
itself via `spec.csi.nodeStageSecretRef` → a Secret whose `encryptionPassphrase`
key Bard uses verbatim (it overrides any KMS). Two ways to populate it:

**1. Keep ceph-csi's passphrase (simplest).** Recover the passphrase ceph-csi used
for this volume (from its KMS / per-volume secret) and put it in the node-stage
Secret. Bard opens the existing header with it. Downside: you carry ceph-csi's
passphrase as a Bard secret indefinitely.

**2. Re-key the LUKS header to a passphrase you choose (one-time, no data
re-encryption).** dm-crypt wraps one *master volume key* per keyslot; adding a
keyslot leaves the master key — and therefore every encrypted block — untouched, so
only the small keyslot area is rewritten regardless of volume size:

```sh
# while the workload is quiesced (cutover step 1):
sudo rbd -p <pool> map <image>            # -> /dev/rbdN
sudo cryptsetup luksAddKey /dev/rbdN      # prompts: EXISTING (ceph-csi) then NEW passphrase
sudo cryptsetup luksRemoveKey /dev/rbdN   # optional: retire the ceph-csi passphrase
sudo rbd -p <pool> unmap /dev/rbdN
```

Then put the NEW passphrase in the node-stage Secret.

```yaml
# add to the generated Bard PV (spec.csi):
nodeStageSecretRef: { name: <pvc>-luks, namespace: <ns> }
---
apiVersion: v1
kind: Secret
metadata: { name: <pvc>-luks, namespace: <ns> }
stringData: { encryptionPassphrase: "<the passphrase Bard should use>" }
```

> Converting an adopted volume to a Bard **KMS-managed** key (the derived,
> secrets-metadata, or Vault provider) instead of an explicit passphrase is not a
> one-liner — those keys are provider-specific material Bard computes internally —
> and is a follow-up. For now bridge with the explicit `encryptionPassphrase`
> secret (option 1 or 2).

`adopt-ceph-csi-rbd.sh` refuses `encrypted=true` unless you pass
`--allow-encrypted`, which then threads `encrypted: "true"` (and any
`encryptionKMSID`) into the generated volume attributes and prints a reminder. It
does **not** move key material — that is the one manual step above. If you would
rather not deal with the header at all, migrate the volume by copy instead (a fresh
Bard-encrypted target, data re-encrypted under Bard's own key).

## Caveats

- **Single writer during cutover.** The image can be mapped by only one node at a
  time for RWO. Do not skip the scale-to-0; mapping it under Bard while ceph-csi
  still holds it risks corruption.
- **Encrypted volumes need a passphrase bridge** — see [Encrypted volumes](#encrypted-volumes)
  above. The adopt script refuses `encrypted=true` without `--allow-encrypted`.
- **Image features must be mappable on your nodes.** ceph-csi's default features
  (`layering`, `exclusive-lock`, `object-map`, `fast-diff`, `deep-flatten`) map
  fine under krbd on modern kernels. If a node's kernel is older, use the
  `mounter: rbd-nbd` option (userspace) or strip unsupported features first.
- **Capacity must match the real image.** The generated PV copies the source PV's
  capacity; if you hand-edit, keep it equal to `rbd info <pool>/<image>`'s size.
- **Snapshots/clones don't carry their ceph-csi journal.** The image's *data*
  migrates; ceph-csi's snapshot/omap bookkeeping does not. Re-take snapshots under
  Bard after adoption if you need them managed.

## Rollback

Nothing destructive happens to the image, so reverting is just re-creating the
ceph-csi objects (or restoring your saved copy of the original PV) and pointing
the workload back. Keep a `kubectl get pv <ceph-csi-pv> -o yaml > backup.yaml`
before step 4 if you want a literal restore artifact.
