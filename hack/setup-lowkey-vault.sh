#!/usr/bin/env bash
# Stand up a local lowkey-vault (free Azure Key Vault emulator) for proving the
# azure-kv KMS provider in-cluster on the k3s tier.
#
#   bash hack/setup-lowkey-vault.sh         # -> lowkey-vault on galileo:8453, vault "bard"
#   bash hack/setup-lowkey-vault.sh delete  # teardown
#
# The k3s VMs reach it at galileo 192.168.1.225:8453 (like the Ceph mon). Two
# lowkey-vault quirks are handled here:
#   - it routes vaults by host:PORT, so the "bard" vault is aliased to <ip>:<port>;
#     the alias MUST include the port or every request 404s "Unable to find active
#     vault". (Default vault host is bard.localhost; the alias adds the IP.)
#   - 8443 is the Ceph dashboard on this host, so lowkey-vault runs on 8453 (set both
#     the container server.port and the published port to 8453 so routing matches).
# Auth is a dummy bearer token -- lowkey-vault does not validate AAD -- so the
# provider uses authMethod: token; its self-signed cert is handled with
# insecureSkipVerify. See the azure-kv config in deploy/20-config.yaml.
set -euo pipefail
NAME=bard-lowkey
HOST_IP="${LOWKEY_HOST_IP:-192.168.1.225}"
PORT="${LOWKEY_PORT:-8453}"

if [ "${1:-}" = "delete" ]; then
  podman rm -f "$NAME" >/dev/null 2>&1 || true
  echo "torn down $NAME"
  exit 0
fi

podman rm -f "$NAME" >/dev/null 2>&1 || true
podman run -d --name "$NAME" -p "${PORT}:${PORT}" \
  -e LOWKEY_ARGS="--server.port=${PORT}" \
  -e LOWKEY_VAULT_NAMES="bard" \
  -e LOWKEY_VAULT_ALIASES="bard.localhost=${HOST_IP}:${PORT}" \
  docker.io/nagyesta/lowkey-vault:7.3.0 >/dev/null

echo -n "waiting for lowkey-vault on ${HOST_IP}:${PORT} "
for _ in $(seq 1 40); do
  code=$(curl -sk --max-time 3 -o /dev/null -w '%{http_code}' \
    "https://${HOST_IP}:${PORT}/secrets/_ready?api-version=7.4" -H 'Authorization: Bearer x' 2>/dev/null || true)
  # any HTTP reply (e.g. 404 for the missing secret) means the vault routes + serves.
  if [ -n "$code" ] && [ "$code" != "000" ]; then echo " up (HTTP $code)"; exit 0; fi
  echo -n .; sleep 2
done
echo " TIMEOUT"
podman logs "$NAME" 2>&1 | tail -10
exit 1
