#!/usr/bin/env bash
# Drive the HARDENED (Wolfi) LVM plugin *container* against the real host VG over
# its socket -- thick path (the hardened image lacks thin-provisioning-tools, by
# design). Proves the Chainguard-Wolfi lvm2/mkfs/mount toolset works on real
# storage, and exercises a write -> umount -> remount -> read round-trip.
#
# Prereqs:
#   - VG exists:    sudo bash hack/setup-lvm-fixture.sh   (-> bard-vg)
#   - image built:  podman build -t localhost/bard-plugin-lvm:hardened \
#                     -f Dockerfile.plugin-lvm.hardened .
#   - image in ROOT's podman (this needs root for /dev + lvm); bridge the rootless
#     build into root storage:
#       podman save -o /tmp/h.tar localhost/bard-plugin-lvm:hardened && \
#         sudo podman load -i /tmp/h.tar
# Run:  sudo bash hack/lvm-hardened-container-test.sh
set -uo pipefail
IMG=localhost/bard-plugin-lvm:hardened
VG=bard-vg
WORK=$(mktemp -d /tmp/lvm-hard.XXXXXX); mkdir -p "$WORK/sock"
printf 'instances:\n  galileo:\n    vg: %s\n' "$VG" > "$WORK/cfg.yaml"
CID=""
declare -a CREATED=()
cleanup() {
  set +e
  [ -n "$CID" ] && podman stop -t1 "$CID" >/dev/null 2>&1
  for lv in "${CREATED[@]}"; do sudo lvremove -f "$VG/$lv" >/dev/null 2>&1; done
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

sudo vgs "$VG" >/dev/null 2>&1 || { echo "VG $VG missing"; exit 1; }

CID=$(podman run -d --rm --privileged \
  -v /dev:/dev -v /run/lvm:/run/lvm -v /etc/lvm:/etc/lvm \
  -v "$WORK/sock:/sock" -v "$WORK/cfg.yaml:/cfg.yaml:ro" \
  "$IMG" --socket=/sock/lvm.sock --config=/cfg.yaml)
post() { curl -fsS --unix-socket "$WORK/sock/lvm.sock" -X POST "http://x$1" -d "$2"; }
jget() { python3 -c 'import sys,json;print(json.load(sys.stdin)["'"$1"'"])'; }
for _ in $(seq 1 100); do post /info '{}' >/dev/null 2>&1 && break; sleep 0.1; done

echo "## /info"; post /info '{}'; echo
echo "## socket perms inside the container (expect 0660)"
podman exec "$CID" sh -c 'ls -ln /sock/lvm.sock'

echo "## thick create (no thinPool) + verify it is NOT thin"
LV=$(post /volume/create '{"name":"lvmhard","instance":"galileo","capacityBytes":536870912,"fsType":"ext4"}' | jget name)
CREATED+=("$LV")
ATTR=$(sudo lvs --noheadings -o lv_attr "$VG/$LV" | tr -d ' ')
echo "  $LV attr=$ATTR"
[ "${ATTR:0:1}" = "V" ] && { echo "FAIL: expected thick, got thin"; exit 1; }
echo "  (attr[0]='${ATTR:0:1}', not 'V' => thick) OK"

V='{"instance":"galileo","location":"'"$VG"'","name":"'"$LV"'"}'
echo "## stage+publish, write data (inside container mount ns)"
post /node/stage   '{"volume":'"$V"',"stagingPath":"/stg","fsType":"ext4"}' >/dev/null
post /node/publish '{"volume":'"$V"',"stagingPath":"/stg","targetPath":"/pub","fsType":"ext4"}' >/dev/null
podman exec "$CID" sh -c 'echo bard-lvm-hardened-proof > /pub/proof.txt && sync'

echo "## umount (unpublish+unstage), then remount and read back -> persistence round-trip"
post /node/unpublish '{"volume":'"$V"',"targetPath":"/pub"}' >/dev/null
post /node/unstage   '{"volume":'"$V"',"stagingPath":"/stg"}' >/dev/null
post /node/stage     '{"volume":'"$V"',"stagingPath":"/stg2","fsType":"ext4"}' >/dev/null
post /node/publish   '{"volume":'"$V"',"stagingPath":"/stg2","targetPath":"/pub2","fsType":"ext4"}' >/dev/null
GOT=$(podman exec "$CID" cat /pub2/proof.txt 2>/dev/null)
echo "  read back: '$GOT'"
post /node/unpublish '{"volume":'"$V"',"targetPath":"/pub2"}' >/dev/null
post /node/unstage   '{"volume":'"$V"',"stagingPath":"/stg2"}' >/dev/null
[ "$GOT" = "bard-lvm-hardened-proof" ] || { echo "FAIL: data mismatch"; exit 1; }

echo "## teardown (plugin lvremoves its own LV)"
post /volume/delete '{"volume":'"$V"'}' >/dev/null
sudo lvs "$VG/$LV" >/dev/null 2>&1 && { echo "FAIL: LV survived delete"; exit 1; }
CREATED=()
echo "PASS: hardened (Wolfi) LVM container drove thick create + mkfs/mount + persistence round-trip on real $VG"
