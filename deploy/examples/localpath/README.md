# Example: localpath backend via a plugin written in **Python**

Adds a **localpath** backend to Bard through the `bard-plugin-localpath` plugin.
Its reason to exist is the language: it is written in **Python, stdlib only**, to
demonstrate that Bard's plugin contract is language-agnostic. A Bard plugin is
just an HTTP server speaking JSON over a unix socket (see [pkg/bardplugin](../../../pkg/bardplugin)),
so it can be written in *anything* — no Go, no SDK, no generated code. The same
core dispatches to it exactly as it does the Go plugins.

A volume is a **subdirectory** under a configured base path; the node bind-mounts
it. Filesystem, ReadWriteOnce (or ReadWriteMany if the base path is a shared
mount). No block device, no snapshots, no expansion — deliberately minimal so the
focus stays on the wire contract, not the storage.

## Locality model

Like the LVM plugin's shared-VG model, the base path is treated as a **shared**
instance: `CreateVolume` (mkdir, run in Bard's controller) and the node's
bind-mount both reach the same directory. `Capabilities.NodeLocal` is **false**.
On the single-host dev cluster a `hostPath` is shared by every kind node; on a
real multi-node cluster point `basePath` at a shared mount (NFS, etc.) so the
controller's mkdir and any node's mount see the same tree.

## Try it without a cluster

The fastest proof is the standalone test — it starts the Python plugin under a
trap and drives the full contract over the socket with `curl`, exactly as core
would, then cleans up:

```sh
sudo bash hack/localpath-plugin-test.sh
# -> PASS: Python plugin spoke the full Bard contract over the unix socket
```

## In a cluster

1. Build + load the image (rootless build, rootful kind — see CLAUDE.md):

   ```sh
   podman build -t ghcr.io/kindacoolhamster/bard-plugin-localpath:dev -f Dockerfile.plugin-localpath .
   podman save -o /tmp/bard-plugin-localpath.tar ghcr.io/kindacoolhamster/bard-plugin-localpath:dev
   sudo kind load image-archive /tmp/bard-plugin-localpath.tar --name bard
   ```

2. Tell Bard about the backend (adding a backend is just adding a CR):

   ```yaml
   apiVersion: bard.io/v1alpha1
   kind: BackendCluster
   metadata:
     name: galileo-localpath
   spec:
     backendType: localpath
     instance: galileo
     zone: galileo
     plugin:
       endpoint: /var/lib/bard/plugins/localpath.sock
   ```

3. Apply config/StorageClass and the sidecar patches, then restart the bard pods:

   ```sh
   kubectl apply -f deploy/examples/localpath/config.yaml
   kubectl -n kube-system patch deployment bard-csi-controller \
     --type=strategic --patch-file deploy/examples/localpath/sidecar-controller.patch.yaml
   kubectl -n kube-system patch daemonset bard-csi-node \
     --type=strategic --patch-file deploy/examples/localpath/sidecar-node.patch.yaml
   # apply the BackendCluster above, then restart the bard controller + node pods
   ```

4. Use it:

   ```sh
   kubectl apply -f - <<'EOF'
   apiVersion: v1
   kind: PersistentVolumeClaim
   metadata: { name: localpath-test }
   spec:
     accessModes: ["ReadWriteOnce"]
     storageClassName: bard-localpath
     resources: { requests: { storage: 1Gi } }
   EOF
   ```

The same Bard driver simultaneously serves Ceph RBD, CephFS, NFS, and LVM — and
now a backend whose plugin isn't even written in Go. One CSI driver, many
backends, any language, all out of tree.
