# Enabling control-plane attach (ControllerPublish)

Most Bard backends map on the node (Ceph RBD, CephFS, NFS, LVM) and need **no**
attach — they ship with `attachRequired: false` and no external-attacher. A
backend that attaches as a control-plane operation (e.g. the **iSCSI** plugin,
which masks a LUN to the node's initiator) needs the attach machinery turned on.

Because `CSIDriver.spec.attachRequired` is a **single, cluster-global, immutable
field** and Bard is one CSI driver for many backends, turning attach on is
all-or-nothing: every volume then gets a VolumeAttachment, and node-mapped
backends return an immediate no-op ControllerPublish (harmless). Bard advertises
the `PUBLISH_UNPUBLISH_VOLUME` capability only when a registered backend actually
attaches, so the attacher has real work only once you deploy such a plugin.

This overlay turns it on:

```sh
# 1. CSIDriver with attachRequired: true. The field is IMMUTABLE, so on an
#    existing install you must delete the object first (no volumes are mounting
#    during the swap):
kubectl delete csidriver csi.bard.io --ignore-not-found
kubectl apply -f deploy/examples/attach/csidriver.yaml

# 2. RBAC the external-attacher needs (patch VolumeAttachments + status):
kubectl apply -f deploy/examples/attach/rbac.yaml

# 3. Add the csi-attacher sidecar to the controller Deployment:
kubectl -n kube-system patch deployment bard-csi-controller \
  --type=strategic --patch-file deploy/examples/attach/controller-attacher-patch.yaml
```

To turn attach back off, reverse: remove the sidecar, delete+recreate the
CSIDriver with `attachRequired: false`. (The Helm chart does all of this from one
value: `attach.enabled=true`.)
