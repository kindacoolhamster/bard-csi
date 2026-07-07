# Copy-migrating a PVC from any CSI to Bard CSI

When the source is **not** an rbd image Bard can re-wrap — AWS EBS, GCE-PD, Azure
Disk, NFS, hostPath/local-path, any provisioner that presents a PVC — there is no
in-place adoption; the bytes have to move. This recipe is backend-agnostic: a Job
mounts the source PVC **read-only** and a freshly-provisioned Bard PVC, copies
(`rsync` for a filesystem volume, `dd` for a raw block volume), and verifies by
checksum.

If your source *is* a ceph-csi RBD volume, use
[../migrate-ceph-csi/README.md](../migrate-ceph-csi/README.md) instead — in-place
adoption copies nothing and is near-instant. Copy migration is the fallback for
everything else.

## Consistency: quiesce the source first

A copy of a volume that is being written is inconsistent (torn writes, a database
mid-transaction). **Scale the source workload to 0 before copying.** For an RWO
source this is enforced for you: the workload pod holds the single attach, so the
copy Job stays `Pending` until you scale down. The helper refuses `--run` while
the source PVC is still mounted. For RWX sources, quiesce every writer yourself.

## One command

[`hack/migrate-copy.sh`](../../../hack/migrate-copy.sh) reads the source PVC,
generates a Bard target PVC (same size/access-modes/volume-mode) plus the copy
Job, and prints the cutover runbook. Generate-only by default; `--run` applies and
waits for the checksum-verified copy.

```sh
# 1. Quiesce the source.
kubectl -n <ns> scale <deploy|statefulset>/<name> --replicas=0

# 2. Copy into a new Bard PVC (<source>-bard by default) and verify.
hack/migrate-copy.sh -n <ns> -c <source-pvc> -s <bard-storageclass> --run
#   filesystem  -> rsync -aHAXS + md5 manifest compare
#   raw block   -> dd + cmp over the source length
#   look for MIGRATE_COPY_OK in the Job log.
```

Mode (`fs`/`block`) auto-detects from the source `volumeMode`; override with
`-m`. Target name with `-t`. Drop `--run` to just print the manifest + runbook.

## Cutover: point the workload at the copy

Pick one:

- **(a) Edit the claim reference.** Change the workload's `claimName` from
  `<source>` to `<source>-bard` and scale back up. Simplest; the PVC name changes.

- **(b) Preserve the original PVC name.** Keep the workload manifest untouched by
  rebinding the populated Bard PV under the original name:

  ```sh
  # the target's PV must survive deleting its PVC
  kubectl patch pv "$(kubectl -n <ns> get pvc <source>-bard -o jsonpath='{.spec.volumeName}')" \
    -p '{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}'
  kubectl -n <ns> delete pvc <source> <source>-bard       # both claims
  # recreate a PVC named <source> bound to the populated Bard PV:
  kubectl -n <ns> apply -f - <<EOF
  apiVersion: v1
  kind: PersistentVolumeClaim
  metadata: { name: <source>, namespace: <ns> }
  spec:
    accessModes: ["ReadWriteOnce"]
    storageClassName: ""
    resources: { requests: { storage: <size> } }
    volumeName: <the-bard-PV-name>
  EOF
  # (then set that PV's reclaimPolicy back to Delete if you want CSI to own it)
  ```

Scale the workload up, verify your app, **then** delete the old source PVC and its
backend volume once you are satisfied.

## Verify

The Job self-verifies (filesystem: an md5 manifest of every file on each side;
block: `cmp` over the source byte-length, so a larger rounded-up Bard volume still
passes). `MIGRATE_COPY_OK` in the Job log means the copy matched. After cutover,
check your workload reads its data and is writable.

## Caveats

- **Downtime = the copy.** Unlike in-place adoption (a pod restart), the workload
  is down for the whole copy. Size the maintenance window to the data volume.
- **Capacity.** The target requests the source's capacity; the backend may round
  up. For block, verification compares only the source length, so a larger target
  is fine.
- **Access modes carry over verbatim.** RBD serves multi-node only as raw block; a
  RWX *filesystem* source has no RBD equivalent (re-architect, or use CephFS).
- **The Job image needs egress** to `apk add rsync` (filesystem mode only). On an
  air-gapped cluster, bake an rsync-containing image and swap `image:`.
- **Special files.** `rsync -aHAXS` preserves perms/owners/symlinks/hardlinks/
  ACLs/xattrs/sparseness; `lost+found` is excluded. Device nodes inside a volume
  are not a normal case and are not special-cased.
- **Not for live databases without quiescing.** Stop the DB (step 1) or take an
  application-consistent backup and restore into the Bard PVC instead.
