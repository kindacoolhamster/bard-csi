# Upgrading past the CSI VolumeGroupSnapshot withdrawal

Bard used to implement CSI `VolumeGroupSnapshot`. It no longer does, and the
driver no longer advertises `GROUP_CONTROLLER_SERVICE`.

The reason is a hard spec requirement. CSI v1.12 (`spec.md`, `CreateVolumeGroupSnapshot`):

> This group snapshot MUST give a write-order consistency guarantee or fail if
> that's not possible. That is to say, all of the volume snapshots in the group
> MUST be taken at the same point-in-time relative to a stream of write traffic.

Bard's implementation snapshotted each member volume in sequence — across
backend instances, in the multi-cluster case — so members could land at
different points in the write stream. That is a MUST violation, and the spec's
own remedy for "can't do this" is to fail rather than to return a group that
does not hold the guarantee.

This is unrelated to the csi-addons `VolumeGroup` operations, which are a
different API and remain supported.

**This page only matters if you actually created CSI group snapshots.** The
feature was beta and gated off by default: the `csi-snapshotter` sidecar needed
`--feature-gates=CSIVolumeGroupSnapshot=true` (Bard's chart set this) *and* the
cluster-singleton `snapshot-controller` needed the same gate, which
`hack/install-snapshotter.sh` never set for you. If you never hand-added the
gate to the cluster controller, you have no group snapshots and nothing to do.

Check before you start:

```sh
kubectl get volumegroupsnapshots.groupsnapshot.storage.k8s.io -A
kubectl get volumegroupsnapshotcontents.groupsnapshot.storage.k8s.io
```

If both are empty, upgrade normally.

## Why cleanup is manual

CSI forbids the shortcut. From the `DeleteSnapshot` section of the same spec:

> The CO SHALL NOT call this RPC with a snapshot for which SP provided a
> non-empty `group_snapshot_id` field at creation time. [...] For such snapshots
> SP MUST delete the entire snapshot group via a `DeleteVolumeGroupSnapshotRequest`
> call.

So Kubernetes will not reclaim a group member through the ordinary snapshot
path, even though Bard's members are ordinary, independently deletable backend
snapshots. Once `DeleteVolumeGroupSnapshot` is gone, the automated route is gone
with it, and the remaining work is out-of-band.

## Path A — before upgrading (strongly preferred)

While the old driver is still running, delete the group snapshots through
Kubernetes and let the retired `GroupController` do the reclaim:

```sh
kubectl delete volumegroupsnapshots.groupsnapshot.storage.k8s.io -A --all
# confirm the contents drained rather than hanging on a finalizer
kubectl get volumegroupsnapshotcontents.groupsnapshot.storage.k8s.io
```

Wait for the contents to disappear, then run Path C's orphan scan, then upgrade.

## Path B — you already upgraded

`VolumeGroupSnapshotContent` objects with `deletionPolicy: Delete` will hang on
their finalizer indefinitely. The per-driver `csi-snapshotter` sidecar is what
issues `DeleteVolumeGroupSnapshot`, and it no longer runs the group controller;
the cluster `snapshot-controller` handles binding and lifecycle but cannot
substitute for it. Re-adding the gate via `sidecars.snapshotter.extraArgs` will
not help either — the group RBAC and the driver's `GroupController` registration
are both gone.

1. Record the member handles before deleting anything. Each is an ordinary Bard
   snapshot handle (`swsk|1|<backend>|<instance>|<location>|<name>@<snap>`):

   ```sh
   kubectl get volumegroupsnapshotcontents.groupsnapshot.storage.k8s.io -o yaml \
     > /tmp/bard-group-snapshot-contents.yaml
   grep -o 'swsk|[^"]*' /tmp/bard-group-snapshot-contents.yaml | sort -u
   ```

   Keep that file. The per-member handles live under each content's `status`;
   the exact field name depends on your external-snapshotter version, which is
   why this dumps the whole object rather than a fixed JSONPath.

2. Delete the backing snapshots with the backend's own tooling, using that
   instance's credentials. For a `ceph-rbd` handle, `<location>` is the pool and
   `<name>` is `image@snap`:

   ```sh
   rbd --id <instance-user> -m <instance-mon> snap rm <pool>/<image>@<snap>
   ```

   A group can span instances, so you may need credentials for more than one
   cluster. Deleting a member does not affect other members or their sources.

3. Drop the stuck objects. Do this **after** step 2 — removing the finalizer is
   what strands the backend snapshot if you skip ahead:

   ```sh
   kubectl patch volumegroupsnapshotcontents.groupsnapshot.storage.k8s.io <name> \
     --type=merge -p '{"metadata":{"finalizers":null}}'
   kubectl delete volumegroupsnapshotcontents.groupsnapshot.storage.k8s.io <name>
   ```

   The namespaced `VolumeGroupSnapshot` objects may need the same treatment.

4. Run Path C's orphan scan to confirm nothing was left behind.

## Path C — members leaked by a *failed* group create

The retired implementation snapshotted members in sequence and returned on the
first failure **without rolling back the members it had already created**. A
group create that failed partway could therefore leave backend snapshots that
no `VolumeGroupSnapshot` object ever referenced — so neither Path A nor Path B
will find them, and they cost real space.

The consistency scanner already detects exactly this shape:

```sh
kubectl bard inspect
```

Look for `orphan-snapshot` findings — a snapshot present on the backend with no
`VolumeSnapshotContent` referencing it. Confirm with a second run to rule out an
in-flight snapshot, then remove the leftovers with the backend's own tooling as
in Path B step 2.

This check needs the backend to implement snapshot listing. The first-party Go
backends do; a targetd-managed iSCSI instance has no snapshots at all, and the
Python localpath demo plugin does not list. On a backend that cannot list, audit
by hand against the snapshot names your group snapshots used.
