#!/usr/bin/env bash
# Stand up a real-kernel k3s cluster on multipass VMs and deploy Bard on it.
#
# WHY: kind nodes are nested containers that share the host kernel, so krbd
# (`rbd map` -> /sys/bus/rbd/add) is rejected and we are forced onto rbd-nbd --
# whose unmap is non-deterministic in that environment (zombie rbd watchers ->
# orphaned images). These VMs have their OWN kernel, so krbd (the PRODUCTION
# default mounter) works and unmap is reliable. This is the integration tier;
# keep kind for the fast loop (go test / csi-sanity / helm lint).
#
#   sudo apt/snap install multipass    # once (snap install multipass)
#   bash hack/setup-k3s-vms.sh         # build + launch + deploy
#   bash hack/setup-k3s-vms.sh delete  # tear the VMs down
#
# Ceph: the VMs reach the host mon at $CEPH_MON over multipass NAT. The cephx key
# comes from hack/secret.yaml (untracked). Bard runs with mounter=krbd (the
# rbd-nbd line in deploy/20-config.yaml is dropped here).
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
SERVER=k3s-server
AGENT=k3s-agent
CEPH_MON=${CEPH_MON:-192.168.1.225:3300}
KCFG="$HOME/.kube/config-k3s"
CI="$HOME/k3s-ci.yaml"
IMAGES=(ghcr.io/kindacoolhamster/bard-csi:dev ghcr.io/kindacoolhamster/bard-plugin-ceph-rbd:dev)

if [ "${1:-}" = "delete" ]; then
  multipass delete --purge "$SERVER" "$AGENT" 2>/dev/null || true
  echo "deleted $SERVER + $AGENT"
  exit 0
fi

command -v multipass >/dev/null || { echo "multipass not installed (snap install multipass)"; exit 1; }

# cloud-init: load the kernel modules Bard's node plane needs (and persist them):
# rbd for krbd, dm_crypt for LUKS-encrypted volumes.
cat > "$CI" <<'EOF'
#cloud-config
write_files:
  - path: /etc/modules-load.d/bard.conf
    content: |
      rbd
      dm_crypt
runcmd:
  - [ modprobe, rbd ]
  - [ modprobe, dm_crypt ]
EOF

launch() { # name memory
  multipass info "$1" >/dev/null 2>&1 && { echo "$1 exists"; return; }
  multipass launch 24.04 --name "$1" --cpus 2 --memory "$2" --disk 12G --cloud-init "$CI"
}
launch "$SERVER" 2G
launch "$AGENT"  1500M
SERVER_IP=$(multipass info "$SERVER" --format csv | awk -F, 'NR==2{print $3}')

# k3s: single server (SQLite -- no etcd), lean (no traefik/servicelb).
if ! multipass exec "$SERVER" -- test -f /usr/local/bin/k3s; then
  multipass exec "$SERVER" -- bash -c \
    'curl -sfL https://get.k3s.io | sudo INSTALL_K3S_EXEC="--disable traefik --disable servicelb --write-kubeconfig-mode 644" sh -'
fi
TOKEN=$(multipass exec "$SERVER" -- sudo cat /var/lib/rancher/k3s/server/node-token)
if ! multipass exec "$AGENT" -- test -f /usr/local/bin/k3s; then
  multipass exec "$AGENT" -- bash -c \
    "curl -sfL https://get.k3s.io | sudo K3S_URL=https://$SERVER_IP:6443 K3S_TOKEN='$TOKEN' sh -"
fi

# kubeconfig to the host (point at the server VM, not 127.0.0.1).
mkdir -p "$(dirname "$KCFG")"
multipass exec "$SERVER" -- sudo cat /etc/rancher/k3s/k3s.yaml | sed "s/127.0.0.1/$SERVER_IP/" > "$KCFG"
chmod 600 "$KCFG"
export KUBECONFIG="$KCFG"
until [ "$(kubectl get nodes --no-headers 2>/dev/null | grep -c ' Ready ')" = 2 ]; do sleep 4; done

# k3s uses containerd, not podman: import the locally-built Bard images on BOTH
# nodes (sidecars are pulled from registry.k8s.io automatically).
for img in "${IMAGES[@]}"; do
  name="$(basename "${img%%:*}")"; tar="$HOME/$name.tar"
  podman image exists "$img" || { echo "build $img first (see CLAUDE.md)"; exit 1; }
  podman save -o "$tar" "$img"
  for vm in "$SERVER" "$AGENT"; do
    multipass transfer "$tar" "$vm:/tmp/$name.tar"
    multipass exec "$vm" -- sudo k3s ctr images import "/tmp/$name.tar"
  done
done

# Deploy Bard with krbd (drop the rbd-nbd dev line from the config). deploy/
# ships documentation IPs; hack/config-local.yaml (untracked, like the secret)
# carries the real mon/KMS addresses and takes precedence when present.
CFG="$REPO/deploy/20-config.yaml"
[ -f "$REPO/hack/config-local.yaml" ] && CFG="$REPO/hack/config-local.yaml"
kubectl apply -f "$REPO/deploy/05-crd-backendcluster.yaml" -f "$REPO/deploy/10-rbac.yaml" -f "$REPO/deploy/00-csidriver.yaml"
sed '/mounter: rbd-nbd/d' "$CFG" | kubectl apply -f -
kubectl apply -f "$REPO/hack/secret.yaml"
kubectl apply -f "$REPO/deploy/30-controller.yaml" -f "$REPO/deploy/40-node.yaml"
# csi-addons needs its own CRDs + controller-manager (installed separately); drop
# the sidecars here so they don't crashloop.
kubectl -n kube-system patch deployment bard-csi-controller --type=strategic \
  -p '{"spec":{"template":{"spec":{"containers":[{"name":"csi-addons","$patch":"delete"}]}}}}' || true
kubectl -n kube-system patch daemonset bard-csi-node --type=strategic \
  -p '{"spec":{"template":{"spec":{"containers":[{"name":"csi-addons","$patch":"delete"}]}}}}' || true
# external-snapshotter cluster singleton (CRDs + version-matched controller) so
# snapshots/group-snapshots work out of the box on this dev tier. The Helm chart
# leaves this to the admin (it is one-per-cluster); here we install it directly.
bash "$REPO/hack/install-snapshotter.sh"
# Now the snapshot/group classes in 50-storageclass.yaml have their CRDs present.
kubectl apply -f "$REPO/deploy/50-storageclass.yaml"
kubectl label node "$SERVER" "$AGENT" topology.kubernetes.io/zone=galileo --overwrite

kubectl -n kube-system rollout status deploy/bard-csi-controller --timeout=180s
kubectl -n kube-system rollout status ds/bard-csi-node --timeout=180s
echo
echo "k3s + Bard ready. Use it with:  export KUBECONFIG=$KCFG"
echo "krbd smoke test:                kubectl apply -f hack/test-pvc.yaml"
