#!/usr/bin/env bash
# Runs bard-plugin-conformance against the Python localpath plugin -- proof the
# conformance bar is implementation-agnostic (the plugin under test shares no
# code with the tool). Control-plane checks run as the invoking user; run as
# root to add the node plane (bind mounts).
#
#   bash hack/conformance-localpath-test.sh          # control plane
#   sudo bash hack/conformance-localpath-test.sh     # + node plane
set -uo pipefail

PLUGIN=${PLUGIN:-plugins/localpath/bard-plugin-localpath}
TOOL=${TOOL:-/tmp/bard-plugin-conformance}
SOCK=$(mktemp -u /tmp/lp-conf.XXXXXX.sock)
BASE=$(mktemp -d /tmp/lp-conf-data.XXXXXX)
CFG=$(mktemp /tmp/lp-conf-cfg.XXXXXX.json)
LOG=$(mktemp /tmp/lp-conf-plugin.XXXXXX.log)  # unique: a fixed name breaks under sudo (fs.protected_regular)

[ -f "$PLUGIN" ] || { echo "plugin not found at $PLUGIN (run from the repo root)"; exit 1; }
# Build the tool when go is on PATH (it isn't under sudo); else use a prebuilt one.
if command -v go >/dev/null 2>&1; then
  go build -o "$TOOL" ./cmd/bard-plugin-conformance || exit 1
elif [ ! -x "$TOOL" ]; then
  echo "go not found and $TOOL missing; run once without sudo (or: go build -o $TOOL ./cmd/bard-plugin-conformance)"
  exit 1
fi

PID=""
cleanup() {
  [ -n "$PID" ] && kill "$PID" 2>/dev/null
  rm -rf "$BASE"; rm -f "$SOCK" "$CFG"
}
trap cleanup EXIT INT TERM

printf '{"instances":{"conftest":{"basePath":"%s"}}}\n' "$BASE" > "$CFG"
python3 "$PLUGIN" --socket="$SOCK" --config="$CFG" >"$LOG" 2>&1 &
PID=$!

NODE=""
[ "$(id -u)" = 0 ] && NODE="-node"
"$TOOL" -instance conftest $NODE "$SOCK"
rc=$?
[ $rc -eq 0 ] && echo "OK: localpath plugin is conformant" || echo "FAILED (rc=$rc); plugin log: $LOG"
exit $rc
