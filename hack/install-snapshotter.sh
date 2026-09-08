#!/usr/bin/env bash
# Install the external-snapshotter cluster singleton (CRDs + snapshot-controller)
# at a single coherent version -- the prerequisite Bard's csi-snapshotter sidecar
# needs but deliberately does NOT bundle (it is one-per-cluster, shared by every
# CSI driver, so the Helm chart leaves it to the admin -- see charts/bard-csi).
#
# This applies the manifests and pins the controller image to $VERSION so the
# cluster singleton and Bard's snapshotter sidecar stay version-matched. That pin
# is load-bearing, not tidiness: upstream's own setup-snapshot-controller.yaml
# pins the controller IMAGE to v8.0.1 even under the v8.2.0 tag, and that older
# controller speaks the v1alpha1 group API while the v8.2.0 CRDs are v1beta1 --
# it then stalls, and with the group-snapshot gate on that stall blocks PLAIN
# snapshots too.
#
# The group CRDs installed below are needed only for CSI VolumeGroupSnapshot,
# which Bard serves for ceph-rbd. They are harmless when unused, and they must be
# installed here rather than by the Helm chart for the same reason the rest of
# this file exists: they are a cluster singleton shared by every CSI driver.
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

echo "==> pin snapshot-controller image -> ${CTRL_IMAGE}"
kubectl -n kube-system set image deploy/snapshot-controller "snapshot-controller=${CTRL_IMAGE}"
kubectl -n kube-system rollout status deploy/snapshot-controller --timeout=120s

echo "snapshotter ${VERSION} ready (CRDs + version-matched controller)."
echo "NOTE: CSI VolumeGroupSnapshot additionally needs the cluster snapshot-controller"
echo "      run with --feature-gates=CSIVolumeGroupSnapshot=true, matching Bard's chart"
echo "      value sidecars.snapshotter.groupSnapshots."
