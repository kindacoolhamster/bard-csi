# Observability dogfood scenarios

This suite keeps the Bard Grafana dashboard populated with healthy CSI traffic
and two bounded failure streams, then adds inert Kubernetes objects that make
`kubectl bard inspect` demonstrate its ERROR and WARN output. It is intended for
a trusted, disposable dogfood cluster with the localpath backend. It deliberately
leaves Pending resources and scanner findings until you run `down`.

It does **not** stop plugins, alter node topology, delete backing directories,
fill a filesystem, or change Bard's backend configuration. Those would be poor
failure-injection choices on a single-node control plane.

## Run it

The cluster needs a configured `localpath` Bard backend and the metrics stack
described in [monitoring.md](../../../docs/monitoring.md). From the repository
root:

```sh
export KUBECONFIG=/path/to/cluster-kubeconfig
bash hack/observability-scenarios.sh up
bash hack/observability-scenarios.sh status
```

The recurring job runs once per minute. Give Prometheus one scrape interval,
then open the Bard dashboard with a recent time range such as **Last 15 minutes**.

## What appears in Grafana

| Scenario | Expected dashboard evidence |
|---|---|
| Stable mounted PVC | kubelet volume-stat series and normal node Stage/Publish calls |
| One-minute generic ephemeral PVC | continuing successful placement, Create/Delete, Stage/Publish, plugin latency, and sidecar operation rates |
| One-minute unsupported localpath clone | continuing non-OK CreateVolume rate and `plugin failures` with `result=invalid_argument` |
| One-minute StorageClass naming a missing backend | continuing CreateVolume error ratio and `dispatch failures` with `decision=unresolved` |

The two failure CronJobs create generic ephemeral PVCs whose consumer Pods remain
Pending until a 40-second Job deadline. A new bounded attempt runs each minute so
Prometheus observes real counter increases and the error panels do not go stale;
finished failures are TTL-cleaned. Localpath reports usage for its host filesystem
rather than enforcing the PVC's requested size, so this suite writes only a
one-megabyte churn payload and never attempts to exercise the dashboard's
nearly-full thresholds.

## What `kubectl bard` reports

Run:

```sh
bash hack/observability-scenarios.sh inspect
```

If `kubectl-bard` is not on `PATH`, the wrapper runs the same read-only scanner
inside the controller pod. It discovers a Helm-labeled controller Deployment in
`kube-system`; set `BARD_NAMESPACE` or `BARD_CONTROLLER` when more than one Bard
release could match. While the fixtures exist, expected findings include:

- `ERROR unknown-instance` for an unclaimed PV whose Bard handle names a retired
  localpath instance;
- `WARN unparseable-handle` for an unclaimed PV with a malformed handle;
- two `WARN stale-attachment` findings because one synthetic VolumeAttachment
  references both a nonexistent node and a nonexistent PV;
- the localpath backend's normal `INFO unverifiable`, because this small demo
  plugin intentionally does not implement `ListVolumes`;
- a `SKIP collect` for snapshots when snapshot CRDs are not installed.

The scanner therefore exits `1` while the ERROR fixture is installed. Both
static PVs use `Retain`, are never claimed, and do not correspond to real
backend data. The VolumeAttachment is inert because this localpath deployment
does not run an external-attacher.

## Cleanup

```sh
bash hack/observability-scenarios.sh down
```

Cleanup deletes the scenario namespace, the two StorageClasses, both synthetic
PVs, and the synthetic VolumeAttachment by exact name. It also waits for any
dynamic PVs recorded for the namespace to disappear through their normal
`Delete` reclaim policy. The unrelated `bard-dogfood` smoke workload is not
touched. Before applying or deleting anything, the wrapper verifies that every
existing fixed-name object carries the suite's ownership annotation; a collision
with an unrelated object aborts the entire operation.
