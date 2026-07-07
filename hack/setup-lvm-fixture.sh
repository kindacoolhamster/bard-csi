#!/usr/bin/env bash
# Provide a host LVM volume group (bard-vg) for the LVM plugin to carve volumes
# from, on a dev host whose disks are otherwise full (root VG + Ceph OSDs leave
# zero free extents). Idempotent; safe to re-run.
#
# Why an RBD-backed VG: every existing VG on this host is 100% allocated
# (vg0 = root+swap, three ceph-* VGs = one OSD each), so `lvcreate` has nowhere
# to go. We carve a fresh block device for it. A loopback file would work too;
# we back it with a Ceph RBD image instead -- real block device, real capacity,
# and it keeps the root filesystem clean. The LVM plugin neither knows nor cares
# what backs the PV; it just lvcreates into bard-vg.
#
# NOTE (dev fixture only): this VG lives on the *host*. In the kind cluster every
# "node" is a container sharing this host, so bard-vg is visible to all of them
# -- fine for proving the plugin's create/format/mount/delete path, but it does
# NOT model true per-node LVM locality. A real cluster has a dedicated VG per
# node. Do not bake any of this into the plugin.
#
# Not persistent across reboot: `rbd map` must be re-run (this script re-maps an
# existing image idempotently, so just re-run it after a reboot).
#
# Run as root:   sudo bash hack/setup-lvm-fixture.sh
# Tear down:     sudo bash hack/setup-lvm-fixture.sh delete
set -euo pipefail

POOL="${BARD_LVM_POOL:-k8s-csi-test}"
IMAGE="${BARD_LVM_IMAGE:-bard-lvm}"
SIZE="${BARD_LVM_SIZE:-20G}"
VG="${BARD_LVM_VG:-bard-vg}"
SPEC="$POOL/$IMAGE"

mapped_dev() { rbd showmapped 2>/dev/null | awk -v i="$IMAGE" '$3==i || $4==i {print $NF; exit}'; }

if [[ "${1:-}" == "delete" ]]; then
  echo ">> tearing down $VG / $SPEC"
  vgremove -f "$VG" 2>/dev/null || true
  dev="$(mapped_dev)"
  [[ -n "$dev" ]] && { pvremove -ff -y "$dev" 2>/dev/null || true; rbd unmap "$dev"; }
  rbd rm "$SPEC" 2>/dev/null || echo "   (image $SPEC already gone)"
  echo ">> done"
  exit 0
fi

# 1. RBD image (create if absent)
if rbd info "$SPEC" >/dev/null 2>&1; then
  echo ">> image $SPEC exists"
else
  echo ">> creating image $SPEC ($SIZE)"
  rbd create "$SPEC" --size "$SIZE"
fi

# 2. map it (krbd -- reliable on the bare host, unlike rbd-nbd inside kind)
dev="$(mapped_dev)"
if [[ -n "$dev" ]]; then
  echo ">> $SPEC already mapped at $dev"
else
  dev="$(rbd map "$SPEC")"
  echo ">> mapped $SPEC at $dev"
fi

# 3. PV + VG (create if absent)
pvs "$dev" >/dev/null 2>&1 || { echo ">> pvcreate $dev"; pvcreate "$dev"; }
if vgs "$VG" >/dev/null 2>&1; then
  echo ">> vg $VG exists"
else
  echo ">> vgcreate $VG $dev"
  vgcreate "$VG" "$dev"
fi

echo
vgs "$VG"
echo ">> ready: lvcreate into '$VG' (backed by $SPEC at $dev)"
