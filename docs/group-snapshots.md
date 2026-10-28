# CSI VolumeGroupSnapshot (ceph-rbd)

Snapshot a set of PVCs as **one cut**: every member is taken at the same point in
the write stream, and each member can still be restored on its own into a new
PVC. This is what a multi-volume application (a database with separate data and
WAL volumes, a sharded StatefulSet) needs to be restorable to a coherent state.

Bard shipped a different implementation of this API once and withdrew it. The
withdrawal, and how to clean up group snapshots it created, is
[docs/upgrade-group-snapshots.md](upgrade-group-snapshots.md); this page is about
the current one. The short version of the difference: the old implementation
snapshotted members one at a time, which cannot give the write-order consistency
guarantee CSI v1.12 makes mandatory. The current one takes a single atomic cut.

## How it works

Two Ceph primitives, not a loop:

- **`rbd group snap create`** cuts every image in an `rbd group` at one
  quiesced point. This is the write-order consistency guarantee; nothing built
  out of independent per-volume `CreateSnapshot` calls can provide it.
- **`rbd clone --snap-id`** clones an individual member of that cut. Cloning
  from a non-user-type snapshot arrived in **Ceph Squid v19.2.0**; before that,
  the members of a group snapshot were not individually restorable, which is
  exactly why the original design traded the guarantee away.

A member snapshot's CSI id therefore carries the member's **numeric rbd snap
id**, encoded as `image@#<id>`. Ceph's own name for such a snapshot
(`.group.<poolid>_<groupid>_<snapid>`) is not stable — `rbd group snap rm`
renames a still-referenced member into a trash namespace — while the id survives.

## Requirements

| | |
|---|---|
| Backend | `ceph-rbd` only. Other backends have no equivalent primitive; a group request naming one of their volumes fails `FAILED_PRECONDITION`. |
| Ceph | **Squid v19.2.0 or newer**, on the OSDs as well as the client. Bard probes this per backend instance by running the real primitive chain on a throwaway 4MiB image, and refuses the group snapshot if the probe fails. It is not a version-string check: vendor backports make versions a poor proxy, and the `rbd` binary inside Bard's own image says nothing about your cluster. |
| Scope | One backend instance. A group cannot span Ceph clusters — there is no quiesce protocol that would give one cut across independent clusters — so a request whose volumes span instances fails `FAILED_PRECONDITION`. |
| Cluster | external-snapshotter v8+ with the **group CRDs** installed, and a `snapshot-controller` run with `--feature-gates=CSIVolumeGroupSnapshot=true`. `hack/install-snapshotter.sh` installs the CRDs; the gate on the cluster controller is yours to set. |

Bard advertises the CSI `GROUP_CONTROLLER_SERVICE` **only** when a registered
backend can honour the guarantee, so on a deployment without ceph-rbd the API is
simply absent rather than present and broken.

## Enabling it

Helm (off by default; the render fails if you enable it without ceph-rbd):

```yaml
sidecars:
  snapshotter:
    groupSnapshots: true
volumeGroupSnapshotClasses:
  - name: bard-rbd-group-snapshot
    deletionPolicy: Delete
```

Raw manifests: `deploy/30-controller.yaml` already carries the sidecar feature
gate and `deploy/50-storageclass.yaml` the `VolumeGroupSnapshotClass`.

Then group your PVCs by label:

```yaml
apiVersion: groupsnapshot.storage.k8s.io/v1beta1
kind: VolumeGroupSnapshot
metadata:
  name: db-consistent
spec:
  volumeGroupSnapshotClassName: bard-rbd-group-snapshot
  source:
    selector:
      matchLabels: { app: my-db }
```

external-snapshotter materialises one `VolumeSnapshot` per member, and each
restores into a PVC the ordinary way (`dataSource` → that member snapshot).

## Known limitation: overlapping member sets

An rbd image belongs to **at most one group**. Bard keys the backing group on a
hash of the member set — deliberately not on the group snapshot's name, so that
many snapshots of the *same* PVC set over time (ordinary hourly retention) share
one group.

The consequence: two group snapshots whose member sets **overlap but differ**
cannot be live at the same time. Add a PVC to a StatefulSet and take a group
snapshot of the new label set while snapshots of the old set are still retained,
and the new request fails:

```
FAILED_PRECONDITION: volume replicapool/csi-vol-abc could not join consistency
group replicapool/csi-gsnap-... (it is held in group replicapool/csi-gsnap-...
by group snapshot(s) csi-gs-...)
```

**Failing cleanly is the correct behaviour here, not a bug.** The alternative —
silently dropping the contested volume — would hand back a group snapshot
missing a member the caller asked for, which is worse than a clear refusal. Plan
retention around it: let the older group snapshots of the previous member set
expire before group-snapshotting the new set, or keep the membership of a
group-snapshotted set stable.

Note this is about the *set*, not the count: repeatedly snapshotting the same
PVCs is fine and is the normal case.

## Deleting

Delete the `VolumeGroupSnapshot`. CSI forbids reclaiming an individual member
through the ordinary `DeleteSnapshot` — Bard refuses it explicitly — because the
members are one atomic object at the storage layer.

Removing the last group snapshot from a backing rbd group also disbands the
group; the member **volumes** are never touched. A member with a live clone (a
restored PVC) is moved to Ceph's trash namespace and auto-purged when its last
clone goes, the same lazy delete ordinary snapshot-with-clones deletion uses, so
deleting a group snapshot never breaks a PVC restored from it.
