# CLAUDE.md — working in this repo

Bard CSI: a multi-backend Kubernetes CSI driver whose headline feature is **one
StorageClass provisioning across multiple backend instances/zones**, chosen
per-volume by topology. Backends are all out-of-tree plugins: Ceph RBD, CephFS,
NFS, LVM. See [README.md](README.md) for architecture; this file is the
operational/dev guide.

## Toolchain & environment

- Go 1.26; the build is plain `go build` (no code generation step).
- Container images build with **podman** (docker works identically). The dev
  recipes below assume a Ceph mon reachable from the cluster; if the mon runs
  on the kind host itself, the kind cluster must be **rootful** (see gotchas).
- The `Makefile` is a recipe reference — every target is a plain command you
  can also run directly.
- Machine-specific settings (paths, kubeconfigs, your dev Ceph's addresses)
  belong in an untracked `CLAUDE.local.md`, which Claude Code reads alongside
  this file. The examples below use placeholder addresses; substitute your own.

## Build & test

```sh
go build ./... && go vet ./... && go test ./...   # hermetic; uses internal/fakerun
gofmt -l .                                         # must be empty
go build -o bin/bard-csi ./cmd/bard-csi            # the core binary
```

`go test ./internal/driver -run TestSanity` runs the upstream csi-sanity suite
against the driver backed by the in-memory fake command runner (no Ceph needed).

### Real-Ceph backend test (control plane, no cluster, no sudo)

```sh
BARD_CEPH_TEST=1 CEPH_MON=<mon-host>:3300 CEPH_POOL=<pool> \
CEPH_USER=<user> CEPH_KEY=<key> \
go test ./internal/cephplugin/ -run TestRealCeph -v
```

### Real-VG LVM plugin test (real VG, no cluster — needs sudo for lvm/mount)

The LVM plugin's storage is host-local, and the nested kind nodes cannot see the
host VG (see gotchas), so its full lifecycle is proven by driving the *real
plugin binary* over its unix socket against `bard-vg` directly, not through kind:

```sh
sudo bash hack/setup-lvm-fixture.sh                # -> bard-vg (RBD-backed), once
go build -o /tmp/bard-plugin-lvm ./cmd/bard-plugin-lvm
sudo bash hack/lvm-plugin-test.sh /tmp/bard-plugin-lvm
```

`hack/lvm-plugin-test.sh` drives the full HTTP+JSON contract over the socket
(/info, thin /volume/create, /node/stage+publish, write, /snapshot/create,
/volume/create from sourceSnapshot, read the data back, teardown) and asserts the
thin attr (`Vwi-...`), the read-only snapshot (`Vri-...`), and the restored data.
**It starts the plugin under a `trap ... EXIT` that always kills it and removes
its socket + test LVs** -- out of cluster there is no controller pod to reap the
plugin, so never start the binary with a bare `&` (it orphans). It auto-creates
the `bard-thin` pool.

### iSCSI plugin test (real LIO target, no cluster -- the ATTACH backend)

The iSCSI plugin is the reference **attach-style** backend: making a volume
reachable is a control-plane op (`ControllerPublish` masks the LUN to the staging
node's initiator IQN via a targetcli ACL), not a node-only map. Its control plane
is host/configfs-coupled (like LVM's VG), so it's proven by driving the *binary*
over its socket against a real LIO target on the host:

```sh
sudo bash hack/setup-iscsi-fixture.sh              # LIO + bard-vg + iscsid, once
go build -o /tmp/bard-plugin-iscsi ./cmd/bard-plugin-iscsi
sudo bash hack/iscsi-plugin-test.sh /tmp/bard-plugin-iscsi
```

`hack/iscsi-plugin-test.sh` drives the **full attach contract plus thin
snapshots/clone and CHAP** (live-proven 2026-07-10): /info (asserts
`requiresControllerPublish` AND `snapshots`), create (thin LV + LIO backstore +
per-volume target with `authentication=1`), /controller/publish (asserts the ACL
for `iqn.2025-01.io.bard:init-<node>` appears carrying the CHAP userid, and no
credential in the publishContext), /node/stage (iscsiadm CHAP login under a
per-node iface) + publish + write, /snapshot/create (asserts a read-only thin LV
`Vri-...` with NO LIO export, listed with its origin), restore into a LARGER
volume (own target; point-in-time -- post-snapshot writes absent; fs grown at
stage), **online expand under a live session** (lvextend propagates through the
LIO block backstore -> session rescan -> fs grown, data intact), a
**wrong-password CHAP login rejected** by LIO, /controller/unpublish (ACL gone),
snapshot+volume deletes (targets + backstores + LVs all reaped -- no orphan). Trap-cleaned like the LVM test; auto-creates the `bard-thin` pool. CHAP
credentials are per-instance files (`--chap-dir/<instance>`, Secret
`bard-iscsi-chap`, 2 lines userid/password or 4 with a mutual pair) read by BOTH
planes -- never in the ConfigMap/StorageClass/PublishContext. Each volume = its
own target (one LUN), so login/logout is per-volume clean (no session
ref-counting). Gotchas hit live: `targetcli` create takes the **parent** path +
`name=` (`/backstores/block create name=x dev=...`), NOT the object path
(`/backstores/block/x create ...` -> "No such path"); a duplicate **backstore**
create says `Storage object block/<x> exists` (no "already", unlike
targets/ACLs/LUNs), which broke idempotent create retries until `isExists`
learned that phrasing; and `iscsiadm` refuses to create/update an **iface a live
session is using** (exit 15, "Could not create new interface"), so staging a 2nd
volume on a node failed until `ensureIface` became read-first -- a bug
single-volume tests structurally cannot catch.

**Fully IN-CLUSTER live-proven 2026-07-11** on the multipass k3s dogfood cluster
(target host = `k3s-server` with a loop-backed `bard-vg`, controller pinned there
via `controller.nodeSelector`; attach flipped on with `helm --set
attach.enabled=true` after deleting the immutable CSIDriver): provision ->
CROSS-NODE attach (VolumeAttachment -> ACL for `init-k3s-agent`) -> CHAP login
from the agent over the VM network -> `/dev/sdX` -> mount -> write; a 2nd volume
staged on the SAME node (the ensureIface fix, live); VolumeSnapshot -> ready
(read-only thin LV, no LIO export); point-in-time restore into a LARGER volume
(post-snapshot writes absent, fs grown at stage); delete-everything reap back to
just the thin pool -- zero targets/backstores/sessions left. FOUR
in-cluster-only gotchas found + fixed (none reachable by the host harness):
(1) the plugin image needs **thin-provisioning-tools** (in-container lvm shells
to `thin_check`); (2) targetcli 2.1.5x unconditionally enumerates tcmu-runner
over the SYSTEM D-Bus (`Gio.bus_get_sync`) on every command, so the controller
sidecar must mount the host `/run/dbus` or every call dies `g-io-error-quark:
Could not connect`; (3) **an LIO network portal binds in the netns of the
process that creates it** -- without `hostNetwork` on the controller pod the
portal listens inside the POD netns (targetcli shows it [OK], initiators get
connection-refused; a portal from a dead pod netns must be deleted + recreated);
(4) **iscsiadm+iscsid are a version-matched pair with distro-specific DB paths**
(Debian `/etc/iscsi`, RHEL `/var/lib/iscsi`), so a container iscsiadm driving
the host's iscsid puts CHAP node records where the daemon never looks and the
login hangs at negotiation (LIO dmesg: "iSCSI Login negotiation failed") --
fixed by running iscsiadm chrooted into the host root (`--iscsiadm-chroot=/host`,
the standard CSI-driver approach; mount host `/` at `/host`). Plus a
host-module prereq on the target node: `dm_snapshot` (`lvcreate -s` shells to
modprobe, impossible in-container; the fixture loads it). A SECOND round on the
branch tip (2026-07-12, incl. in-cluster online PVC expand 2Gi->3Gi, no pod
restart) caught two more that only cold/restart states expose: (5) **an
INACTIVE thin pool cannot be activated by in-container lvm** (no udev to serve
the activation -- the host udevd's completion handshake lives in the host IPC
namespace; `.../bard--thin_tmeta: open failed`); round 1 masked it because a
host-side command had left the pool active, but the first volume after a node
reboot or after the last LV's removal always hits it. Fixed in the plugin:
every state-changing lvm command runs with `--config
'activation{udev_sync=0 udev_rules=0}'` (lvm manages /dev nodes itself; also
correct on udev hosts -- harness re-proven from an inactive pool; ported to
the LVM plugin too, which shares this logic and had the same latent bug). (6) **the
node session state must survive plugin pod restarts**: stateDir was
container-ephemeral, so a mid-lifetime pod restart made every later NodeUnstage
a silent no-op that leaked the iSCSI session past volume deletion. Fixed both
ways: the node patch persists `/var/lib/bard/iscsi` as a hostPath, AND
NodeUnstage now derives the session identity (target IQN from the volume name,
portal from the instance, LUN 0) and logs out even with no record --
regression-proven live (stage -> kill the node plugin pod -> delete workload ->
zero leaked sessions). **Conformance now covers attach backends** and runs
against the real fixture via `hack/conformance-iscsi-test.sh` (27 PASS): the
tool gained the controller/publish -> node -> unpublish leg (`-node-id` must
match the plugin's `--node-id`), and its first run caught three more bugs --
two MORE unclassified phrasings (`No storage object named` broke delete
idempotency; `No matching sessions found` broke repeated unstage via the
derived-logout fallback) and a hollow NodeReclaimSpace (the backstore never
advertised UNMAP; now `emulate_tpu=1` best-effort at create, and an
un-discardable stack is a clean no-op instead of a forever-failing job). A
control plane on a non-target node uses remote LIO management
(`management: targetd`) -- see the "Known follow-ups" section (DONE,
live-proven in-cluster 2026-07-20).

## Local end-to-end (rootful kind + real Ceph)

```sh
# 1. build + save the driver image (rootless podman)
podman build -t ghcr.io/kindacoolhamster/bard-csi:dev . && \
  rm -f /tmp/bard-csi-image.tar && \
  podman save -o /tmp/bard-csi-image.tar ghcr.io/kindacoolhamster/bard-csi:dev

# 2. stand up the cluster + all host fixes (idempotent)
sudo bash hack/setup-rootful-kind.sh          # teardown: ... delete

# 3. deploy (snapshot CRDs first time only) + the real secret
export KUBECONFIG=$HOME/.kube/config-bard
kubectl apply -f https://raw.githubusercontent.com/kubernetes-csi/external-snapshotter/v7.0.2/client/config/crd/snapshot.storage.k8s.io_volumesnapshotclasses.yaml
# (+ the volumesnapshotcontents / volumesnapshots CRDs and snapshot-controller)
kubectl apply -f deploy/05-crd-backendcluster.yaml   # CRD before its CRs (avoids apply race)
kubectl apply -f deploy/
kubectl apply -f hack/secret.yaml             # real cephx key, gitignored
kubectl apply -f hack/config-local.yaml       # your real mon/KMS addresses
# (deploy/20-config.yaml ships documentation IPs; config-local.yaml is your
#  untracked copy of it with real values -- same pattern as the secret)
# core reads backends from BackendCluster CRs (--config-source=crd); the old
# ConfigMap path still works with --config-source=file for out-of-cluster runs.

# 4. smoke test
kubectl apply -f hack/test-pvc.yaml
kubectl wait --for=condition=Ready pod/bard-test --timeout=150s
kubectl exec bard-test -- cat /data/proof.txt   # -> bard-csi-works
```

After changing driver code: rebuild+save the image, `kind load image-archive
/tmp/bard-csi-image.tar --name bard` (via sudo), then restart the pods
(`kubectl -n kube-system delete pod -l 'app in (bard-csi-controller,bard-csi-node)'`).
Same `:dev` tag is replaced in containerd by the load, so IfNotPresent picks it up.

## Real-node integration (k3s on multipass VMs) — the krbd tier

kind nodes share the host kernel, so krbd (`rbd map` -> `/sys/bus/rbd/add`) is
rejected and we are forced onto rbd-nbd, whose unmap is non-deterministic there
(zombie watchers -> orphaned images; see Known limitations). A small k3s cluster
on **real-kernel VMs** runs the **production-default krbd mounter** and unmaps
reliably. Use this tier for the node plane (krbd, mounts, multi-node topology,
fencing); keep **kind for the fast loop** (`go test`, csi-sanity, helm lint).

```sh
snap install multipass          # once
bash hack/setup-k3s-vms.sh      # 2 VMs + k3s + Bard(krbd); teardown: ... delete
export KUBECONFIG=$HOME/.kube/config-k3s
kubectl apply -f hack/test-pvc.yaml   # -> mounts /dev/rbd0 (krbd), not /dev/nbdN
```

The script loads the `rbd` module via cloud-init, installs k3s (SQLite, no etcd --
so RBD-disk latency is a non-issue), imports the locally-built Bard images into
each node's **containerd** (`k3s ctr images import`, NOT `kind load`), and deploys
`deploy/` with the `mounter: rbd-nbd` line dropped (krbd is the default). It then
runs `hack/install-snapshotter.sh` (the external-snapshotter cluster singleton the
chart deliberately doesn't bundle) so snapshots work out of the box. VMs reach the
host mon over multipass NAT. VM disks live on the host root disk, decoupled from
the Ceph-under-test. csi-addons sidecars are stripped (their CRDs/controller-manager
aren't installed, and sidecars without them crashloop).

**The full RBD surface is proven on krbd here** (each reaped cleanly on delete,
pool back to baseline -- none of the rbd-nbd zombie-watcher flakiness): basic
provision/mount (`/dev/rbd0`, not `/dev/nbdN`), raw block (`volumeMode: Block`),
`mountOptions` (noatime), readOnly publish (write rejected), online volume expand
(ext4 + btrfs `resize max`, and encrypted volumes -- LUKS via `cryptsetup resize` +
fscrypt), `btrfs` fsType, and snapshot -> restore-to-new-PVC.
**LUKS encryption** is proven here too (needs `dm_crypt` on the VMs -- cloud-init
loads it; apply `hack/secret-encryption.yaml` for the master key): the derived
provider (`bard-rbd-encrypted`) and the `secrets-metadata` KMS
(`bard-rbd-encrypted-k8s`, wrapped DEK in `bard.encryptionDEK` image-meta) both
mount `/dev/mapper/bard-luks-*`, leave **only ciphertext in Ceph** (verified: LUKS2
header + zero plaintext in `rbd export`), and survive an unstage/restage (passphrase
re-derived, nothing persisted). **Multi-node** is proven on the two real nodes: a
RWO volume per node; with attach on, a 2nd pod for one RWO PVC on the other node is
correctly blocked (`Multi-Attach error`); and the **single-writer failover fence
runs for real** -- stop `k3s-agent` with a staged RWO volume (its krbd watcher
persists), force-delete the pod + VolumeAttachment, reschedule to `k3s-server`, and
the server's NodeStage **blocklists the stale agent watcher** (the exact
`<agent-ip>:0/<nonce>` shows up in `ceph osd blocklist ls`) before taking over,
data intact. The blocklist entry auto-expires (~1h); `profile rbd` can't `rm` it.
**`hack/install-snapshotter.sh`** installs the snap + group CRDs and a
version-matched snapshot-controller, and re-pins the controller image to the
version arg (default `v8.2.0`) -- upstream's own `setup-snapshot-controller.yaml`
mispins it to `v8.0.1`, and because Bard's csi-snapshotter sidecar runs the
group-snapshot gate, that stale controller stalls *plain* snapshots too (the
VolumeSnapshot sits at `readyToUse: false` until the group CRDs exist AND the
controller is the matched version).

**Real-workload RBD resiliency demo (PostgreSQL).** `hack/demo-postgres.yaml` is
a single-instance Postgres 16 StatefulSet with its data dir on a `bard-rbd` PVC --
a real DB doing fsync'd writes through the rbd device, used to prove RBD holds up
under a real app, not just csi-sanity. `hack/demo-postgres-resilience.sh` drives
four scenarios (pick with `A B C D` args; `cleanup` tears down): **A** pod
reschedule (data survives unstage->restage), **B** the single-writer failover
fence (stop the agent node, force the volume to the server -- the server's
NodeStage blocklists the agent's stale krbd watcher in Ceph *before* mounting;
needs multipass + the two VMs, DB pinned to the agent), **C** snapshot->restore to
a 2nd Postgres showing only point-in-time rows, **D** online PVC expand with no
pod restart. All four PASS on krbd; artifacts in `~/k3s-rbd-proof/postgres/`
(SUMMARY.md + 01..04 logs). NOTE this demo's failover test reverts the galileo
instance to the **krbd default** (undoes the rbd-nbd healer-test override) and
leaves the StatefulSet running as the standing workload.

## Hard-won gotchas (don't re-derive these)

- **kind nodes cannot see the host's devices/VG.** A kind "node" is a podman
  container with its *own* `/dev`; `hostPath: /dev` mounts the *node container's*
  `/dev`, not the host's. Verified: a host LV's `/dev/dm-N` and `/dev/bard-vg/*`
  do **not** appear inside a kind node. So a host VG (e.g. the RBD-backed
  `bard-vg`) is unreachable from the in-cluster pods — which is why the LVM
  plugin is proven against the real VG via its binary (see test recipe), not via
  kind. This is the same nested-`/dev` wall as krbd/rbd-nbd needing per-node
  `mknod`.
- **`findmnt --target` walks UP to the containing filesystem**, so it is non-empty
  for *any* path that resides on a mount — wrong for an "is this path itself a
  mount?" idempotency check (it always reports mounted and skips the real mount).
  Use `findmnt --mountpoint`. The fake runner models `--mountpoint` semantics, so
  this bug hid behind green tests until a real-VG run surfaced it.
- **CSI topology labels are immutable per node.** The node plugin reports its zone
  under `topology.csi.bard.io/zone` (sourced from the node's
  `topology.kubernetes.io/zone` label, read via the API -- `--zone-label`). The
  node-driver-registrar writes that to the Node once; if you *change* a node's
  zone, kubelet refuses with `detected topology value collision ... but existing
  label is ...` and the registrar crashloops. Fix: delete the stale
  `topology.csi.bard.io/zone` label from the node and restart the node pod so it
  re-registers. (One-time, only when changing an already-registered node's zone.)
- **Multi-cluster demo wiring:** label nodes per zone
  (`kubectl label node <n> topology.kubernetes.io/zone=<cluster>`), then per
  cluster add a `BackendCluster` (zone == that label), an instance in
  `bard-ceph-config`, and a key in `bard-ceph-keys`. A pod's zone (where it
  schedules) then picks the cluster -- one StorageClass, many Cephs. Proven with
  `hack/demo-multicluster.yaml` + `hack/test-multicluster.yaml` (a 2nd pool +
  scoped user on the same mon stands in for a 2nd cluster).
- **Do NOT pin `pool` on the StorageClass for multi-cluster.** The plugin uses
  the SC's `pool` param *over* the resolved instance's own pool, so a pinned pool
  forces every cluster to the same pool name -- it silently routed a kepler-zone
  volume into galileo's pool (then the scoped user couldn't map it). Leave `pool`
  off `bard-rbd`; each instance carries its pool in `bard-ceph-config`. Only pin
  it when all clusters genuinely share one pool name.
- **Mon on the kind host itself forces a rootful cluster.** Rootless podman
  (pasta) cannot reach the host's own LAN IP, and Ceph's monmap forces that
  address → the cluster MUST be **rootful**. Rootless kind silently can't reach
  the mon.
- **docker + podman coexisting on one host:** Docker sets `FORWARD` policy DROP, which
  blocks the podman bridge's internet egress → kind nodes can't pull sidecar
  images. The setup script adds a `DOCKER-USER` ACCEPT for the podman subnet.
- **krbd does not work in kind:** `rbd map` writes `/sys/bus/rbd/add`, which the
  kernel rejects from a nested kind node even privileged + `/sys` rw. Use the
  **`mounter: rbd-nbd`** option (userspace NBD). On real nodes, `krbd` (default)
  is fine and faster.
- **rbd-nbd needs `/dev/nbdN` nodes**, which kind nodes lack (no udev). The
  setup script `mknod`s them per node. The nbd module must be loaded on the host.
- **rbd-nbd map returns before the device is sized** → mkfs sees "size zero".
  The driver polls `blockdev --getsize64` (`waitForDevice`) after mapping.
- **Credentials are plugin-resolved per instance — core never sees a key.** Each
  backend *plugin* mounts its own keys Secret and reads the file named for each
  instance id: the Ceph RBD plugin reads `/etc/bard-ceph-keys/<instance>` (Secret
  `bard-ceph-keys`), CephFS `/etc/bard-cephfs-keys/<instance>`. The key is keyed
  off the instance in the volume handle / dispatch result. The StorageClass has
  NO CSI secret params; that is what lets one class address many clusters (a
  StorageClass secret template has no node-zone token to select a per-cluster
  secret). Real keys live in untracked `hack/secret*.yaml` (templates
  `*.example.yaml`); never commit one. A CSI-passed secret is still honoured as a
  plugin fallback.
- **LUKS encryption is node-plane and key state is derived, not stored.** An
  `encrypted: "true"` StorageClass marks the volume (carried to the node in the
  volume context); the node maps the rbd image, then `cryptsetup luksFormat`/`open`s
  it and mounts the decrypted `/dev/mapper/bard-luks-<hash>` -- Ceph only stores
  ciphertext. The passphrase is HKDF-derived from the instance's master key
  (Secret `bard-ceph-encryption`, mounted `/etc/bard-ceph-encryption/<instance>`,
  `--encryption-key-dir`) + the volume id, so it is the same on every restage with
  nothing persisted; a CSI `encryptionPassphrase` secret overrides it. The mapper
  name is derived from the staging path, so NodeUnstage closes it with no recorded
  state and the close is a no-op for unencrypted volumes. Rotating an instance's
  master key rekeys all its volumes -- keep masters backed up; losing one makes
  its encrypted volumes unrecoverable. The plugin image carries `cryptsetup-bin`.
  **Pluggable KMS:** the passphrase source is a `keyService` provider selected per
  volume by the `encryptionKMSID` StorageClass param (carried to the node in the
  volume context). Empty id => the derived master-key provider above. A configured
  id => its provider, from the `bard-ceph-kms` ConfigMap (`--kms-config`). The
  Vault provider (KV v2) generates a random per-volume passphrase and stores it in
  Vault create-only (cas=0), so the plaintext key lives only in Vault and node
  memory -- never in Ceph, the volume context, or plugin config. The
  **secrets-metadata** provider (`type: secrets-metadata`, alias `kubernetes`)
  needs no external KMS: it mints a per-volume random DEK, AES-GCM-wraps it with a
  KEK derived (HKDF) from the instance master key (the same `--encryption-key-dir`
  Secret the derived provider uses), and stores the wrapped DEK in rbd image-meta
  (`bard.encryptionDEK`) -- only ciphertext touches Ceph, and `deleteKey` is a
  no-op since `rbd rm` takes the metadata with it. Distinct from the derived
  default in that each volume's key is independent random material, not a
  deterministic function of the volume id. The whole KMS is plugin-internal (core
  never sees a key), so a plugin in any language ships its own providers. An
  explicit `encryptionPassphrase` CSI secret overrides any KMS.
  Node plugin reaches Vault over `hostNetwork`; a dev Vault runs on the host
  (`vault server -dev`), token in the untracked `bard-ceph-vault-token` Secret.
  **KMS cleanup on delete:** CSI `DeleteVolume` carries no volume context, so the
  KMS id is recorded on the image at create (`rbd image-meta set` key
  `bard.encryptionKMSID`); `DeleteVolume` reads it back and calls the provider's
  `deleteKey` (Vault: DELETE the KV metadata) BEFORE `rbd rm`, so a failure leaves
  the image + its recorded id for a retry (both steps idempotent). This means the
  **controller** plugin also needs `--kms-config` + the Vault token, not just the
  node. The derived provider's `deleteKey` is a no-op (nothing stored).
  **Vault auth:** `authMethod: token` (static, `tokenFile`) or `kubernetes` -- the
  latter logs in with the plugin's SA JWT (auto-mounted at the default projected
  path) for a short-lived token, cached until ~30s before lease expiry, with one
  re-login on a 401/403. Needs `vault auth enable kubernetes` configured against
  the kind API (Vault is on hostNetwork, so `kubernetes_host` can be the
  kubeconfig server URL via 127.0.0.1) with a reviewer SA bound to
  `system:auth-delegator`, and a role binding the plugin SAs (`bard-csi-node`,
  `bard-csi-controller`, kube-system) to a policy granting the KV path.
  **dm-crypt in nested kind:** needs the `dm_crypt` module loaded on the host
  (`modprobe dm_crypt`) since kind nodes share the host kernel; the plugin is
  already privileged with host `/dev`. If a live encrypted mount fails where the
  unit tests pass, suspect a missing host module (same class as nbd/rbd modules).
- **cephx client caps:** `client.k8s-csi-test` needs `mon 'profile rbd'` (a bad
  mon cap makes `rbd` hang fetching the OSDMap, not error).
- **krbd shares ONE rados client per (cluster,user) per node — cephx caps changes
  don't reach existing sessions.** Granting new caps (e.g. adding an EC `dataPool`'s
  pool) only affects NEW client instances; a node that already holds any krbd map
  for that user keeps its old ticket, and the OSDs return EPERM on the new pool —
  surfaced by the block layer as mkfs/mount `Input/output error` (dmesg:
  `rbd: ... result -1`, reads AND writes both). This masqueraded as "krbd doesn't
  support EC pools" for a whole session; krbd handles EC data pools fine (kernel
  4.11+, live-proven end to end 2026-07-02). Remedies: grant caps *before* the
  user's first map on a node, or SC `mapOptions: noshare` (fresh client instance
  per map). Related: an orphaned krbd map's watcher blocks `rbd rm`; the
  external-provisioner keeps retrying Released PVs, so the image + PV reap
  themselves the moment the stale map is unmapped.
- **Clone snapshots with format v2.** `rbd clone` defaults to v1, which rejects an
  unprotected parent snapshot ("parent snapshot must be protected") -- and the
  plugin does not protect snapshots. Pass `--rbd-default-clone-format 2`: v2 needs
  no protect step and lets the parent snapshot be deleted while clones exist. This
  broke restore-from-snapshot on Ceph 20.2 and hid behind the fake runner (which
  doesn't model protect) until a live restore surfaced it. The same clone path
  backs volume-group-snapshot restore.
- **Encrypted clone/restore must inherit the source's key.** A clone (`rbd clone`
  from a snapshot, or `rbd cp`) copies the source's LUKS header byte-for-byte, so
  the clone is encrypted under the *source's* passphrase -- but `rbd clone/cp`
  copies **no image-meta**, and every KMS provider keys off the new volume's
  identity, so a naive clone resolves the wrong key and `cryptsetup open` fails.
  `CreateVolume` now calls `inheritEncryption` on an encrypted clone: it copies the
  source's encryption descriptor (`bard.encryptionKMSID`, `bard.encryptionKeyID`,
  and any `bard.encryptionDEK`) onto the clone and, for Vault, duplicates the KV
  entry into the clone's own slot. So the clone is **self-contained** -- opens with
  the source key yet `DeleteVolume` removes only its own key (no shared-key
  ref-count hazard; deleting a clone never breaks the source). The key identity
  rides to the node in the volume context (`encryptionKeyID`); the derived provider
  re-derives from it, secrets-metadata/Vault use the clone's own copied slot. Two
  caveats: (1) **restore to a compatible (same-KMS) StorageClass** -- the ciphertext
  is bound to the source's provider, so restoring under a different `encryptionKMSID`
  fails to open; (2) the descriptor is read from the **source image at restore
  time**, so deleting the source image before restoring its snapshot (clone-v2
  allows this) loses the descriptor -- the data is intact but unopenable. Unit-proven
  for all three providers (`encryption_clone_test.go`: clone opens with the source
  key, clone-delete preserves the source, derived chains to the root identity).
  **Live-proven on the k3s krbd tier (2026-06-20)** for both deployed providers
  (`bard-rbd-encrypted` derived, `bard-rbd-encrypted-k8s` secrets-metadata): fresh
  encrypted PVC -> VolumeSnapshot -> restore-to-new-PVC mounts
  `/dev/mapper/bard-luks-*` with byte-identical data (so it opened with the
  inherited source key), the descriptor copy is visible in image-meta (the clone
  carries `bard.encryptionKeyID = <source pool/image>`, and for secrets-metadata a
  byte-identical `bard.encryptionDEK`), and deleting the clone reaps only its image
  with the source untouched. Also proven on a **real workload**: an encrypted
  PostgreSQL (1M pgbench rows, page checksums) snapshot->restored to a 2nd Postgres
  that came up clean (crash-consistent WAL replay), full heap scan -> 0 checksum
  failures, `pg_amcheck --heapallindexed` clean, row count + `sum(abalance)`
  identical pre/post. NOTE the k3s VMs lose `dm_crypt` on reboot (cloud-init only
  loads it at first boot); now persisted via `/etc/modules-load.d/dm_crypt.conf` on
  both VMs -- the symptom of a missing module is a fresh encrypted stage failing
  with `cryptsetup ... No usable keyslot is available` (a LUKS2 header with a digest
  but zero keyslots), same class as the nbd/rbd host-module gotchas.
- **AWS KMS provider is stdlib-only and emulator-testable.** `type: aws-kms`
  (`internal/cephplugin/kms_aws.go`) does envelope encryption: GenerateDataKey ->
  the plaintext DEK is the LUKS passphrase, the KMS-encrypted blob is stored in
  `bard.encryptionDEK` image-meta (same key secrets-metadata uses, so it inherits
  clone-support + no-op delete for free), Decrypt on reopen. SigV4 + the KMS JSON
  1.1 API are hand-rolled on net/http (NO aws-sdk-go -- keeps the image lean), so an
  `endpoint:` override points it at a VPC endpoint or a local emulator. Creds:
  `credentialsFile` (mounted Secret, AWS shared-creds ini) > inline > `AWS_*` env.
  **Live-proven 2026-06-20 on k3s** against a local emulator: encrypted PVC ->
  snapshot -> restore mounts `/dev/mapper` with byte-identical data (clone Decrypts
  the inherited DEK), self-contained on clone-delete, pool back to baseline. GOTCHA:
  **LocalStack `latest` (2026.x) now refuses to boot without a paid license token**
  (exit 55, "License activation failed") -- the free KMS emulator path is
  `nsmithuk/local-kms` instead (`podman run -d -p 4599:8080 nsmithuk/local-kms`,
  speaks the KMS API, CreateKey via curl with `X-Amz-Target: TrentService.CreateKey`,
  ignores SigV4). Reachable from the k3s VMs at the host's LAN IP
  (`<host-lan-ip>:4599`) like the mon. Config wired in `bard-ceph-kms` (provider `aws-localkms`) + SC
  `bard-rbd-encrypted-aws`.
- **Azure Key Vault provider stores the passphrase as a KV secret.** `type:
  azure-kv` (`internal/cephplugin/kms_azure.go`) is the secret-store model (ceph-csi
  azure-kv parity), so structurally it is the Vault provider with the Key Vault REST
  surface + AAD auth: PUT/GET/DELETE `https://<vault>.vault.azure.net/secrets/<hash>`
  with a bearer token. External store => it implements `keyCloner` (clone gets its
  own secret copy; self-contained, like Vault). Auth is AAD client-credentials
  (`tenantId`/`clientId`/`clientSecret[File]`) or `authMethod: token` (static, for
  emulators). stdlib REST+OAuth, no Azure SDK. `caFile`/`insecureSkipVerify` for an
  emulator or Azure Stack endpoint. Hermetic-proven (fake AAD + KV server: token
  flow, bearer, secret CRUD, clone self-containment, static-token, error path) AND
  **live-proven IN-CLUSTER on k3s 2026-06-20** against the `lowkey-vault` emulator
  (`hack/setup-lowkey-vault.sh`). Full lifecycle on SC `bard-rbd-encrypted-azure`:
  provision stores the LUKS passphrase as a Key Vault secret (lowkey secret count 1,
  image-meta only `bard.encryptionKMSID=azure-kv` -- the secret lives in Key Vault,
  not the image; Ceph = LUKS ciphertext), snapshot->restore writes an INDEPENDENT
  secret via cloneKey (count 1->2) + decrypts, delete clone removes only its secret
  (->1, source intact), delete source removes its secret (->0, no orphan). Used
  `authMethod: token` with a dummy bearer (lowkey-vault doesn't validate AAD) +
  `insecureSkipVerify` (self-signed cert) -- the static-token mode is what made Azure
  in-cluster tractable. GOTCHAS: lowkey-vault routes vaults by **host:PORT**, so the
  vault alias to the host's IP MUST include the port (`bard.localhost=<host-lan-ip>:8453`)
  or every request 404s "Unable to find active vault"; and it runs on **8453** (8443
  is typically the Ceph dashboard -- `ceph config set mgr mgr/dashboard/server_port`
  frees it if ever needed). The optional `bard-ceph-azure` Secret (mounted `/etc/bard-ceph-azure`,
  deploy 30/40) carries a real Azure `clientSecretFile`/CA for production (the
  emulator test uses the inline dummy token instead). Config: SC
  `bard-rbd-encrypted-azure`, commented provider in `bard-ceph-kms`.
- **KMIP provider stores the passphrase as a SecretData object on an HSM.** `type:
  kmip` (`internal/cephplugin/kms_kmip.go`) is the on-prem / FIPS / federal path and
  ceph-csi's kmip parity model: Register a SecretData over mutual-TLS KMIP, record
  its UID in `bard.encryptionKMIPUID` image-meta, Get on reopen, Destroy on delete
  (the object lives in the HSM, so unlike the wrapped-DEK providers it is NOT reaped
  by rbd rm -- DeleteVolume Destroys it before rbd rm, while the UID meta still
  exists). External store => implements `keyCloner` (clone Registers an INDEPENDENT
  object; self-contained). **This is the ONE provider that pulls a dependency**
  (`github.com/gemalto/kmip-go` -- TTLV is a binary protocol not worth hand-rolling):
  binary 11M->12M, ~1% of the image -- an accepted lean-image tradeoff.
  Proven two ways: hermetic (the library's own in-process KMIP server over TLS:
  Register/Get/Destroy round-trip, clone self-containment, idempotent destroy) AND
  **live against PyKMIP** (a different implementation -- real interop) via the
  env-gated `TestKMIPLive`:
  ```sh
  # PyKMIP in a container (python:3.11; latest pip-installs pykmip). Generate ECDSA
  # certs (see below) into /tmp/pykmip, mount at /certs, auth_suite=TLS1.2.
  podman run -d --name bard-pykmip -p 5696:5696 -v /tmp/pykmip:/certs:Z python:3.11-slim \
    bash -c "pip install -q pykmip && pykmip-server -f /certs/server.conf"
  BARD_CSI_KMIP_TEST=1 KMIP_ENDPOINT=127.0.0.1:5696 \
  KMIP_CLIENT_CERT=/tmp/pykmip/client.crt KMIP_CLIENT_KEY=/tmp/pykmip/client.key \
  KMIP_CA=/tmp/pykmip/ca.crt go test ./internal/cephplugin -run TestKMIPLive -v
  ```
  GOTCHA (cost a real debugging loop): PyKMIP's `TLS1.2` auth_suite offers
  `ECDHE-ECDSA-*-GCM` (modern, Go-default-compatible) only for an **ECDSA** server
  cert; with an RSA cert it offers only legacy `ECDHE-RSA-*-CBC-SHA256/384`, which a
  modern Go client won't negotiate => `tls: handshake failure`. Fix is an ECDSA
  server cert, NOT weakening the client's cipher defaults. Also: `tls.Client` (per-
  request) needs `ServerName` set explicitly (unlike `tls.Dial`); the provider
  defaults it to the endpoint host. **Also live-proven IN-CLUSTER on k3s 2026-06-20**
  (the full pod/PVC/snapshot lifecycle, not just the client `TestKMIPLive`): wired the
  `kmip` provider into `bard-ceph-kms` pointing at the PyKMIP container at the host's
  `<host-lan-ip>:5696` (reachable from the VMs like the mon -- needs the server cert
  SAN to include that IP; `hack/setup-pykmip.sh` now adds it), mounted the client
  cert/key + CA as the `bard-ceph-kmip-certs` Secret (an optional secret volume at
  `/etc/bard-ceph-kmip` added to deploy 30-controller/40-node, mirroring the vault
  token). Proven on SC `bard-rbd-encrypted-kmip`: provision Registers the LUKS
  passphrase as a SecretData in PyKMIP (image-meta `bard.encryptionKMIPUID=1`, Ceph
  holds only LUKS ciphertext), snapshot->restore Registers an INDEPENDENT 2nd object
  (PyKMIP count 1->2) and decrypts, deleting the clone Destroys only its object
  (back to 1, source intact), deleting the source Destroys its object (count 0 -- NO
  HSM ORPHAN). PyKMIP object count read from its sqlite `managed_objects`. Config: SC
  `bard-rbd-encrypted-kmip`, commented provider in `bard-ceph-kms`. KMS providers are
  now COMPLETE for ceph-csi parity (derived, secrets-metadata, Vault, AWS KMS, Azure
  KV, KMIP) and **all six are live-proven in-cluster on k3s** (the full pod/PVC/
  snapshot lifecycle) as of 2026-06-20.
- **fscrypt is the filesystem-level alternative to block LUKS.** StorageClass
  `encryptionType: file` (vs the default `block`) -> `internal/cephplugin/fscrypt.go`.
  The volume is a plain ext4 with the kernel encrypt feature (`mkfs.ext4 -O encrypt`);
  after mount, NodeStage adds an fscrypt master key (HKDF of the SAME KMS passphrase
  the LUKS path uses -- so fscrypt composes with EVERY KMS provider) to the FS keyring
  via `FS_IOC_ADD_ENCRYPTION_KEY` and sets a v2 policy on `<staging>/bard-fscrypt`;
  NodePublish bind-mounts that encrypted subdir as the pod's volume root (the FS root
  with lost+found stays hidden). Uses x/sys/unix ioctls directly (no fscrypt CLI, no
  new dep). The key lives in the per-superblock keyring (dropped on unmount), so
  every stage re-adds it -- nothing persisted, re-derived like the derived LUKS key.
  Idempotent: re-add key + `SET_ENCRYPTION_POLICY` returning EEXIST are both success.
  Restrictions: **ext4 only**, **not for raw block** volumes (no filesystem to host
  the policy) -- both rejected at NodeStage. The crypto trade-off vs LUKS: fscrypt
  encrypts file contents + names but NOT filesystem metadata or free space. Like
  LUKS's real cryptsetup, the ioctls can't run under the fake runner, so they're
  **live-proven, not unit-faked** (unit tests cover the key derivation + LUKS-skip +
  isFsCrypt). **Live-proven on k3s 2026-06-20** (`bard-rbd-encrypted-fscrypt` SC,
  kernel `CONFIG_FS_ENCRYPTION=y`): wrote a marked plaintext, then `rbd export` showed
  the Ceph image holds ONLY ciphertext -- the content marker, a 200KB plaintext run,
  AND the filename all absent; restage re-derived the key and read the data back; and
  it composes with snapshot/clone (a restored fscrypt PVC decrypts via the inherited
  `encryptionKeyID` key). **Online expand works for both encryption modes
  (live-proven 2026-06-20, 1Gi->2Gi, data intact).** `NodeExpand` now: (1) strips the
  bind subpath from `findmnt SOURCE` (`/dev/rbdN[/bard-fscrypt]` -> `/dev/rbdN`) so
  resize2fs hits the bare device -- the fscrypt fix; (2) for a LUKS mapper, runs
  `cryptsetup resize <name> --key-file <pass>` BEFORE resize2fs, re-resolving the
  passphrase through the KMS exactly as DeleteVolume does (KMS id + key id read from
  image-meta -- works for every provider + clones). GOTCHA: `cryptsetup resize`
  without a key file fails `Nothing to read on input` -- it prompts for the volume key
  because the kernel keyring isn't reliably reachable across plugin invocations in a
  container; supplying the passphrase via `--key-file` is the fix (this also closes a
  pre-existing gap: LUKS online expand never worked before -- resize2fs was hitting an
  un-grown mapper). The LUKS-resize path is live-proven, not unit-faked (it needs the
  real KMS harness, like the LUKS open path).
- **CephFS encryption is fscrypt-only and shares the RBD KMS via `internal/cephenc`.**
  The KMS providers + fscrypt helpers were extracted from the ceph-rbd plugin into a
  backend-agnostic `internal/cephenc` package (a 4-method `Host` interface:
  MasterKeyDir/ConnFor/MetaGet/MetaSet; LUKS stays RBD-side). The CephFS plugin
  implements Host with **subvolume metadata** as the key store (spec = `<fs>/<subvol>`,
  `ceph fs subvolume metadata get/set`), so all six providers work. `encrypted: "true"`
  on a `bard-cephfs` SC -> fscrypt-encrypted `bard-fscrypt` subdir inside the subvolume,
  bind-published as the pod root. **Live-proven on k3s 2026-06-21** (kernel 6.8, derived
  provider): fresh encrypted PVC mounts, host-side `mount` of the subvolume WITHOUT the
  key shows the **filename encrypted** (`rU27Lgt...,AE`, plaintext name + content marker
  both absent in Ceph) and content reads `Required key not available`; restage re-derives
  the key and reads back. GOTCHAS hit live (two): (1) **the kernel CephFS client needs
  `ms_mode=` to speak msgr2** -- a v2-only mon port (`:3300`) makes the kernel mount fail
  `socket closed (con state V1_BANNER)` / "no mds server is up" (RBD's librados/krbd
  negotiate v2 automatically; the kernel cephfs client defaults to v1). Fix: set
  `mountOptions: [ms_mode=prefer-crc]` on the cephfs SC (Bard threads SC mountOptions ->
  the kernel mount), or use the v1 port `:6789`. Also needs `modprobe ceph` on the nodes
  (the in-container mount.ceph can't modprobe -- same class as the rbd/nbd/dm_crypt
  host-module gotchas). (2) **Encrypted CephFS volumes CANNOT be restored from a snapshot
  or cloned** -- `ceph fs subvolume snapshot clone` copies an fscrypt-encrypted
  subvolume's ciphertext as **opaque data** without preserving the fscrypt context (the
  clone's file shows the padded ciphertext size, 4096, not the 37-byte plaintext i_size),
  so the cloned tree is unmountable and NodeStage blocks in the fscrypt ioctl. Unlike RBD
  (block-level clone copies the LUKS header+ciphertext byte-for-byte), CephFS fscrypt +
  subvolume clone do not yet compose. `CreateVolume` rejects the combination fail-fast
  (`InvalidArgument`, PVC stays Pending) rather than hang the node -- live-verified. The
  fscrypt ioctl path is live-only (can't run under the fake runner), like RBD fscrypt.
  Chart: the cephfs profile in `_profiles.tpl` now defines `encryptionMount`/`kmsMount`
  so `plugins.cephfs.encryption`/`kms` wire `--encryption-key-dir`/`--kms-config`.
- **rbd group snapshots are not the basis for VolumeGroupSnapshot here.** Verified:
  `rbd group snap create` makes crash-consistent per-image snapshots in the
  `group` namespace (`.group.2_...`), but those are NOT independently clonable
  (`rbd clone` can't find them) -- they're for whole-group rollback. CSI needs each
  member individually restorable, so Bard's VolumeGroupSnapshot instead snapshots
  each source volume with the normal CreateSnapshot and bundles them (core's
  GroupController). Upside: a group can span multiple instances/clusters. Downside:
  members are sequential, so per-volume crash consistent, not atomic across the
  group.
- **VolumeGroupSnapshot needs external-snapshotter v8.2.0, version-matched.** The
  group CRDs (`groupsnapshot.storage.k8s.io`), the snapshot-controller, and the
  csi-snapshotter sidecar must all agree on the API version, or nothing is
  processed (the VGS just sits with empty status, no content created). Gotchas hit
  live: (1) the v8.2.0 `setup-snapshot-controller.yaml` pins the **v8.0.1** image,
  which speaks the OLD `v1alpha1` group API and the OLD flag
  `--enable-volume-group-snapshots` -- it crashloops against the v8.2.0
  (`v1beta1`) CRDs ("could not find v1alpha1 volumegroupsnapshots"). Bump the
  snapshot-controller image to **v8.2.0**, whose flag is
  `--feature-gates=CSIVolumeGroupSnapshot=true` (BETA, default off) -- same gate
  as the csi-snapshotter sidecar (already set in deploy/30-controller.yaml). (2) A
  stale OLD-version snapshot-controller pod can keep holding the leader lease after
  the upgrade and silently ignore group snapshots; make sure only the new pods
  run. The driver advertises GROUP_CONTROLLER_SERVICE (Identity) + the
  GroupController service. Live-proven: a 2-PVC group snapshot -> 2 ready member
  VolumeSnapshots -> each restored to its own PVC with the correct distinct data.
- **`profile rbd` grants `osd blocklist add` but NOT `blocklist rm`.** Verified:
  `ceph ... osd blocklist add 1.2.3.4:0/0` succeeds, `... rm` returns EACCES. So
  the single-writer fence path (NodeStage blocklists a stale watcher of an
  exclusive volume before taking it over) works on the scoped user as-is, and it
  must NOT depend on un-blocklisting -- Ceph auto-expires blocklist entries
  (default 1h), which is the intended cleanup when the old node is gone.
- **Single-writer fencing isn't reproducible in nested kind, but IS proven on the
  k3s tier.** In kind a host `rbd status` does NOT see an in-cluster rbd-nbd map's
  watcher (separate netns), so there the fence is only unit-test-proven (seeded
  `rbd status`) -- confirm it doesn't regress normal RWO staging and no more. On
  the **real-kernel k3s VMs the failover fence runs for real and is verified**:
  the host mon sees each node's krbd watcher (`<node-ip>:0/<nonce>`), so stopping
  a node with a staged RWO volume + force-failing the volume over makes the new
  node's NodeStage `ceph osd blocklist add` the stale watcher (it shows up in
  `osd blocklist ls`) before taking over. See the k3s tier section.
- **NFS + kind co-located:** kind exhausts the default `fs.inotify.max_user_instances=128`,
  which starves the host NFS server's `nfsdcld`/`mountd` and makes **NFSv4 mounts
  hang** (host loopback *and* from pods). Fix: `sysctl -w fs.inotify.max_user_instances=8192`
  (the setup script now does this) and restart `nfsdcld nfs-mountd nfs-server`.
- **After driver code changes, rebuild AND reload the image** before deploying.
  A stale `:dev` image silently runs old code — symptom seen: `unknown backend
  type "nfs"` (old binary) when the source already had plugin support. Do all
  three together (build + `kind load` + restart pods — see the e2e section, or
  the `Makefile` `redeploy` recipe) so build and deploy can't drift.

## Ceph cluster (dev)

Any reachable Ceph cluster works for the dev loop — a single-host cephadm
install is enough. You need a pool and a scoped cephx user (key goes in the
untracked `hack/secret.yaml`; template `hack/secret.example.yaml`). Verify
connectivity before debugging anything else:
`rbd --id <user> --key <key> -m <mon-host>:3300 -p <pool> ls`.
Record your cluster's actual addresses in `CLAUDE.local.md`.

## Known limitations

- **rbd-nbd unmap is non-deterministic in this nested kind cluster.** In the
  triple-nested dev setup (pod -> kind-node-container -> rootful-podman -> host),
  `rbd-nbd unmap <device>` sometimes does not free the device, so a deleted PVC
  can keep an rbd watcher that blocks `rbd rm` (orphaned image). This is an
  *environment* artifact: on real nodes rbd-nbd unmap is reliable.

  The plugin's node plane is written to be production-correct and to NOT mask
  this: NodeStage records the mapped device (so NodeUnstage finds it even when
  the staging mount is gone) and is idempotent (won't double-map on retry);
  NodeUnstage verifies the device is actually freed and, if not, returns an
  error so kubelet retries -- it never reports success while leaking. So in kind
  you'll see NodeUnstage retry-loop with "device ... still mapped after unmap"
  rather than a silent leak; on a real node the unmap succeeds and the loop ends.
  Control plane (provision/snapshot/expand) and the mount path are unaffected.

## Known follow-ups

- More backends (cloud disk).
- **iSCSI + control-plane attach: DONE.** `cmd/bard-plugin-iscsi` +
  `internal/iscsiplugin` is the reference **attach-style** backend, and wiring it
  meant making `ControllerPublish`/`Unpublish` real end to end (they were declared
  but inert). The contract gained `/controller/publish` + `/controller/unpublish`
  (optional `ControllerPublisher`), core gained the RPCs + the `PUBLISH_UNPUBLISH`
  capability (advertised ONLY when a registered backend attaches), and PublishContext
  is threaded into NodeStage. Because `CSIDriver.attachRequired` is one cluster-
  global immutable field, attach is a deploy/chart toggle (`attach.enabled`, default
  off) that flips it + adds the external-attacher; node-mapped backends no-op the
  publish. See `deploy/examples/attach/` + `deploy/examples/iscsi/`.
  **iSCSI snapshots/clone + CHAP: DONE** (2026-07-10, branch
  `feat/iscsi-snapshots-chap`): thin-LV snapshots/restore/clone mirroring the LVM
  plugin (instance/SC `thinPool`; clone exported through its own target, fs grown
  at stage) + per-instance CHAP (`chapAuth: true`, creds Secret mounted on both
  planes) -- all live-proven by the extended `hack/iscsi-plugin-test.sh`.
  **iSCSI dm-multipath: DONE** (2026-07-19, branch feat/iscsi-multipath):
  instance `portals` list (2+ entries) -> explicit per-address LIO portals
  (must delete BOTH default-portal forms first -- current targetcli auto-creates
  `::0:3260` dual-stack, not `0.0.0.0:3260`; deleting a portal only removes the
  LISTENER, live sessions survive), node logs in through every portal and mounts
  the multipathd-assembled mapper via its `dm-uuid-mpath-<wwid>` by-id link
  (name-independent; wwid from sysfs, `naa.<hex>` -> mpath id `3<hex>`; `multipath
  -f` REJECTS the by-id symlink -- resolve to the dm node first). NodeUnstage
  flushes best-effort BEFORE logout but takes its authoritative map-gone check
  AFTER -- a flushed map re-assembles while paths live (wedged in-cluster unstage
  forever until reordered). PublishContext gains `portals` (additive; `portal`
  stays = first). Host prereq: multipathd on nodes. Proven: unit suite,
  `hack/iscsi-multipath-test.sh` (traffic-cut failover under live I/O -- LIO
  portal delete does NOT fail a path), single-portal harness + conformance
  regressions, and fully in-cluster (2-node k3s: cross-node 2-path mapper mount,
  portal-IP loss under live I/O, recovery, online expand through the mapper via
  in-container `multipathd resize map`, snapshot/restore each with own map,
  plugin-restart + delete-all -> zero leaked sessions/maps/LVs).
  **iSCSI remote LIO management (`management: targetd`): DONE** (2026-07-20,
  branch `feat/iscsi-targetd`): an instance can name `management: targetd` +
  `targetdEndpoint`/`targetdPool`/`targetIqn` (no `vg`) to have the controller
  drive a REMOTE LIO host over [targetd](https://github.com/open-iscsi/targetd)'s
  JSON-RPC API (`:18700`) instead of local targetcli/configfs -- the controller
  no longer needs to run on the target host, closing the one remaining
  same-host coupling the attach-style backend had. targetd exposes every
  volume as a LUN under ONE FIXED target IQN (unlike local mode's
  one-target-per-volume), so `internal/iscsiplugin`'s node plane gained
  generic shared-target session refcounting (`otherRecordsForIQN` scans
  recorded state for other volumes sharing a target IQN; `withTargetLock`, a
  per-target flock, closes the TOCTOU where two concurrent unstages of the
  last two volumes on a target both see the other's record and both skip the
  final logout): NodeStage rescans instead of re-logging in when a session is
  already up, NodeUnstage detaches only the one SCSI device (a raw
  `<sysfs>/class/block/<sd>/device/delete` write) and leaves the session up
  unless it is the last volume. Local mode hits the identical code path but,
  since its target IQN is unique per volume, always resolves to "last" --
  proven unchanged by construction, not a mode check. Two things a targetd
  instance rejects cleanly (`Unsupported`/`InvalidArgument`, never silently):
  **snapshots/clones** (targetd's `vol_copy` is a synchronous full copy,
  unsafe under provisioner retries) and **CHAP** (`chapAuth: true` --
  live-verified against targetd 0.10.4 that `export_create` unconditionally
  hardcodes the shared target's TPG `authentication` attribute to `"0"` on
  every export with no API to override it, so credentials set via its
  `initiator_set_auth` are never actually enforced: a packet capture showed
  the login response still advertising `AuthMethod=CHAP`, but the kernel
  initiator aborting before the actual challenge, for both a correct password
  and none at all -- access control on a targetd instance is IQN-based ACLs
  only). Credentials: `bard-iscsi-targetd` Secret, 2-line (username,
  password) per instance, mounted **controller-only** (`--targetd-dir`) --
  the node plane never builds a targetd RPC client. Chart passthrough gates
  the same way chap does but into the controller volumes/args only.
  Host-level proof: `hack/targetd-plugin-test.sh` against a real targetd (the
  fixture firewalls the admin API to loopback by default -- `EXPOSURE` note in
  `hack/setup-targetd-fixture.sh` -- a real deployment must open `:18700`
  deliberately to exactly the controller's subnet). **Fully in-cluster
  live-proven 2026-07-20** on the site2 dev2/dev3 k3s tier: targetd installed
  on dev2 (the existing LIO target host, separate loop-backed
  `bard-targetd-vg`), controller pinned to **dev3 -- the NON-target node, the
  actual proof** -- provisioned two volumes, cross-node attach (ACL for
  `init-dev3` created via the remote RPC), a 2nd volume on the SAME node
  sharing ONE iscsiadm session (distinct LUN devices, live-verified via the
  node's `iscsiadm -m session`), unstage ordering both ways (not-last: session
  stays up, only that volume's `/dev/sdX` disappears; last: full logout),
  online expand 1Gi->2Gi with no pod restart (targetd `vol_resize` + node fs
  grow, data intact), and a full teardown back to targetd `vol_list`/
  `export_list` empty with zero orphaned k8s objects. Cluster restored to its
  local-only standing config afterward (targetd stays installed on dev2,
  unused, port back to loopback-only).
- **ListVolumes / ListSnapshots: DONE (all first-party Go plugins).** Optional CSI
  RPCs, aggregated + paginated (offset token) in core across backends, snapshots
  filterable by source/snapshot id; advertised only when a registered backend
  lists. Each plugin opts in via the `VolumeLister`/`SnapshotLister` interfaces:
  ceph-rbd (`rbd ls`/`rbd snap ls`), cephfs (`fs subvolume ls`/`snapshot ls`), nfs
  (readdir the export / `.snapshots`), lvm + iscsi (`lvs`, filtered to `bard-`/
  `snap-`, excluding thin pools). iSCSI has no snapshots so no SnapshotLister.
  **NFS snapshot provenance:** the tarball name doesn't record its source, so
  CreateSnapshot now writes a `.snapshots/<id>.src` sidecar; a snapshot without one
  carries no source and is dropped by core's handle validation. Only the Python
  localpath demo stays non-listing (deliberately minimal). csi-sanity covers the
  list/pagination/filter specs against ceph-rbd via the fake runner.
- **lvm + iscsi operability: DONE.** Both now also implement GetCapacity (VG
  vg_free), GetVolumeHealth (LV exists), and NodeReclaimSpace (fstrim -> frees the
  thin pool on a thin LV) -- the optionals ceph-rbd already had.
- **Non-Go plugin demo: DONE.** `plugins/localpath/bard-plugin-localpath` is a
  backend plugin written in **Python, stdlib only** (no Go, no SDK, no deps) --
  proof the contract is just HTTP+JSON over a unix socket and so language-agnostic.
  A volume is a subdirectory bind-mounted on the node (shared-base-path model, like
  LVM's shared VG; `NodeLocal=false`). Proven by `hack/localpath-plugin-test.sh`
  (trap-cleaned; drives the full contract over the socket with `curl` and writes
  data through the bind mounts). Packaging: `Dockerfile.plugin-localpath`
  (python:3-slim + util-linux), `deploy/examples/localpath/`.
- **Plugin live config reload.** Core now live-watches the BackendCluster CRD and
  hot-swaps its registry+dispatcher with no restart, but a plugin still reads its
  OWN per-instance config (e.g. `bard-ceph-config`) at *its* startup -- so adding
  a brand-new instance also needs the plugin sidecar to reload to learn the new
  mon/pool/user. (Re-pointing zones / changing the default / removing an instance
  of an already-known backend is fully live.)
- **Multi-zone dispatch: DONE** and proven end to end (`hack/demo-multicluster.yaml`
  + `hack/test-multicluster.yaml`): one `bard-rbd` StorageClass, a pod per zone,
  each volume provisioned into a different Ceph instance/pool via that cluster's
  own scoped user. The dev stand-in is a 2nd pool (`k8s-csi-kepler`) + user on the
  same mon; a real 2nd cluster only changes `kepler`'s `monitors`. Remaining:
  swap in a genuinely separate cluster when hardware exists.
- **True node-local LVM.** The LVM plugin today is *shared-VG* (the controller
  drives lvcreate/lvremove; `NodeLocal=false`), which fits a VG every node can
  reach. Real per-node LVM (the VG is backed by one node's disks) needs a node
  agent reconciling a per-volume CRD — the TopoLVM pattern — because CSI
  `DeleteVolume` runs on the controller, which cannot reach another node's VG.
  This is also what an in-cluster kind E2E would require (host VG is invisible to
  the nested nodes).
- ~~Image `ceph-common` is Pacific 16.2.15 (Debian bookworm)~~ DONE: the ceph-rbd
  and cephfs plugin images now install the Ceph client from the UPSTREAM apt repo
  (`download.ceph.com/debian-<CEPH_RELEASE>`, build-arg default `tentacle` = v20,
  matching the clusters) on the same bookworm-slim base — current client + CVE
  cadence without ceph-csi's ~GB quay.io/ceph/ceph base.
