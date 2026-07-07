# Example: RBD mirroring / DR via csi-addons (VolumeReplication)

Mirrors a `bard-rbd` volume to a second Ceph cluster for disaster recovery — the
csi-addons **VolumeReplication** operation, backed by **snapshot-based RBD
mirroring** (`rbd mirror image ...`). Bard serves the real csi-addons Replication
API, so a ceph-csi user's existing `VolumeReplication` / `VolumeReplicationClass`
resources (and a Ramen DR setup) work against Bard unchanged.

Six operations, all volume-scoped and dispatched to the owning backend:

| VolumeReplication state / op | rbd |
|---|---|
| EnableVolumeReplication | `rbd mirror image enable <img> snapshot` + `mirror snapshot schedule add` |
| PromoteVolume (`primary`) | `rbd mirror image promote [--force]` |
| DemoteVolume (`secondary`) | `rbd mirror image demote` |
| ResyncVolume (`resync`) | `rbd mirror image resync` |
| DisableVolumeReplication | `rbd mirror image disable` |
| GetVolumeReplicationInfo | latest complete mirror-snapshot time (RPO) |

The per-instance provisioning user (`mon 'profile rbd'`) already has the caps for
every mirror op (verified), so — unlike NetworkFence — no special fence/mirror user
is required.

**Snapshot-restored volumes (COW clones) mirror automatically.** rbd refuses to
enable snapshot-based mirroring on a clone whose parent is not mirrored; Bard
detects that, flattens the clone **in the background** (deduplicated across
retries), and fails the attempt with a clear message — the csi-addons controller's
next reconcile finds a parent-free image and the enable succeeds, so the
VolumeReplication converges to `Primary` hands-free. The reconcile is never blocked
on a multi-minute data copy (ceph-csi requires a `flattenMode: force` class knob
that flattens inline, or fails outright). Set `flattenMode: never` on the
VolumeReplicationClass to opt out and surface the raw error instead.

## 1. Cluster prerequisites (one-time, admin)

**Pool peering is set up by the Ceph admin, not by Bard.** On both clusters' pool:

```sh
# On each cluster:
rbd mirror pool enable <pool> image           # image-mode (per-image opt-in)
# Bootstrap a peer token on the primary and import it on the secondary:
#   primary:   rbd mirror pool peer bootstrap create --site-name <a> <pool>
#   secondary: rbd mirror pool peer bootstrap import --site-name <b> --direction rx-only <pool> <token-file>
# Run an rbd-mirror daemon on the secondary:
#   ceph orch apply rbd-mirror --placement=<host>
```

Also install the cluster-wide csi-addons CRDs + controller-manager (same as the
[ReclaimSpace](../reclaimspace/) / [NetworkFence](../networkfence/) examples) — the
`replication.storage.openshift.io` CRDs ship in the csi-addons `crds.yaml`.

## 2. Bard

Bard's controller-side csi-addons sidecar + the `volumereplications` RBAC are wired
([deploy/10-rbac.yaml](../../10-rbac.yaml)); enable the sidecar with the chart
(`sidecars.csiAddons.enabled=true`). VolumeReplication is advertised only when a
backend that can mirror (ceph-rbd) is registered.

## 3. Mirror a volume

```sh
# Edit volumereplicationclass.yaml: set schedulingInterval (RPO) and the dataSource
# PVC, then apply.
kubectl apply -f deploy/examples/mirroring/volumereplicationclass.yaml

# Bard enables mirroring + a snapshot schedule, and the image starts replicating to
# the peer cluster. Check status / RPO:
kubectl get volumereplication bard-rbd-replication \
  -o jsonpath='{.status.state}{"  lastSync="}{.status.lastSyncTime}{"\n"}'

# On the secondary Ceph cluster the image appears and reports up+replaying:
#   rbd -p <pool> ls ; rbd -p <pool> mirror image status <img>
```

## Two-cluster failover runbook

A real failover flips `replicationState` across **two** Kubernetes clusters -- each
with its own Bard install pointing at its site's Ceph, and csi-addons in both. Ramen
orchestrates these flips in production; the steps below are the manual equivalent and
are proven end to end (both a planned relocate and the CR sequence). All storage ops
are driven by `VolumeReplication` CRs -> Bard -> `rbd mirror image ...`.

Terms: **cluster A** = current primary (app runs here, its Ceph is the mirror source);
**cluster B** = DR standby (its Ceph is the replica; the rbd-mirror daemon runs here).

### 0. Prerequisites (one-time)

- Bard installed in **both** clusters. In cluster B, Bard's config points at cluster
  B's Ceph as an instance (see [deploy/20-config.yaml](../../20-config.yaml)); label
  cluster B's nodes for that zone.
- Pool mirroring + peer bootstrapped and an `rbd-mirror` daemon on cluster B (section
  1 above).
- csi-addons CRDs + controller-manager in both clusters, and Bard's csi-addons sidecar
  enabled (`sidecars.csiAddons.enabled=true`) so each Bard registers a CSIAddonsNode.

### 1. Steady state (cluster A)

The app runs on cluster A with a `bard-rbd` PVC and a `VolumeReplication`
(`replicationState: primary`, this file). Bard enables snapshot mirroring + a
schedule; the image replicates to cluster B, which reports `up+replaying`.

### 2. Fail over

**Planned relocate (A reachable) -- demote A, then promote B:**

```sh
# --- cluster A ---: stop the app, then demote (Bard takes a final mirror snapshot).
kubectl -n <ns> scale <workload> --replicas=0
kubectl -n <ns> patch volumereplication <name> --type=merge \
  -p '{"spec":{"replicationState":"secondary"}}'
# Wait until cluster B's replica reports the remote image is no longer primary:
#   rbd -p <pool> mirror image status <img>   # "remote image is not primary"
```

```sh
# --- cluster B ---: bind a STATIC PV to the mirrored image (same image name, cluster
# B's instance in the handle), then a VolumeReplication(primary) promotes it via Bard.
```
```yaml
apiVersion: v1
kind: PersistentVolume
metadata: { name: failover-pv }
spec:
  capacity: { storage: 1Gi }
  accessModes: ["ReadWriteOnce"]
  storageClassName: ""
  csi:
    driver: csi.bard.io
    # handle: swsk|1|ceph-rbd|<clusterB-instance>|<pool>|<image>  (same <image> as A)
    volumeHandle: "swsk|1|ceph-rbd|site-b|k8s-csi-test|csi-vol-XXXX"
    fsType: ext4
  claimRef: { namespace: <ns>, name: failover-pvc }
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: { name: failover-pvc, namespace: <ns> }
spec:
  accessModes: ["ReadWriteOnce"]
  storageClassName: ""
  volumeName: failover-pv
  resources: { requests: { storage: 1Gi } }
---
# A VolumeReplicationClass WITHOUT schedulingInterval (this image is not primary yet;
# a schedule add would run before the promote). It MUST carry the replication-secret
# params or the controller-manager errors "resource name may not be empty".
apiVersion: replication.storage.openshift.io/v1alpha1
kind: VolumeReplicationClass
metadata: { name: bard-rbd-replication }
spec:
  provisioner: csi.bard.io
  parameters:
    replication.storage.openshift.io/replication-secret-name: bard-ceph-keys
    replication.storage.openshift.io/replication-secret-namespace: kube-system
---
apiVersion: replication.storage.openshift.io/v1alpha1
kind: VolumeReplication
metadata: { name: failover-repl, namespace: <ns> }
spec:
  volumeReplicationClass: bard-rbd-replication
  replicationState: primary          # Bard PromoteVolume -> rbd mirror image promote
  dataSource: { apiGroup: "", kind: PersistentVolumeClaim, name: failover-pvc }
```

```sh
# --- cluster B ---: wait for the promote, then start the app against failover-pvc.
kubectl -n <ns> get volumereplication failover-repl \
  -o jsonpath='{.status.state}{"  "}{.status.conditions[?(@.type=="Completed")].reason}{"\n"}'
#   -> Primary  Promoted   (rbd info shows "mirroring primary: true")
```

**Unplanned failover (A is gone):** skip the cluster-A demote and force-promote on B --
set `--force` on the promote (a `force-promote` intent on the VR); this creates two
primaries (split-brain), expected when A is assumed dead.

### 3. Fail back / repair split-brain

When the old primary A returns, **resync** it from the new primary B
(`replicationState: resync` on A's VolumeReplication -> `rbd mirror image resync`),
then relocate back with the same demote/promote sequence in the other direction.
