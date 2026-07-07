#!/usr/bin/env bash
# End-to-end test of the Python localpath plugin, driving it over its unix socket
# exactly like Bard's core would -- proof the HTTP+JSON contract works the same in
# a non-Go plugin. Run as root (bind mounts). Like the LVM test, the plugin is
# started under a trap that always kills it and removes its socket + data on exit.
#
#   sudo bash hack/localpath-plugin-test.sh
set -uo pipefail

PLUGIN=${PLUGIN:-plugins/localpath/bard-plugin-localpath}
SOCK=/tmp/localpath.sock
BASE=/tmp/bard-localpath-data
CFG=$(mktemp /tmp/localpath-cfg.XXXXXX.json)
STG=/tmp/lp-stg; PUB=/tmp/lp-pub

[ -f "$PLUGIN" ] || { echo "plugin not found at $PLUGIN (run from the repo root)"; exit 1; }

PID=""
cleanup() {
  [ -n "$PID" ] && kill "$PID" 2>/dev/null
  for d in "$PUB" "$STG"; do mountpoint -q "$d" 2>/dev/null && umount "$d" 2>/dev/null; done
  rm -rf "$BASE" "$STG" "$PUB"; rm -f "$SOCK" "$CFG"
}
trap cleanup EXIT INT TERM

mkdir -p "$BASE"
printf '{"instances":{"galileo":{"basePath":"%s"}}}\n' "$BASE" > "$CFG"
python3 "$PLUGIN" --socket="$SOCK" --config="$CFG" >/tmp/localpath-plugin.log 2>&1 &
PID=$!

post() { curl -fsS --unix-socket "$SOCK" -X POST "http://x$1" -d "$2"; }
jget() { python3 -c 'import sys,json;print(json.load(sys.stdin)["'"$1"'"])'; }
for _ in $(seq 1 100); do post /info '{}' >/dev/null 2>&1 && break; sleep 0.1; done

echo "## /info"; post /info '{}'; echo

echo "## create"
R=$(post /volume/create '{"name":"pyvol","instance":"galileo","capacityBytes":1073741824}')
echo "$R"; LV=$(echo "$R" | jget name); CTX_PATH=$(echo "$R" | python3 -c 'import sys,json;print(json.load(sys.stdin)["context"]["path"])')
[ -d "$CTX_PATH" ] || { echo "FAIL: volume dir $CTX_PATH not created"; exit 1; }

echo "## stage + publish + write through the bind mounts"
V='{"instance":"galileo","location":"'"$BASE"'","name":"'"$LV"'"}'
CTX='{"path":"'"$CTX_PATH"'"}'
post /node/stage   '{"volume":'"$V"',"stagingPath":"'"$STG"'","context":'"$CTX"'}' >/dev/null
post /node/publish '{"volume":'"$V"',"stagingPath":"'"$STG"'","targetPath":"'"$PUB"'"}' >/dev/null
echo "bard-python-plugin-works" > "$PUB/proof.txt"; sync

echo "## the data written via the published bind mount lands in the backing dir:"
echo -n "  backing store: "; cat "$CTX_PATH/proof.txt" 2>&1
[ "$(cat "$CTX_PATH/proof.txt" 2>/dev/null)" = "bard-python-plugin-works" ] || { echo "FAIL: data not in backing dir"; exit 1; }

echo "## teardown"
post /node/unpublish '{"volume":'"$V"',"targetPath":"'"$PUB"'"}' >/dev/null
post /node/unstage   '{"volume":'"$V"',"stagingPath":"'"$STG"'"}' >/dev/null
post /volume/delete  '{"volume":'"$V"'}' >/dev/null
[ -d "$CTX_PATH" ] && { echo "FAIL: volume dir survived delete"; exit 1; }

echo "## negative: an unsupported op returns a structured error (HTTP 400)"
echo -n "  "; curl -s -o /dev/null -w "%{http_code}" --unix-socket "$SOCK" -X POST http://x/volume/expand -d '{"volume":'"$V"',"newSizeBytes":2}'; echo " (expect 400)"

echo "PASS: Python plugin spoke the full Bard contract over the unix socket"
