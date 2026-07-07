# Example: cross-namespace volume data sources (restore a snapshot from another namespace)

Restore a PVC from a `VolumeSnapshot` that lives in a **different namespace** —
e.g. a platform team keeps golden snapshots in `team-src`, and app namespaces
restore from them. Authorization is explicit and namespace-scoped: the *owning*
namespace publishes a Gateway API **ReferenceGrant** saying who may reference
what; without it the provisioner refuses
(`accessing <ns>/<snap> ... isn't allowed` — verified live, both directions).

This is Kubernetes' `CrossNamespaceVolumeDataSource` feature (alpha), driven
entirely by the external-provisioner — no Bard driver code involved. It is an
open ceph-csi ask (ceph/ceph-csi#3588); with Bard the recipe below is all it
takes.

## 1. Cluster prerequisites (one-time, admin)

```sh
# a) Feature gate on the control plane (alpha). k3s example -- add to
#    /etc/rancher/k3s/config.yaml on the server node and restart k3s:
#      kube-apiserver-arg:
#        - feature-gates=CrossNamespaceVolumeDataSource=true
#      kube-controller-manager-arg:
#        - feature-gates=CrossNamespaceVolumeDataSource=true

# b) The Gateway API ReferenceGrant CRD (standard channel):
kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/gateway-api/v1.2.1/config/crd/standard/gateway.networking.k8s.io_referencegrants.yaml
```

## 2. Bard side

- The csi-provisioner needs the matching gate — extend its `--feature-gates`
  arg in [deploy/30-controller.yaml](../../30-controller.yaml):
  `--feature-gates=Topology=true,CrossNamespaceVolumeDataSource=true`
- RBAC to read ReferenceGrants ships in
  [deploy/10-rbac.yaml](../../10-rbac.yaml) / the chart (harmless when unused).

## 3. Grant + restore

```sh
kubectl apply -f referencegrant.yaml   # in the SOURCE namespace
kubectl apply -f restore.yaml          # the PVC with a namespaced dataSourceRef
```

The restored PVC provisions like any snapshot restore (COW clone). Deleting the
ReferenceGrant immediately blocks *new* restores from that namespace.
