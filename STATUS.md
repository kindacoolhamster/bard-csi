# Status & roadmap

Feature inventory for bard-csi and the backends it ships. See the root
[README.md](README.md) for architecture and how to deploy.

## Implemented

The **Ceph RBD** backend is the most complete and carries most of the depth below;
the other backends implement the subset that fits their storage shape. Implemented:
provisioning, delete, attach-on-node (map/format/mount), bind
publish, raw block (`volumeMode: Block`), snapshots, clone-from-snapshot,
volume-group snapshots (`VolumeGroupSnapshot`, can span multiple instances),
`ListVolumes` / `ListSnapshots` (aggregated + paginated across backends),
control-plane attach (`ControllerPublishVolume`/`Unpublish` with the
external-attacher, opt-in -- the iSCSI backend uses it for per-node LUN masking;
node-mapped backends no-op it),
CephFS shallow read-only volumes (`backingSnapshot` -- mount a snapshot ROX with
no clone),
controller + node expand, `NodeGetVolumeStats`, `GetCapacity` (CSIStorageCapacity),
ReadWriteOncePod, RBD image-feature/stripe/object-size params, erasure-coded
backing pool (`dataPool`), custom object-name prefixes
(`volumeNamePrefix`, `snapshotNamePrefix`, `volumeGroupNamePrefix`), per-instance
cluster-name image metadata (`clusterName` -> ceph-csi's
`csi.ceph.com/cluster/name` key), rbd-nbd client-log control
(`cephLogDir`/`cephLogStrategy`), thin-rbd-tuned mkfs defaults with a
`mkfsOptions` override, krbd->rbd-nbd mounter fallback (`tryOtherMounters`),
volume health monitoring
(`ControllerGetVolume` + `VOLUME_CONDITION`, surfaced by the external-health-monitor
sidecar), single-writer fencing (blocklist a stale writer from a prior node on
ReadWriteOnce failover), space reclamation (`ReclaimSpace` via the csi-addons API
-> `rbd sparsify` on the controller + `fstrim` on the node), node network fencing
(`NetworkFence` via the csi-addons API -> `osd blocklist range`, plus
`GET_CLIENTS_TO_FENCE` to discover the client to fence -- for failover/DR),
RBD mirroring/DR (`VolumeReplication` via the csi-addons API -> snapshot-based
`rbd mirror image` enable/promote/demote/resync, for cross-cluster DR -- proven
end to end as a two-cluster Ramen-style failover: demote on cluster A, promote on
cluster B's own Bard, app comes up on the mirrored data intact; snapshot-restored
clones mirror hands-free -- Bard auto-flattens them out of band and the
VolumeReplication converges on retry, `flattenMode: never` opts out),
consistency groups (`VolumeGroup` via the csi-addons API -> `rbd group`
create/modify/delete/get/list; the building block for group replication, and
deleting a group never deletes its member volumes),
encryption key rotation (`EncryptionKeyRotation` via the csi-addons API -> rotate a
LUKS volume's keyslot to fresh KMS-minted material without re-encrypting the data),
`ModifyVolume` (VolumeAttributesClass -> live rbd QoS; on CephFS -> MDS pinning,
`pinType`/`pinSetting` -> `ceph fs subvolume pin`),
cross-namespace volume data sources (restore a PVC from another namespace's
VolumeSnapshot, authorized by Gateway API ReferenceGrants -- the alpha
CrossNamespaceVolumeDataSource gate + provisioner flag; see
deploy/examples/crossnamespace/),
encryption-at-rest -- block-level LUKS (dm-crypt, with optional cipher/key-size/
sector-size tuning and dm-integrity authenticated encryption) by default or
filesystem-level
fscrypt, selected per-volume via `encryptionType` -- with pluggable KMS providers
(a per-instance derived master key by default, HashiCorp Vault, Kubernetes
secrets-metadata -- a per-volume random key wrapped into the image metadata -- AWS
KMS envelope encryption with static keys or STS web-identity/IRSA auth, Azure Key
Vault, or KMIP for an on-prem HSM / key manager, selected per-volume via
`encryptionKMSID`; both encryption modes use the same KMS
providers), topology dispatch across instances, `BackendCluster` CRD
config with a live watch (re-point a zone / change the default / add-remove an
instance with no restart).

The Vault provider authenticates with a static token or, preferably, the plugin's
ServiceAccount via Vault Kubernetes auth; the AWS KMS provider does envelope
encryption (the data key is wrapped by a KMS key and only the ciphertext is stored,
in the image metadata) and is stdlib-only (SigV4 + the KMS JSON API, no AWS SDK), so
it points at a VPC endpoint or a local emulator via an `endpoint` override -- and
authenticates either with static keys or, on EKS, by exchanging the plugin's projected
ServiceAccount token for an IAM role over STS web-identity (`aws-sts-metadata`, the IRSA
path, so no static AWS keys are stored); the Azure
Key Vault provider stores the per-volume passphrase as a Key Vault secret over AAD
client-credentials auth (stdlib REST + OAuth, no Azure SDK); the KMIP provider
stores the passphrase as a SecretData object on an HSM / key manager over mutual TLS
(the one provider that pulls a dependency -- TTLV is a binary protocol -- adding ~1%
to the image); a deleted volume's key is removed from the KMS. Encryption composes
with snapshots: a
snapshot or clone of an encrypted volume inherits the source's key (the clone copies
the LUKS header byte-for-byte), so a restored PVC opens with the source's passphrase
while owning an independent key entry -- deleting the clone never touches the
source's key.

The **CephFS** backend places subvolumes in a configurable **subvolume group**
(`subvolumeGroup` StorageClass param or per-instance config, default `csi` for
ceph-csi parity -- the group rides in the volume handle, so a migrated StorageClass
can adopt existing ceph-csi subvolumes under group `csi`), and supports the same
custom name prefixes (`volumeNamePrefix`, `snapshotNamePrefix`). It also supports
encryption-at-rest, using the same pluggable KMS
providers. CephFS has no block device, so it is filesystem-level fscrypt only (a
kernel-mounter requirement): a marked volume gets an fscrypt-encrypted data directory
inside its subvolume, keyed by a KMS passphrase, so Ceph stores only ciphertext
(file contents and names). The KMS layer and fscrypt helpers are shared with RBD
(`internal/cephenc`), so every KMS provider works via the subvolume metadata store.

The **iSCSI** backend is the reference **attach-style** backend: a volume is an
LVM logical volume exported through an LIO target, and making it reachable is a
control-plane operation (`ControllerPublishVolume` masks the LUN to the staging
node's initiator IQN) rather than a node-side map. Beyond block/ReadWriteOnce/
expand it supports thin-pool snapshots and clone-from-snapshot,
`ListVolumes`/`ListSnapshots`, optional per-instance CHAP (a 2-line
userid/password Secret, or 4 lines with a mutual pair, read by both planes and
never present in the ConfigMap, StorageClass, or PublishContext), and
**dm-multipath** -- an instance `portals` list yields explicit per-address LIO
portals, and the node logs in through every portal and mounts the
multipathd-assembled mapper by its `dm-uuid-mpath-<wwid>` link, with failover
proven under live I/O. Management is local targetcli/configfs by default, or
`management: targetd` to drive a **remote** LIO host over targetd's JSON-RPC
API, so the controller no longer has to run on the target host.

## Not yet

On the CephFS backend, **encrypted volumes cannot be restored from a snapshot or
cloned** -- CephFS subvolume clone does not preserve the fscrypt context (unlike RBD's
block-level clone, which copies the LUKS header byte-for-byte); the combination is
rejected rather than producing an unmountable volume.
Also: true node-local LVM (TopoLVM-style node agent + per-volume CRD; today's LVM
plugin is shared-VG) and a cloud-disk backend.

On a **targetd-managed** iSCSI instance (`management: targetd`), snapshots,
clone, and CHAP are unavailable -- and are rejected explicitly rather than
silently ignored. targetd's `vol_copy` is a synchronous full copy (unsafe under
provisioner retries), and its `export_create` hardcodes the shared target's TPG
`authentication` attribute to `"0"` with no API to override it, so CHAP
credentials are never actually enforced; access control on a targetd instance is
IQN-based ACLs only. Local (targetcli) management supports all three.
