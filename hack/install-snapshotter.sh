#!/usr/bin/env bash
# Install the external-snapshotter cluster singleton (CRDs + snapshot-controller)
# at a single coherent version -- the prerequisite Bard's csi-snapshotter sidecar
# needs but deliberately does NOT bundle (it is one-per-cluster, shared by every
# CSI driver, so the Helm chart leaves it to the admin -- see charts/bard-csi).
#
# This applies the manifests and pins the controller image to $VERSION so the
# cluster singleton and Bard's snapshotter sidecar stay version-matched.
#
#   KUBECONFIG=... bash hack/install-snapshotter.sh            # v8.2.0 (default)
#   KUBECONFIG=... bash hack/install-snapshotter.sh v8.2.0
#
# Idempotent: re-running just re-applies (CRDs/RBAC) and re-pins the image.
set -euo pipefail

VERSION="${1:-v8.2.0}"
B="https://raw.githubusercontent.com/kubernetes-csi/external-snapshotter/${VERSION}"
CTRL_IMAGE="registry.k8s.io/sig-storage/snapshot-controller:${VERSION}"

echo "==> snapshot CRDs (${VERSION})"
for c in \
  snapshot.storage.k8s.io_volumesnapshotclasses \
  snapshot.storage.k8s.io_volumesnapshotcontents \
  snapshot.storage.k8s.io_volumesnapshots; do
  kubectl apply -f "${B}/client/config/crd/${c}.yaml"
done

echo "==> snapshot-controller RBAC + deployment"
kubectl apply -f "${B}/deploy/kubernetes/snapshot-controller/rbac-snapshot-controller.yaml"
kubectl apply -f "${B}/deploy/kubernetes/snapshot-controller/setup-snapshot-controller.yaml"

echo "==> pin snapshot-controller image -> ${CTRL_IMAGE}"
kubectl -n kube-system set image deploy/snapshot-controller "snapshot-controller=${CTRL_IMAGE}"
kubectl -n kube-system rollout status deploy/snapshot-controller --timeout=120s

echo "snapshotter ${VERSION} ready (CRDs + version-matched controller)."
