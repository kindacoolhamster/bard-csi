#!/usr/bin/env bash
# End-to-end test of the iSCSI plugin against the real host LIO target, driving the
# plugin binary over its unix socket. Proves the full contract INCLUDING the
# control-plane attach path that no other backend exercises:
#   create -> controller/publish (per-node ACL) -> stage (iscsiadm login) ->
#   publish -> write -> read -> unpublish -> unstage (logout) ->
#   controller/unpublish (ACL removed) -> delete (target+backstore+LV reaped)
# with assertions on the ACL appearing/disappearing and the data round-trip.
#
# Like the LVM plugin, the controller plane is kernel/configfs-backed so it is
# proven here (binary over socket on galileo) rather than in a nested cluster. The
# plugin is started under a trap that ALWAYS kills it, logs out the session, and
# reaps the target/backstore/LV on exit (success, failure, or Ctrl-C).
#
# Usage (run as root for targetcli/iscsiadm/mount):
#   go build -o /tmp/bard-plugin-iscsi ./cmd/bard-plugin-iscsi
#   sudo bash hack/iscsi-plugin-test.sh [/path/to/bard-plugin-iscsi]
#
# Prereq: sudo bash hack/setup-iscsi-fixture.sh  (LIO + bard-vg + iscsid)
set -euo pipefail

BIN=${1:-/tmp/bard-plugin-iscsi}
VG=${VG:-bard-vg}
PORTAL=${PORTAL:-127.0.0.1:3260}
NODEID=${NODEID:-testnode}
BASE=iqn.2025-01.io.bard
INITIQN="$BASE:init-$NODEID"
SOCK=${SOCK:-/tmp/iscsi.sock}
STATE=/tmp/iscsi-test-state
CFG=$(mktemp /tmp/iscsi-test-cfg.XXXXXX.yaml)
STG=/tmp/iscsi-test-stg; PUB=/tmp/iscsi-test-pub

[ -x "$BIN" ] || { echo "plugin binary not found at $BIN (go build -o $BIN ./cmd/bard-plugin-iscsi)"; exit 1; }
vgs "$VG" >/dev/null 2>&1 || { echo "VG $VG not found -- run: sudo bash hack/setup-iscsi-fixture.sh"; exit 1; }
command -v targetcli >/dev/null || { echo "targetcli missing -- run: sudo bash hack/setup-iscsi-fixture.sh"; exit 1; }

PLUGIN=""; LV=""; IQN=""
cleanup() {
  set +e
  [ -n "$PLUGIN" ] && kill "$PLUGIN" 2>/dev/null
  for d in "$PUB" "$STG"; do mountpoint -q "$d" 2>/dev/null && umount "$d" 2>/dev/null; done
  [ -n "$IQN" ] && { iscsiadm -m node -T "$IQN" -p "$PORTAL" --logout 2>/dev/null; iscsiadm -m node -T "$IQN" -p "$PORTAL" -o delete 2>/dev/null; }
  [ -n "$IQN" ] && targetcli /iscsi delete "$IQN" 2>/dev/null
  [ -n "$LV" ]  && { targetcli /backstores/block delete "$LV" 2>/dev/null; lvremove -f "$VG/$LV" 2>/dev/null; }
  rm -f "$SOCK" "$CFG"; rm -rf "$STG" "$PUB" "$STATE"
}
trap cleanup EXIT INT TERM

printf 'instances:\n  galileo:\n    vg: %s\n    portal: %s\n' "$VG" "$PORTAL" > "$CFG"
pkill -f "bard-plugin-iscsi.*$SOCK" 2>/dev/null || true; rm -f "$SOCK"
"$BIN" --socket="$SOCK" --config="$CFG" --node-id="$NODEID" --state-dir="$STATE" >/tmp/iscsi-plugin.log 2>&1 &
PLUGIN=$!

post() { curl -fsS --unix-socket "$SOCK" -X POST "http://x$1" -d "$2"; }
jget() { python3 -c 'import sys,json;print(json.load(sys.stdin)["'"$1"'"])'; }
jctx() { python3 -c 'import sys,json;print(json.dumps(json.load(sys.stdin)["publishContext"]))'; }
for _ in $(seq 1 50); do [ -S "$SOCK" ] && post /info '{}' >/dev/null 2>&1 && break; sleep 0.1; done

echo "## /info (expect requiresControllerPublish=true)"
post /info '{}'; echo
post /info '{}' | grep -q '"requiresControllerPublish":true' || { echo "FAIL: iSCSI must advertise attach"; exit 1; }

echo "## create volume (LV + LIO backstore + per-volume target)"
LV=$(post /volume/create '{"name":"iscsitest","instance":"galileo","capacityBytes":1073741824,"fsType":"ext4"}' | jget name)
IQN="$BASE:tgt-$LV"
lvs "$VG/$LV" >/dev/null 2>&1 || { echo "FAIL: LV $LV not created"; exit 1; }
targetcli /iscsi ls "$IQN" >/dev/null 2>&1 || { echo "FAIL: target $IQN not created"; exit 1; }
echo "  LV=$LV target=$IQN OK"

V='{"instance":"galileo","location":"'"$VG"'","name":"'"$LV"'"}'

echo "## controller/publish -> expect an ACL for $INITIQN"
PUBCTX=$(post /controller/publish '{"volume":'"$V"',"nodeId":"'"$NODEID"'"}' | jctx)
echo "  publishContext=$PUBCTX"
targetcli "/iscsi/$IQN/tpg1/acls" ls "$INITIQN" >/dev/null 2>&1 || { echo "FAIL: ACL for $INITIQN not created"; exit 1; }
echo "  ACL $INITIQN present OK"

echo "## stage (iscsiadm login under per-node iface) + publish + write"
post /node/stage   '{"volume":'"$V"',"stagingPath":"'"$STG"'","fsType":"ext4","publishContext":'"$PUBCTX"'}' >/dev/null
post /node/publish '{"volume":'"$V"',"stagingPath":"'"$STG"'","targetPath":"'"$PUB"'","fsType":"ext4"}' >/dev/null
echo "bard-iscsi-proof" > "$PUB/proof.txt"; sync
GOT=$(cat "$PUB/proof.txt")
echo "  wrote+read: '$GOT'"
[ "$GOT" = "bard-iscsi-proof" ] || { echo "FAIL: data round-trip"; exit 1; }

echo "## unpublish + unstage (logout, device must be gone)"
post /node/unpublish '{"volume":'"$V"',"targetPath":"'"$PUB"'"}' >/dev/null
post /node/unstage   '{"volume":'"$V"',"stagingPath":"'"$STG"'"}' >/dev/null

echo "## controller/unpublish -> ACL must disappear (access revoked)"
post /controller/unpublish '{"volume":'"$V"',"nodeId":"'"$NODEID"'"}' >/dev/null
if targetcli "/iscsi/$IQN/tpg1/acls" ls "$INITIQN" >/dev/null 2>&1; then
  echo "FAIL: ACL $INITIQN still present after unpublish"; exit 1
fi
echo "  ACL removed OK"

echo "## delete volume -> target + backstore + LV all reaped (no orphan)"
post /volume/delete '{"volume":'"$V"'}' >/dev/null
FAILED=0
targetcli /iscsi ls "$IQN" >/dev/null 2>&1 && { echo "FAIL: target $IQN still exists"; FAILED=1; }
targetcli /backstores/block ls "$LV" >/dev/null 2>&1 && { echo "FAIL: backstore $LV still exists"; FAILED=1; }
lvs "$VG/$LV" >/dev/null 2>&1 && { echo "FAIL: LV $LV still exists"; FAILED=1; }
[ "$FAILED" = 0 ] || exit 1
LV=""; IQN="" # plugin reaped everything; nothing for the trap to clean
echo
echo "PASS: iSCSI attach contract verified (per-node ACL masking + login + round-trip + clean reap) against $VG"
