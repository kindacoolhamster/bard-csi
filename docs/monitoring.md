# Monitoring Bard CSI

Bard exposes Prometheus metrics from the core (controller and node), and can wire
up the CSI sidecars' own metrics and ship a Grafana dashboard. Everything here is
off by default.

```sh
helm upgrade --install bard-csi oci://ghcr.io/kindacoolhamster/charts/bard-csi \
  -n kube-system --version <version> \
  --set metrics.enabled=true \
  --set metrics.serviceMonitor.enabled=true \
  --set metrics.podMonitor.enabled=true \
  --set metrics.dashboard.enabled=true
```

The `serviceMonitor`/`podMonitor` options need the prometheus-operator CRDs. The
dashboard option needs a Grafana configured to load dashboards from ConfigMaps
labelled `grafana_dashboard: "1"`; otherwise import
`charts/bard-csi/dashboards/bard-csi.json` by hand.

## What Bard emits

| Metric | Type | Labels | What it answers |
|---|---|---|---|
| `bard_csi_grpc_requests_total` | counter | `method`, `code` | CSI RPC rate and error rate |
| `bard_csi_grpc_request_duration_seconds` | histogram | `method` | RPC latency |
| `bard_csi_grpc_requests_in_flight` | gauge | — | concurrent RPCs |
| `bard_csi_plugin_requests_total` | counter | `backend`, `instance`, `endpoint`, `result` | which backend instance is failing, and how |
| `bard_csi_plugin_request_duration_seconds` | histogram | `backend`, `instance`, `endpoint` | per-instance backend latency |
| `bard_csi_plugin_requests_in_flight` | gauge | `backend` | a wedged plugin (climbs and never falls) |
| `bard_csi_volume_placement_attempts_total` | counter | `backend`, `instance`, `zone`, `decision`, `result` | where volumes land, and *why* |

All three planes report: the controller, the node, and the csi-addons server.

### `decision` is the one to watch

`bard_csi_volume_placement_attempts_total` carries how dispatch chose the instance:

- `preferred` / `requisite` — the scheduler's topology selected the instance. This
  is multi-zone dispatch working.
- `default` — nothing matched, so the configured default instance took it.
- `single_instance` — the backend has exactly one instance, so the choice was
  unambiguous.
- `unresolved` (with `result="dispatch_error"`) — dispatch could not pick an
  instance at all. Usually a missing zone label, a `BackendCluster` whose zone no
  node advertises, or no configured default.

A fleet where every placement is `default` looks perfectly healthy on every other
panel while doing no topology-aware placement whatsoever. That is the failure this
label exists to catch.

### What the plugin metrics do *not* measure

`bard_csi_plugin_request_duration_seconds` is the **core-to-plugin socket round
trip**, not the backend command inside the plugin. A plugin that waits 8s on its
own lock and then runs `rbd` for 100ms reports 8.1s here. Attributing time
*within* a plugin would require a plugin-contract change (`pkg/bardplugin` is
versioned and additive-only, and every plugin — including third-party and the
stdlib-Python demo — would have to implement it), so it is deliberately out of
scope.

`result` distinguishes `success`, `not_found`, `already_exists`,
`invalid_argument`, `unsupported`, `plugin_error`, `transport_error`, `timeout`,
`canceled` and `decode_error`.

Only **`not_found` and `unsupported`** are routine: a repeated `DeleteVolume`
legitimately finds nothing, and capability probing legitimately gets "not
supported". Those two are excluded from the dashboard's failure panel.

`already_exists` is **not** routine here. Bard's sentinel means *"already exists
with different properties"* (`internal/backend/backend.go`) — a genuine conflict,
not the successful idempotent retry that CSI's spec expects to return the existing
volume. `invalid_argument` usually means a bad StorageClass parameter. Both fail
provisioning for real, so both are counted as failures.

## Scraping

The controller gets a **headless** Service (`clusterIP: None`) fronting the core
and each enabled sidecar port, scraped by the ServiceMonitor. Headless only avoids
spending a ClusterIP on a scrape-only Service — a ServiceMonitor targets individual
EndpointSlice addresses rather than the Service VIP, so per-pod series are per-pod
either way.

The node plane is a DaemonSet with no Service, so it is scraped per-pod by the
PodMonitor, which relabels the node name onto every series (a pod name changes on
every DaemonSet roll and cannot answer "is this one node misbehaving?").

**`prometheus.io/*` annotations are not offered.** They describe a single endpoint
per pod, and the controller pod exposes up to six (core plus five sidecars). Use
the operator CRDs, or write your own scrape config against the Service.

### Node ports and `hostNetwork`

`metrics.nodePort` (default **9815**) must differ from `metrics.port` (9809) and
from every sidecar port (9810–9814). The node DaemonSet runs with `hostNetwork`
whenever any enabled node plugin requires it — and the **iscsi profile sets
`hostNetwork` on both planes** — so on the controller's own node the controller
pod and a node pod share the host network namespace. Two listeners on one port
means the second never binds, and the core only *warning-logs* that, so CSI keeps
working while a scrape target is silently dead. The chart fails the render on any
collision between enabled ports.

No `hostPort` is declared — `containerPort` is metadata only, and under
`hostNetwork` the process binds the host port regardless.

Prometheus must be able to reach node IPs on that port for the PodMonitor to
return anything.

## Volume usage comes from kubelet, not Bard

Bard's node core advertises the CSI `VOLUME_CONDITION` capability and implements
`NodeGetVolumeStats` with `VolumeCondition`, so kubelet can export per-PVC usage
and health without any Bard-side metric:

- `kubelet_volume_stats_used_bytes` / `_capacity_bytes` / `_available_bytes`
- `kubelet_volume_stats_inodes_used` / `_inodes_free`
- `kubelet_volume_stats_health_status_abnormal` — driven by the `VolumeCondition`
  Bard returns

Kubernetes 1.36 still treats the kubelet `CSIVolumeHealth` feature gate as alpha
and defaults it to false. Confirm it on every kubelet with
`kubernetes_feature_enabled{name="CSIVolumeHealth"}`: a value of `0` explains an
absent health series. Once the gate is enabled, healthy supported volumes export
`_health_status_abnormal` with value `0`, while abnormal volumes export `1`. An
absent health series means health telemetry is unavailable (or no volume has
been sampled), never that every volume is healthy.

The dashboard's "fullest volumes" and "volume health condition" panels use
these directly. The usage table normalizes the `exported_namespace` label that
some Kubernetes distributions add, de-duplicates by namespace and PVC, and
shows used bytes, capacity bytes, and their ratio. Duplicating these metrics in
the driver would be strictly worse.

**These are cluster-wide, not Bard-only.** Kubelet does not label volume stats by
CSI driver, so those two panels show every driver's volumes. Narrowing them needs a
join through PV metadata from kube-state-metrics:

```promql
topk(20,
  (kubelet_volume_stats_used_bytes / clamp_min(kubelet_volume_stats_capacity_bytes, 1))
  * on (namespace, persistentvolumeclaim) group_left
    max by (namespace, persistentvolumeclaim) (
      kube_persistentvolumeclaim_info
      * on (namespace, persistentvolumeclaim) group_left(csi_driver)
        label_replace(kube_persistentvolume_info{csi_driver="csi.bard.io"}, "x", "$1", "x", "(.*)")
    )
)
```

The shipped panels stay unfiltered so they work without kube-state-metrics; swap in
the join if you run it and want Bard-only.

Note also that `$namespace` cannot be applied to these series: on Bard's own
metrics that label is the *driver's* namespace, but on kubelet volume stats it is
the *PVC's* namespace. They are different things.

## Capacity

Bard does **not** emit a capacity metric. The chart already sets
`storageCapacity: true` by default, which runs external-provisioner with
`--enable-capacity`, so the cluster carries real `CSIStorageCapacity` objects that
respect StorageClass parameters and topology. A driver-side poller would duplicate
that with worse semantics — in particular it could not honour a StorageClass that
overrides `pool`, and would cheerfully report a healthy default pool while every
provision through that class failed.

The dashboard's capacity panel reads
`kube_customresource_csistoragecapacity_capacity_bytes`. `CSIStorageCapacity` is
a built-in `storage.k8s.io` API type, not a CRD, so kube-state-metrics'
[CustomResourceState](https://github.com/kubernetes/kube-state-metrics/blob/main/docs/metrics/extend/customresourcestate-metrics.md)
collector cannot produce this series for it. The example
[`kube-prometheus-stack-values.yaml`](../deploy/examples/observability/kube-prometheus-stack-values.yaml)
therefore adds a small, non-root exporter through `extraManifests`. It polls the
in-cluster API every 30 seconds with a ServiceAccount allowed only to list
`csistoragecapacities`, and exposes the exact metric through a ClusterIP Service
and ServiceMonitor. If that overlay is not installed, the capacity panel is
simply empty; everything else on the dashboard works.

The panel groups by **StorageClass and Bard zone, not backend instance**. A
`CSIStorageCapacity` object identifies its scope with a node-topology selector,
so the exporter preserves the useful StorageClass, namespace, object, and Bard
zone labels. It emits one gauge per object; the dashboard's `max by
(storageclass, zone)` avoids double-counting multiple objects in one zone.

Without the capacity exporter, `kubectl get csistoragecapacity -A` shows the
same data directly. When a backend cannot report capacity, Bard returns
`math.MaxInt64` (`9223372036854775807`) as an effectively-unlimited sentinel;
the dashboard maps values `>=9e18` to `Unreported` instead of displaying an
exabyte-sized capacity.

## Drift metrics are not exported (yet)

`kubectl bard inspect` (see [inspect.md](inspect.md)) finds ghost PVs, orphaned
backend volumes, topology breakage and stale VolumeAttachments — obvious dashboard
material. It is deliberately **not** wired to metrics yet:

- Plugin unix sockets are pod-local, so a scan can only run inside the controller
  pod. A standalone exporter would have to duplicate the sockets, config,
  credentials and RBAC.
- A scan issues five unpaginated cluster-wide LISTs (PVs, VolumeSnapshotContents,
  Nodes, CSINodes, VolumeAttachments) plus `ListVolumes` and `ListSnapshots` per
  backend, plus a health probe per ghost-PV candidate. Running that on a timer
  inside the availability-critical provisioning path is not something to add
  casually.

Run it on demand instead. Exporting it safely is tracked in the repository as
[Safe drift-metrics exporter](../STATUS.md#safe-drift-metrics-exporter); it is not
implemented yet.

## Caveats

- **`instance="all"`** marks the genuinely cross-instance calls (`ListVolumes`,
  `ListSnapshots`, `ListVolumeGroups`, and the startup `/info` probe). Nothing
  reserves that name, so a backend instance actually *called* `all` would be
  indistinguishable from them in aggregations. Avoid naming an instance `all`.
- **Never-exercised series are absent, not zero.** A healthy install shows "No
  data" on the failure panels rather than a flat zero line, because a counter that
  has never been incremented does not exist. Alert on `absent()`-tolerant
  expressions, not on the series simply being missing.
- **A hand-written scrape config must add a `namespace` target label** — the
  dashboard's `$namespace` variable depends on it. Operator-generated
  ServiceMonitor/PodMonitor scrapes provide it automatically.
