# bard-csi Helm chart

Deploys the **Bard CSI driver runtime**: the backend-agnostic core (controller +
node), the CSI sidecars, and the backend **plugin sidecars** you enable.

## What this chart owns vs. what you own

**The chart owns** the driver runtime: core, CSI sidecars (provisioner, resizer,
snapshotter, health-monitor, registrar, liveness-probe), plugin sidecar wiring,
RBAC, the `BackendCluster` CRD, the `CSIDriver` object, and (optional) the
StorageClass / VolumeSnapshotClass / VolumeAttributesClass / VolumeGroupSnapshotClass.

For a **first-party backend** (ceph-rbd, cephfs) you describe it in its own terms
(mons, pool/fsName, user, zone) under `plugins.<backend>.instances`, and the chart
generates the plugin config ConfigMap, the `BackendCluster` CRs, and all the sidecar
wiring (args, mounts, host flags). **You own** only:

- **Credentials** — the backend's keys (for the Ceph backends, cephx keys, plus any
  LUKS master keys / Vault tokens) — in Secrets you create; the chart references them
  by name, never their content. One key per instance id (e.g. in `bard-ceph-keys`).
- **The network** between the driver and the backend.

(Advanced / custom plugins use the low-level override path and additionally own the
config ConfigMap + `BackendCluster` CRs — see "The plugin model".)

This split keeps core backend-agnostic and lets one StorageClass span many
backend instances/zones.

## Install

```sh
# 1. (prereq) external-snapshotter — the snapshotter sidecar is ON by default
#    (disable with --set sidecars.snapshotter.enabled=false). It is a cluster
#    singleton this chart deliberately does NOT bundle; install its CRDs + a
#    snapshot-controller pinned to the SAME version the sidecar uses (v8.2.0):
#      V=v8.2.0; B=https://raw.githubusercontent.com/kubernetes-csi/external-snapshotter/$V
#      for c in snapshot.storage.k8s.io_volumesnapshot{classes,contents,s} \
#               groupsnapshot.storage.k8s.io_volumegroupsnapshot{classes,contents,s}; do
#        kubectl apply -f "$B/client/config/crd/$c.yaml"; done
#      kubectl apply -f "$B/deploy/kubernetes/snapshot-controller/rbac-snapshot-controller.yaml"
#      kubectl apply -f "$B/deploy/kubernetes/snapshot-controller/setup-snapshot-controller.yaml"
#      # upstream's manifest MISPINS the controller image to v8.0.1, which — with the
#      # group-snapshot gate the sidecar sets — stalls even PLAIN snapshots; force it:
#      kubectl -n kube-system set image deploy/snapshot-controller \
#        snapshot-controller=registry.k8s.io/sig-storage/snapshot-controller:$V
#    (from a source checkout, hack/install-snapshotter.sh does exactly this.)

# 2. create the credentials Secret (one cephx key per instance id)
kubectl -n kube-system create secret generic bard-ceph-keys \
  --from-literal=galileo="$(ceph auth get-key client.k8s-csi-test)"

# 3. install — the released chart + images pull straight from the registry:
helm install bard-csi oci://ghcr.io/kindacoolhamster/charts/bard-csi \
  --version 0.1.0-rc.2 -n kube-system -f my-values.yaml   # see Releases for the current version
#    (From a source checkout, `helm install bard-csi charts/bard-csi …` uses the
#     dev-PLACEHOLDER appVersion, whose images are NOT published → ImagePullBackOff;
#     build/load your own images, or --set image.tag / plugins.<backend>.image.tag.)

# 4. label your nodes per zone: kubectl label node <n> topology.kubernetes.io/zone=<zone>
```

## The plugin model

`plugins` is a **map keyed by backend type**. There are two ways to configure one.

### (A) High-level — first-party backends (recommended)

Describe the backend in **its own terms** under `instances`; the chart fills in all
Bard-internal plumbing (image, socket, args, mounts, host flags) from a built-in
profile, and renders the config ConfigMap **and** a `BackendCluster` per instance.
You supply only `enabled`, `keysSecret` (the Secret you created), and `instances`:

```yaml
plugins:
  ceph-rbd:
    enabled: true
    keysSecret: bard-ceph-keys             # one cephx key per instance id
    instances:
      galileo:
        monitors: ["192.0.2.1:3300"]
        pool: k8s-csi-test
        user: k8s-csi-test
        zone: galileo                      # defaults to the instance id
        default: true
        # mounter: rbd-nbd                  # krbd (default) preferred on real nodes
    # optional: encryption: { masterKeySecret: bard-ceph-encryption }
    # optional: kms: { configMap: bard-ceph-kms, vaultTokenSecret: bard-ceph-vault-token }
  cephfs:
    enabled: true
    keysSecret: bard-cephfs-keys
    instances:
      galileo: { monitors: ["192.0.2.1:3300"], fsName: bardfs, user: k8s-cephfs, zone: galileo, default: true }
    # optional: encryption/kms as above (fscrypt-at-rest; same KMS providers as ceph-rbd)
```

Per-instance fields: **ceph-rbd** `monitors[], pool, user, zone, default,
[mounter], [readAffinity], [radosNamespace], [clusterName]`; **cephfs** `monitors[], fsName,
user, zone, default, [mounter: kernel|fuse|nfs], [subvolumeGroup], [nfsCluster],
[nfsServer]`. Both Ceph backends take the optional `encryption:`/`kms:` blocks;
`--kms-config` is emitted **only** when a `kms:` block is present, so a no-KMS
install never references a missing file.

### (B) Low-level override — custom plugins / full control

OMIT `instances` and specify the wiring yourself; a plugin with `instances` ignores
these fields, and a plugin without them uses exactly what you give (the escape
hatch). You also own its config ConfigMap (via `configMaps:`) and `backendClusters`.

```yaml
plugins:
  my-plugin:
    enabled: true
    image: { repository: ghcr.io/me/bard-plugin-x, tag: "" }
    socket: x.sock                          # under the shared /var/lib/bard/plugins
    backendClusters: [ { name: x-east, instance: east, zone: east, default: true } ]
    controller:
      enabled: true
      args: ["--config=/etc/x/config.yaml"]
      volumes: [ { name: config, mountPath: /etc/x, readOnly: true, configMap: { name: x-config } } ]
    node:
      enabled: true
      privileged: true       # map block devices
      hostDev: true          # mount host /dev
      kubeletDir: true        # Bidirectional mount into the kubelet dir
      hostNetwork: true      # reach the backend on the host net (pod-level)
      hostPID: true          # stable ns for mapper daemons (pod-level)
      args: [...]
      volumes: [...]
```

`--socket` is injected from `socket`; `plugins`, `/dev`, and the kubelet dir are
chart-managed and referenced by flag; your `volumes` mount namespaced as
`<plugin>-<vol>`. **Pod-level host flags** (`hostNetwork`, `hostPID`) are OR-ed
across all enabled node plugins, since they are pod-wide.

## Key values

| Key | Default | Notes |
|---|---|---|
| `image.repository` / `image.tag` | core image / `appVersion` | Bard core |
| `storageCapacity` | `true` | CSIStorageCapacity (CSIDriver + provisioner + RBAC) |
| `zoneLabel` | `topology.kubernetes.io/zone` | node label the driver reads for topology |
| `sidecars.*.enabled` / `.image` | on / pinned | RBAC tracks what's enabled |
| `sidecars.snapshotter.groupSnapshots` | `true` | adds the group-snapshot gate + RBAC |
| `sidecars.csiAddons.enabled` | `false` | csi-addons ops: ReclaimSpace, NetworkFence, VolumeReplication (mirroring/DR), VolumeGroup, EncryptionKeyRotation (sidecar + endpoint + RBAC); needs the csi-addons controller installed separately |
| `attach.enabled` | `false` | control-plane attach (iSCSI): flips the CSIDriver's `attachRequired` (immutable) + adds the external-attacher + RBAC. Node-mapped backends no-op it |
| `node.kubeletDir` | `/var/lib/kubelet` | override for non-standard distros |
| `plugins` | ceph-rbd + cephfs profiles (disabled) | the backend plugins to run |
| `plugins.<backend>.instances` | `{}` | high-level: backend-native config per instance; generates the config ConfigMap + a BackendCluster (zone→instance) each |
| `plugins.<backend>.keysSecret` | `bard-<backend>-keys` | name of the credentials Secret you create (one key per instance id; cephx for the Ceph backends) |
| `storageClasses` / `volume*Classes` | `[]` | optional; off by default |

## Notes & caveats

- **The Ceph backends need kernel modules on every node.** The default Ceph RBD
  mounter (krbd) maps images through the node kernel, so each node must have the
  **`rbd`** module loaded (`modprobe rbd`; persist in `/etc/modules-load.d/`) —
  without it NodeStage fails and the PVC never mounts. Feature-conditional, same
  treatment: **`nbd`** for the `mounter: rbd-nbd` alternative, **`dm_crypt`** for
  LUKS encryption (`encrypted: "true"`), and **`ceph`** for the CephFS kernel
  mounter.
- **CSIDriver and the ClusterRoles are cluster-scoped singletons** keyed by the
  driver name — one bard-csi per cluster.
- **CRD**: the `BackendCluster` CRD ships in `crds/` (Helm installs it on first
  install but does not upgrade/delete it — manage CRD upgrades deliberately).
- **Validate before installing**: `helm lint charts/bard-csi` and
  `helm template ... | kubectl apply --dry-run=server -f -`.
