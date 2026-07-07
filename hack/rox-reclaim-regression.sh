#!/usr/bin/env bash
# Regression suite for the RBD read-only-by-access-mode fix and the csi-addons
# ReclaimSpace RBAC fix. Run on the k3s krbd tier. Trap-cleaned.
#
#   export KUBECONFIG=$HOME/.kube/config-k3s
#   bash hack/rox-reclaim-regression.sh
#
# The point is to prove the access-mode read-only map did NOT turn writable volumes
# read-only (the time-bomb risk), while ROX really is read-only, and that reclaim
# still works end to end.
set -uo pipefail

NS=default
KEYARGS=(--conf /dev/null -m 192.168.1.225:3300 --id k8s-csi-test --keyfile /etc/bard-ceph-keys/galileo)
POOL=k8s-csi-test
PASS=0; FAIL=0
grn(){ printf '\033[32mPASS\033[0m %s\n' "$*"; PASS=$((PASS+1)); }
red(){ printf '\033[31mFAIL\033[0m %s\n' "$*"; FAIL=$((FAIL+1)); }
kc(){ kubectl -n "$NS" "$@"; }
nodepod(){ kubectl -n kube-system get pod -l app=bard-csi-node -o jsonpath="{range .items[*]}{.metadata.name}{' '}{.spec.nodeName}{'\n'}{end}" | awk -v n="$1" '$2==n{print $1}'; }
SERVERNP=$(nodepod k3s-server)
imgof(){ kc get pv "$(kc get pvc "$1" -o jsonpath='{.spec.volumeName}')" -o jsonpath='{.spec.csi.volumeHandle}' | awk -F'|' '{print $NF}'; }
used_mib(){ kubectl -n kube-system exec "$SERVERNP" -c ceph-rbd-plugin -- rbd "${KEYARGS[@]}" du "$POOL/$1" 2>/dev/null | awk 'END{u=$(NF-1); if($NF=="GiB")u=u*1024; printf "%d", u}'; }
ready(){ for i in $(seq 1 "${2:-48}"); do [ "$(kc get pod "$1" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)" = True ] && return 0; sleep 5; done; return 1; }
snapready(){ for i in $(seq 1 36); do [ "$(kc get volumesnapshot "$1" -o jsonpath='{.status.readyToUse}' 2>/dev/null)" = true ] && return 0; sleep 5; done; return 1; }

cleanup(){
  kc delete pod -l regression=rox-reclaim --grace-period=0 --force >/dev/null 2>&1
  kc delete pvc -l regression=rox-reclaim >/dev/null 2>&1
  kc delete volumesnapshot -l regression=rox-reclaim >/dev/null 2>&1
  kc delete reclaimspacejob -l regression=rox-reclaim >/dev/null 2>&1
}
trap cleanup EXIT
cleanup

echo "=== 1. RWO filesystem must be WRITABLE (time-bomb check) ==="
cat <<YAML | kc apply -f - >/dev/null
apiVersion: v1
kind: PersistentVolumeClaim
metadata: { name: reg-rwo-fs, labels: { regression: rox-reclaim } }
spec: { accessModes: [ReadWriteOnce], storageClassName: bard-rbd, resources: { requests: { storage: 1Gi } } }
---
apiVersion: v1
kind: Pod
metadata: { name: reg-rwo-fs, labels: { regression: rox-reclaim } }
spec:
  containers: [{ name: a, image: busybox:1.36, command: ["sh","-c","sleep 36000"], volumeMounts: [{ name: v, mountPath: /d }] }]
  volumes: [{ name: v, persistentVolumeClaim: { claimName: reg-rwo-fs } }]
YAML
ready reg-rwo-fs && kc exec reg-rwo-fs -- sh -c 'echo w > /d/x && sync && cat /d/x' 2>/dev/null | grep -q w \
  && grn "RWO fs writable" || red "RWO fs NOT writable (read-only regression!)"

echo "=== 2. RWO raw block must be WRITABLE ==="
cat <<YAML | kc apply -f - >/dev/null
apiVersion: v1
kind: PersistentVolumeClaim
metadata: { name: reg-rwo-blk, labels: { regression: rox-reclaim } }
spec: { accessModes: [ReadWriteOnce], volumeMode: Block, storageClassName: bard-rbd, resources: { requests: { storage: 1Gi } } }
---
apiVersion: v1
kind: Pod
metadata: { name: reg-rwo-blk, labels: { regression: rox-reclaim } }
spec:
  containers: [{ name: a, image: busybox:1.36, command: ["sh","-c","sleep 36000"], volumeDevices: [{ name: v, devicePath: /dev/xvda }] }]
  volumes: [{ name: v, persistentVolumeClaim: { claimName: reg-rwo-blk } }]
YAML
ready reg-rwo-blk && kc exec reg-rwo-blk -- sh -c 'echo RWOBLK | dd of=/dev/xvda bs=512 conv=notrunc 2>/dev/null && dd if=/dev/xvda bs=512 count=1 2>/dev/null | head -c 6' 2>/dev/null | grep -q RWOBLK \
  && grn "RWO block writable" || red "RWO block NOT writable (read-only regression!)"

echo "=== 3. ROX block (from snapshot, no readOnly:true) must be READ-ONLY ==="
cat <<YAML | kc apply -f - >/dev/null
apiVersion: v1
kind: PersistentVolumeClaim
metadata: { name: reg-src, labels: { regression: rox-reclaim } }
spec: { accessModes: [ReadWriteOnce], volumeMode: Block, storageClassName: bard-rbd, resources: { requests: { storage: 1Gi } } }
---
apiVersion: v1
kind: Pod
metadata: { name: reg-src, labels: { regression: rox-reclaim } }
spec:
  containers: [{ name: a, image: busybox:1.36, command: ["sh","-c","echo REGROXDATA | dd of=/dev/xvda bs=512 conv=notrunc 2>/dev/null; sync; sleep 36000"], volumeDevices: [{ name: v, devicePath: /dev/xvda }] }]
  volumes: [{ name: v, persistentVolumeClaim: { claimName: reg-src } }]
YAML
ready reg-src
cat <<YAML | kc apply -f - >/dev/null
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata: { name: reg-snap, labels: { regression: rox-reclaim } }
spec: { volumeSnapshotClassName: bard-rbd-snapshot, source: { persistentVolumeClaimName: reg-src } }
YAML
snapready reg-snap
cat <<YAML | kc apply -f - >/dev/null
apiVersion: v1
kind: PersistentVolumeClaim
metadata: { name: reg-rox-blk, labels: { regression: rox-reclaim } }
spec:
  accessModes: [ReadOnlyMany]
  volumeMode: Block
  storageClassName: bard-rbd
  resources: { requests: { storage: 1Gi } }
  dataSource: { name: reg-snap, kind: VolumeSnapshot, apiGroup: snapshot.storage.k8s.io }
---
apiVersion: v1
kind: Pod
metadata: { name: reg-rox-blk, labels: { regression: rox-reclaim } }
spec:
  containers: [{ name: a, image: busybox:1.36, command: ["sh","-c","sleep 36000"], volumeDevices: [{ name: v, devicePath: /dev/xvda }] }]
  volumes: [{ name: v, persistentVolumeClaim: { claimName: reg-rox-blk } }]
YAML
if ready reg-rox-blk; then
  rd=$(kc exec reg-rox-blk -- sh -c 'dd if=/dev/xvda bs=512 count=1 2>/dev/null | head -c 10' 2>/dev/null)
  kc exec reg-rox-blk -- sh -c 'echo HACK | dd of=/dev/xvda bs=512 conv=notrunc 2>/dev/null' 2>/dev/null
  after=$(kc exec reg-rox-blk -- sh -c 'dd if=/dev/xvda bs=512 count=1 2>/dev/null | head -c 10' 2>/dev/null)
  if [ "$rd" = REGROXDATA ] && [ "$after" = REGROXDATA ]; then grn "ROX block read-only (write had no effect, data intact)"; else red "ROX block NOT protected (read='$rd' after-write='$after')"; fi
else red "ROX block pod not ready"; fi

echo "=== 4. ROX filesystem must be REJECTED at provision ==="
cat <<YAML | kc apply -f - >/dev/null
apiVersion: v1
kind: PersistentVolumeClaim
metadata: { name: reg-rox-fs, labels: { regression: rox-reclaim } }
spec: { accessModes: [ReadOnlyMany], storageClassName: bard-rbd, resources: { requests: { storage: 1Gi } } }
---
apiVersion: v1
kind: Pod
metadata: { name: reg-rox-fs, labels: { regression: rox-reclaim } }
spec:
  containers: [{ name: a, image: busybox:1.36, command: ["sh","-c","sleep 3600"], volumeMounts: [{ name: v, mountPath: /d, readOnly: true }] }]
  volumes: [{ name: v, persistentVolumeClaim: { claimName: reg-rox-fs } }]
YAML
sleep 12
if kc get events --field-selector involvedObject.name=reg-rox-fs 2>/dev/null | grep -qi 'MULTI_NODE_READER_ONLY for a filesystem'; then
  grn "ROX filesystem rejected"
else red "ROX filesystem NOT rejected (phase=$(kc get pvc reg-rox-fs -o jsonpath='{.status.phase}'))"; fi

echo "=== 5. RWX raw block (multi-node) must be WRITABLE on both nodes ==="
cat <<YAML | kc apply -f - >/dev/null
apiVersion: v1
kind: PersistentVolumeClaim
metadata: { name: reg-rwx-blk, labels: { regression: rox-reclaim } }
spec: { accessModes: [ReadWriteMany], volumeMode: Block, storageClassName: bard-rbd, resources: { requests: { storage: 1Gi } } }
---
apiVersion: v1
kind: Pod
metadata: { name: reg-rwx-a, labels: { regression: rox-reclaim } }
spec:
  nodeSelector: { kubernetes.io/hostname: k3s-server }
  containers: [{ name: a, image: busybox:1.36, command: ["sh","-c","sleep 36000"], volumeDevices: [{ name: v, devicePath: /dev/xvda }] }]
  volumes: [{ name: v, persistentVolumeClaim: { claimName: reg-rwx-blk } }]
---
apiVersion: v1
kind: Pod
metadata: { name: reg-rwx-b, labels: { regression: rox-reclaim } }
spec:
  nodeSelector: { kubernetes.io/hostname: k3s-agent }
  containers: [{ name: a, image: busybox:1.36, command: ["sh","-c","sleep 36000"], volumeDevices: [{ name: v, devicePath: /dev/xvda }] }]
  volumes: [{ name: v, persistentVolumeClaim: { claimName: reg-rwx-blk } }]
YAML
if ready reg-rwx-a && ready reg-rwx-b; then
  a=$(kc exec reg-rwx-a -- sh -c 'echo RWXA | dd of=/dev/xvda bs=512 seek=0 conv=notrunc 2>/dev/null && echo ok' 2>/dev/null)
  b=$(kc exec reg-rwx-b -- sh -c 'echo RWXB | dd of=/dev/xvda bs=512 seek=1 conv=notrunc 2>/dev/null && echo ok' 2>/dev/null)
  [ "$a" = ok ] && [ "$b" = ok ] && grn "RWX block writable on both nodes" || red "RWX block write failed (a=$a b=$b)"
else red "RWX block pods not ready"; fi

echo "=== 6. ReclaimSpace ONLINE (mounted -> node fstrim) succeeds + frees space ==="
cat <<YAML | kc apply -f - >/dev/null
apiVersion: v1
kind: PersistentVolumeClaim
metadata: { name: reg-reclaim-on, labels: { regression: rox-reclaim } }
spec: { accessModes: [ReadWriteOnce], storageClassName: bard-rbd, resources: { requests: { storage: 2Gi } } }
---
apiVersion: v1
kind: Pod
metadata: { name: reg-reclaim-on, labels: { regression: rox-reclaim } }
spec:
  containers: [{ name: a, image: busybox:1.36, command: ["sh","-c","dd if=/dev/urandom of=/d/big bs=1M count=400 2>/dev/null; sync; rm /d/big; sync; sleep 36000"], volumeMounts: [{ name: v, mountPath: /d }] }]
  volumes: [{ name: v, persistentVolumeClaim: { claimName: reg-reclaim-on } }]
YAML
if ready reg-reclaim-on; then
  sleep 8; img=$(imgof reg-reclaim-on); before=$(used_mib "$img")
  cat <<YAML | kc apply -f - >/dev/null
apiVersion: csiaddons.openshift.io/v1alpha1
kind: ReclaimSpaceJob
metadata: { name: reg-reclaim-on, labels: { regression: rox-reclaim } }
spec: { target: { persistentVolumeClaim: reg-reclaim-on } }
YAML
  for i in $(seq 1 48); do r=$(kc get reclaimspacejob reg-reclaim-on -o jsonpath='{.status.result}' 2>/dev/null); [ -n "$r" ] && break; sleep 5; done
  after=$(used_mib "$img")
  echo "  result=$r  used ${before}MiB -> ${after}MiB"
  [ "$r" = Succeeded ] && [ "$after" -lt $((before/2)) ] && grn "online reclaim freed space" || red "online reclaim (result=$r ${before}->${after})"
else red "reclaim-online pod not ready"; fi

echo "=== 7. ReclaimSpace OFFLINE (unmounted -> controller sparsify) succeeds + frees space ==="
cat <<YAML | kc apply -f - >/dev/null
apiVersion: v1
kind: PersistentVolumeClaim
metadata: { name: reg-reclaim-off, labels: { regression: rox-reclaim } }
spec: { accessModes: [ReadWriteOnce], storageClassName: bard-rbd, resources: { requests: { storage: 2Gi } } }
---
apiVersion: v1
kind: Pod
metadata: { name: reg-reclaim-off-w, labels: { regression: rox-reclaim } }
spec:
  containers: [{ name: a, image: busybox:1.36, command: ["sh","-c","dd if=/dev/zero of=/d/zeros bs=1M count=400 2>/dev/null; sync; echo done; sleep 5"], volumeMounts: [{ name: v, mountPath: /d }] }]
  restartPolicy: Never
  volumes: [{ name: v, persistentVolumeClaim: { claimName: reg-reclaim-off } }]
YAML
for i in $(seq 1 36); do [ "$(kc get pod reg-reclaim-off-w -o jsonpath='{.status.phase}' 2>/dev/null)" = Succeeded ] && break; sleep 5; done
img=$(imgof reg-reclaim-off); before=$(used_mib "$img")
pv=$(kc get pvc reg-reclaim-off -o jsonpath='{.spec.volumeName}')
kc delete pod reg-reclaim-off-w --grace-period=0 --force >/dev/null 2>&1
# Wait for FULL detach (no VolumeAttachment) before the job: with attach enabled,
# csi-addons routes to the node (online fstrim) path while a VA still exists, but the
# globalmount is already gone -> fstrim fails. Once detached it uses the offline
# (controller sparsify) path. (Skipping this wait was the original false failure.)
for i in $(seq 1 36); do
  [ -z "$(kubectl get volumeattachment -o jsonpath="{range .items[?(@.spec.source.persistentVolumeName=='$pv')]}{.metadata.name}{end}" 2>/dev/null)" ] && break
  sleep 5
done
cat <<YAML | kc apply -f - >/dev/null
apiVersion: csiaddons.openshift.io/v1alpha1
kind: ReclaimSpaceJob
metadata: { name: reg-reclaim-off, labels: { regression: rox-reclaim } }
spec: { target: { persistentVolumeClaim: reg-reclaim-off } }
YAML
for i in $(seq 1 48); do r=$(kc get reclaimspacejob reg-reclaim-off -o jsonpath='{.status.result}' 2>/dev/null); [ -n "$r" ] && break; sleep 5; done
after=$(used_mib "$img")
echo "  result=$r  used ${before}MiB -> ${after}MiB"
[ "$r" = Succeeded ] && [ "$after" -lt $((before/2)) ] && grn "offline reclaim freed space" || red "offline reclaim (result=$r ${before}->${after})"

echo "=== 8. existing Postgres (normal RWO) integrity intact ==="
if kc get pod bard-postgres-0 >/dev/null 2>&1; then
  kc exec bard-postgres-0 -- pg_amcheck -U postgres --heapallindexed -d bard >/dev/null 2>&1 \
    && grn "postgres amcheck clean (normal RWO unaffected)" || red "postgres amcheck failed"
else echo "  (bard-postgres-0 not present, skipping)"; fi

echo
echo "==================== REGRESSION SUMMARY: $PASS passed, $FAIL failed ===================="
[ "$FAIL" -eq 0 ]
