#!/usr/bin/env bash
# Stand up the host iSCSI target infrastructure the iSCSI plugin needs: the LIO
# kernel target (configfs + modules) and targetcli, plus the open-iscsi initiator
# for the node-plane login. The plugin creates the actual per-volume targets/LUNs
# itself -- this just provisions the substrate, and the bard-vg the LUNs are
# carved from (reusing the LVM fixture). Idempotent; safe to re-run.
#
# WHY a host fixture (not in-cluster): LIO is kernel/configfs-backed, so the
# control plane must run where it can reach the target config -- here, galileo.
# hack/iscsi-plugin-test.sh then drives the plugin binary over its socket against
# this target to prove the full contract incl. per-node ACL masking, exactly like
# the LVM plugin (see CLAUDE.md). The k3s VMs reach this target's portal over NAT.
#
# Run as root:   sudo bash hack/setup-iscsi-fixture.sh
# Tear down:     sudo bash hack/setup-iscsi-fixture.sh delete   (clears LIO config)
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"

if [[ "${1:-}" == "delete" ]]; then
  echo ">> clearing LIO config"
  command -v targetcli >/dev/null && targetcli clearconfig confirm=True 2>/dev/null || true
  echo ">> done (bard-vg left intact; use setup-lvm-fixture.sh delete to remove it)"
  exit 0
fi

# 1. targetcli (LIO admin) -- ships python3-rtslib-fb.
if ! command -v targetcli >/dev/null; then
  echo ">> installing targetcli-fb"
  apt-get update -qq && apt-get install -y --no-install-recommends targetcli-fb open-iscsi
fi

# 2. configfs + LIO kernel modules (target side), the iSCSI initiator module,
#    and dm_snapshot (lvcreate -s checks for the snapshot dm target and shells to
#    modprobe -- which an in-container lvm cannot do, so the HOST must have it
#    loaded; same class as the rbd/nbd/dm_crypt host-module gotchas).
mountpoint -q /sys/kernel/config || { echo ">> mounting configfs"; mount -t configfs none /sys/kernel/config; }
for m in target_core_mod iscsi_target_mod configfs scsi_transport_iscsi iscsi_tcp dm_snapshot; do
  modprobe "$m" 2>/dev/null || echo "   (modprobe $m: already builtin or unavailable)"
done

# 3. the open-iscsi initiator daemon (node-plane login needs iscsid running).
systemctl enable --now iscsid 2>/dev/null || service iscsid start 2>/dev/null || true

# 3b. dm-multipath (the multi-portal node plane): the HOST daemon assembles the
#     per-portal sd devices into one mapper device; the plugin only waits for the
#     /dev/disk/by-id/dm-uuid-mpath-<wwid> link, which exists under ANY map-naming
#     policy -- so an existing /etc/multipath.conf is left alone (only created if
#     absent). find_multipaths yes/on both claim a device once 2 paths share a wwid.
if ! command -v multipathd >/dev/null; then
  echo ">> installing multipath-tools"
  apt-get install -y --no-install-recommends multipath-tools
fi
if [[ ! -e /etc/multipath.conf ]]; then
  echo ">> writing minimal /etc/multipath.conf (find_multipaths yes)"
  printf 'defaults {\n    find_multipaths yes\n}\n' > /etc/multipath.conf
  systemctl restart multipathd 2>/dev/null || true
fi
systemctl enable --now multipathd 2>/dev/null || service multipathd start 2>/dev/null || true

# 4. the bard-vg the plugin carves LUN backstores from (reuse the LVM fixture).
if ! vgs bard-vg >/dev/null 2>&1; then
  echo ">> bard-vg missing -- running the LVM fixture to create it"
  bash "$HERE/setup-lvm-fixture.sh"
fi

echo
echo ">> LIO ready:"
targetcli ls / 2>/dev/null | head -8 || true
vgs bard-vg
echo ">> the plugin will create per-volume targets under iqn.2025-01.io.bard:tgt-*"
