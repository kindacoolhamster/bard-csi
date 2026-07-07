# Example: NFS backend via an out-of-tree plugin

This shows Bard provisioning NFS volumes through the `bard-plugin-nfs` plugin
**without any change to Bard's code or binary** — only config and a sidecar.

## 1. Tell Bard about the plugin backend

Apply a `BackendCluster` for this NFS instance (adding a backend is just adding
a CR):

```yaml
apiVersion: bard.io/v1alpha1
kind: BackendCluster
metadata:
  name: galileo-nfs
spec:
  backendType: nfs
  instance: nfs-galileo
  zone: galileo
  default: true
  plugin:
    endpoint: /var/lib/bard/plugins/nfs.sock
```

## 2. Add the plugin sidecar to Bard's pods

The plugin runs beside Bard, sharing a unix-socket `emptyDir`. Apply the
strategic-merge patches in this directory to both Bard workloads:

```sh
kubectl -n kube-system patch deployment bard-csi-controller \
  --type=strategic --patch-file deploy/examples/nfs/sidecar-controller.patch.yaml
kubectl -n kube-system patch daemonset bard-csi-node \
  --type=strategic --patch-file deploy/examples/nfs/sidecar-node.patch.yaml
```

The patches add the `nfs-plugin` sidecar (privileged, for mounts), a shared
`plugins` `emptyDir` socket dir mounted into both Bard's container and the
sidecar, and the plugin's config. The node patch also shares the kubelet dir
with `mountPropagation: Bidirectional` so the plugin's mounts reach kubelet.

## 3. Apply config + StorageClass

```sh
kubectl apply -f deploy/examples/nfs/config.yaml   # plugin config + StorageClass
# (re-apply the patched 20-config / controller / node manifests)
```

## 4. Use it

```sh
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata: { name: nfs-test }
spec:
  accessModes: ["ReadWriteMany"]
  storageClassName: bard-nfs
  resources: { requests: { storage: 1Gi } }
EOF
```

A pod using this PVC gets a subdirectory of `/srv/nfs/bard` on the NFS server,
mounted at its volume path. The same Bard driver is simultaneously serving Ceph
RBD via `bard-rbd` — one CSI driver, many backends, the NFS one fully out of tree.

## Prerequisite: an NFS server

For the dev host, export a directory (see CLAUDE.md):

```sh
sudo mkdir -p /srv/nfs/bard && sudo chmod 777 /srv/nfs/bard
echo '/srv/nfs/bard *(rw,sync,no_subtree_check,no_root_squash)' | sudo tee -a /etc/exports
sudo exportfs -ra && sudo systemctl restart nfs-server nfs-mountd nfsdcld
```

> **Running NFS on the same host as kind?** kind's kubelet/containerd consume
> many inotify instances; the default `fs.inotify.max_user_instances=128` can
> starve the NFS server's `nfsdcld`/`mountd`, making **NFSv4 mounts hang**. Bump
> it: `sudo sysctl -w fs.inotify.max_user_instances=8192` (the rootful-kind setup
> script does this for you).
