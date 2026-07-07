#!/usr/bin/env bash
# Resilience proof for the Ceph RBD backend, driven through a REAL workload: a
# single-instance PostgreSQL StatefulSet (hack/demo-postgres.yaml) whose data
# dir lives on a bard-rbd PVC. Run against the real-kernel k3s tier (krbd), NOT
# nested kind -- the failover fence needs a real per-node krbd watcher the mon
# can see (see CLAUDE.md "Real-node integration").
#
#   export KUBECONFIG=$HOME/.kube/config-k3s
#   bash hack/demo-postgres-resilience.sh            # all scenarios
#   bash hack/demo-postgres-resilience.sh A C D      # pick scenarios (skip the
#                                                    # node-failure one)
#
# Scenarios:
#   A  pod reschedule        -- delete the pod; data survives unstage->restage.
#   B  failover fence        -- stop the node hosting the DB, force the volume
#                               over to the other node; the new node's NodeStage
#                               blocklists the dead node's stale rbd watcher in
#                               Ceph BEFORE taking over (single-writer safety).
#                               Homelab-specific: needs multipass + 2 VMs named
#                               k3s-server / k3s-agent, DB pinned to the agent.
#   C  snapshot -> restore   -- snapshot the live DB, write more to the source,
#                               restore the snapshot to a new PVC + 2nd Postgres;
#                               the clone shows the point-in-time rows only.
#   D  online expand         -- grow the PVC while the DB runs; resize2fs online,
#                               no pod restart.
#
# Best-effort; each scenario prints PASS/FAIL. Not trap-cleaned -- the StatefulSet
# is meant to stay up as the demo workload; scenario C's restore artifacts are
# removed at the end of C, and `... cleanup` tears everything down.
set -uo pipefail

NS=${NS:-default}
SS=bard-postgres
POD=${SS}-0
PVC=data-${POD}
PSQL="psql -U postgres -d bard -v ON_ERROR_STOP=1"
CONN='--conf /dev/null -m 192.168.1.225:3300 --id k8s-csi-test --keyfile /etc/bard-ceph-keys/galileo'
POOL=k8s-csi-test
MON_VM_SERVER=k3s-server
MON_VM_AGENT=k3s-agent

red()  { printf '\033[31m%s\033[0m\n' "$*"; }
grn()  { printf '\033[32m%s\033[0m\n' "$*"; }
hdr()  { printf '\n=== %s ===\n' "$*"; }

kc()    { kubectl -n "$NS" "$@"; }
pg()    { kc exec "$POD" -- $PSQL "$@"; }
rows()  { kc exec "$POD" -- psql -U postgres -d bard -tAc "SELECT count(*) FROM ledger;" 2>/dev/null | tr -d '[:space:]'; }
nodepod_on() { # node-plugin pod on a given node
  kc -n kube-system get pod -l app=bard-csi-node \
    -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.nodeName}{"\n"}{end}' \
    2>/dev/null | awk -v n="$1" '$2==n{print $1}'
}
wait_ready() { # pod, timeout-secs
  local p=$1 t=${2:-180} i
  for ((i=0; i<t; i+=5)); do
    [ "$(kc get pod "$p" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)" = "True" ] && return 0
    sleep 5
  done
  return 1
}
img_for_pvc() { # echoes the rbd image (volume handle's last field) for a PVC
  local pv; pv=$(kc get pvc "$1" -o jsonpath='{.spec.volumeName}')
  kc get pv "$pv" -o jsonpath='{.spec.csi.volumeHandle}' | awk -F'|' '{print $NF}'
}

ensure_up() {
  kc get pod "$POD" >/dev/null 2>&1 || { red "no $POD -- apply hack/demo-postgres.yaml first"; exit 1; }
  wait_ready "$POD" 180 || { red "$POD not ready"; exit 1; }
  kc exec "$POD" -- psql -U postgres -d bard -tAc \
    "CREATE TABLE IF NOT EXISTS ledger (id serial primary key, note text, ts timestamptz default now());" >/dev/null
}

scenario_A() {
  hdr "A: pod reschedule (data survives unstage->restage)"
  local before; before=$(rows)
  pg -c "INSERT INTO ledger (note) VALUES ('A-before-reschedule');" >/dev/null
  echo "rows before delete: $(rows) (was $before)"
  kc delete pod "$POD" --wait=true >/dev/null
  wait_ready "$POD" 180 || { red "A FAIL: pod not ready after reschedule"; return 1; }
  local after; after=$(rows)
  kc get pod "$POD" -o wide --no-headers
  if [ "$after" -ge $((before + 1)) ]; then grn "A PASS: data intact ($after rows after reschedule)"; else red "A FAIL: rows=$after"; return 1; fi
}

scenario_B() {
  hdr "B: single-writer failover fence (stop node, fence stale watcher)"
  command -v multipass >/dev/null || { red "B SKIP: multipass not found"; return 0; }
  # The DB must be on the agent so we can stop it and fail over to the (control-plane) server.
  local node; node=$(kc get pod "$POD" -o jsonpath='{.spec.nodeName}')
  if [ "$node" != "$MON_VM_AGENT" ]; then
    echo "moving DB to $MON_VM_AGENT (cordon $MON_VM_SERVER)"
    kc cordon "$MON_VM_SERVER" >/dev/null; kc delete pod "$POD" --wait=true >/dev/null
    wait_ready "$POD" 180; kc uncordon "$MON_VM_SERVER" >/dev/null
  fi
  node=$(kc get pod "$POD" -o jsonpath='{.spec.nodeName}')
  [ "$node" = "$MON_VM_AGENT" ] || { red "B SKIP: DB not on $MON_VM_AGENT"; return 0; }

  local img apod watcher
  img=$(img_for_pvc "$PVC"); apod=$(nodepod_on "$MON_VM_AGENT")
  watcher=$(kc -n kube-system exec "$apod" -c ceph-rbd-plugin -- \
    sh -c "rbd $CONN status $POOL/$img 2>/dev/null" | sed -n 's/.*watcher=\([0-9.:/]*\).*/\1/p' | head -1)
  echo "agent krbd watcher to be fenced: $watcher"
  pg -c "INSERT INTO ledger (note) VALUES ('B-pre-node-failure');" -c "CHECKPOINT;" >/dev/null
  local before; before=$(rows)

  echo "stopping $MON_VM_AGENT ..."
  multipass exec "$MON_VM_AGENT" -- sudo systemctl stop k3s-agent
  for ((i=0;i<120;i+=5)); do
    [ "$(kc get node "$MON_VM_AGENT" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" != "True" ] && break; sleep 5
  done
  echo "$MON_VM_AGENT NotReady; forcing volume over to $MON_VM_SERVER"
  kc delete pod "$POD" --grace-period=0 --force >/dev/null 2>&1
  local va; va=$(kc get volumeattachment -o json | \
    python3 -c "import sys,json;[print(v['metadata']['name']) for v in json.load(sys.stdin)['items'] if v['spec'].get('nodeName')=='$MON_VM_AGENT']" 2>/dev/null)
  for v in $va; do
    kc patch volumeattachment "$v" -p '{"metadata":{"finalizers":[]}}' --type=merge >/dev/null 2>&1
    kc delete volumeattachment "$v" --grace-period=0 --force >/dev/null 2>&1
  done
  wait_ready "$POD" 240 || { red "B FAIL: pod did not recover on $MON_VM_SERVER"; multipass exec "$MON_VM_AGENT" -- sudo systemctl start k3s-agent; return 1; }

  local spod fenced after
  spod=$(nodepod_on "$MON_VM_SERVER")
  fenced=$(kc -n kube-system exec "$spod" -c ceph-rbd-plugin -- sh -c "ceph $CONN osd blocklist ls" 2>/dev/null | grep -F "$watcher")
  after=$(rows)
  echo "new placement: $(kc get pod "$POD" -o jsonpath='{.spec.nodeName}'); rows=$after (was $before)"
  echo "restoring $MON_VM_AGENT ..."; multipass exec "$MON_VM_AGENT" -- sudo systemctl start k3s-agent
  if [ -n "$fenced" ] && [ "$after" -ge "$before" ]; then
    grn "B PASS: stale watcher $watcher blocklisted in Ceph, data intact"
    echo "  blocklist entry: $fenced"
  else
    red "B FAIL: watcher fenced='$fenced' rows=$after/$before"; return 1
  fi
}

scenario_C() {
  hdr "C: snapshot -> restore (point-in-time clone)"
  pg -c "CHECKPOINT;" >/dev/null
  local at_snap; at_snap=$(rows)
  echo "rows at snapshot: $at_snap"
  cat <<YAML | kc apply -f - >/dev/null
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata: { name: pg-snap-1 }
spec:
  volumeSnapshotClassName: bard-rbd-snapshot
  source: { persistentVolumeClaimName: $PVC }
YAML
  for ((i=0;i<180;i+=5)); do
    [ "$(kc get volumesnapshot pg-snap-1 -o jsonpath='{.status.readyToUse}' 2>/dev/null)" = "true" ] && break; sleep 5
  done
  [ "$(kc get volumesnapshot pg-snap-1 -o jsonpath='{.status.readyToUse}')" = "true" ] || { red "C FAIL: snapshot not ready"; return 1; }
  pg -c "INSERT INTO ledger (note) VALUES ('C-AFTER-snapshot-live-only');" >/dev/null
  cat <<YAML | kc apply -f - >/dev/null
apiVersion: v1
kind: PersistentVolumeClaim
metadata: { name: pg-restore }
spec:
  accessModes: ["ReadWriteOnce"]
  storageClassName: bard-rbd
  resources: { requests: { storage: 1Gi } }
  dataSource: { name: pg-snap-1, kind: VolumeSnapshot, apiGroup: snapshot.storage.k8s.io }
---
apiVersion: v1
kind: Pod
metadata: { name: bard-postgres-restore, labels: { app: bard-postgres-restore } }
spec:
  containers:
    - name: postgres
      image: postgres:16-alpine
      env:
        - { name: PGDATA, value: /var/lib/postgresql/data/pgdata }
        - { name: POSTGRES_PASSWORD, value: bardpw }
      volumeMounts: [{ name: data, mountPath: /var/lib/postgresql/data }]
      readinessProbe: { exec: { command: ["pg_isready","-U","postgres","-d","bard"] }, initialDelaySeconds: 5, periodSeconds: 5 }
  volumes: [{ name: data, persistentVolumeClaim: { claimName: pg-restore } }]
YAML
  wait_ready bard-postgres-restore 240 || { red "C FAIL: restore pod not ready"; return 1; }
  local clone; clone=$(kc exec bard-postgres-restore -- psql -U postgres -d bard -tAc "SELECT count(*) FROM ledger;" | tr -d '[:space:]')
  local liveonly; liveonly=$(kc exec bard-postgres-restore -- psql -U postgres -d bard -tAc "SELECT count(*) FROM ledger WHERE note='C-AFTER-snapshot-live-only';" | tr -d '[:space:]')
  echo "clone rows: $clone (snapshot was $at_snap); live-only rows in clone: $liveonly (must be 0)"
  kc delete pod bard-postgres-restore --wait=false >/dev/null 2>&1
  kc delete pvc pg-restore --wait=false >/dev/null 2>&1
  kc delete volumesnapshot pg-snap-1 --wait=false >/dev/null 2>&1
  if [ "$clone" = "$at_snap" ] && [ "$liveonly" = "0" ]; then grn "C PASS: point-in-time clone (no post-snapshot writes)"; else red "C FAIL"; return 1; fi
}

scenario_D() {
  hdr "D: online volume expand (DB stays up)"
  local r0; r0=$(kc get pod "$POD" -o jsonpath='{.status.containerStatuses[0].restartCount}')
  local before; before=$(kc exec "$POD" -- df -P /var/lib/postgresql/data | awk 'END{print $2}')
  local want; want=$(kc get pvc "$PVC" -o jsonpath='{.spec.resources.requests.storage}')
  # bump 1Gi->2Gi (or +1Gi if already grown)
  local cur_gi; cur_gi=$(echo "$want" | tr -dc '0-9'); local new_gi=$((cur_gi + 1))
  kc patch pvc "$PVC" -p "{\"spec\":{\"resources\":{\"requests\":{\"storage\":\"${new_gi}Gi\"}}}}" >/dev/null
  local target_kb=$(( new_gi * 1024 * 1024 * 90 / 100 ))  # ~90% of new size in 1K blocks
  for ((i=0;i<180;i+=5)); do
    local now; now=$(kc exec "$POD" -- df -P /var/lib/postgresql/data | awk 'END{print $2}')
    [ "${now:-0}" -ge "$target_kb" ] && break; sleep 5
  done
  local after; after=$(kc exec "$POD" -- df -P /var/lib/postgresql/data | awk 'END{print $2}')
  local r1; r1=$(kc get pod "$POD" -o jsonpath='{.status.containerStatuses[0].restartCount}')
  echo "fs 1K-blocks: $before -> $after ; pod restarts: $r0 -> $r1 ; pvc cap: $(kc get pvc "$PVC" -o jsonpath='{.status.capacity.storage}')"
  if [ "$after" -gt "$before" ] && [ "$r0" = "$r1" ]; then grn "D PASS: grew online, no restart"; else red "D FAIL"; return 1; fi
}

cleanup() {
  hdr "cleanup"
  kc delete pod bard-postgres-restore --ignore-not-found --wait=false
  kc delete pvc pg-restore --ignore-not-found --wait=false
  kc delete volumesnapshot pg-snap-1 --ignore-not-found --wait=false
  kc delete -f "$(dirname "$0")/demo-postgres.yaml" --ignore-not-found
  kc delete pvc "$PVC" --ignore-not-found
}

main() {
  if [ "${1:-}" = "cleanup" ]; then cleanup; return; fi
  ensure_up
  local want=("$@"); [ ${#want[@]} -eq 0 ] && want=(A B C D)
  local rc=0
  for s in "${want[@]}"; do
    case "$s" in
      A) scenario_A || rc=1;; B) scenario_B || rc=1;;
      C) scenario_C || rc=1;; D) scenario_D || rc=1;;
      *) red "unknown scenario: $s";;
    esac
  done
  hdr "done"; [ $rc -eq 0 ] && grn "all selected scenarios PASS" || red "some scenarios FAILED"
  return $rc
}
main "$@"
