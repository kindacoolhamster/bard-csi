#!/usr/bin/env bash
# Runs bard-plugin-conformance against the iSCSI plugin on the host LIO fixture
# -- the attach-backend counterpart of conformance-localpath-test.sh, exercising
# the controller/publish + node legs (CHAP on) plus every declared capability.
# Trap-cleaned: kills the plugin and sweeps any leaked targets/backstores/LVs.
#
#   go build -o /tmp/bard-plugin-iscsi ./cmd/bard-plugin-iscsi
#   go build -o /tmp/bard-plugin-conformance ./cmd/bard-plugin-conformance
#   sudo bash hack/conformance-iscsi-test.sh   # root: targetcli/iscsiadm/mounts
#
# Prereq: sudo bash hack/setup-iscsi-fixture.sh  (LIO + bard-vg + iscsid)
# NOTE -node-id must match the plugin's --node-id: the node plane logs in with
# the initiator IQN derived from it, while the ACL from controller/publish is
# for the node id the TOOL sends -- mismatched values are a (correct)
# authorization failure at login.
# NOTE this stays pointed at a LOCAL (targetcli-managed) instance, not a
# targetd one, on purpose: the conformance tool hard-gates its snapshot/clone
# tests on the plugin's DECLARED capabilities (/info), which are plugin-global,
# not per-instance -- a targetd instance rejects snapshot/clone per-request
# (CodeUnsupported) while /info still advertises snapshots=true because a
# local instance in the same process supports it. Snapshot/clone conformance
# against a targetd instance would need per-instance capability plumbing this
# plugin doesn't have; targetd's own contract (including its clean Unsupported
# rejection) is instead covered end to end by hack/targetd-plugin-test.sh.
set -uo pipefail
BIN=${1:-/tmp/bard-plugin-iscsi}
TOOL=${TOOL:-/tmp/bard-plugin-conformance}
VG=bard-vg; POOL=bard-thin; PORTAL=127.0.0.1:3260; NODEID=confnode
SOCK=$(mktemp -u /tmp/iscsi-conf.XXXXXX.sock)
CFG=$(mktemp /tmp/iscsi-conf-cfg.XXXXXX.yaml)
CHAPDIR=$(mktemp -d /tmp/iscsi-conf-chap.XXXXXX)
STATE=$(mktemp -d /tmp/iscsi-conf-state.XXXXXX)
LOG=$(mktemp /tmp/iscsi-conf-plugin.XXXXXX.log)

PID=""
cleanup() {
  set +e
  [ -n "$PID" ] && kill "$PID" 2>/dev/null
  # sweep anything the tool leaked (it cleans up itself on success)
  for t in $(targetcli /iscsi ls 2>/dev/null | grep -o 'iqn.2025-01.io.bard:tgt-[a-z0-9-]*'); do
    iscsiadm -m node -T "$t" -p "$PORTAL" --logout 2>/dev/null
    iscsiadm -m node -T "$t" -p "$PORTAL" -o delete 2>/dev/null
    targetcli /iscsi delete "$t" 2>/dev/null
  done
  for l in $(lvs --noheadings -o lv_name "$VG" 2>/dev/null | tr -d ' ' | grep -E '^(bard|snap)-[0-9a-f]{16}$'); do
    targetcli /backstores/block delete "$l" 2>/dev/null
    lvremove -f "$VG/$l" 2>/dev/null
  done
  rm -rf "$CHAPDIR" "$STATE"; rm -f "$SOCK" "$CFG"
}
trap cleanup EXIT INT TERM

lvs "$VG/$POOL" >/dev/null 2>&1 || lvcreate --type thin-pool -L 4G -n "$POOL" "$VG" >/dev/null
printf 'instances:\n  galileo:\n    vg: %s\n    portal: %s\n    thinPool: %s\n    chapAuth: true\n' "$VG" "$PORTAL" "$POOL" > "$CFG"
printf 'bard\nconformance-chap-secret\n' > "$CHAPDIR/galileo"
"$BIN" --socket="$SOCK" --config="$CFG" --node-id="$NODEID" --state-dir="$STATE" --chap-dir="$CHAPDIR" >"$LOG" 2>&1 &
PID=$!
for _ in $(seq 1 50); do [ -S "$SOCK" ] && break; sleep 0.1; done

NODE=""
[ "$(id -u)" = 0 ] && NODE="-node"
"$TOOL" -instance galileo -node-id "$NODEID" $NODE "$SOCK"
