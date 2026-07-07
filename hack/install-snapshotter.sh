#!/usr/bin/env bash
# Install the external-snapshotter cluster singleton (CRDs + snapshot-controller)
# at a single coherent version -- the prerequisite Bard's csi-snapshotter sidecar
# needs but deliberately does NOT bundle (it is one-per-cluster, shared by every
# CSI driver, so the Helm chart leaves it to the admin -- see charts/bard-csi).
#
# This exists because upstream's own setup-snapshot-controller.yaml pins the
# controller IMAGE to v8.0.1 even under the v8.2.0 tag; that old controller speaks
# the v1alpha1 group API and stalls against the v8.2.0 (v1beta1) group CRDs -- and
# that stall blocks PLAIN snapshots too when the group-snapshot gate is on (which
# Bard's sidecar sets). So we apply the manifests and then FORCE the controller
# image to $VERSION, making sidecar == controller == group-CRD API all coherent.
#
#   KUBECONFIG=... bash hack/install-snapshotter.sh            # v8.2.0 (default)
#   KUBECONFIG=... bash hack/install-snapshotter.sh v8.2.0
#
# Idempotent: re-running just re-applies (CRDs/RBAC) and re-pins the image.
set -euo pipefail

VERSION="${1:-v8.2.0}"
B="https://raw.githubusercontent.com/kubernetes-csi/external-snapshotter/${VERSION}"
CTRL_IMAGE="registry.k8s.io/sig-storage/snapshot-controller:${VERSION}"

echo "==> snapshot + group-snapshot CRDs (${VERSION})"
for c in \
  snapshot.storage.k8s.io_volumesnapshotclasses \
  snapshot.storage.k8s.io_volumesnapshotcontents \
  snapshot.storage.k8s.io_volumesnapshots \
  groupsnapshot.storage.k8s.io_volumegroupsnapshotclasses \
  groupsnapshot.storage.k8s.io_volumegroupsnapshotcontents \
  groupsnapshot.storage.k8s.io_volumegroupsnapshots; do
  kubectl apply -f "${B}/client/config/crd/${c}.yaml"
done

echo "==> snapshot-controller RBAC + deployment"
kubectl apply -f "${B}/deploy/kubernetes/snapshot-controller/rbac-snapshot-controller.yaml"
kubectl apply -f "${B}/deploy/kubernetes/snapshot-controller/setup-snapshot-controller.yaml"

# The fix: upstream's manifest mispins the image; force it to the matched version.
echo "==> pin snapshot-controller image -> ${CTRL_IMAGE}"
kubectl -n kube-system set image deploy/snapshot-controller "snapshot-controller=${CTRL_IMAGE}"
kubectl -n kube-system rollout status deploy/snapshot-controller --timeout=120s

echo "snapshotter ${VERSION} ready (CRDs + version-matched controller)."
