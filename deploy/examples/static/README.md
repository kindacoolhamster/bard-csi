# Example: static (pre-provisioned) RBD volumes

Bring an **existing** RBD image under Bard management by hand-authoring a
PersistentVolume, instead of having a StorageClass provision a new one. Use this
to import images created out-of-band (migrated from another system, restored from
backup, or shared read-only) without Bard ever running CreateVolume.

A statically provisioned volume is **owned by the admin, not by CSI**: Bard must
never delete it. Two layers enforce that:

1. **`persistentVolumeReclaimPolicy: Retain`** on the PV — with Retain, Kubernetes
   never calls the driver's DeleteVolume at all. Always set this.
2. **`bard.static` image metadata** — defence in depth. If the image carries
   `bard.static=true`, Bard's DeleteVolume is a no-op (returns success without
   `rbd rm`), so even a misconfigured `reclaimPolicy: Delete` cannot destroy the
   image. KMS key material is left untouched too.

## Steps

```sh
# 1. The image already exists (or create it):
rbd -p mypool create preexisting --size 1G

# 2. Mark it static so Bard will never reap it:
rbd image-meta set mypool/preexisting bard.static true

# 3. Author the PV + PVC (edit instance/pool/image/size below), then:
kubectl apply -f deploy/examples/static/static-pv.yaml
```

## The volume handle

The PV's `csi.volumeHandle` is Bard's volume id. Hand-author it as:

```
swsk|1|<backendType>|<instance>|<pool>|<image>
```

e.g. `swsk|1|ceph-rbd|galileo|mypool|preexisting`. The fields are: magic `swsk`,
encoding version `1`, backend type (`ceph-rbd`), the instance/zone id from your
`bard-ceph-config` (which selects the cluster + cephx key), the rbd pool, and the
image name. The node plugin maps `<pool>/<image>` directly from this — no
CreateVolume, no generated metadata.

## Encrypted / other options

If the pre-existing image is LUKS-encrypted by Bard, carry the same node-side
attributes the StorageClass would have set, in `volumeAttributes` (e.g.
`encrypted: "true"`, `encryptionKMSID: <id>`). For a plain image, only `fsType`
matters (or `volumeMode: Block` for raw block).
