# Example: LVM backend via an out-of-tree plugin

Adds an **LVM** backend to Bard through the `bard-plugin-lvm` plugin — no change
to Bard's code or binary. A volume is a **logical volume** carved from a host
volume group (VG); the node formats and mounts the LV's block device. Block,
ReadWriteOnce, expandable.

## Locality model — read this first

This plugin treats the VG as a **shared** storage instance: Bard's *controller*
runs `lvcreate`/`lvremove`/`lvextend`, and the node only formats + mounts the
resulting device. That is correct when every node can reach the same VG — a
single-host dev cluster (kind "nodes" are containers sharing one host VG), or a
VG on shared block storage. `Capabilities.NodeLocal` is therefore **false**.

It is deliberately **not** true node-local LVM. Real per-node LVM (an LV exists
only on the node whose disks back the VG) needs a node agent reconciling a
per-volume CRD — the TopoLVM pattern — because CSI `DeleteVolume` runs on the
controller, which cannot reach another node's VG. That is a documented follow-up.

A consequence of the shared model: the **controller** sidecar is privileged and
mounts the host `/dev`, `/run/lvm`, `/etc/lvm` (it drives device-mapper). The
network backends keep an unprivileged controller; LVM cannot.

## Prerequisites — a volume group with free extents

The plugin allocates from an existing VG; it does not create one. On a host
whose disks are full (root VG + Ceph OSDs), use the dev fixture, which carves a
fresh block device from Ceph and makes `bard-vg`:

```sh
sudo bash hack/setup-lvm-fixture.sh           # -> bard-vg (RBD-backed); teardown: ... delete
```

On a real node, point the plugin's config at whatever VG has free space.

## 1. Tell Bard about the plugin backend

Apply a `BackendCluster` for this LVM instance (adding a backend is just adding
a CR):

```yaml
apiVersion: bard.io/v1alpha1
kind: BackendCluster
metadata:
  name: galileo-lvm
spec:
  backendType: lvm
  instance: galileo
  zone: galileo
  default: true
  plugin:
    endpoint: /var/lib/bard/plugins/lvm.sock
```

## 2. Apply config/StorageClass and the sidecar patches

```sh
kubectl apply -f deploy/examples/lvm/config.yaml
kubectl -n kube-system patch deployment bard-csi-controller \
  --type=strategic --patch-file deploy/examples/lvm/sidecar-controller.patch.yaml
kubectl -n kube-system patch daemonset bard-csi-node \
  --type=strategic --patch-file deploy/examples/lvm/sidecar-node.patch.yaml
# apply the BackendCluster above and restart the bard pods
```

## 3. Use it

```sh
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata: { name: lvm-test }
spec:
  accessModes: ["ReadWriteOnce"]
  storageClassName: bard-lvm
  resources: { requests: { storage: 1Gi } }
EOF
```

The same Bard driver simultaneously serves Ceph RBD, CephFS, and NFS — one CSI
driver, many backends, all out of tree. LVM adds a **host-local block** shape.
