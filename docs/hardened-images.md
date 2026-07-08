# Hardened / distroless images, and building on your own base

Bard is built so you can run it on a minimal, CVE-tracked base (distroless,
Chainguard, UBI-micro, …) without forking. This doc covers what makes that
possible, how to do it, and the honest gaps.

## What's already in your favour

- **Every binary is `CGO_ENABLED=0` static.** Core and all plugin binaries link
  no libc, so they run on *any* base — including `scratch`/distroless `static`,
  where a dynamically linked binary would fail with a missing-loader error.
- **No shell dependency at runtime.** The Go plugins invoke their tools with
  `exec.CommandContext(name, …)` (PATH lookup) — never `sh -c` — so a shell-less
  base is fine. (The localpath plugin is the one exception: it's a Python script,
  so it needs a Python interpreter, not a shell.)
- **No hardcoded tool paths.** Tools are resolved on `PATH`, so it doesn't matter
  whether your base puts `mount` in `/usr/bin`, `/bin`, or `/sbin`.
- **Core carries no storage tooling at all.** It proxies every storage op to a
  plugin over a socket, so the core image is just the static binary — already on
  a minimal base by default (`cgr.dev/chainguard/static`).

This is the payoff of the plugin split: in a monolithic CSI driver the image
inherits the *union* of every backend's tooling, so the least-hardenable
dependency (the Ceph client) sets the floor for the whole driver. Here, each
plugin's tooling footprint is isolated to its own image, so you harden each one
independently and the heavy ones don't contaminate the rest.

## The `RUNTIME_BASE` build-arg

Every Dockerfile takes `RUNTIME_BASE`, so swapping the final base is a build-arg,
not a fork:

```sh
# core on Google distroless instead of the default Chainguard static
podman build --build-arg RUNTIME_BASE=gcr.io/distroless/static-debian12 \
  -t my/bard-csi -f Dockerfile .

# a plugin on your licensed Chainguard org base
podman build --build-arg RUNTIME_BASE=cgr.dev/my-org/wolfi-base \
  -t my/bard-plugin-lvm -f Dockerfile.plugin-lvm.hardened .
```

The catch for the **tool-carrying** plugins (everything except core): the package
*install* layer is package-manager-specific. Debian uses `apt-get`; Chainguard
Wolfi/Alpine use `apk`. So crossing base *families* means swapping the install
line, not just `RUNTIME_BASE`. Two patterns are provided:

- **localpath** does it in one Dockerfile via a `PKG` build-arg (`apk` default,
  `--build-arg PKG=apt` for Debian) — it only needs two packages, so the branch
  is trivial. It ships hardened-by-default (Wolfi).
- **LVM** keeps the proven Debian `Dockerfile.plugin-lvm` as default and adds
  `Dockerfile.plugin-lvm.hardened` (Wolfi) as a worked tool-carrying example.

For your own hardened build of any plugin, the portable contract is just: **start
from the static binary** (build it, or pull it from a release) and put the runtime
tools below on `PATH`. Assemble that however your pipeline likes — a Dockerfile,
or declaratively with `apko` (image = a package list) + `melange` (to build any
APK that isn't in a catalog; see the gaps below).

## Per-plugin runtime dependencies (as binaries, not package names)

Express deps as the binaries the plugin execs — package names differ by base.

| Plugin | Runtime binaries needed |
| --- | --- |
| **core** | *(none — static binary only)* |
| **localpath** | `python3`, `mount`, `umount`, `findmnt` |
| **nfs** | `mount.nfs` (nfs-utils), `mount`, `umount`, `findmnt`, `tar`, `gzip` |
| **lvm** | `lvcreate`/`lvremove`/`lvextend`/`lvs` (lvm2), `mkfs.ext4`, `mkfs.xfs`, `mount`, `umount`, `findmnt`, `blkid`; **thin pools also need** `thin_check`/`thin_repair` (thin-provisioning-tools) |
| **ceph-rbd** | `rbd`, `rbd-nbd` (ceph-common + rbd-nbd), `cryptsetup`, `mkfs.ext4`, `mkfs.xfs`, `mount`, `umount`, `findmnt`, `blkid` |
| **cephfs** | `mount.ceph` (ceph-common), `ceph-fuse`, `mount`, `umount`, `findmnt` |

## Debian → Wolfi package mapping, and the gotchas

| Need | Debian package | Wolfi package(s) |
| --- | --- | --- |
| `mount`/`umount` | `util-linux` | **`mount`, `umount`** (separate!) |
| `findmnt` | `util-linux` | **`findmnt`** (separate!) |
| `blkid` | `util-linux` | `blkid` |
| `mkfs.ext4` | `e2fsprogs` | `e2fsprogs` |
| `mkfs.xfs` | `xfsprogs` | `xfsprogs` |
| lvm tools | `lvm2` | `lvm2` |
| `cryptsetup` | `cryptsetup-bin` | `cryptsetup` |
| `python3` | `python3` | `python3` |
| `mount.nfs` | `nfs-common` | `nfs-utils` |
| `tar`,`gzip` | `tar`,`gzip` | `tar`,`gzip` |

**The split gotcha:** Debian's `util-linux` bundles `mount`/`umount`/`findmnt`;
Wolfi ships each as its own package. Installing only `util-linux` on Wolfi leaves
those binaries missing — verified, and the reason `Dockerfile.plugin-lvm.hardened`
lists `mount umount findmnt` explicitly.

## The honest gaps (where a license / custom APK earns its keep)

These two dependencies are **not in the public Wolfi catalog**, so a fully
hardened build of the affected images needs a custom APK (build it with `melange`)
or a licensed Chainguard repo that carries it:

- **Ceph client** (`ceph-common`, `rbd-nbd`, `ceph-fuse`) → blocks hardened
  **ceph-rbd** and **cephfs** images. This is the big one. Until you supply it,
  those two stay on a slim Debian base while everything else goes hardened — which
  is exactly the per-image isolation the plugin split buys you. Note the slim
  Debian images still get a *current* client: they install it from the upstream
  Ceph apt repo (`CEPH_RELEASE` build-arg), not Debian's EOL snapshot — so
  staying un-hardened there costs base-image CVE posture, not client currency.
- **thin-provisioning-tools** (`thin_check`/`thin_repair`) → the hardened **lvm**
  image supports **thick LVM only**. The binary detects thin from `lv_attr` at
  runtime and degrades cleanly: thick volumes work; a thin StorageClass simply
  fails when `lvm2` can't find the thin tools. Add the package to recover thin.

So, answering the two design questions directly:

- *With a Chainguard license, can a CD pipeline add the needed packages to a
  hardened base for a plugin image?* **Yes** — `apk add` from your authenticated
  repos, or assemble with `apko`/`melange` (the latter also lets you build the
  Ceph/thin APKs the public catalog lacks).
- *Do we provide enough to build the plugins on your own base?* **Yes** — the
  static binary is the portable artifact, `RUNTIME_BASE` swaps the base without a
  fork, and the tables above are the complete runtime-dependency contract.

## Running core nonroot: the plugin socket + fsGroup (verified in K8s)

A hardened base often defaults to a **nonroot** user (`cgr.dev/chainguard/static`
is UID 65532), whereas Google distroless `static` defaults to root. That surfaces
a real issue the moment core runs nonroot, because core talks to each plugin over
a unix socket the plugin *creates*:

- The plugin (typically root) creates its socket ~0755. `connect()` needs **write**
  on the socket, so a nonroot core gets `connect: permission denied` — core fails
  to build the driver and crashloops. (Seen live; a plain `docker run` as root
  hides it — you only catch it in K8s.)

The fix, with least privilege in mind (the socket drives the full plugin API, so
it must not be loose):

1. **The plugin chmods its socket to `0660`** (not `0666`) — done in
   `pkg/bardplugin.Serve` and the Python plugin. Owner + group only; never "other".
2. **The pod sets a shared `fsGroup`** (chart `podSecurityContext.fsGroup`, default
   65532). The `plugins` dir is a per-pod **emptyDir**, which Kubernetes chowns to
   that fsGroup with the setgid bit, so the socket is created group-owned by
   fsGroup, and every container in the pod (each gets fsGroup as a supplemental
   group) can connect. Verified live: `srw-rw---- root 65532 ceph-rbd.sock`.

**Security boundary.** The socket lives in a per-pod emptyDir, so it is reachable
only by (a) the pod's own containers — already one trust domain — and (b) root on
the node, which is already omnipotent. It is **not** world-writable and **not**
visible to other pods or the network. The `0660` (vs `0666`) choice matters as
defense-in-depth: were the plugins dir ever repointed at a shared `hostPath`,
`0666` would let any pod mounting it drive storage ops; `0660` + a dedicated
fsGroup does not.

**The node core is the exception — it runs as root.** Unlike the controller core,
the node core *binds* its CSI socket in `/csi`, a kubelet **hostPath** (root-owned)
that fsGroup does not manage, so a nonroot user can't bind it. The chart pins the
node core to `runAsUser: 0`. This matches upstream CSI node plugins (ceph-csi's
`csi-rbdplugin` and GCP PD's `gce-pd-driver` node containers run privileged with no
`runAsUser`, i.e. root, because the mount path needs `CAP_SYS_ADMIN`). The image is
still hardened (no shell, no package manager); only the UID is root. So: **hardened
nonroot pays off on the controller; on the node you get a hardened image running as
root.**

> If you harden a *plugin* image too (so its socket needs the chmod), rebuild it
> from this tree — the `0660` chmod is in `pkg/bardplugin.Serve`, so every Go plugin
> inherits it; an out-of-tree plugin in another language should chmod its socket the
> same way (the Python localpath plugin shows it).

## It's already drop-in for the Helm chart

Whatever you build, the chart selects it by name with no template changes:

```yaml
# values.yaml
image: { repository: my/bard-csi, tag: hardened }          # core
plugins:
  lvm:
    image: { repository: my/bard-plugin-lvm, tag: hardened }
```
