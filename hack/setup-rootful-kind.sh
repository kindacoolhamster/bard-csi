#!/usr/bin/env bash
# Set up a ROOTFUL podman kind cluster for bard-csi, including every host-level
# fix the node-side rbd mount needs. Idempotent; safe to re-run.
#
# Why rootful: the Ceph mon is co-located on this host. Rootless podman (pasta)
# cannot route a pod to the host's own LAN IP, and Ceph's monmap forces clients
# to that address. Rootful podman puts the kind nodes on a normal bridge that
# routes to the host LAN.
#
# Host fixes applied (all needed to mount rbd inside kind nodes):
#   - load rbd + nbd kernel modules (krbd / rbd-nbd backends)
#   - allow the podman bridge subnet through docker's FORWARD DROP policy, so
#     kind nodes can pull images from the internet (DOCKER-USER ACCEPT)
#   - create /dev/nbdN nodes inside each kind node (no udev in kind to make them)
#
# Run as root:   sudo bash hack/setup-rootful-kind.sh
# Tear down:     sudo bash hack/setup-rootful-kind.sh delete
#
# Then drive the cluster as your normal user:
#   export KUBECONFIG=$HOME/.kube/config-bard
#   kubectl get nodes
set -euo pipefail

REAL_USER="${SUDO_USER:-$USER}"
REAL_HOME="$(getent passwd "$REAL_USER" | cut -d: -f6)"
KIND="$REAL_HOME/.local/bin/kind"
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KUBECONFIG_OUT="$REAL_HOME/.kube/config-bard"
IMAGE_TAR="/tmp/bard-csi-image.tar"
CLUSTER=bard
NBD_COUNT=64   # match the host's nbds_max; rbd-nbd's nondeterministic unmap in
               # nested kind leaks devices, so a small pool gets exhausted
               # ("rbd-nbd: failed to find unused device") under multi-volume use
export KIND_EXPERIMENTAL_PROVIDER=podman

if [[ "${1:-}" == "delete" ]]; then
  "$KIND" delete cluster --name "$CLUSTER" || true
  exit 0
fi
if [[ $EUID -ne 0 ]]; then
  echo "must run as root (sudo bash $0)" >&2
  exit 1
fi

echo "==> loading kernel modules (rbd for krbd, nbd for rbd-nbd, dm_crypt for LUKS)"
modprobe rbd
modprobe nbd nbds_max=64
modprobe dm_crypt # encrypted volumes: kind nodes share the host kernel
printf 'rbd\nnbd\ndm_crypt\n' >/etc/modules-load.d/bard-csi.conf

echo "==> raising inotify limits (kind exhausts the default 128; also starves a"
echo "    co-located NFS server's nfsdcld, hanging NFSv4 mounts)"
sysctl -w fs.inotify.max_user_instances=8192 >/dev/null
sysctl -w fs.inotify.max_user_watches=1048576 >/dev/null
printf 'fs.inotify.max_user_instances=8192\nfs.inotify.max_user_watches=1048576\n' >/etc/sysctl.d/99-bard-inotify.conf

echo "==> creating rootful kind cluster '$CLUSTER'"
"$KIND" create cluster --config "$REPO_DIR/hack/kind-cluster.yaml" --wait 120s

NODES=$("$KIND" get nodes --name "$CLUSTER")

echo "==> allowing podman bridge egress through docker's FORWARD DROP (if docker present)"
if iptables -nL DOCKER-USER >/dev/null 2>&1; then
  firstnode=$(echo "$NODES" | head -1)
  gw=$(podman inspect "$firstnode" --format '{{range .NetworkSettings.Networks}}{{.Gateway}}{{end}}')
  subnet="$(echo "$gw" | cut -d. -f1-3).0/24"
  for dir in -s -d; do
    iptables -C DOCKER-USER $dir "$subnet" -j ACCEPT 2>/dev/null ||
      iptables -I DOCKER-USER $dir "$subnet" -j ACCEPT
  done
  echo "    allowed $subnet in DOCKER-USER"
else
  echo "    DOCKER-USER chain absent (docker not installed) -- skipping"
fi

echo "==> creating /dev/nbd0..$((NBD_COUNT-1)) inside each node (no udev in kind)"
for node in $NODES; do
  podman exec "$node" bash -c "for i in \$(seq 0 $((NBD_COUNT-1))); do [ -e /dev/nbd\$i ] || mknod /dev/nbd\$i b 43 \$i; done"
done

echo "==> exporting kubeconfig to $KUBECONFIG_OUT (owned by $REAL_USER)"
mkdir -p "$(dirname "$KUBECONFIG_OUT")"
"$KIND" get kubeconfig --name "$CLUSTER" >"$KUBECONFIG_OUT"
chown "$REAL_USER":"$REAL_USER" "$KUBECONFIG_OUT"

if [[ -f "$IMAGE_TAR" ]]; then
  echo "==> loading driver image from $IMAGE_TAR"
  "$KIND" load image-archive "$IMAGE_TAR" --name "$CLUSTER"
else
  echo "!! $IMAGE_TAR not found; build it as $REAL_USER first:"
  echo "   podman build -t ghcr.io/kindacoolhamster/bard-csi:dev . && podman save -o $IMAGE_TAR ghcr.io/kindacoolhamster/bard-csi:dev"
fi

echo
echo "Cluster ready. As $REAL_USER:"
echo "   export KUBECONFIG=$KUBECONFIG_OUT"
echo "   kubectl apply -f deploy/ && kubectl apply -f hack/secret.yaml"
echo "   # + hack/config-local.yaml if your mon isn't deploy/'s documentation IP"
