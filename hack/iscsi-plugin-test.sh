#!/usr/bin/env bash
# End-to-end test of the iSCSI plugin against the real host LIO target, driving the
# plugin binary over its unix socket. Proves the full contract INCLUDING the
# control-plane attach path that no other backend exercises, CHAP enforcement, and
# the thin snapshot/restore surface:
#   create (thin LV + LIO target, authentication=1) ->
#   controller/publish (per-node ACL + CHAP creds on it) ->
#   stage (iscsiadm CHAP login) -> publish -> write ->
#   snapshot (read-only thin LV, NOT exported) -> write more (post-snapshot) ->
#   restore into a LARGER volume (own target; fs grown at stage) ->
#   point-in-time check (pre-snapshot data present, post-snapshot data absent) ->
#   CHAP negative (wrong-password login rejected) ->
#   unwind + delete (targets + backstores + LVs + snapshot all reaped)
#
# Like the LVM plugin, the controller plane is kernel/configfs-backed so it is
# proven here (binary over socket on galileo) rather than in a nested cluster. The
# plugin is started under a trap that ALWAYS kills it, logs out sessions, and
# reaps the targets/backstores/LVs on exit (success, failure, or Ctrl-C).
#
# Usage (run as root for targetcli/iscsiadm/mount):
#   go build -o /tmp/bard-plugin-iscsi ./cmd/bard-plugin-iscsi
#   sudo bash hack/iscsi-plugin-test.sh [/path/to/bard-plugin-iscsi]
#
# Prereq: sudo bash hack/setup-iscsi-fixture.sh  (LIO + bard-vg + iscsid)
set -euo pipefail

BIN=${1:-/tmp/bard-plugin-iscsi}
VG=${VG:-bard-vg}
POOL=${POOL:-bard-thin}
PORTAL=${PORTAL:-127.0.0.1:3260}
NODEID=${NODEID:-testnode}
BASE=iqn.2025-01.io.bard
INITIQN="$BASE:init-$NODEID"
CHAPUSER=bard
CHAPPASS=bardchapsecret123
SOCK=${SOCK:-/tmp/iscsi.sock}
STATE=/tmp/iscsi-test-state
CFG=$(mktemp /tmp/iscsi-test-cfg.XXXXXX.yaml)
CHAPDIR=$(mktemp -d /tmp/iscsi-test-chap.XXXXXX)
STG=/tmp/iscsi-test-stg; PUB=/tmp/iscsi-test-pub
STG2=/tmp/iscsi-test-stg2; PUB2=/tmp/iscsi-test-pub2

[ -x "$BIN" ] || { echo "plugin binary not found at $BIN (go build -o $BIN ./cmd/bard-plugin-iscsi)"; exit 1; }
vgs "$VG" >/dev/null 2>&1 || { echo "VG $VG not found -- run: sudo bash hack/setup-iscsi-fixture.sh"; exit 1; }
command -v targetcli >/dev/null || { echo "targetcli missing -- run: sudo bash hack/setup-iscsi-fixture.sh"; exit 1; }

# Ensure a thin pool exists (idempotent) -- snapshots/clone need thin LVs.
lvs "$VG/$POOL" >/dev/null 2>&1 || lvcreate --type thin-pool -L 4G -n "$POOL" "$VG" >/dev/null

PLUGIN=""; LV=""; LV2=""; SNAP=""; IQN=""; IQN2=""
cleanup() {
  set +e
  [ -n "$PLUGIN" ] && kill "$PLUGIN" 2>/dev/null
  for d in "$PUB2" "$STG2" "$PUB" "$STG"; do mountpoint -q "$d" 2>/dev/null && umount "$d" 2>/dev/null; done
  for t in "$IQN" "$IQN2"; do
    [ -n "$t" ] || continue
    iscsiadm -m node -T "$t" -p "$PORTAL" --logout 2>/dev/null
    iscsiadm -m node -T "$t" -p "$PORTAL" -o delete 2>/dev/null
    targetcli /iscsi delete "$t" 2>/dev/null
  done
  for l in "$LV" "$LV2"; do
    [ -n "$l" ] || continue
    targetcli /backstores/block delete "$l" 2>/dev/null
    lvremove -f "$VG/$l" 2>/dev/null
  done
  [ -n "$SNAP" ] && lvremove -f "$VG/$SNAP" 2>/dev/null
  rm -f "$SOCK" "$CFG"; rm -rf "$STG" "$PUB" "$STG2" "$PUB2" "$STATE" "$CHAPDIR"
}
trap cleanup EXIT INT TERM

printf 'instances:\n  galileo:\n    vg: %s\n    portal: %s\n    thinPool: %s\n    chapAuth: true\n' \
  "$VG" "$PORTAL" "$POOL" > "$CFG"
printf '%s\n%s\n' "$CHAPUSER" "$CHAPPASS" > "$CHAPDIR/galileo"
chmod 600 "$CHAPDIR/galileo"
pkill -f "bard-plugin-iscsi.*$SOCK" 2>/dev/null || true; rm -f "$SOCK"
"$BIN" --socket="$SOCK" --config="$CFG" --node-id="$NODEID" --state-dir="$STATE" --chap-dir="$CHAPDIR" >/tmp/iscsi-plugin.log 2>&1 &
PLUGIN=$!

post() { curl -fsS --unix-socket "$SOCK" -X POST "http://x$1" -d "$2"; }
jget() { python3 -c 'import sys,json;print(json.load(sys.stdin)["'"$1"'"])'; }
jctx() { python3 -c 'import sys,json;print(json.dumps(json.load(sys.stdin)["publishContext"]))'; }
for _ in $(seq 1 50); do [ -S "$SOCK" ] && post /info '{}' >/dev/null 2>&1 && break; sleep 0.1; done

echo "## /info (expect requiresControllerPublish=true AND snapshots=true)"
post /info '{}'; echo
post /info '{}' | grep -q '"requiresControllerPublish":true' || { echo "FAIL: iSCSI must advertise attach"; exit 1; }
post /info '{}' | grep -q '"snapshots":true' || { echo "FAIL: iSCSI must advertise snapshots"; exit 1; }

echo "## create volume (thin LV + LIO backstore + per-volume target, CHAP required)"
LV=$(post /volume/create '{"name":"iscsitest","instance":"galileo","capacityBytes":1073741824,"fsType":"ext4"}' | jget name)
IQN="$BASE:tgt-$LV"
lvs "$VG/$LV" >/dev/null 2>&1 || { echo "FAIL: LV $LV not created"; exit 1; }
ATTR=$(lvs --noheadings -o lv_attr "$VG/$LV" | tr -d ' ')
[ "${ATTR:0:1}" = "V" ] || { echo "FAIL: $LV is not a thin volume (attr=$ATTR)"; exit 1; }
targetcli /iscsi ls "$IQN" >/dev/null 2>&1 || { echo "FAIL: target $IQN not created"; exit 1; }
targetcli "/iscsi/$IQN/tpg1" get attribute authentication | grep -q 'authentication=1' \
  || { echo "FAIL: CHAP instance must set authentication=1 on the TPG"; exit 1; }
echo "  LV=$LV (thin, attr=$ATTR) target=$IQN authentication=1 OK"

V='{"instance":"galileo","location":"'"$VG"'","name":"'"$LV"'"}'

echo "## controller/publish -> expect an ACL for $INITIQN carrying the CHAP creds"
PUBCTX=$(post /controller/publish '{"volume":'"$V"',"nodeId":"'"$NODEID"'"}' | jctx)
echo "  publishContext=$PUBCTX"
echo "$PUBCTX" | grep -q "$CHAPPASS" && { echo "FAIL: CHAP password leaked into publishContext"; exit 1; }
targetcli "/iscsi/$IQN/tpg1/acls" ls "$INITIQN" >/dev/null 2>&1 || { echo "FAIL: ACL for $INITIQN not created"; exit 1; }
targetcli "/iscsi/$IQN/tpg1/acls/$INITIQN" get auth userid | grep -q "userid=$CHAPUSER" \
  || { echo "FAIL: CHAP userid not set on the ACL"; exit 1; }
echo "  ACL $INITIQN present with CHAP userid OK (no creds in publishContext)"

echo "## stage (iscsiadm CHAP login under per-node iface) + publish + write"
post /node/stage   '{"volume":'"$V"',"stagingPath":"'"$STG"'","fsType":"ext4","publishContext":'"$PUBCTX"'}' >/dev/null
post /node/publish '{"volume":'"$V"',"stagingPath":"'"$STG"'","targetPath":"'"$PUB"'","fsType":"ext4"}' >/dev/null
echo "bard-iscsi-proof" > "$PUB/proof.txt"; sync
GOT=$(cat "$PUB/proof.txt")
echo "  wrote+read: '$GOT'"
[ "$GOT" = "bard-iscsi-proof" ] || { echo "FAIL: data round-trip"; exit 1; }

echo "## snapshot (read-only thin LV, control-plane only -- no LIO export)"
SNAP=$(post /snapshot/create '{"name":"iscsisnap","sourceVolume":'"$V"'}' | jget name)
SATTR=$(lvs --noheadings -o lv_attr "$VG/$SNAP" | tr -d ' ')
[ "${SATTR:0:3}" = "Vri" ] || { echo "FAIL: $SNAP is not a read-only thin snapshot (attr=$SATTR)"; exit 1; }
targetcli /iscsi ls "$BASE:tgt-$SNAP" >/dev/null 2>&1 && { echo "FAIL: snapshot must not be exported through LIO"; exit 1; }
post /snapshot/list '{}' | grep -q '"name":"'"$SNAP"'"' || { echo "FAIL: /snapshot/list must report $SNAP"; exit 1; }
post /snapshot/list '{}' | grep -q '"sourceVolume":{[^}]*"name":"'"$LV"'"' || { echo "FAIL: snapshot must carry its origin $LV"; exit 1; }
echo "  SNAP=$SNAP (attr=$SATTR, listed with origin, no target) OK"

echo "## write post-snapshot data (must NOT appear in the restore)"
echo "post-snapshot" > "$PUB/after.txt"; sync

echo "## restore into a LARGER (2Gi) volume -> own target, fs grown at stage"
LV2=$(post /volume/create '{"name":"iscsirestore","instance":"galileo","capacityBytes":2147483648,"fsType":"ext4","sourceSnapshot":{"instance":"galileo","location":"'"$VG"'","name":"'"$SNAP"'"}}' | jget name)
IQN2="$BASE:tgt-$LV2"
SIZE2=$(lvs --noheadings --units b --nosuffix -o lv_size "$VG/$LV2" | tr -d ' ')
[ "$SIZE2" = "2147483648" ] || { echo "FAIL: restored LV not grown to 2Gi (size=$SIZE2)"; exit 1; }
targetcli /iscsi ls "$IQN2" >/dev/null 2>&1 || { echo "FAIL: restored volume has no target"; exit 1; }
V2='{"instance":"galileo","location":"'"$VG"'","name":"'"$LV2"'"}'
PUBCTX2=$(post /controller/publish '{"volume":'"$V2"',"nodeId":"'"$NODEID"'"}' | jctx)
post /node/stage   '{"volume":'"$V2"',"stagingPath":"'"$STG2"'","fsType":"ext4","publishContext":'"$PUBCTX2"'}' >/dev/null
post /node/publish '{"volume":'"$V2"',"stagingPath":"'"$STG2"'","targetPath":"'"$PUB2"'","fsType":"ext4"}' >/dev/null
GOT2=$(cat "$PUB2/proof.txt")
[ "$GOT2" = "bard-iscsi-proof" ] || { echo "FAIL: restored data mismatch ('$GOT2')"; exit 1; }
[ -e "$PUB2/after.txt" ] && { echo "FAIL: post-snapshot data leaked into the restore (not point-in-time)"; exit 1; }
FSSIZE=$(df -B1 --output=size "$PUB2" | tail -1 | tr -d ' ')
[ "$FSSIZE" -gt 1900000000 ] || { echo "FAIL: restored fs not grown to the 2Gi device (fs=$FSSIZE)"; exit 1; }
echo "  restore: point-in-time data OK, post-snapshot file absent, fs grown to $FSSIZE OK"

echo "## unwind the restored volume"
post /node/unpublish '{"volume":'"$V2"',"targetPath":"'"$PUB2"'"}' >/dev/null
post /node/unstage   '{"volume":'"$V2"',"stagingPath":"'"$STG2"'"}' >/dev/null
post /controller/unpublish '{"volume":'"$V2"',"nodeId":"'"$NODEID"'"}' >/dev/null

echo "## unpublish + unstage the source (logout, device must be gone)"
post /node/unpublish '{"volume":'"$V"',"targetPath":"'"$PUB"'"}' >/dev/null
post /node/unstage   '{"volume":'"$V"',"stagingPath":"'"$STG"'"}' >/dev/null

echo "## CHAP negative: a wrong-password login must be REJECTED (ACL still present)"
iscsiadm -m discovery -t sendtargets -p "$PORTAL" -I bard >/dev/null
iscsiadm -m node -T "$IQN" -p "$PORTAL" -I bard --op update -n node.session.auth.authmethod -v CHAP
iscsiadm -m node -T "$IQN" -p "$PORTAL" -I bard --op update -n node.session.auth.username -v "$CHAPUSER"
iscsiadm -m node -T "$IQN" -p "$PORTAL" -I bard --op update -n node.session.auth.password -v wrong-password-123
if iscsiadm -m node -T "$IQN" -p "$PORTAL" -I bard --login >/dev/null 2>&1; then
  echo "FAIL: login with a wrong CHAP password succeeded"; exit 1
fi
iscsiadm -m node -T "$IQN" -p "$PORTAL" -o delete 2>/dev/null || true
echo "  wrong-password login rejected OK"

echo "## controller/unpublish -> ACL must disappear (access revoked)"
post /controller/unpublish '{"volume":'"$V"',"nodeId":"'"$NODEID"'"}' >/dev/null
if targetcli "/iscsi/$IQN/tpg1/acls" ls "$INITIQN" >/dev/null 2>&1; then
  echo "FAIL: ACL $INITIQN still present after unpublish"; exit 1
fi
echo "  ACL removed OK"

echo "## delete snapshot + volumes -> targets + backstores + LVs all reaped (no orphan)"
post /snapshot/delete '{"snapshot":{"instance":"galileo","location":"'"$VG"'","name":"'"$SNAP"'"}}' >/dev/null
lvs "$VG/$SNAP" >/dev/null 2>&1 && { echo "FAIL: snapshot LV $SNAP still exists"; exit 1; }
post /volume/delete '{"volume":'"$V2"'}' >/dev/null
post /volume/delete '{"volume":'"$V"'}' >/dev/null
FAILED=0
for t in "$IQN" "$IQN2"; do targetcli /iscsi ls "$t" >/dev/null 2>&1 && { echo "FAIL: target $t still exists"; FAILED=1; }; done
for l in "$LV" "$LV2"; do
  targetcli /backstores/block ls "$l" >/dev/null 2>&1 && { echo "FAIL: backstore $l still exists"; FAILED=1; }
  lvs "$VG/$l" >/dev/null 2>&1 && { echo "FAIL: LV $l still exists"; FAILED=1; }
done
[ "$FAILED" = 0 ] || exit 1
LV=""; LV2=""; SNAP=""; IQN=""; IQN2="" # plugin reaped everything; nothing for the trap to clean
echo
echo "PASS: iSCSI attach + CHAP + thin snapshot/restore contract verified against $VG"
