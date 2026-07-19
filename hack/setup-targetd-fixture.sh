#!/usr/bin/env bash
# Stand up a real targetd (remote LIO/LVM management daemon, JSON-RPC over
# HTTP :18700) for the iSCSI plugin's `management: targetd` mode -- the fixture
# hack/targetd-plugin-test.sh drives the plugin against.
#
# targetd is NOT packaged for Debian/Ubuntu and NOT on PyPI: it installs from
# upstream git into a venv. On libblockdev 3.x hosts (Ubuntu >= 24.10 etc.)
# upstream 0.10.4 needs a small mechanical compat shim (applied idempotently
# below; RHEL-family distro packages carry equivalent patches downstream):
#   - plugin_specs_from_names() is gone     -> build PluginSpec objects (field
#     assignment, NOT ctor args -- new PyGObject ignores Boxed ctor args)
#   - GLib.GError / bd.LVMError as except-classes -> GLib.Error
#   - the bd.lvm.<fn> namespace             -> flat bd.lvm_<fn> names
#
# The daemon owns a DEDICATED VG (bard-targetd-vg, loop-backed -- NOT bard-vg:
# targetd assumes it owns its pools) and one server-side target IQN; exports are
# per-(initiator,volume) LUN mappings on that shared target.
#
# Run as your user (sudo used internally):  bash hack/setup-targetd-fixture.sh
# Tear down:                                bash hack/setup-targetd-fixture.sh delete
# API password lands in ~/.bard-targetd-pass (user admin).
set -euo pipefail

VENV="$HOME/bard-targetd-venv"
IMG="$HOME/bard-targetd-disk.img"
VG=bard-targetd-vg
PASSFILE="$HOME/.bard-targetd-pass"
TARGET_IQN=${TARGET_IQN:-iqn.2003-01.org.linux-iscsi.$(hostname -s):targetd}
PORTAL_ADDRS=${PORTAL_ADDRS:-'"127.0.0.1"'}   # comma-separated, each quoted

if [[ "${1:-}" == "delete" ]]; then
  echo ">> stopping targetd + removing its VG/loop/venv"
  sudo systemctl stop bard-targetd 2>/dev/null || true
  sudo systemctl reset-failed bard-targetd 2>/dev/null || true
  if sudo vgs "$VG" >/dev/null 2>&1; then
    sudo lvremove -fy "$VG" 2>/dev/null || true
    LOOP=$(sudo pvs --noheadings -o pv_name,vg_name 2>/dev/null | awk -v vg="$VG" '$2==vg{print $1}')
    sudo vgremove -f "$VG"
    [ -n "${LOOP:-}" ] && sudo losetup -d "$LOOP" 2>/dev/null || true
  fi
  rm -f "$IMG" "$PASSFILE"; rm -rf "$VENV"
  echo ">> done (/etc/target/targetd.yaml left in place)"
  exit 0
fi

# 1. deps: venv/pip, GI + libblockdev 3 (2.0-generation names as fallback),
#    rtslib via targetcli-fb, lvm2.
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends \
  python3-venv python3-pip python3-gi python3-setproctitle python3-yaml \
  targetcli-fb lvm2 >/dev/null
sudo apt-get install -y -qq gir1.2-blockdev-3.0 libblockdev-lvm3 >/dev/null 2>&1 \
  || sudo apt-get install -y -qq gir1.2-blockdev-2.0 libblockdev-lvm2 >/dev/null

# 2. targetd from upstream git, in a venv that can see the distro GI stack.
[ -d "$VENV" ] || python3 -m venv --system-site-packages "$VENV"
"$VENV/bin/python" -c 'import targetd' 2>/dev/null \
  || "$VENV/bin/pip" install --quiet "git+https://github.com/open-iscsi/targetd.git"

# 3. libblockdev-3 compat shim (idempotent: skips whatever upstream has fixed).
LVMPY=$(ls "$VENV"/lib/python*/site-packages/targetd/backends/lvm.py)
"$VENV/bin/python" - "$LVMPY" <<'PYEOF'
import sys
p = sys.argv[1]; s = open(p).read(); orig = s
old = "requested_plugins = bd.plugin_specs_from_names(REQUESTED_PLUGIN_NAMES)"
if old in s:
    s = s.replace(old, '''if hasattr(bd, "plugin_specs_from_names"):
    requested_plugins = bd.plugin_specs_from_names(REQUESTED_PLUGIN_NAMES)
else:  # libblockdev 3.x: build specs by FIELD assignment (Boxed ctor args are ignored)
    _n2p = {"lvm": bd.Plugin.LVM}
    requested_plugins = []
    for _n in REQUESTED_PLUGIN_NAMES:
        _spec = bd.PluginSpec(); _spec.name = _n2p[_n]; requested_plugins.append(_spec)''')
s = s.replace("except GLib.GError as err:", "except GLib.Error as err:")
s = s.replace("except bd.LVMError", "except GLib.Error")
s = s.replace("bd.lvm.", "bd.lvm_")
if s != orig:
    open(p, "w").write(s); print(">> compat shim applied to", p)
else:
    print(">> compat shim not needed (already applied or upstream fixed)")
PYEOF

# 4. the dedicated VG on a loop file (NOT reboot-persistent: re-losetup +
#    vgchange -ay after a host restart, like the other loop fixtures).
if ! sudo vgs "$VG" >/dev/null 2>&1; then
  [ -f "$IMG" ] || truncate -s 6G "$IMG"
  LOOP=$(sudo losetup --show -f "$IMG")
  sudo vgcreate "$VG" "$LOOP"
fi

# 5. config + password (generated once, kept across re-runs).
[ -f "$PASSFILE" ] || { openssl rand -hex 12 > "$PASSFILE"; chmod 600 "$PASSFILE"; }
sudo mkdir -p /etc/target
printf 'password: %s\nblock_pools: [%s]\ntarget_name: %s\nportal_addresses: [%s]\n' \
  "$(cat "$PASSFILE")" "$VG" "$TARGET_IQN" "$PORTAL_ADDRS" | sudo tee /etc/target/targetd.yaml >/dev/null
sudo chmod 600 /etc/target/targetd.yaml

# 6. run under systemd (never a bare `&`); restart to pick up config changes.
sudo systemctl stop bard-targetd 2>/dev/null || true
sudo systemctl reset-failed bard-targetd 2>/dev/null || true
sudo systemd-run --unit bard-targetd "$VENV/bin/targetd" >/dev/null
sleep 2
systemctl is-active --quiet bard-targetd || { echo "FAIL: targetd did not start"; sudo journalctl -u bard-targetd -n 10 --no-pager; exit 1; }

# 7. smoke: the API must answer pool_list with our VG.
OUT=$(curl -sS -u admin:"$(cat "$PASSFILE")" -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"pool_list","params":{}}' http://127.0.0.1:18700/targetrpc)
echo "$OUT" | grep -q "$VG" || { echo "FAIL: pool_list did not report $VG: $OUT"; exit 1; }
echo ">> targetd ready on :18700 (target $TARGET_IQN, pool $VG)"
echo ">> API: user admin, password in $PASSFILE"
