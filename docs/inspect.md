# `kubectl bard inspect` — the consistency scanner

Storage drivers accumulate drift: a delete that never finished leaves an
orphaned image eating pool space, a force-failed node leaves a stale
VolumeAttachment, a re-zoned node quietly breaks provisioning. Kubernetes
can't see any of it, because Kubernetes only knows what its own objects claim.

`kubectl bard inspect` joins **three planes** and reports where they disagree:

1. **Kubernetes state** — PVs, VolumeSnapshotContents, VolumeAttachments,
   Node/CSINode topology.
2. **Bard's config** — the BackendCluster instances and zones currently
   served.
3. **Backend truth** — what actually exists on every backend, enumerated
   through each plugin's `ListVolumes`/`ListSnapshots` over the same contract
   Bard core uses.

The join key is exact: a Bard volume handle encodes
`backend|instance|location|name`, and the scanner matches it byte-for-byte
against what each plugin reports. The scan is **read-only** — every finding
carries a remediation hint, and Bard never deletes data on its own.

## Running it

```sh
go install github.com/kindacoolhamster/bard-csi/cmd/kubectl-bard@latest
kubectl bard inspect                  # controller in kube-system (static manifests)
kubectl bard inspect -n storage --selector app=my-release-bard-csi-controller
```

No Go toolchain? Each GitHub Release ships prebuilt `kubectl-bard-<os>-<arch>`
binaries (Linux, macOS, Windows) — download one, rename it to `kubectl-bard`
(`.exe` on Windows), and put it on your `PATH`; kubectl then finds it as
`kubectl bard`.

The wrapper finds the controller pod and runs `bard-csi --inspect` inside it —
the controller pod is the one place the backend plugin sockets are reachable.
No wrapper needed if you prefer raw kubectl:

```sh
kubectl -n kube-system exec deploy/bard-csi-controller -c bard-csi -- \
  /usr/local/bin/bard-csi --inspect
```

`--output json` gives the machine-readable form (add it to either command).

Exit codes: `0` consistent (or only warnings/info), `1` at least one ERROR
finding, `2` the scan could not run. That makes it cron-able: schedule it and
alert on non-zero.

## The checks

| Check | Severity | Meaning |
|---|---|---|
| `ghost-pv` | ERROR | A PV's backing volume no longer exists on the backend. The data this PV promises is gone — restores, mounts, and clones will fail. Every ghost candidate is double-checked with a direct per-volume probe where the plugin supports health, so a listing gap doesn't cry wolf. |
| `ghost-snapshot` | ERROR | A VolumeSnapshotContent's backing snapshot is gone; restores from it will fail. |
| `plugin-missing-instance` | ERROR | Core dispatches to an instance (its BackendCluster exists) but the plugin has no config for it — its volumes are invisible to the plugin and every operation on them fails. Without the probe this would misreport as a wall of ghosts; found live on this scanner's first real run. |
| `unknown-instance` | ERROR | A PV/snapshot references a backend instance that is not in the current config; delete/expand/snapshot for it will fail until the BackendCluster is restored. |
| `list-inconsistent` | WARN | A volume is missing from the plugin's list but a direct probe says it exists — the plugin's `ListVolumes` is dropping entries. |
| `orphan-volume` | WARN | A volume exists on the backend but no PV references it — leaked storage (e.g. a delete that never completed). |
| `orphan-snapshot` | WARN | Same, for snapshots. |
| `stale-attachment` | WARN | A VolumeAttachment references a node or PV that no longer exists — debris from a force-deleted node or failover. |
| `node-no-zone` | WARN/INFO | A node has no zone label. Zones are deliberately required — topology is how one StorageClass addresses many clusters — and an unlabeled node registers **no** topology keys, so provisioning for pods there fails with the cryptic `no topology key found`. WARN when the plugin already registered without topology; INFO for unlabeled nodes it hasn't reached yet. |
| `zone-collision` | WARN | A node's zone label disagrees with the topology it registered. Kubelet refuses to change a registered topology value, so the registrar will crashloop on the next node-pod restart. The hint gives the one-time recovery. |
| `zone-no-backend` | WARN | A node's zone is served by no instance of some backend type (and no default instance covers it) — PVCs for that backend can't provision there. Mirrors the dispatcher's exact fallback rules. |
| `backend-no-nodes` | INFO | An instance's zone has no nodes — its volumes are currently unmountable. Expected mid-migration. |
| `unverifiable` | INFO | PVs on a backend whose plugin doesn't implement listing can't be checked against backend truth. |
| `collect` | SKIP | A collection could not run (RBAC denied, snapshot CRDs not installed, a plugin's list failed). A degraded scan is never silent about what it didn't see. |

## Caveats

- **In-flight operations look like drift.** A volume created moments ago may
  not have its PV object yet, and a PV mid-deletion may briefly ghost. The
  scanner takes no locks and stops nothing; re-run before acting on a WARN —
  real drift persists, races don't.
- **A pool shared by several Kubernetes clusters produces cross-cluster
  "orphans".** The scanner sees one cluster's PVs, so another cluster's live
  volumes in the same pool report as `orphan-volume` here. That is why orphans
  are WARN, not ERROR, and why the hint says to check before deleting. (Found
  the hard way: the first live run of this scanner flagged three orphans — one
  was a second dev cluster's live volume in the shared pool, two were real
  leaks from clusters long deleted.)
- **Backend truth needs listing plugins.** All first-party plugins (ceph-rbd,
  cephfs, nfs, lvm, iscsi) implement it; a plugin that doesn't degrades to
  `unverifiable`, not to false findings.
- **RBAC**: the chart grants the controller ServiceAccount the read-only
  access the scanner needs unconditionally. On the static manifests the
  grants are already present. A denied read degrades to a SKIP finding.
