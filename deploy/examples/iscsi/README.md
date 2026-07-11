# Example: iSCSI backend via an out-of-tree plugin

Adds an **iSCSI** backend to Bard through the `bard-plugin-iscsi` plugin — and is
the **reference attach-style backend**: unlike Ceph RBD/LVM (which map on the
node), making an iSCSI volume reachable is a *control-plane* operation. Bard's
controller masks the volume's LUN to the staging node's initiator
(`ControllerPublishVolume`); only then can that node log in and mount it. A volume
is an LVM logical volume exported through an LIO target. Block, ReadWriteOnce,
expandable; with a thin pool also snapshots + clone, and optionally CHAP.

## Per-node LUN masking — why this needs attach

`ControllerPublish` adds an ACL for the node's initiator IQN; `ControllerUnpublish`
removes it. Without the ACL the node's login is **rejected**, so the single-writer
guarantee holds at the iSCSI transport, not just in Kubernetes (an open/shared
target would let any node see every LUN — a data-corruption and security hole).
The node's initiator IQN is derived deterministically from its CSI node id, so the
controller (which sets the ACL) and the node (which logs in) agree with no lookup;
the node logs in under a dedicated iscsiadm iface, never touching the host's global
initiatorname.

## Prerequisites

1. **Turn on control-plane attach** (this is the only backend that needs it):
   see [../attach/](../attach/) — it flips the CSIDriver's `attachRequired` and adds
   the external-attacher. (Helm: `--set attach.enabled=true`.)
2. **The host LIO target + a VG** the LUNs are carved from:
   ```sh
   sudo bash hack/setup-iscsi-fixture.sh     # LIO + bard-vg + iscsid; teardown: ... delete
   ```

## Locality (read this) — same host-coupling as LVM

LIO lives in the host kernel's configfs, so the **control plane** (lvcreate +
targetcli) only works where it can reach the target — exactly the shared-host
constraint the LVM plugin documents. The production-correct logic, **including
per-node ACL masking**, is proven by driving the plugin binary over its socket
against a real target (`hack/iscsi-plugin-test.sh`), and the **node plane** (real-
kernel iSCSI login over the network → `/dev/sdX` → mount) is proven from a k3s VM
(see CLAUDE.md). A full in-cluster control plane on a node that *isn't* the target
needs remote LIO management (e.g. `targetd`) — a documented follow-up.

## Apply

```sh
kubectl apply -f deploy/examples/iscsi/config.yaml   # ConfigMap + BackendCluster + StorageClass
kubectl -n kube-system patch deployment bard-csi-controller \
  --type=strategic --patch-file deploy/examples/iscsi/sidecar-controller.patch.yaml
kubectl -n kube-system patch daemonset bard-csi-node \
  --type=strategic --patch-file deploy/examples/iscsi/sidecar-node.patch.yaml
# restart the bard pods to pick up the sidecars
```

## Use it

```sh
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata: { name: iscsi-test }
spec:
  accessModes: ["ReadWriteOnce"]
  storageClassName: bard-iscsi
  resources: { requests: { storage: 1Gi } }
EOF
```

A pod using the PVC triggers: provision (LV + LIO target) → attach (ACL for the
scheduled node) → login + mount on that node. Deleting it detaches (ACL removed)
and reaps the target + backstore + LV.

## Snapshots and clone (thin pool)

Set `thinPool` on the instance (or as a StorageClass `thinPool` parameter) and
volumes become copy-on-write thin LVs, exactly like the LVM plugin: a
VolumeSnapshot makes a read-only thin snapshot (a control-plane object — it gets
no LIO export), and restore/clone makes a writable thin snapshot grown to the
requested size, exported through its own target. The pool is pre-created once:
`lvcreate --type thin-pool -L 20G -n bard-thin <vg>`. Snapshot/clone of a thick
(no-pool) volume is rejected. Needs the external-snapshotter cluster singleton,
same as every other backend (`hack/install-snapshotter.sh`).

## CHAP

`chapAuth: true` on an instance enforces CHAP on the data path: the target
requires authentication (`authentication=1`), ControllerPublish sets the
credentials on the node's ACL, and the node sets them on its record before
login — a wrong password is rejected by LIO. Credentials come from the
`bard-iscsi-chap` Secret (one key per instance: 2 lines userid/password, or 4
with a mutual pair), mounted into **both** plugin sidecars; they never appear in
the StorageClass, the volume context, or the PublishContext (which is stored in
the API-visible VolumeAttachment). See the commented Secret in
[config.yaml](config.yaml).

## Not yet (follow-ups)

Multipath, and remote LIO management (`targetd`) for a fully in-cluster control
plane on a node that isn't the target host.
