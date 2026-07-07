# Example: CephFS backend via an out-of-tree plugin

Adds a **CephFS** backend (a shared, ReadWriteMany filesystem) to Bard through
the `bard-plugin-cephfs` plugin — no change to Bard's code or binary. A volume is
a CephFS *subvolume* (a quota'd directory tree); there is no block device or
format, just a mount, so it exercises a very different shape than the Ceph RBD
plugin.

## Prerequisites (on the Ceph cluster)

```sh
ceph fs volume create bardfs                 # creates pools + deploys an MDS
ceph auth get-or-create client.k8s-cephfs \
  mon 'allow r' mgr 'allow rw' osd 'allow rw' mds 'allow rw'   # dev caps
ceph auth get-key client.k8s-cephfs          # -> the bard-cephfs-keys Secret
```

## 1. Tell Bard about the plugin backend

Apply a `BackendCluster` for this CephFS instance (no edit to existing config —
adding a backend is just adding a CR):

```yaml
apiVersion: bard.io/v1alpha1
kind: BackendCluster
metadata:
  name: galileo-cephfs
spec:
  backendType: cephfs
  instance: galileo
  zone: galileo
  default: true
  plugin:
    endpoint: /var/lib/bard/plugins/cephfs.sock
```

## 2. Apply config/StorageClass, the keys Secret, and the sidecar patches

```sh
kubectl apply -f deploy/examples/cephfs/config.yaml
kubectl apply -f hack/secret-cephfs.yaml      # bard-cephfs-keys (untracked; see *.example.yaml)
kubectl -n kube-system patch deployment bard-csi-controller \
  --type=strategic --patch-file deploy/examples/cephfs/sidecar-controller.patch.yaml
kubectl -n kube-system patch daemonset bard-csi-node \
  --type=strategic --patch-file deploy/examples/cephfs/sidecar-node.patch.yaml
# re-apply the patched bard-csi-config and restart the bard pods
```

## 3. Use it (ReadWriteMany)

```sh
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata: { name: cephfs-test }
spec:
  accessModes: ["ReadWriteMany"]
  storageClassName: bard-cephfs
  resources: { requests: { storage: 1Gi } }
EOF
```

Multiple pods can mount this PVC at once and share files — that is the headline
CephFS capability. The same Bard driver simultaneously serves Ceph RBD (block,
RWO) via `bard-rbd`: one CSI driver, many backends, all out of tree.

## NFS transport (mounter: nfs)

The same plugin can serve a CephFS subvolume over **NFS** instead of the native
ceph client (a CephFS subvolume exported through NFS-Ganesha). Because the volume is
still a subvolume, **snapshots, clone, and expand work identically** whether it is
mounted natively or over NFS.

Set `mounter: nfs` on the instance plus `nfsCluster`/`nfsServer` (see
[config.yaml](config.yaml)). On the Ceph side, stand up a Ganesha gateway once:

```sh
ceph nfs cluster create bard-nfs    # deploys NFS-Ganesha via the mgr/cephadm
```

CreateVolume then also runs `ceph nfs export create cephfs ...` for each volume
and the node mounts `<nfsServer>:/<subvolume>` over NFS — no ceph client or cephx
key required on the node. Use it when nodes can't run the ceph kernel/fuse client
but can reach an NFS gateway.

## Shallow read-only volumes (backingSnapshot)

A `ReadOnlyMany` PVC restored from a CephFS snapshot can mount the snapshot's
`.snap/<snap>` directory **directly** — no clone, no data copy, instant — via the
`bard-cephfs-shallow` StorageClass (`backingSnapshot: "true"`). Many PVCs can
share one snapshot cheaply, e.g. fan-out read replicas of a dataset:

```sh
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata: { name: cephfs-ro }
spec:
  accessModes: ["ReadOnlyMany"]
  storageClassName: bard-cephfs-shallow
  resources: { requests: { storage: 1Gi } }
  dataSource:
    name: my-cephfs-snapshot          # an existing VolumeSnapshot
    kind: VolumeSnapshot
    apiGroup: snapshot.storage.k8s.io
EOF
```

The shallow volume owns no subvolume, so deleting the PVC leaves the snapshot and
its source untouched. (A normal `bard-cephfs` restore still does a full clone, so
the restored PVC is independent and writable.)
