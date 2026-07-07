# Example: space reclamation via csi-addons (ReclaimSpace)

Reclaims unused space from a Ceph RBD volume's backing image — the csi-addons
`ReclaimSpace` operation. Bard serves the **real csi-addons gRPC API**, so a
ceph-csi user's existing `ReclaimSpaceJob`/`ReclaimSpaceCronJob` resources work
against Bard unchanged. Both phases are implemented:

- **controller (offline)** runs `rbd sparsify`, deallocating runs of zeroed
  blocks back to the pool;
- **node (online)** runs `fstrim` on the live mount, so filesystem discards reach
  the rbd image and Ceph frees the trimmed blocks.

How it fits together:

- **Bard core** serves the csi-addons Identity + `ReclaimSpaceController` services
  on a second socket in the controller pod, and the `ReclaimSpaceNode` service in
  the node DaemonSet (`--csi-addons-endpoint`, already set in
  [deploy/30-controller.yaml](../../30-controller.yaml) and
  [deploy/40-node.yaml](../../40-node.yaml)).
- A **csi-addons sidecar** in each plane (controller + node) registers a
  `CSIAddonsNode` and runs the jobs against that plane's socket.
- Only backends that implement space reclamation advertise it — today **Ceph
  RBD** (`SpaceReclaimer`). CephFS/NFS don't, and the operation is a no-op there.

## 1. Install the cluster-wide csi-addons CRDs + controller-manager

Like the snapshot-controller, this is installed once per cluster (not part of
Bard's own deploy):

```sh
kubectl apply -f https://github.com/csi-addons/kubernetes-csi-addons/releases/download/v0.12.0/crds.yaml
kubectl apply -f https://github.com/csi-addons/kubernetes-csi-addons/releases/download/v0.12.0/rbac.yaml
kubectl apply -f https://github.com/csi-addons/kubernetes-csi-addons/releases/download/v0.12.0/setup-controller.yaml
```

`rbac.yaml` is required: it creates the `csi-addons-controller-manager`
ServiceAccount + RBAC that `setup-controller.yaml`'s Deployment references. Without
it the controller-manager pod never schedules (`serviceaccount ... not found`).

## 2. Apply Bard's deploy (the sidecar + RBAC are already included)

`deploy/30-controller.yaml` carries the `csi-addons` sidecar and
`deploy/10-rbac.yaml` grants it the `csiaddonsnodes` + `reclaimspacejobs`
permissions. After `kubectl apply -f deploy/`, confirm the node registered:

```sh
kubectl get csiaddonsnode -A      # should list csi.bard.io
```

## 3. Reclaim space on a PVC

```sh
kubectl apply -f deploy/examples/reclaimspace/reclaimspacejob.yaml
kubectl describe reclaimspacejob bard-reclaim   # -> Result: Succeeded, reclaimed bytes
```

For continuous reclamation, use a `ReclaimSpaceCronJob` (same `target`, on a
schedule) or annotate the namespace/PVC with
`reclaimspace.csiaddons.openshift.io/schedule` so csi-addons creates the cron job
automatically.
