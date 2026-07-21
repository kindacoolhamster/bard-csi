# bard-csi

A "jack of all trades" CSI driver for Kubernetes: one driver, many storage
backends, and — unlike most existing CSIs — **a single StorageClass that can
provision across multiple backend instances / zones**.

**New here?** [docs/quickstart.md](docs/quickstart.md) gets you from an empty
kind cluster to a working PVC in about three minutes, with no external storage
needed.

Every backend is an out-of-tree plugin. The flagship is the **Ceph RBD** backend —
the most feature-rich (see [Status](#status)) and where the deeper features are
proven — but it is one backend among several: **CephFS** (a shared, ReadWriteMany
filesystem), **NFS**, **LVM** (host-local block volumes), and **iSCSI** (network
block that attaches via the control plane, with per-node LUN masking). Together they
show the backend contract generalises across very different storage shapes (network
block, distributed filesystem, network filesystem, host-local block, and
control-plane attach).

The headline capability — one StorageClass provisioning across multiple backend
instances/zones — is itself backend-agnostic: most CSI drivers bake a single
backend target into the StorageClass, so a class can reach only one. Bard resolves
the target per-volume from where the workload is scheduled instead.

## Why this is different

A normal CSI bakes the target backend (e.g. a specific Ceph cluster's monitors)
into the StorageClass, so a class can only ever reach one instance. bard-csi
inverts that:

- The StorageClass names only a **backend type** (e.g. `backend: ceph-rbd`) and a
  logical pool/location — *not* a specific instance.
- With `volumeBindingMode: WaitForFirstConsumer`, provisioning is deferred until
  a pod is scheduled.
- The node plugin reports its zone via `NodeGetInfo`; the external-provisioner
  (with `Topology=true`) feeds that zone into `CreateVolume`.
- The **dispatcher** maps zone → a concrete backend instance.
- Each backend plugin resolves its own per-instance credentials from a Secret it
  mounts, keyed by instance id (for the Ceph plugin, a cephx key) — not via
  StorageClass secret params (which have no node-zone token and so can't select a
  per-instance secret).

Net result: one StorageClass, N backend instances (e.g. N Ceph clusters), picked
per-volume by where the workload landed.

## Architecture

Bard **core** is backend-agnostic: it implements the CSI spec, resolves topology
to a backend instance, and **proxies every operation to an out-of-tree plugin**
over a unix socket. Storage backends — including the first-party Ceph RBD one —
are plugins that bring their own tools; core ships none (its image is a ~22 MB
static distroless binary). See [docs/writing-a-plugin.md](docs/writing-a-plugin.md).

```
┌────────────────────────────────────────────────┐
│  Bard core  (distroless, backend-agnostic)     │
│   CSI gRPC (Identity/Controller/Node)          │  internal/driver
│   Dispatcher: zone → backend instance          │  internal/dispatch
│   Plugin proxy (backend registry)              │  internal/backend(/plugin)
└───────────────┬────────────────────────────────┘
                │  HTTP+JSON over a unix socket   (pkg/bardplugin)
        ┌───────┴────────┐
        ▼                ▼
   ceph-rbd plugin    nfs plugin          (sidecars; ship their own tools)
   cmd/bard-plugin-*  internal/cephplugin
```

| Package | Responsibility |
|---|---|
| `pkg/bardplugin` | Public plugin SDK: the HTTP+JSON contract, the `Backend` interface, and `Serve()`. Plugins can be written in any language. |
| `internal/backend` | Internal `Backend` interface + capability model + registry (what core programs against). |
| `internal/backend/plugin` | `Client`: dials a plugin's socket and implements `backend.Backend` by proxying to it. |
| `internal/cephplugin` | Ceph RBD plugin backend — depends only on `pkg/bardplugin`, exactly like a third-party plugin. |
| `cmd/bard-plugin-ceph-rbd`, `cmd/bard-plugin-nfs` | The plugin binaries (own images, own tools). |
| `internal/dispatch` | Resolves `(StorageClass params, topology)` → `(backend, instance, zone)`. |
| `internal/driver` | The three CSI gRPC services + the unix-socket gRPC server. |
| `internal/volumeid` | The self-describing CSI volume handle (`swsk\|1\|ceph-rbd\|east\|pool\|name`). |
| `internal/config` | Loads the backend/instance/zone config from `BackendCluster` CRs (listed from the API at startup) or a file. |
| `cmd/bard-csi` | Core binary; runs as `--controller` or `--node`. |

## Adding a backend

Every backend — including the first-party Ceph RBD one — is an **out-of-tree
plugin**, so you add one without forking Bard or rebuilding core:

1. Implement the small HTTP+JSON contract (Go authors implement
   `bardplugin.Backend` and call `Serve()`; any language works).
2. Ship it as a container; run it as a sidecar in Bard's controller + node pods,
   sharing the plugin socket dir.
3. Add a `plugin` backend entry to Bard's config pointing at the socket.

See [docs/writing-a-plugin.md](docs/writing-a-plugin.md). Worked examples:
[Ceph RBD](cmd/bard-plugin-ceph-rbd/main.go) (network block, RWO),
[CephFS](cmd/bard-plugin-cephfs/main.go) (distributed filesystem, RWX),
[NFS](cmd/bard-plugin-nfs/main.go) (a network filesystem),
[LVM](cmd/bard-plugin-lvm/main.go) (host-local block, RWO),
[iSCSI](cmd/bard-plugin-iscsi/main.go) (network block with control-plane attach —
the reference `ControllerPublish` backend, masking a LUN to the node's initiator),
and [localpath](plugins/localpath/bard-plugin-localpath) — a directory bind-mount
written in **Python, stdlib only**, the standing proof that "any language" is
literal: not a line of Go, yet core dispatches to it identically.
The division of labour: **the plugin brings the tools; Bard core helps them talk
to Kubernetes; the host provides kernel capabilities** (rbd/nbd modules, `/dev`).

## Build & test

```sh
make build     # -> bin/bard-csi
make test
make vet
make image VERSION=0.1.0
```

## Deploy

Raw manifests:

```sh
# The CRD must exist before its CRs; apply it first, then the rest.
kubectl apply -f deploy/05-crd-backendcluster.yaml
# Edit deploy/20-config.yaml (BackendCluster clusters/zones) and the Secret keys.
kubectl apply -f deploy/
```

Or the Helm chart ([charts/bard-csi](charts/bard-csi)), which assembles the driver
runtime + the backend plugin sidecars you enable (backend connection config +
credentials stay yours, referenced by name):

```sh
helm install bard-csi charts/bard-csi -n kube-system --set 'plugins.ceph-rbd.enabled=true'
```

Manifests: `00-csidriver`, `05-crd-backendcluster` (the `BackendCluster` CRD),
`10-rbac`, `20-config` (BackendCluster CRs + the plugin's ConfigMap/Secret),
`30-controller` (Deployment + provisioner/snapshotter/resizer sidecars),
`40-node` (DaemonSet + node-driver-registrar), `50-storageclass`
(StorageClass + VolumeSnapshotClass + VolumeGroupSnapshotClass +
VolumeAttributesClass). Core reads its backends from the
BackendCluster CRs at startup (`--config-source=crd`); `--config-source=file`
keeps the old ConfigMap path for out-of-cluster runs.

## Day-2: the consistency scanner

Kubernetes only knows what its own objects claim. `kubectl bard inspect` joins
that against **backend truth** — what actually exists on every configured
backend, enumerated through the same plugin contract core provisions with —
plus the driver's topology config, and reports the drift: PVs whose backing
volume is gone, orphaned volumes eating pool space after a failed delete,
stale attachments left by a dead node, zone labels that will crashloop the
registrar. Read-only, every finding carries a remediation hint, exit codes
made for cron. See [docs/inspect.md](docs/inspect.md).

```sh
go install github.com/kindacoolhamster/bard-csi/cmd/kubectl-bard@latest
kubectl bard inspect
```

## Status

The implemented-feature inventory and the roadmap live in [STATUS.md](STATUS.md).

The six backends are **not equally proven**, and the feature list is the union
across all of them. **Ceph RBD** and **iSCSI** are the two carrying real weight
(Stable); **CephFS** is Beta; **NFS** and **LVM** are Experimental; **localpath**
is a Python reference plugin, not for production. See
[Backend maturity](STATUS.md#backend-maturity) for what each tier means and the
per-backend caveats.

## Contributing & license

Contributions welcome — see [CONTRIBUTING.md](CONTRIBUTING.md); backend
plugins in any language are the easiest entry point
([docs/writing-a-plugin.md](docs/writing-a-plugin.md)). Security reports:
[SECURITY.md](SECURITY.md). Licensed under [Apache-2.0](LICENSE).
