#!/usr/bin/env bash
# End-to-end test of the iSCSI plugin's dm-multipath support against the real
# host LIO target, driving the plugin binary over its unix socket. Two portals
# on loopback (127.0.0.1 + 127.0.0.2 -- /8, no host setup) back ONE target; the
# node plane logs in through both and mounts the multipathd-assembled mapper:
#   create (thin LV + target with EXPLICIT per-address portals; the dual-stack
#           default portal ::0:3260 -- and legacy 0.0.0.0:3260 -- deleted) ->
#   controller/publish (ctx carries "portals" alongside "portal") ->
#   stage (login BOTH portals, mount the dm-uuid-mpath device) -> write ->
#   FAILOVER (iptables-DROP one portal's traffic under live I/O -- deleting the
#             LIO portal only removes the listener, the live session survives;
#             reads+writes keep working on the surviving path; unblock; both
#             paths recover) ->
#   ONLINE EXPAND (lvextend -> rescan -> multipathd resize -> fs grow) ->
#   unstage (map flushed BEFORE logout; both sessions + path devices gone) ->
#   delete (target + backstore + LV reaped).
#
# All mapper asserts go through /dev/disk/by-id/dm-uuid-mpath-<wwid> -- NEVER
# map names -- so the host's user_friendly_names setting is irrelevant (the
# plugin itself has the same discipline). hack/iscsi-plugin-test.sh remains the
# single-portal regression; this script does not replace it.
#
# Usage (run as root):
#   go build -o /tmp/bard-plugin-iscsi ./cmd/bard-plugin-iscsi
#   sudo bash hack/iscsi-multipath-test.sh [/path/to/bard-plugin-iscsi]
#
# Prereq: sudo bash hack/setup-iscsi-fixture.sh  (LIO + bard-vg + iscsid + multipathd)
set -euo pipefail

BIN=${1:-/tmp/bard-plugin-iscsi}
VG=${VG:-bard-vg}
POOL=${POOL:-bard-thin}
PORTAL1=${PORTAL1:-127.0.0.1:3260}
PORTAL2=${PORTAL2:-127.0.0.2:3260}
NODEID=${NODEID:-mpnode}
BASE=iqn.2025-01.io.bard
INITIQN="$BASE:init-$NODEID"
CHAPUSER=bard
CHAPPASS=bardchapsecret123
SOCK=${SOCK:-/tmp/iscsi-mp.sock}
STATE=/tmp/iscsi-mp-state
CFG=$(mktemp /tmp/iscsi-mp-cfg.XXXXXX.yaml)
CHAPDIR=$(mktemp -d /tmp/iscsi-mp-chap.XXXXXX)
STG=/tmp/iscsi-mp-stg; PUB=/tmp/iscsi-mp-pub

[ -x "$BIN" ] || { echo "plugin binary not found at $BIN (go build -o $BIN ./cmd/bard-plugin-iscsi)"; exit 1; }
vgs "$VG" >/dev/null 2>&1 || { echo "VG $VG not found -- run: sudo bash hack/setup-iscsi-fixture.sh"; exit 1; }
command -v targetcli >/dev/null || { echo "targetcli missing -- run: sudo bash hack/setup-iscsi-fixture.sh"; exit 1; }
command -v multipathd >/dev/null || { echo "multipathd missing -- run: sudo bash hack/setup-iscsi-fixture.sh"; exit 1; }
systemctl is-active --quiet multipathd 2>/dev/null || { echo "multipathd not running -- run: sudo bash hack/setup-iscsi-fixture.sh"; exit 1; }

lvs "$VG/$POOL" >/dev/null 2>&1 || lvcreate --type thin-pool -L 4G -n "$POOL" "$VG" >/dev/null

PLUGIN=""; LV=""; IQN=""; MPLINK=""; IPT=0
cleanup() {
  set +e
  [ "$IPT" = 1 ] && iptables -D INPUT -d 127.0.0.2 -p tcp --dport 3260 -m comment --comment bard-mp-test -j DROP 2>/dev/null
  [ -n "$PLUGIN" ] && kill "$PLUGIN" 2>/dev/null
  mountpoint -q "$PUB" 2>/dev/null && umount "$PUB" 2>/dev/null
  mountpoint -q "$STG" 2>/dev/null && umount "$STG" 2>/dev/null
  [ -n "$MPLINK" ] && [ -e "$MPLINK" ] && multipath -f "$(readlink -f "$MPLINK")" 2>/dev/null
  if [ -n "$IQN" ]; then
    for p in "$PORTAL1" "$PORTAL2"; do
      iscsiadm -m node -T "$IQN" -p "$p" --logout 2>/dev/null
      iscsiadm -m node -T "$IQN" -p "$p" -o delete 2>/dev/null
    done
    targetcli /iscsi delete "$IQN" 2>/dev/null
  fi
  if [ -n "$LV" ]; then
    targetcli /backstores/block delete "$LV" 2>/dev/null
    lvremove -f "$VG/$LV" 2>/dev/null
  fi
  rm -f "$SOCK" "$CFG"; rm -rf "$STG" "$PUB" "$STATE" "$CHAPDIR"
}
trap cleanup EXIT INT TERM

printf 'instances:\n  galileo:\n    vg: %s\n    portals: ["%s", "%s"]\n    thinPool: %s\n    chapAuth: true\n' \
  "$VG" "$PORTAL1" "$PORTAL2" "$POOL" > "$CFG"
printf '%s\n%s\n' "$CHAPUSER" "$CHAPPASS" > "$CHAPDIR/galileo"
chmod 600 "$CHAPDIR/galileo"
pkill -f "bard-plugin-iscsi.*$SOCK" 2>/dev/null || true; rm -f "$SOCK"
"$BIN" --socket="$SOCK" --config="$CFG" --node-id="$NODEID" --state-dir="$STATE" --chap-dir="$CHAPDIR" >/tmp/iscsi-mp-plugin.log 2>&1 &
PLUGIN=$!

post() { curl -fsS --unix-socket "$SOCK" -X POST "http://x$1" -d "$2"; }
jget() { python3 -c 'import sys,json;print(json.load(sys.stdin)["'"$1"'"])'; }
jctx() { python3 -c 'import sys,json;print(json.dumps(json.load(sys.stdin)["publishContext"]))'; }
for _ in $(seq 1 50); do [ -S "$SOCK" ] && post /info '{}' >/dev/null 2>&1 && break; sleep 0.1; done

echo "## /info (sanity -- multipath adds no capability bits)"
post /info '{}' | grep -q '"requiresControllerPublish":true' || { echo "FAIL: iSCSI must advertise attach"; exit 1; }

echo "## create volume -> thin LV + target with EXPLICIT portals (no default portal)"
LV=$(post /volume/create '{"name":"mptest","instance":"galileo","capacityBytes":1073741824,"fsType":"ext4"}' | jget name)
IQN="$BASE:tgt-$LV"
PLIST=$(targetcli "/iscsi/$IQN/tpg1/portals" ls 2>/dev/null)
echo "$PLIST" | grep -q "127.0.0.1:3260" || { echo "FAIL: explicit portal $PORTAL1 missing"; exit 1; }
echo "$PLIST" | grep -q "127.0.0.2:3260" || { echo "FAIL: explicit portal $PORTAL2 missing"; exit 1; }
echo "$PLIST" | grep -Eq '0\.0\.0\.0|::0' && { echo "FAIL: default catch-all portal still present"; exit 1; }
echo "  target=$IQN portals={$PORTAL1,$PORTAL2}, no default portal OK"

V='{"instance":"galileo","location":"'"$VG"'","name":"'"$LV"'"}'

echo "## controller/publish -> ctx must carry portals (both) + portal (first)"
PUBCTX=$(post /controller/publish '{"volume":'"$V"',"nodeId":"'"$NODEID"'"}' | jctx)
echo "  publishContext=$PUBCTX"
echo "$PUBCTX" | grep -Eq '"portals": ?"'"$PORTAL1"','"$PORTAL2"'"' || { echo "FAIL: ctx portals missing/wrong"; exit 1; }
echo "$PUBCTX" | grep -Eq '"portal": ?"'"$PORTAL1"'"' || { echo "FAIL: ctx portal must stay = first portal"; exit 1; }
echo "$PUBCTX" | grep -q "$CHAPPASS" && { echo "FAIL: CHAP password leaked into publishContext"; exit 1; }

echo "## stage -> login BOTH portals, mount the multipathd-assembled mapper"
post /node/stage   '{"volume":'"$V"',"stagingPath":"'"$STG"'","fsType":"ext4","publishContext":'"$PUBCTX"'}' >/dev/null
post /node/publish '{"volume":'"$V"',"stagingPath":"'"$STG"'","targetPath":"'"$PUB"'","fsType":"ext4"}' >/dev/null
NSESS=$(iscsiadm -m session | { grep -c "$IQN" || true; })
[ "$NSESS" = 2 ] || { echo "FAIL: expected 2 sessions, got $NSESS"; exit 1; }
SD1=$(readlink -f "/dev/disk/by-path/ip-$PORTAL1-iscsi-$IQN-lun-0")
WWID=$(tr -d ' ' < "/sys/class/block/$(basename "$SD1")/device/wwid")
MPLINK="/dev/disk/by-id/dm-uuid-mpath-3${WWID#naa.}"
[ -e "$MPLINK" ] || { echo "FAIL: dm-uuid mapper link $MPLINK missing"; exit 1; }
SRC=$(findmnt -no SOURCE "$STG")
[ "$(readlink -f "$SRC")" = "$(readlink -f "$MPLINK")" ] || { echo "FAIL: mount source $SRC != mapper $MPLINK"; exit 1; }
NPATHS=$(multipath -ll "$(readlink -f "$MPLINK")" | { grep -c "active ready running" || true; })
[ "$NPATHS" = 2 ] || { echo "FAIL: expected 2 active paths, got $NPATHS"; exit 1; }
echo "bard-mp-proof" > "$PUB/proof.txt"; sync
echo "  2 sessions, mount=mapper($MPLINK), 2 active paths, data written OK"

echo "## FAILOVER: block portal $PORTAL2 traffic under live I/O -- the mount must survive"
# Deleting the LIO portal only removes the LISTENER; the established session
# stays healthy (verified live), so a real path failure needs the traffic cut:
# DROP everything to 127.0.0.2:3260. Reversible; the trap also removes it.
iptables -I INPUT -d 127.0.0.2 -p tcp --dport 3260 -m comment --comment bard-mp-test -j DROP
IPT=1
FAILED_SEEN=0
for _ in $(seq 1 45); do
  # keep direct I/O flowing so the blocked path actually sees (and fails) requests
  dd if=/dev/zero of="$PUB/burn" bs=64k count=16 oflag=direct conv=fsync 2>/dev/null || true
  multipath -ll "$(readlink -f "$MPLINK")" | grep -Eq "failed|faulty" && { FAILED_SEEN=1; break; }
  sleep 2
done
[ "$FAILED_SEEN" = 1 ] || { echo "FAIL: no failed path reported within 90s"; exit 1; }
GOT=$(cat "$PUB/proof.txt")
[ "$GOT" = "bard-mp-proof" ] || { echo "FAIL: read through surviving path"; exit 1; }
echo "bard-mp-failover" > "$PUB/during-failover.txt"; sync
echo "  I/O survived on one path (read+write), failed path reported OK"

echo "## unblock $PORTAL2 -> both paths must recover"
iptables -D INPUT -d 127.0.0.2 -p tcp --dport 3260 -m comment --comment bard-mp-test -j DROP
IPT=0
echo "  (traffic unblocked)"
RECOVERED=0
for _ in $(seq 1 60); do
  N=$(multipath -ll "$(readlink -f "$MPLINK")" 2>/dev/null | { grep -c "active ready running" || true; })
  [ "$N" = 2 ] && { RECOVERED=1; break; }; sleep 2
done
[ "$RECOVERED" = 1 ] || { echo "FAIL: 2nd path did not recover"; exit 1; }
[ "$(cat "$PUB/during-failover.txt")" = "bard-mp-failover" ] || { echo "FAIL: failover-era data lost"; exit 1; }
echo "  both paths active again, data intact OK"

echo "## online expand 1Gi -> 2Gi under the live multipath mount"
post /volume/expand '{"volume":'"$V"',"newSizeBytes":2147483648}' | grep -q '"nodeExpansionRequired":true' \
  || { echo "FAIL: expand must require node expansion"; exit 1; }
post /node/expand '{"volume":'"$V"',"volumePath":"'"$PUB"'"}' >/dev/null
FSSIZE=$(df -B1 --output=size "$PUB" | tail -1 | tr -d ' ')
[ "$FSSIZE" -gt 1900000000 ] || { echo "FAIL: fs not grown on the mapper (fs=$FSSIZE)"; exit 1; }
[ "$(cat "$PUB/proof.txt")" = "bard-mp-proof" ] || { echo "FAIL: data lost across expand"; exit 1; }
echo "  fs grown online to $FSSIZE on the mapper, data intact OK"

echo "## unstage -> map flushed, BOTH sessions + path devices gone"
post /node/unpublish '{"volume":'"$V"',"targetPath":"'"$PUB"'"}' >/dev/null
post /node/unstage   '{"volume":'"$V"',"stagingPath":"'"$STG"'"}' >/dev/null
[ -e "$MPLINK" ] && { echo "FAIL: mapper link still present after unstage"; exit 1; }
iscsiadm -m session 2>/dev/null | grep -q "$IQN" && { echo "FAIL: sessions leaked past unstage"; exit 1; }
for p in "$PORTAL1" "$PORTAL2"; do
  [ -e "/dev/disk/by-path/ip-$p-iscsi-$IQN-lun-0" ] && { echo "FAIL: path device for $p leaked"; exit 1; }
done
echo "  mapper flushed, 0 sessions, path devices gone OK"

echo "## unpublish + delete -> target/backstore/LV reaped"
post /controller/unpublish '{"volume":'"$V"',"nodeId":"'"$NODEID"'"}' >/dev/null
post /volume/delete '{"volume":'"$V"'}' >/dev/null
targetcli /iscsi ls "$IQN" >/dev/null 2>&1 && { echo "FAIL: target $IQN still exists"; exit 1; }
targetcli /backstores/block ls "$LV" >/dev/null 2>&1 && { echo "FAIL: backstore $LV still exists"; exit 1; }
lvs "$VG/$LV" >/dev/null 2>&1 && { echo "FAIL: LV $LV still exists"; exit 1; }
LV=""; IQN=""; MPLINK=""  # plugin reaped everything; nothing for the trap to clean
echo
echo "PASS: iSCSI dm-multipath contract verified against $VG ($PORTAL1 + $PORTAL2)"
