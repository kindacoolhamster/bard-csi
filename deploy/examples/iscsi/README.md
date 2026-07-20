# Example: iSCSI backend via an out-of-tree plugin

Adds an **iSCSI** backend to Bard through the `bard-plugin-iscsi` plugin — and is
the **reference attach-style backend**: unlike Ceph RBD/LVM (which map on the
node), making an iSCSI volume reachable is a *control-plane* operation. Bard's
controller masks the volume's LUN to the staging node's initiator
(`ControllerPublishVolume`); only then can that node log in and mount it. A volume
is an LVM logical volume exported through an LIO target. Block, ReadWriteOnce,
expandable; with a thin pool also snapshots + clone, and optionally CHAP.

## Helm users: use the chart profile

If you're installing via the `charts/bard-csi` Helm chart, you can skip the
"Apply (non-Helm / raw YAML)" section below — Locality, Prerequisites, and CHAP
still apply. The chart renders the `iscsi` plugin profile natively (the same
ConfigMap/BackendCluster/sidecar wiring documented there), so there's no
`kubectl patch` to run by hand and `helm upgrade` keeps the sidecars. Minimal
values:

```yaml
attach:
  enabled: true                            # REQUIRED -- iSCSI is attach-style
plugins:
  iscsi:
    enabled: true
    instances:
      galileo: { vg: bard-vg, portal: 192.0.2.1:3260, zone: galileo, default: true }
controller:
  nodeSelector: { kubernetes.io/hostname: <target-node> }   # see Locality below
```

See [charts/bard-csi/README.md](../../../charts/bard-csi/README.md#the-plugin-model)
for the full field list, the CHAP secret shape, and the immutable-CSIDriver
note. The "Apply" section below — the raw ConfigMap/StorageClass and the
sidecar `kubectl patch` files — is the **non-Helm** path (hand-patching a
`deploy/` install), still fully supported; everything else in this file
(Locality, Prerequisites, CHAP) applies regardless of how you install.

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
targetcli) only works where it can reach the target. **Fully in-cluster works
when the target host is a cluster node**: pin the controller there
(`--set controller.nodeSelector."kubernetes\.io/hostname"=<target-node>`) and
every other node attaches over the network — proven end to end on a 2-node k3s
cluster (provision → cross-node CHAP attach → snapshot → restore, clean reap).
A control plane on a node that *isn't* the target needs remote LIO management
instead: see `management: targetd` in the chart README (an instance drives a
remote LIO host over targetd's JSON-RPC API, so the controller no longer needs
`nodeSelector`-pinning to that host — the coupling above is specific to local,
`vg`-based instances).

In-cluster **node prerequisites** (same class as every host-module gotcha):
`iscsid` running on every node (any distro's iscsi-initiator package), and on the
target node the LIO modules + `dm_snapshot` (lvm shells out to modprobe for the
snapshot target, which an in-container lvm cannot do) — `hack/setup-iscsi-fixture.sh`
loads all of them. The sidecar patches in this directory carry three hard-won
in-cluster requirements as comments: the controller pod must be `hostNetwork`
(an LIO portal binds in the netns of its creating process), it must mount the
host's `/run/dbus` (targetcli enumerates tcmu-runner over D-Bus unconditionally),
and the node plugin runs `iscsiadm` **chrooted into the host root**
(`--iscsiadm-chroot=/host`) because iscsiadm+iscsid are a version-matched pair
with distro-specific DB paths.

## Apply (non-Helm / raw YAML)

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

Snapshots are **crash-consistent at the target**: the LV is snapshotted beneath
the initiator, so writes still in the node's page cache are not included — the
same semantics as any array-side snapshot (Ceph RBD included). Quiesce or
`fsync` the workload first if you need application consistency.

## CHAP

`chapAuth: true` on an instance enforces CHAP on the data path: the target
requires authentication (`authentication=1`), ControllerPublish sets the
credentials on the node's ACL, and the node sets them on its record before
login — a wrong password is rejected by LIO. Credentials come from the
`bard-iscsi-chap` Secret (one key per instance: 2 lines userid/password, or 4
with a mutual pair), mounted into **both** plugin sidecars; they never appear in
the StorageClass, the volume context, the PublishContext (which is stored in the
API-visible VolumeAttachment), or in error messages (redacted). See the
commented Secret in [config.yaml](config.yaml).

Two accepted limitations to know about: `targetcli`/`iscsiadm` only take
credentials on the command line, so the password is briefly visible in
`/proc/<pid>/cmdline` on the controller/node host while those commands run
(inherent to the tools; the same tradeoff every iSCSI driver makes). And
**discovery is unauthenticated** — anyone who can reach the portal can list
target IQNs; CHAP + per-node ACLs gate the actual login, but keep the portal on
a network initiators belong on.

## Multipath (2+ portals)

Give an instance a `portals` list (see `config.yaml`) and every volume's target
is created with one **explicit LIO portal per address** (the catch-all default
portal -- `::0:3260` on current targetcli, `0.0.0.0:3260` historically -- is
removed so each address is a distinct path). The node plane logs in through
every portal and mounts the **multipathd-assembled mapper device** (tracked via
its `/dev/disk/by-id/dm-uuid-mpath-*` link, so the host's map-naming policy
does not matter); path failover/recovery is the host multipathd's job.
NodeUnstage flushes the map before logging out -- the authoritative "map gone"
check runs *after* logout, because a flushed map re-assembles while its paths
are still live. **Host prereq: multipathd running on every node** (same class
as iscsid); single-portal instances behave exactly as before, no multipathd
needed. Proven end to end by `hack/iscsi-multipath-test.sh` (including a
traffic-cut failover under live I/O) and in-cluster (portal-IP loss under a
running pod, online expand through the mapper, restart-then-delete leak check).

## Remote LIO management (`management: targetd`)

DONE: see `management: targetd` in the chart README (`charts/bard-csi/README.md`)
for the instance shape, the CHAP/snapshot limitations, and the controller-only
credentials Secret; `hack/targetd-plugin-test.sh` is the live proof against a
real targetd host.
