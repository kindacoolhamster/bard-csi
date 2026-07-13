#!/usr/bin/env bash
# End-to-end test of the LVM plugin against the real host VG, driving the plugin
# binary over its unix socket (the LVM plugin's storage is host-local and the
# nested kind nodes cannot see the host VG, so it is proven this way, not in kind).
#
# This wrapper exists to fix the orphaned-process footgun: out of cluster there is
# no controller pod to reap the plugin, so it starts the plugin under a trap that
# ALWAYS kills it and removes its socket on exit (success, failure, or Ctrl-C) and
# cleans up the test LVs it created.
#
# Usage (run as root for lvm/mount):
#   go build -o /tmp/bard-plugin-lvm ./cmd/bard-plugin-lvm
#   sudo bash hack/lvm-plugin-test.sh [/path/to/bard-plugin-lvm]
#
# Prereqs: the VG must exist (sudo bash hack/setup-lvm-fixture.sh -> bard-vg).
set -euo pipefail

BIN=${1:-/tmp/bard-plugin-lvm}
VG=${VG:-bard-vg}
POOL=${POOL:-bard-thin}
SOCK=${SOCK:-/tmp/lvm.sock}
CFG=$(mktemp /tmp/lvm-test-cfg.XXXXXX.yaml)
STG=/tmp/lvm-test-stg; PUB=/tmp/lvm-test-pub
STG2=/tmp/lvm-test-stg2; PUB2=/tmp/lvm-test-pub2

[ -x "$BIN" ] || { echo "plugin binary not found at $BIN (build it first: go build -o $BIN ./cmd/bard-plugin-lvm)"; exit 1; }
sudo vgs "$VG" >/dev/null 2>&1 || { echo "VG $VG not found -- run: sudo bash hack/setup-lvm-fixture.sh"; exit 1; }

PLUGIN=""
declare -a CREATED=()
cleanup() {
  set +e
  [ -n "$PLUGIN" ] && kill "$PLUGIN" 2>/dev/null
  for d in "$PUB2" "$PUB" "$STG2" "$STG"; do mountpoint -q "$d" 2>/dev/null && umount "$d" 2>/dev/null; done
  for lv in "${CREATED[@]}"; do lvremove -f "$VG/$lv" >/dev/null 2>&1; done
  rm -f "$SOCK" "$CFG"
  rm -rf "$STG" "$PUB" "$STG2" "$PUB2"
}
trap cleanup EXIT INT TERM

# Ensure a thin pool exists (idempotent).
sudo lvs "$VG/$POOL" >/dev/null 2>&1 || sudo lvcreate --type thin-pool -L 4G -n "$POOL" "$VG" >/dev/null

printf 'instances:\n  galileo:\n    vg: %s\n' "$VG" > "$CFG"
pkill -f "bard-plugin-lvm.*$SOCK" 2>/dev/null || true; rm -f "$SOCK"
"$BIN" --socket="$SOCK" --config="$CFG" >/tmp/lvm-plugin.log 2>&1 &
PLUGIN=$!

post() { curl -fsS --unix-socket "$SOCK" -X POST "http://x$1" -d "$2"; }
jget() { python3 -c 'import sys,json;print(json.load(sys.stdin)["'"$1"'"])'; }
# Wait for the plugin to bind its socket (with a real sleep: an unslept loop
# burns its iterations in microseconds and races plugin startup).
for _ in $(seq 1 50); do [ -S "$SOCK" ] && post /info '{}' >/dev/null 2>&1 && break; sleep 0.1; done

echo "## /info"; post /info '{}'; echo

echo "## thin create + verify it is a thin LV"
LV=$(post /volume/create '{"name":"lvmtest","instance":"galileo","capacityBytes":1073741824,"fsType":"ext4","parameters":{"thinPool":"'"$POOL"'"}}' | jget name)
CREATED+=("$LV")
ATTR=$(sudo lvs --noheadings -o lv_attr "$VG/$LV" | tr -d ' ')
[ "${ATTR:0:1}" = "V" ] || { echo "FAIL: $LV is not a thin volume (attr=$ATTR)"; exit 1; }
echo "  $LV attr=$ATTR (V = thin) OK"

echo "## stage + publish + write data"
V='{"instance":"galileo","location":"'"$VG"'","name":"'"$LV"'"}'
post /node/stage   '{"volume":'"$V"',"stagingPath":"'"$STG"'","fsType":"ext4"}' >/dev/null
post /node/publish '{"volume":'"$V"',"stagingPath":"'"$STG"'","targetPath":"'"$PUB"'","fsType":"ext4"}' >/dev/null
echo "bard-lvm-thin-proof" > "$PUB/proof.txt"; sync
post /node/unpublish '{"volume":'"$V"',"targetPath":"'"$PUB"'"}' >/dev/null
post /node/unstage   '{"volume":'"$V"',"stagingPath":"'"$STG"'"}' >/dev/null

echo "## snapshot"
SNAP=$(post /snapshot/create '{"name":"lvmsnap","sourceVolume":'"$V"'}' | jget name)
CREATED+=("$SNAP")
echo "  snapshot $SNAP attr=$(sudo lvs --noheadings -o lv_attr "$VG/$SNAP" | tr -d ' ')"

echo "## restore into a new thin volume + read the data back"
LV2=$(post /volume/create '{"name":"lvmrestore","instance":"galileo","capacityBytes":1073741824,"fsType":"ext4","parameters":{"thinPool":"'"$POOL"'"},"sourceSnapshot":{"instance":"galileo","location":"'"$VG"'","name":"'"$SNAP"'"}}' | jget name)
CREATED+=("$LV2")
V2='{"instance":"galileo","location":"'"$VG"'","name":"'"$LV2"'"}'
post /node/stage   '{"volume":'"$V2"',"stagingPath":"'"$STG2"'","fsType":"ext4"}' >/dev/null
post /node/publish '{"volume":'"$V2"',"stagingPath":"'"$STG2"'","targetPath":"'"$PUB2"'","fsType":"ext4"}' >/dev/null
GOT=$(cat "$PUB2/proof.txt" 2>/dev/null || true)
post /node/unpublish '{"volume":'"$V2"',"targetPath":"'"$PUB2"'"}' >/dev/null
post /node/unstage   '{"volume":'"$V2"',"stagingPath":"'"$STG2"'"}' >/dev/null
echo "  restored data: '$GOT'"
[ "$GOT" = "bard-lvm-thin-proof" ] || { echo "FAIL: restored data mismatch"; exit 1; }

echo "## delete the SOURCE volume first -- its snapshot must survive AND stay listed"
post /volume/delete '{"volume":'"$V"'}' >/dev/null
sudo lvs "$VG/$SNAP" >/dev/null 2>&1 || { echo "FAIL: snapshot LV $SNAP must outlive its source"; exit 1; }
# lvm clears the snapshot's origin here; the create-time bardsrc. tag is what
# keeps it listed with its source (core drops sourceless snapshots).
post /snapshot/list '{}' | grep -q '"name":"'"$SNAP"'"' || { echo "FAIL: snapshot must stay listed after its source is deleted"; exit 1; }
post /snapshot/list '{}' | grep -q '"sourceVolume":{[^}]*"name":"'"$LV"'"' || { echo "FAIL: snapshot must keep its recorded source after the source is deleted"; exit 1; }
echo "  source deleted, snapshot still listed with source $LV OK"

echo "## teardown (plugin deletes its own LVs; trap is the safety net)"
post /volume/delete   '{"volume":'"$V2"'}' >/dev/null
post /snapshot/delete '{"snapshot":{"instance":"galileo","location":"'"$VG"'","name":"'"$SNAP"'"}}' >/dev/null
CREATED=() # plugin removed them; nothing for the trap to reap

echo "PASS: thin provisioning + CoW snapshot + restore round-trip verified against $VG"
