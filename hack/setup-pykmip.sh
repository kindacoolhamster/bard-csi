#!/usr/bin/env bash
# Stand up a local PyKMIP server (a free KMIP key manager) for the env-gated
# TestKMIPLive, which drives the real KMIP provider client against a different
# implementation than the hermetic test's in-process server.
#
#   bash hack/setup-pykmip.sh           # -> PyKMIP on 127.0.0.1:5696 + certs in /tmp/pykmip
#   bash hack/setup-pykmip.sh delete    # teardown
#
# Then:
#   BARD_CSI_KMIP_TEST=1 KMIP_ENDPOINT=127.0.0.1:5696 \
#   KMIP_CLIENT_CERT=/tmp/pykmip/client.crt KMIP_CLIENT_KEY=/tmp/pykmip/client.key \
#   KMIP_CA=/tmp/pykmip/ca.crt go test ./internal/cephplugin -run TestKMIPLive -v
#
# GOTCHA baked in below: the certs are ECDSA on purpose. PyKMIP's TLS1.2 auth suite
# only offers modern (ECDHE-ECDSA-GCM) ciphers for an ECDSA server cert; with an RSA
# cert it offers only legacy CBC ciphers a modern Go client won't negotiate, giving
# "tls: handshake failure".
set -euo pipefail

DIR=/tmp/pykmip
NAME=bard-pykmip

if [ "${1:-}" = "delete" ]; then
  podman rm -f "$NAME" >/dev/null 2>&1 || true
  rm -rf "$DIR"
  echo "torn down $NAME + $DIR"
  exit 0
fi

mkdir -p "$DIR"
cd "$DIR"

# ECDSA CA + server (SAN 127.0.0.1) + client certs.
openssl ecparam -name prime256v1 -genkey -noout -out ca.key
openssl req -x509 -new -key ca.key -out ca.crt -days 30 -subj "/CN=bard-kmip-ca"
openssl ecparam -name prime256v1 -genkey -noout -out server.key
openssl req -new -key server.key -out server.csr -subj "/CN=127.0.0.1"
# SAN covers localhost (host-side TestKMIPLive) and galileo's LAN IP (the k3s VMs
# reach the KMIP server there, like the Ceph mon). Override with KMIP_SAN_IP.
printf 'subjectAltName=IP:127.0.0.1,IP:%s\n' "${KMIP_SAN_IP:-192.168.1.225}" > san.ext
openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial -out server.crt -days 30 -extfile san.ext
openssl ecparam -name prime256v1 -genkey -noout -out client.key
openssl req -new -key client.key -out client.csr -subj "/CN=bard-client"
openssl x509 -req -in client.csr -CA ca.crt -CAkey ca.key -CAcreateserial -out client.crt -days 30

cat > server.conf <<'EOF'
[server]
hostname=0.0.0.0
port=5696
certificate_path=/certs/server.crt
key_path=/certs/server.key
ca_path=/certs/ca.crt
auth_suite=TLS1.2
enable_tls_client_auth=False
logging_level=INFO
database_path=/tmp/pykmip.db
EOF

podman rm -f "$NAME" >/dev/null 2>&1 || true
podman run -d --name "$NAME" -p 5696:5696 -v "$DIR:/certs:Z" docker.io/library/python:3.11-slim \
  bash -c "pip install --quiet pykmip 2>/dev/null && pykmip-server -f /certs/server.conf"

echo -n "waiting for PyKMIP on :5696 "
for _ in $(seq 1 40); do
  if timeout 3 bash -c 'cat < /dev/null > /dev/tcp/127.0.0.1/5696' 2>/dev/null; then echo " up"; exit 0; fi
  echo -n .; sleep 3
done
echo " TIMEOUT"; podman logs "$NAME" 2>&1 | tail -10; exit 1
