#!/usr/bin/env bash
# End-to-end test of the iSCSI plugin's `management: targetd` mode: a REMOTE
# LIO host administered over targetd's JSON-RPC API (:18700) instead of local
# targetcli -- the control plane that lets the controller run off the target
# node. Unlike the local per-volume-target model, targetd exposes every volume
# as a LUN under ONE FIXED target IQN, so this harness is what proves the
# shared-target node plane (Task 3.3: session refcounting, rescan-not-relogin,
# not-last-vs-last unstage) against a REAL iscsiadm/LIO substrate, not just the
# fake-runner unit tests.
#
# Flow: /info (targetd capability) -> config smoke: a chapAuth: true targetd
# instance must be rejected at load (see below -- NOT exercised over the
# socket started for the main flow, which uses a clean non-CHAP config) ->
# create vol1+vol2 (targetd RPC) -> controller/publish vol1 (assert an export
# in targetd's export_list, ctx carries the allocated lun) -> node/stage+
# publish vol1 (plain login, no CHAP) -> controller/publish + node/stage+
# publish vol2 on the SAME node (assert the iscsiadm session count stays 1 --
# rescan, not a second login; distinct LUN devices) -> write both ->
# snapshot/create on vol1 -> assert a CLEAN Unsupported error (targetd's
# vol_copy is a synchronous full copy, unsafe under provisioner retries) ->
# unstage vol1 (assert the session is STILL up -- vol2 needs it -- and vol1's
# own sd device is gone) -> unstage vol2 (assert the session is now gone) ->
# expand vol1 (targetd vol_resize, reflected in vol_list) -> unpublish+delete
# both -> targetd vol_list/export_list empty (no orphan on the remote host).
#
# NO CHAP: live-verified (targetd 0.10.4, upstream git main) that
# export_create hardcodes the shared target's TPG `authentication` attribute
# to "0" on EVERY export with no API to override it, so CHAP credentials set
# via initiator_set_auth are never actually enforced -- the login response
# still advertises AuthMethod=CHAP, but the kernel initiator aborts the
# connection immediately after seeing it, before the actual challenge (proven
# with a packet capture, both with a correct password and with none at all).
# inst() now rejects chapAuth: true on a targetd instance at config load
# rather than ship a StorageClass flag that silently protects nothing (see
# TestTargetdInstanceRejectsCHAPAuth); this harness's targetd instance carries
# no chapAuth, and a small preflight below drives a SEPARATE, throwaway plugin
# instance to confirm the rejection fires.
#
# Like the local harness, the plugin runs under a trap that ALWAYS kills it and
# reaps anything left in targetd + any live session, on success, failure, or
# Ctrl-C.
#
# Usage (run as root for iscsiadm/mount):
#   go build -o /tmp/bard-plugin-iscsi ./cmd/bard-plugin-iscsi
#   sudo bash hack/targetd-plugin-test.sh [/path/to/bard-plugin-iscsi]
#
# Prereq: bash hack/setup-targetd-fixture.sh (targetd + bard-targetd-vg; run as
# your own user, NOT sudo -- it sudos internally) and
# sudo bash hack/setup-iscsi-fixture.sh (iscsid; shares the host's LIO/configfs
# with targetd, so no separate LIO substrate is needed).
set -euo pipefail

BIN=${1:-/tmp/bard-plugin-iscsi}
TD_POOL=bard-targetd-vg
TD_IQN=iqn.2003-01.org.linux-iscsi.$(hostname -s):targetd
TD_ENDPOINT=http://127.0.0.1:18700/targetrpc
# setup-targetd-fixture.sh runs as the invoking USER (not sudo) and writes the
# password file under THAT user's $HOME; this script runs under sudo (needs
# root for iscsiadm/mount), where plain $HOME resolves to /root -- resolve the
# real invoking user's home instead, the same class of fix root-PATH-drop
# gotchas elsewhere in this repo need.
REAL_HOME=$(getent passwd "${SUDO_USER:-$USER}" | cut -d: -f6)
TD_PASSFILE=$REAL_HOME/.bard-targetd-pass
PORTAL=127.0.0.1:3260
NODEID=tdtestnode
BASE=iqn.2025-01.io.bard
INITIQN="$BASE:init-$NODEID"
SOCK=${SOCK:-/tmp/iscsi-td.sock}
STATE=/tmp/iscsi-td-test-state
CFG=$(mktemp /tmp/iscsi-td-test-cfg.XXXXXX.yaml)
TDDIR=$(mktemp -d /tmp/iscsi-td-test-tddir.XXXXXX)
STG=/tmp/iscsi-td-test-stg; PUB=/tmp/iscsi-td-test-pub
STG2=/tmp/iscsi-td-test-stg2; PUB2=/tmp/iscsi-td-test-pub2

[ -x "$BIN" ] || { echo "plugin binary not found at $BIN (go build -o $BIN ./cmd/bard-plugin-iscsi)"; exit 1; }
[ -f "$TD_PASSFILE" ] || { echo "targetd not set up -- run: bash hack/setup-targetd-fixture.sh"; exit 1; }
systemctl is-active --quiet bard-targetd || { echo "targetd not running -- run: bash hack/setup-targetd-fixture.sh"; exit 1; }
command -v iscsiadm >/dev/null || { echo "iscsiadm missing -- run: sudo bash hack/setup-iscsi-fixture.sh"; exit 1; }

TD_PASS=$(cat "$TD_PASSFILE")
tdrpc() { # method params_json
  curl -sS -K - -H 'Content-Type: application/json' \
    -d '{"jsonrpc":"2.0","id":1,"method":"'"$1"'","params":'"$2"'}' \
    "$TD_ENDPOINT" <<<"user = \"admin:$TD_PASS\""
}
tdresult() { python3 -c 'import sys,json;print(json.dumps(json.load(sys.stdin)["result"]))'; }
tdcount() { python3 -c 'import sys,json;print(len(json.load(sys.stdin)["result"]))'; }

PLUGIN=""; LV=""; LV2=""; IQN2=""
cleanup() {
  set +e
  [ -n "$PLUGIN" ] && kill "$PLUGIN" 2>/dev/null
  for d in "$PUB2" "$STG2" "$PUB" "$STG"; do mountpoint -q "$d" 2>/dev/null && umount "$d" 2>/dev/null; done
  # log out of the shared target entirely (any leftover session)
  iscsiadm -m node -T "$TD_IQN" -p "$PORTAL" --logout 2>/dev/null
  iscsiadm -m node -T "$TD_IQN" -p "$PORTAL" -o delete 2>/dev/null
  # sweep any export/volume this run left on the remote targetd host
  for l in "$LV" "$LV2"; do
    [ -n "$l" ] || continue
    EXPS=$(tdrpc export_list '{}' | python3 -c '
import sys,json
for e in json.load(sys.stdin)["result"]:
    if e["vol_name"] == "'"$l"'":
        print(e["initiator_wwn"])
' 2>/dev/null)
    for init in $EXPS; do
      tdrpc export_destroy '{"pool":"'"$TD_POOL"'","vol":"'"$l"'","initiator_wwn":"'"$init"'"}' >/dev/null
    done
    tdrpc vol_destroy '{"pool":"'"$TD_POOL"'","name":"'"$l"'"}' >/dev/null
  done
  rm -f "$SOCK" "$CFG"; rm -rf "$STG" "$PUB" "$STG2" "$PUB2" "$STATE" "$TDDIR"
}
trap cleanup EXIT INT TERM

# "remote" (no chapAuth -- see the NO CHAP note above) drives the whole flow;
# "remote-badchap" exists ONLY for the config-rejection preflight below and is
# never otherwise used (no volume is ever created against it).
cat > "$CFG" <<EOF
instances:
  remote:
    management: targetd
    targetdEndpoint: $TD_ENDPOINT
    targetdPool: $TD_POOL
    targetIqn: $TD_IQN
    portal: $PORTAL
  remote-badchap:
    management: targetd
    targetdEndpoint: $TD_ENDPOINT
    targetdPool: $TD_POOL
    targetIqn: $TD_IQN
    portal: $PORTAL
    chapAuth: true
EOF
printf 'admin\n%s\n' "$TD_PASS" > "$TDDIR/remote"
printf 'admin\n%s\n' "$TD_PASS" > "$TDDIR/remote-badchap"
chmod 600 "$TDDIR/remote" "$TDDIR/remote-badchap"

pkill -f "bard-plugin-iscsi.*$SOCK" 2>/dev/null || true; rm -f "$SOCK"
"$BIN" --socket="$SOCK" --config="$CFG" --node-id="$NODEID" --state-dir="$STATE" \
  --targetd-dir="$TDDIR" >/tmp/iscsi-td-plugin.log 2>&1 &
PLUGIN=$!

post() { curl -fsS --unix-socket "$SOCK" -X POST "http://x$1" -d "$2"; }
postraw() { curl -sS -o /dev/null -w "%{http_code}" --unix-socket "$SOCK" -X POST "http://x$1" -d "$2"; }
postbody() { curl -sS --unix-socket "$SOCK" -X POST "http://x$1" -d "$2"; }
jget() { python3 -c 'import sys,json;print(json.load(sys.stdin)["'"$1"'"])'; }
jctx() { python3 -c 'import sys,json;print(json.dumps(json.load(sys.stdin)["publishContext"]))'; }
for _ in $(seq 1 50); do [ -S "$SOCK" ] && post /info '{}' >/dev/null 2>&1 && break; sleep 0.1; done

echo "## /info (expect requiresControllerPublish=true AND snapshots=true -- the local instance's caps still advertise; targetd instances reject per-request)"
post /info '{}'; echo
post /info '{}' | grep -q '"requiresControllerPublish":true' || { echo "FAIL: iSCSI must advertise attach"; exit 1; }

echo "## config preflight: a targetd instance with chapAuth: true must be cleanly rejected (targetd cannot actually enforce it -- see the NO CHAP note above)"
CODE0=$(postraw /volume/create '{"name":"tdvolbadchap","instance":"remote-badchap","capacityBytes":1073741824}')
BODY0=$(postbody /volume/create '{"name":"tdvolbadchap","instance":"remote-badchap","capacityBytes":1073741824}')
[ "$CODE0" = 400 ] || { echo "FAIL: expected HTTP 400 (InvalidArgument) for chapAuth on a targetd instance, got $CODE0: $BODY0"; exit 1; }
echo "$BODY0" | grep -q 'chapAuth' || { echo "FAIL: rejection must explain chapAuth, got: $BODY0"; exit 1; }
echo "  chapAuth: true on a targetd instance cleanly rejected OK"

echo "## create vol1 + vol2 (targetd RPC, plain volumes -- no local LV/backstore/target)"
LV=$(post /volume/create '{"name":"tdvol1","instance":"remote","capacityBytes":1073741824}' | jget name)
LV2=$(post /volume/create '{"name":"tdvol2","instance":"remote","capacityBytes":1073741824}' | jget name)
tdrpc vol_list '{"pool":"'"$TD_POOL"'"}' | tdresult | grep -q "\"$LV\"" || { echo "FAIL: $LV not in targetd vol_list"; exit 1; }
tdrpc vol_list '{"pool":"'"$TD_POOL"'"}' | tdresult | grep -q "\"$LV2\"" || { echo "FAIL: $LV2 not in targetd vol_list"; exit 1; }
echo "  LV=$LV LV2=$LV2 both present in targetd vol_list OK"

V='{"instance":"remote","location":"'"$TD_POOL"'","name":"'"$LV"'"}'
V2='{"instance":"remote","location":"'"$TD_POOL"'","name":"'"$LV2"'"}'

echo "## controller/publish vol1 -> targetd export_create, ctx carries the allocated lun"
PUBCTX=$(post /controller/publish '{"volume":'"$V"',"nodeId":"'"$NODEID"'"}' | jctx)
echo "  publishContext=$PUBCTX"
LUN1=$(echo "$PUBCTX" | python3 -c 'import sys,json;print(json.load(sys.stdin)["lun"])')
EXPS1=$(tdrpc export_list '{}' | python3 -c '
import sys,json
for e in json.load(sys.stdin)["result"]:
    if e["vol_name"] == "'"$LV"'" and e["initiator_wwn"] == "'"$INITIQN"'":
        print(e["lun"])
')
[ "$EXPS1" = "$LUN1" ] || { echo "FAIL: targetd export_list lun ($EXPS1) != publishContext lun ($LUN1)"; exit 1; }
echo "  export present in targetd, lun=$LUN1 matches publishContext OK"

echo "## stage vol1 (iscsiadm login under per-node iface, no CHAP) + publish + write"
post /node/stage   '{"volume":'"$V"',"stagingPath":"'"$STG"'","fsType":"ext4","publishContext":'"$PUBCTX"'}' >/dev/null
post /node/publish '{"volume":'"$V"',"stagingPath":"'"$STG"'","targetPath":"'"$PUB"'","fsType":"ext4"}' >/dev/null
echo "bard-targetd-proof-1" > "$PUB/proof.txt"; sync
[ "$(cat "$PUB/proof.txt")" = "bard-targetd-proof-1" ] || { echo "FAIL: vol1 data round-trip"; exit 1; }
NSESS=$(iscsiadm -m session 2>/dev/null | grep -c "$TD_IQN" || true)
[ "$NSESS" = 1 ] || { echo "FAIL: expected exactly 1 session after staging vol1, got $NSESS"; exit 1; }
echo "  vol1 staged+mounted+written, 1 session up OK"

echo "## controller/publish + stage + publish vol2 on the SAME node -> SHARED target session (Task 3.3)"
PUBCTX2=$(post /controller/publish '{"volume":'"$V2"',"nodeId":"'"$NODEID"'"}' | jctx)
LUN2=$(echo "$PUBCTX2" | python3 -c 'import sys,json;print(json.load(sys.stdin)["lun"])')
[ "$LUN1" != "$LUN2" ] || { echo "FAIL: vol1 and vol2 must get DISTINCT luns, both got $LUN1"; exit 1; }
post /node/stage   '{"volume":'"$V2"',"stagingPath":"'"$STG2"'","fsType":"ext4","publishContext":'"$PUBCTX2"'}' >/dev/null
post /node/publish '{"volume":'"$V2"',"stagingPath":"'"$STG2"'","targetPath":"'"$PUB2"'","fsType":"ext4"}' >/dev/null
echo "bard-targetd-proof-2" > "$PUB2/proof.txt"; sync
[ "$(cat "$PUB2/proof.txt")" = "bard-targetd-proof-2" ] || { echo "FAIL: vol2 data round-trip"; exit 1; }
NSESS2=$(iscsiadm -m session 2>/dev/null | grep -c "$TD_IQN" || true)
[ "$NSESS2" = 1 ] || { echo "FAIL: staging vol2 must REUSE the shared session (rescan, not relogin), got $NSESS2 sessions"; exit 1; }
DEV1=$(findmnt -n -o SOURCE --mountpoint "$STG")
DEV2=$(findmnt -n -o SOURCE --mountpoint "$STG2")
[ "$DEV1" != "$DEV2" ] || { echo "FAIL: vol1 and vol2 must be on DISTINCT devices (lun $LUN1 vs $LUN2), both $DEV1"; exit 1; }
echo "  vol2 staged+mounted+written via the SAME session (lun=$LUN2, dev=$DEV2, still 1 session) OK"

echo "## snapshot/create on vol1 -> targetd instances must reject with a CLEAN Unsupported error"
CODE=$(postraw /snapshot/create '{"name":"tdsnap","sourceVolume":'"$V"'}')
BODY=$(postbody /snapshot/create '{"name":"tdsnap","sourceVolume":'"$V"'}')
[ "$CODE" = 500 ] || { echo "FAIL: expected HTTP 500 for an unsupported snapshot, got $CODE"; exit 1; }
echo "$BODY" | grep -q '"code":"Unsupported"' || { echo "FAIL: expected error code Unsupported, got: $BODY"; exit 1; }
echo "  snapshot/create cleanly rejected (Unsupported) OK"

echo "## unstage vol1 -> the SHARED session must stay up (vol2 still needs it); vol1's own device must be gone"
post /node/unpublish '{"volume":'"$V"',"targetPath":"'"$PUB"'"}' >/dev/null
post /node/unstage   '{"volume":'"$V"',"stagingPath":"'"$STG"'"}' >/dev/null
NSESS3=$(iscsiadm -m session 2>/dev/null | grep -c "$TD_IQN" || true)
[ "$NSESS3" = 1 ] || { echo "FAIL: session must stay up after unstaging vol1 (vol2 is still staged), got $NSESS3"; exit 1; }
[ -e "$DEV1" ] && { echo "FAIL: vol1's own device $DEV1 must be gone after its unstage"; exit 1; }
[ -e "$DEV2" ] || { echo "FAIL: vol2's device $DEV2 must still be present"; exit 1; }
echo "  session still up, vol1's own LUN detached, vol2 untouched OK"

echo "## unstage vol2 -> now the last volume on this target: full logout, session gone"
post /node/unpublish '{"volume":'"$V2"',"targetPath":"'"$PUB2"'"}' >/dev/null
post /node/unstage   '{"volume":'"$V2"',"stagingPath":"'"$STG2"'"}' >/dev/null
NSESS4=$(iscsiadm -m session 2>/dev/null | grep -c "$TD_IQN" || true)
[ "$NSESS4" = 0 ] || { echo "FAIL: session must be gone after unstaging the last volume, got $NSESS4"; exit 1; }
echo "  session gone OK"

echo "## expand vol1 (targetd vol_resize, reflected in vol_list)"
post /volume/expand '{"volume":'"$V"',"newSizeBytes":2147483648}' | grep -q '"nodeExpansionRequired":true' \
  || { echo "FAIL: expand must require node expansion"; exit 1; }
SIZE=$(tdrpc vol_list '{"pool":"'"$TD_POOL"'"}' | python3 -c '
import sys,json
for v in json.load(sys.stdin)["result"]:
    if v["name"] == "'"$LV"'":
        print(v["size"])
')
[ "$SIZE" = "2147483648" ] || { echo "FAIL: targetd vol_list size not resized (got $SIZE)"; exit 1; }
echo "  vol1 resized to 2Gi in targetd vol_list OK"

echo "## unpublish + delete both -> targetd vol_list/export_list back to empty (no orphan)"
post /controller/unpublish '{"volume":'"$V"',"nodeId":"'"$NODEID"'"}' >/dev/null
post /controller/unpublish '{"volume":'"$V2"',"nodeId":"'"$NODEID"'"}' >/dev/null
post /volume/delete '{"volume":'"$V"'}' >/dev/null
post /volume/delete '{"volume":'"$V2"'}' >/dev/null
tdrpc vol_list '{"pool":"'"$TD_POOL"'"}' | tdresult | grep -q "\"$LV\"" && { echo "FAIL: $LV still in targetd vol_list"; exit 1; }
tdrpc vol_list '{"pool":"'"$TD_POOL"'"}' | tdresult | grep -q "\"$LV2\"" && { echo "FAIL: $LV2 still in targetd vol_list"; exit 1; }
NEXP=$(tdrpc export_list '{}' | python3 -c '
import sys,json
print(sum(1 for e in json.load(sys.stdin)["result"] if e["vol_name"] in ("'"$LV"'","'"$LV2"'")))
')
[ "$NEXP" = 0 ] || { echo "FAIL: $NEXP leftover export(s) for our volumes"; exit 1; }
LV=""; LV2="" # plugin reaped everything; nothing for the trap to clean
echo
echo "PASS: iSCSI targetd (remote LIO management) contract + shared-target refcounting verified against $TD_POOL"
