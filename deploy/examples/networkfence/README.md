# Example: node network fencing via csi-addons (NetworkFence)

Fences a node's client network ranges at the **Ceph layer** (`osd blocklist
range`) so a failed/partitioned node can no longer reach the cluster — the
csi-addons **NetworkFence** operation. It is the node-scoped complement to Bard's
per-volume single-writer fence: a DR/failover orchestrator (e.g. Ramen) fences a
whole node before failing its volumes over, then unfences it when it recovers.

Bard serves the **real csi-addons `FenceController` gRPC API**, so a ceph-csi
user's existing `NetworkFence` resources work against Bard unchanged. Only
backends that can fence advertise it — today **Ceph RBD** (`osd blocklist range
add/rm/ls`). Bard advertises the NetworkFence capability **only** when such a
backend is registered.

## How it fits together

- **Bard core** serves the csi-addons `FenceController` service on the controller
  pod's csi-addons socket (controller-side: node fencing is cluster-scoped).
- The **csi-addons sidecar + cluster-wide controller-manager** reconcile a
  `NetworkFence` CR and call Bard's `FenceClusterNetwork` / `UnfenceClusterNetwork`.
- Bard resolves `spec.parameters.clusterID` to the **backend instance** (Bard's
  zone/instance id, e.g. `galileo`) and dispatches `osd blocklist range` on that
  instance's Ceph cluster, using the credentials from `spec.secret`.

## 1. Install the cluster-wide csi-addons CRDs + controller-manager

Same one-per-cluster prerequisite as [the ReclaimSpace example](../reclaimspace/);
skip if already installed:

```sh
kubectl apply -f https://github.com/csi-addons/kubernetes-csi-addons/releases/download/v0.12.0/crds.yaml
kubectl apply -f https://github.com/csi-addons/kubernetes-csi-addons/releases/download/v0.12.0/rbac.yaml
kubectl apply -f https://github.com/csi-addons/kubernetes-csi-addons/releases/download/v0.12.0/setup-controller.yaml
```

Bard's deploy already carries the controller-side csi-addons sidecar and the
`networkfences` RBAC ([deploy/10-rbac.yaml](../../10-rbac.yaml)). With the Helm
chart, enable `sidecars.csiAddons.enabled=true`.

## 2. Create a fence-capable cephx user + Secret

**This is the key prerequisite.** The per-volume provisioning user (`mon 'profile
rbd'`) can *add* a blocklist entry but **cannot remove one** — verified against
Ceph, `osd blocklist range rm` returns `EACCES` — so unfence would fail with it.
NetworkFence therefore needs a user whose mon cap allows the blocklist command:

```sh
ceph auth get-or-create client.k8s-fence \
  mon 'profile rbd, allow command "osd blocklist"' \
  osd 'profile rbd pool=<your-pool>' \
  mgr 'profile rbd pool=<your-pool>'

kubectl -n kube-system create secret generic bard-ceph-fence \
  --from-literal=userID=k8s-fence \
  --from-literal=userKey="$(ceph auth get-key client.k8s-fence)"
```

(`mon 'profile rbd, allow command "osd blocklist"'` is the least privilege that
covers `range add`, `range rm`, and `ls` — confirmed live.)

## 3. Fence / unfence a node

```sh
# Fence: blocklist the node's CIDR(s) — see networkfence.yaml (set cidrs +
# parameters.clusterID to your instance).
kubectl apply -f deploy/examples/networkfence/networkfence.yaml
kubectl get networkfence fence-node-worker3 -o jsonpath='{.status.result}{"\n"}'  # -> Succeeded

# Verify on Ceph (the range appears as a cidr: blocklist entry):
ceph osd blocklist ls | grep cidr:

# Unfence: flip fenceState to Unfenced (or delete the CR).
kubectl patch networkfence fence-node-worker3 --type=merge -p '{"spec":{"fenceState":"Unfenced"}}'
```

Ceph blocklist range entries also auto-expire (default ~1h), so a fence is not
permanently load-bearing if the orchestrator never unfences.

## Discovering what to fence (GET_CLIENTS_TO_FENCE)

Bard also advertises the csi-addons **`GET_CLIENTS_TO_FENCE`** capability and serves
`GetFenceClients`, the helper a DR orchestrator calls to discover the client(s) it
should pass to `FenceClusterNetwork`. For Ceph RBD it returns one client: the cluster
**FSID** (`ceph fsid`) as the id, and the controller's own address toward the mon as a
host CIDR (`/32`/`/128`) — the CLI equivalent of the librados client address ceph-csi
returns. It is a pure read (only `ceph fsid`, so the provisioning user suffices; the
stronger fence user is not required). The capability is advertised whenever a
NetworkFence-capable backend is registered.
