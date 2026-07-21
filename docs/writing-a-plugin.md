# Writing a Bard CSI backend plugin

Bard supports **out-of-tree storage backends**: you can add a backend without
forking Bard or rebuilding its binary. A plugin is a small HTTP+JSON server on a
unix socket; Bard discovers it from config and proxies every volume operation to
it, treating it exactly like a built-in backend.

You can write a plugin in **any language** (it's just HTTP+JSON). Go authors get
an SDK that reduces it to implementing one interface.

## Who owns what

The plugin brings the tools; Bard core helps those tools talk to Kubernetes.
Concretely, three layers:

| Layer | Owns |
|---|---|
| **Host / node** | kernel capabilities a container can't ship — e.g. the `rbd`/`nbd` modules, `/dev/nbd*`, the ability to `mount`. A node prerequisite. |
| **Bard core** | the backend-agnostic CSI↔Kubernetes glue: the single kubelet-registered driver, topology dispatch, volume-handle encoding, the plugin proxy, and the privileged-sidecar deployment slot. **No backend tools.** |
| **Your plugin** | the backend logic **and the userspace tools it needs** (`rbd`, `mount.nfs`, …, baked into your plugin image), plus its own per-backend config and credentials. |

This is deliberate: because the tools live in your plugin image, someone can ship
a *better* plugin for the same backend — newer tools, different logic — and it
works with Bard core unchanged. Core never grows a dependency on any one backend.

## The contract

Each operation is an HTTP `POST` to a fixed path with a JSON request/response
body. `200` is success; a non-`200` carries an `{"code","message"}` error, where
`code` is one of `Internal`, `NotFound`, `AlreadyExists`, `InvalidArgument`,
`Unsupported`.

Pick the code by what CSI mandates for that operation, not by what feels
descriptive — Bard maps them straight to gRPC codes the CO acts on. Two rules
worth committing to memory:

- **`InvalidArgument` for a source you cannot use.** If `/volume/create`
  carries a `sourceSnapshot`/`sourceVolume` you do not support, CSI *requires*
  `INVALID_ARGUMENT`, telling the CO to retry with a different source or none.
  `Unsupported` there means "this RPC does not exist", which is both wrong and
  a conformance failure.
- **`Unsupported` for an RPC disabled in your current mode.** CSI allows
  `UNIMPLEMENTED` for an RPC "not implemented by the plugin *or disabled in the
  plugin's current mode of operation*", and the CO must not retry it. This is
  the code's reason for existing: `/info` capabilities are **per-plugin**, but
  one plugin may serve **instances** with different abilities (the iSCSI plugin
  supports snapshots on locally-managed instances but not targetd-managed
  ones). A capability flag cannot express that; `Unsupported` can.

Everything else that is simply not advertised in your `Capabilities` needs no
error at all — Bard never calls an optional route you did not declare.

The HTTP status mirrors the code (`NotFound`→404, `AlreadyExists`→409,
`InvalidArgument`→400, `Unsupported`→501, everything else→500), but the JSON
`code` is authoritative: Bard dispatches on it and treats the status as
transport decoration. Set both consistently anyway — a mismatched pair misleads
anyone reading your plugin over the wire.

| Path | Request → Response | When |
|---|---|---|
| `/info` | → `Info` (type + contract version + capabilities) | at startup |
| `/volume/create` | `CreateVolumeRequest` → `CreateVolumeResponse` | controller |
| `/volume/delete` | `DeleteVolumeRequest` → `{}` | controller |
| `/volume/expand` | `ExpandVolumeRequest` → `ExpandVolumeResponse` | controller |
| `/snapshot/create` | `CreateSnapshotRequest` → `CreateSnapshotResponse` | controller |
| `/snapshot/delete` | `DeleteSnapshotRequest` → `{}` | controller |
| `/node/stage` | `NodeStageRequest` → `{}` | node |
| `/node/unstage` | `NodeUnstageRequest` → `{}` | node |
| `/node/publish` | `NodePublishRequest` → `{}` | node |
| `/node/unpublish` | `NodeUnpublishRequest` → `{}` | node |
| `/node/expand` | `NodeExpandRequest` → `NodeExpandResponse` | node |

Message schemas are defined in
[`pkg/bardplugin/protocol.go`](../pkg/bardplugin/protocol.go). Key ideas:

- **Volume identity.** On `/volume/create` you return a `location` + `name` you
  choose; Bard echoes them back (with the `instance`) as a `VolumeRef` on every
  later call. Keep them compact and free of `|` — Bard also encodes the backend
  type + instance into the 128-byte CSI volume id.
- **Topology is resolved for you.** Requests carry a concrete `instance`
  (already mapped from the node's zone by Bard's dispatcher). Your plugin owns
  whatever that instance means (an NFS server, a cluster, …) via its own config.
- **Capabilities** in `/info` tell Bard how to treat your volumes (block vs
  file, node-local, snapshots, expand). Implement only what you advertise.

## Contract version & compatibility promise

The wire contract is versioned `MAJOR.MINOR`, independently of Bard releases;
the current version is **1.1** (`bardplugin.ContractVersion`). Report the
version you implement in `/info` as `contractVersion` (the Go SDK fills it in
for you; an absent field means `1.0`). Bard refuses at startup a plugin whose
MAJOR it does not support, **or whose MINOR is newer than it understands**.

That gate is asymmetric on purpose. An older plugin is always safe: everything
it can say, a newer Bard already understands. The reverse does not hold — a
MINOR may add vocabulary to an *existing* route (1.1 added the `Unsupported`
error code), and an older Bard meeting an unknown value degrades it to a
generic `Internal`, turning a terminal failure into one the CO reconciles
indefinitely. Failing fast at startup beats mistranslating at runtime, so pair
a newer plugin with a Bard that speaks its MINOR.

Within a MAJOR version the contract only grows, and only compatibly — **a
plugin built against contract 1.0 keeps working, unchanged, with every Bard
release that speaks major 1**:

- Existing routes and fields are never removed, renamed, or given new meaning.
- New operations arrive as **new routes gated by new capability flags** in
  `/info`; Bard never calls an optional route you did not advertise.
- New fields on existing messages are **optional**, and absent means the old
  behavior. Decode tolerantly: ignore unknown JSON fields (the default for
  Go's and Python's decoders — don't enable a strict mode).

A MINOR bump marks that new optional surface exists. A MAJOR bump is a
breaking change: rare, announced in release notes ahead of time, and shipped
with a transition period during which Bard accepts both majors.

Beyond the schemas, the contract requires these semantics (kubelet and the CSI
sidecars retry operations, and Bard forwards your errors straight to them):

- **`create` is idempotent**: re-creating the same name with the same size
  returns the same volume; the same name with an incompatible size should be
  `AlreadyExists`.
- **`delete` is idempotent**: deleting an absent volume or snapshot returns
  success — not `NotFound`, which Kubernetes would retry forever.
- **Node stage/publish/unstage/unpublish are idempotent** in both directions.
- Returned `location`/`name` contain no `|` and stay compact (they are encoded
  into the 128-byte CSI volume id).

## Verifying a plugin: the conformance tool

`bard-plugin-conformance` drives your plugin over its socket the way Bard core
would and checks the required semantics above plus every optional capability
you declare. It is the acceptance bar for a new backend — a plugin that passes
is indistinguishable from a first-party one as far as core can see:

```sh
go build -o bard-plugin-conformance ./cmd/bard-plugin-conformance
# start your plugin with its own config, then:
./bard-plugin-conformance -instance my-east /var/lib/bard/plugins/my.sock
sudo ./bard-plugin-conformance -instance my-east -node <socket>   # + node plane (real mounts)
```

`-param key=value` passes StorageClass-style parameters, `-size` sets the test
volume size. The checks create and delete real volumes/snapshots under a
unique `conf-*` prefix — point it at a disposable instance. Exit code 0 means
conformant (warnings allowed); each check prints `PASS`/`FAIL`/`WARN`/`SKIP`.

## Optional capabilities

Beyond the core routes above, a plugin can opt into extra operations. In Go you
just implement the matching **optional interface** — the SDK detects it, sets the
capability in `/info`, and wires the route; Bard only calls it when advertised. In
another language, serve the route and set the capability flag yourself.

| Route(s) | Go interface | Capability / CSI feature |
|---|---|---|
| `/capacity` | `CapacityReporter` | `GetCapacity` (CSIStorageCapacity) |
| `/volume/health` | `HealthReporter` | volume condition (`ControllerGetVolume`) |
| `/volume/modify` | `VolumeModifier` | `ControllerModifyVolume` (VolumeAttributesClass) |
| `/volume/reclaimspace`, `/node/reclaimspace` | `SpaceReclaimer`, `NodeSpaceReclaimer` | csi-addons ReclaimSpace |
| `/controller/publish`, `/controller/unpublish` | `ControllerPublisher` | control-plane attach (`ControllerPublishVolume`); needs `attach.enabled` in the deploy. See the iSCSI plugin |
| `/volume/list`, `/snapshot/list` | `VolumeLister`, `SnapshotLister` | `ListVolumes` / `ListSnapshots` (Bard aggregates + paginates) |

## In Go (the SDK)

Implement `bardplugin.Backend` and call `Serve`:

```go
func main() {
    bardplugin.Serve(context.Background(), "/var/lib/bard/plugins/my.sock", &myBackend{})
}
```

The NFS plugin in [`cmd/bard-plugin-nfs`](../cmd/bard-plugin-nfs/main.go) is a
complete worked example (subdir-per-volume on an NFS export).

## Deploying a plugin

A plugin runs as a **sidecar** in Bard's pods, sharing a unix-socket directory:

- In the **controller** Deployment for the control-plane ops, and in the **node**
  DaemonSet for the node ops. (Node-side ops do mounts, so that sidecar is
  privileged and shares the kubelet dir with `mountPropagation: Bidirectional`.)
- Bard's container and your sidecar both mount an `emptyDir` at
  `/var/lib/bard/plugins`; your plugin serves its socket there.

Then add a backend entry to Bard's config pointing at the socket:

```yaml
backends:
  my-storage:
    plugin:
      endpoint: /var/lib/bard/plugins/my.sock
    instances:
      my-east: { zone: zone-east }   # Bard only reads zone; the rest is yours
```

and a StorageClass selecting it (`parameters.backend: my-storage`). No CSI secret
params are needed — your plugin resolves its own credentials/config.

See [`deploy/examples/nfs/`](../deploy/examples/nfs/) for a full sidecar example.
