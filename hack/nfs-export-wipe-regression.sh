#!/usr/bin/env bash
# Live regression for the NFS export wipe: a controller op whose request is
# cancelled mid-flight (provisioner deadline, client disconnect, plugin shutdown)
# used to skip the export unmount and then RemoveAll the temp mountpoint --
# recursing through the still-attached export and deleting every volume on it.
# The unit tests can only model the mount; this drives the real plugin binary
# against a real NFS export over its unix socket, cancels a slow CreateSnapshot by
# disconnecting the client, and asserts an unrelated volume survives. It also
# runs a plain create/delete round trip to prove real-mount cleanup still works.
#
# Point it ONLY at a dedicated, EMPTY throwaway export: a vulnerable binary will
# destroy it. Run as root (mount), from the repo root, on the NFS server host
# (EXPORT_PATH is the export's local directory, used to seed and inspect it):
#
#   sudo bash hack/nfs-export-wipe-regression.sh <plugin-binary> <server> <export> <export-path>
#   e.g. ... /root/bard-plugin-nfs 127.0.0.1 /srv/bard-nfs-test /srv/bard-nfs-test
#
# SIZE (default 2G) sizes the snapshot source so tar is still running when the
# client disconnects after CANCEL_AFTER seconds (default 3).
set -uo pipefail

BIN=${1:?plugin binary}; SERVER=${2:?nfs server}; EXPORT=${3:?export path on the server}; LOCAL=${4:?export dir on this host}
SIZE=${SIZE:-2G}; CANCEL_AFTER=${CANCEL_AFTER:-3}
[ "$(id -u)" = 0 ] || { echo "run as root (mount)"; exit 1; }
[ -x "$BIN" ] || { echo "plugin binary $BIN not executable"; exit 1; }
[ -d "$LOCAL" ] && [ -z "$(ls -A "$LOCAL")" ] || { echo "refusing: $LOCAL must be an existing, EMPTY throwaway export"; exit 1; }

WORK=$(mktemp -d /var/tmp/bard-nfs-wipe.XXXXXX)
SOCK=$WORK/nfs.sock; CFG=$WORK/config.yaml; LOG=$WORK/plugin.log
mkdir -p "$WORK/tmp"

PID=""
cleanup() {
  [ -n "$PID" ] && kill "$PID" 2>/dev/null && wait "$PID" 2>/dev/null
  # A leaked export mount under WORK must be detached before WORK is removed --
  # otherwise this cleanup would be the very bug it tests for.
  findmnt -rn -o TARGET | grep "^$WORK/" | sort -r | while read -r m; do umount -l "$m"; done
  rm -rf --one-file-system "$WORK"
  rm -rf --one-file-system "${LOCAL:?}"/bard-wipe-victim "${LOCAL:?}"/bard-wipe-src "${LOCAL:?}"/.snapshots
}
trap cleanup EXIT INT TERM

echo "## seed export: unrelated volume + ${SIZE} snapshot source"
mkdir -p "$LOCAL/bard-wipe-victim" "$LOCAL/bard-wipe-src"
echo "bard-victim-intact" > "$LOCAL/bard-wipe-victim/proof.txt"
head -c "$SIZE" /dev/urandom > "$LOCAL/bard-wipe-src/big.bin"

printf 'instances:\n  t:\n    server: "%s"\n    export: "%s"\n' "$SERVER" "$EXPORT" > "$CFG"
TMPDIR=$WORK/tmp "$BIN" --socket "$SOCK" --config "$CFG" >"$LOG" 2>&1 &
PID=$!
post() { local path=$1 body=$2; shift 2; curl -sS --unix-socket "$SOCK" -X POST "http://x$path" -d "$body" "$@"; }
for _ in $(seq 1 100); do post /info '{}' >/dev/null 2>&1 && break; sleep 0.1; done

echo "## cancelled CreateSnapshot (client disconnects after ${CANCEL_AFTER}s)"
out=$(post /snapshot/create "{\"name\":\"wipe-test\",\"sourceVolume\":{\"instance\":\"t\",\"location\":\"$EXPORT\",\"name\":\"bard-wipe-src\"}}" \
  --max-time "$CANCEL_AFTER" 2>&1)
rc=$?
[ "$rc" = 28 ] || { echo "SETUP: expected the client to time out (curl 28), got $rc: $out -- raise SIZE?"; sed 's/^/  /' "$LOG"; exit 1; }
# Let the plugin unwind: tar is killed, then the deferred unmount + cleanup run.
for _ in $(seq 1 120); do pgrep -P "$PID" >/dev/null || break; sleep 0.5; done
sleep 2

fail=0
if [ "$(cat "$LOCAL/bard-wipe-victim/proof.txt" 2>/dev/null)" = "bard-victim-intact" ]; then
  echo "PASS: unrelated volume survived the cancelled operation"
else
  echo "FAIL: unrelated volume DESTROYED -- export wiped (ls: $(ls -A "$LOCAL" | tr '\n' ' '))"; fail=1
fi
leaked=$(findmnt -rn -o TARGET | grep -c "^$WORK/" || true)
if [ "$leaked" = 0 ]; then echo "PASS: export unmounted despite the cancelled request"
else echo "FAIL: $leaked export mount(s) left attached under $WORK"; fail=1; fi

echo "## create/delete round trip (real-mount cleanup)"
R=$(post /volume/create '{"name":"sanity","instance":"t","capacityBytes":1048576}')
NAME=$(echo "$R" | python3 -c 'import sys,json;print(json.load(sys.stdin)["name"])' 2>/dev/null)
if [ -n "$NAME" ] && [ -d "$LOCAL/$NAME" ]; then echo "PASS: created $NAME"; else echo "FAIL: create: $R"; fail=1; fi
post /volume/delete "{\"volume\":{\"instance\":\"t\",\"location\":\"$EXPORT\",\"name\":\"$NAME\"}}" >/dev/null
if [ -n "$NAME" ] && [ ! -e "$LOCAL/$NAME" ]; then echo "PASS: deleted $NAME"; else echo "FAIL: delete left $LOCAL/$NAME"; fail=1; fi
if [ -z "$(ls -A "$WORK/tmp")" ]; then echo "PASS: no temp mountpoints left behind"
else echo "FAIL: temp dirs left: $(ls -A "$WORK/tmp" | tr '\n' ' ')"; fail=1; fi

echo "## plugin log"; sed 's/^/  /' "$LOG"
[ "$fail" = 0 ] && echo "RESULT: PASS" || echo "RESULT: FAIL"
exit "$fail"
