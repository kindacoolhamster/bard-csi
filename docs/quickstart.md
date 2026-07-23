# Quickstart — first PVC in about three minutes

This installs Bard with its bundled **localpath demo backend** — volumes are
directories on the node's disk, bind-mounted into pods. Zero external
dependencies: no Ceph, no NFS server, nothing but a Kubernetes cluster. It's
for kicking the tires on a **single-node** cluster (kind, k3s, minikube); for
real storage, swap in a real backend afterwards (see "Where next").

```sh
# 1. a cluster (any single-node cluster works; kind shown), with a zone label:
#    Bard dispatches volumes by the node's zone -- that's the headline feature --
#    so every node needs one. Here there's one node and one zone.
kind create cluster
kubectl label node --all topology.kubernetes.io/zone=quickstart

# 2. the release to install. Pin it: helm's unversioned OCI resolution does not
#    select pre-release versions, and every published Bard chart version is
#    currently a pre-release, so omitting --version resolves nothing ("Could not
#    locate a version matching provided version string"). Pinning the chart and
#    the manifests below to the same tag also stops them drifting apart.
#    See Releases for the current version.
BARD_VERSION=0.1.0-rc.4

# 3. Bard + the demo backend
helm install bard-csi oci://ghcr.io/kindacoolhamster/charts/bard-csi \
  --version "$BARD_VERSION" \
  -n kube-system \
  -f "https://raw.githubusercontent.com/kindacoolhamster/bard-csi/v$BARD_VERSION/deploy/quickstart/values.yaml"

# 4. backend config + StorageClass + a demo PVC/pod
kubectl apply -f "https://raw.githubusercontent.com/kindacoolhamster/bard-csi/v$BARD_VERSION/deploy/quickstart/quickstart.yaml"

# 5. proof
kubectl wait --for=condition=Ready pod/bard-quickstart --timeout=180s
kubectl exec bard-quickstart -- cat /data/hello
# -> bard-csi-quickstart-works
```

Cleanup: `kubectl delete -f .../quickstart.yaml` (the PV and its directory are
reclaimed), then `helm uninstall bard-csi -n kube-system`.

## What just happened

The demo pod's PVC was provisioned through the full CSI control plane: the
external-provisioner asked Bard's controller for a volume, Bard's dispatcher
resolved the `quickstart` instance from the `BackendCluster` CR the chart
created, and proxied the create to the **localpath plugin** — an out-of-tree
backend that is a ~300-line stdlib-only **Python** script speaking HTTP+JSON
over a unix socket. On the node, the same plugin bind-mounted the volume's
directory into the pod. Nothing about the flow is demo-specific: a Ceph RBD
volume takes exactly the same path with `rbd` instead of `mkdir`.

That's Bard's architecture in one install:

- **Core is backend-agnostic** — one CSI driver, topology-aware dispatch
  across backends and instances; backends are plugins in any language.
- **One StorageClass can span many backend instances/zones** — the headline
  feature; the quickstart uses a single instance, the multi-cluster examples
  use several.

## Where next

- **Real backends**: enable the Ceph RBD, CephFS, or iSCSI profile in the chart
  (`plugins.ceph-rbd.instances` in
  [charts/bard-csi/values.yaml](../charts/bard-csi/values.yaml)), or wire
  NFS/LVM via [deploy/examples/](../deploy/examples/). Backends differ in how
  proven they are — check
  [Backend maturity](../STATUS.md#backend-maturity) before picking one.
- **Migrating from ceph-csi**: in-place adoption without copying data — see
  the migration docs/examples.
- **Write your own backend** in any language:
  [docs/writing-a-plugin.md](writing-a-plugin.md), with
  `bard-plugin-conformance` as the acceptance test.

## Notes

- The demo backend is deliberately minimal: filesystem volumes only, no
  snapshots/expansion, and the volume lives on one node's disk — it proves the
  wire contract, not durability. Don't run it on multi-node clusters (the
  controller and the pod's node must share the base path).
- The quickstart skips the external-snapshotter (cluster-wide CRDs +
  controller, installed separately); snapshots with real backends need it —
  see the chart README.
