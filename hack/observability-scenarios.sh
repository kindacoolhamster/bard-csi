#!/usr/bin/env bash
# Lifecycle wrapper for deploy/examples/observability/scenarios.yaml.
# The scenarios intentionally remain active after `up`; run `down` to clean up.
set -euo pipefail

KUBECTL_BIN=${KUBECTL_BIN:-kubectl}
NAMESPACE=bard-observability-scenarios
BARD_NAMESPACE=${BARD_NAMESPACE:-kube-system}
BARD_CONTROLLER=${BARD_CONTROLLER:-}
OWNER_KEY=observability.bard.io/managed-by
OWNER_VALUE=bard-observability-scenarios
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
MANIFEST=$REPO_ROOT/deploy/examples/observability/scenarios.yaml

kc() { "$KUBECTL_BIN" "$@"; }

OWNED_RESOURCES=(
  "namespace/$NAMESPACE"
  storageclass/bard-observability-localpath
  storageclass/bard-observability-dispatch-error
  persistentvolume/bard-observability-unknown-instance
  persistentvolume/bard-observability-malformed-handle
  volumeattachment/bard-observability-stale-attachment
)

# Fixed names make the suite idempotent, but are safe only when every existing
# object proves it belongs to this harness. Check the whole set before mutating
# anything so `down` can never erase a coincidentally named user resource.
preflight_ownership() {
  local ref found owner
  for ref in "${OWNED_RESOURCES[@]}"; do
    if ! found=$(kc get "$ref" --ignore-not-found -o name); then
      printf 'cannot verify ownership of %s; refusing to continue\n' "$ref" >&2
      return 1
    fi
    [[ -z "$found" ]] && continue
    if ! owner=$(kc get "$ref" \
      -o go-template='{{ index .metadata.annotations "observability.bard.io/managed-by" }}'); then
      printf 'cannot read ownership of %s; refusing to continue\n' "$ref" >&2
      return 1
    fi
    if [[ "$owner" != "$OWNER_VALUE" ]]; then
      printf '%s exists without %s=%s; refusing to modify or delete it\n' \
        "$ref" "$OWNER_KEY" "$OWNER_VALUE" >&2
      return 1
    fi
  done
}

usage() {
  cat <<'EOF'
Usage: hack/observability-scenarios.sh up|status|inspect|down

  up       apply the long-running telemetry and alarm scenarios
  status   show scenario workloads and cluster-scoped inspect fixtures
  inspect  run kubectl bard inspect (raw controller fallback if unavailable)
  down     delete every scenario resource and wait for dynamic PV cleanup

KUBECONFIG is inherited normally. Override kubectl with KUBECTL_BIN. The raw
inspect fallback discovers a Bard controller in BARD_NAMESPACE (kube-system);
set BARD_CONTROLLER to a Deployment name if that namespace has multiple installs.
EOF
}

status() {
  kc -n "$NAMESPACE" get pods,pvc,jobs,cronjobs -o wide
  kc get persistentvolumes \
    -l app.kubernetes.io/name=bard-observability-scenarios -o wide
  kc get volumeattachments \
    -l app.kubernetes.io/name=bard-observability-scenarios -o wide
}

up() {
  preflight_ownership
  kc apply -f "$MANIFEST"

  kc -n "$NAMESPACE" wait \
    --for=jsonpath='{.status.phase}'=Bound \
    persistentvolumeclaim/bard-observability-steady --timeout=180s
  kc -n "$NAMESPACE" wait \
    --for=condition=Ready pod/bard-observability-steady --timeout=180s

  # Seed the dashboard immediately instead of waiting for the first minute tick.
  kc -n "$NAMESPACE" delete job bard-observability-churn-now \
    --ignore-not-found --wait=true
  kc -n "$NAMESPACE" create job --from=cronjob/bard-observability-churn \
    bard-observability-churn-now
  kc -n "$NAMESPACE" wait --for=condition=Complete \
    job/bard-observability-churn-now --timeout=180s

  # Seed both failure counters immediately. Their Pods stay Pending until each
  # Job's deadline; the CronJobs repeat this bounded cycle once per minute.
  local failure
  for failure in unsupported-clone dispatch-error; do
    kc -n "$NAMESPACE" delete job "bard-observability-$failure-now" \
      --ignore-not-found --wait=true
    kc -n "$NAMESPACE" create job \
      --from="cronjob/bard-observability-$failure" \
      "bard-observability-$failure-now"
  done

  printf '\nScenario suite is active. Failure-job Pods are intentionally Pending until their deadlines.\n\n'
  status
}

inspect() {
  local rc controller
  set +e
  if command -v kubectl-bard >/dev/null 2>&1; then
    kc bard inspect
    rc=$?
  else
    printf 'kubectl-bard not found on PATH; running the same scanner directly in the controller.\n' >&2
    controller=$BARD_CONTROLLER
    if [[ -z "$controller" ]]; then
      controller=$(kc -n "$BARD_NAMESPACE" get deployments \
        -l app.kubernetes.io/part-of=bard-csi \
        -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
    fi
    if [[ -z "$controller" || "$controller" == *$'\n'* ]]; then
      printf 'expected exactly one Bard controller Deployment in %s; set BARD_CONTROLLER explicitly\n' \
        "$BARD_NAMESPACE" >&2
      rc=2
    else
      kc -n "$BARD_NAMESPACE" exec "deployment/$controller" -c bard-csi -- \
        /usr/local/bin/bard-csi --inspect
      rc=$?
    fi
  fi
  set -e

  printf '\ninspect exit code: %d (1 is expected while the unknown-instance fixture exists)\n' "$rc" >&2
  return "$rc"
}

down() {
  local dynamic_pvs=() pv_output namespace_name rc=0
  preflight_ownership

  if ! namespace_name=$(kc get "namespace/$NAMESPACE" --ignore-not-found -o name); then
    printf 'cannot determine whether scenario namespace exists; refusing partial cleanup\n' >&2
    return 1
  fi

  # Failure to list PVs is not the same as an empty result: without this list we
  # cannot prove cleanup, so abort before deleting anything.
  if ! pv_output=$(kc get persistentvolumes \
    -o jsonpath="{range .items[?(@.spec.claimRef.namespace=='$NAMESPACE')]}{.metadata.name}{'\n'}{end}"); then
    printf 'cannot enumerate scenario PVs; refusing partial cleanup\n' >&2
    return 1
  fi
  while IFS= read -r pv; do
    [[ -n "$pv" ]] && dynamic_pvs+=("$pv")
  done <<< "$pv_output"

  # Start every owned deletion even if one API call fails or namespace teardown
  # is slow. The final status remains non-zero until every wait succeeds.
  if [[ -n "$namespace_name" ]]; then
    kc delete namespace "$NAMESPACE" --wait=false || rc=1
  fi
  kc delete storageclass \
    bard-observability-localpath bard-observability-dispatch-error \
    --ignore-not-found || rc=1
  kc delete persistentvolume \
    bard-observability-unknown-instance bard-observability-malformed-handle \
    --ignore-not-found || rc=1
  kc delete volumeattachment bard-observability-stale-attachment \
    --ignore-not-found || rc=1

  if [[ -n "$namespace_name" ]]; then
    kc wait --for=delete "namespace/$NAMESPACE" --timeout=180s || rc=1
  fi

  for pv in "${dynamic_pvs[@]}"; do
    kc wait --for=delete "persistentvolume/$pv" --timeout=180s || {
      printf 'warning: dynamic PV %s still exists; inspect it before manual removal\n' "$pv" >&2
      rc=1
    }
  done

  if [[ "$rc" -eq 0 ]]; then
    printf 'Bard observability scenarios removed.\n'
  else
    printf 'scenario cleanup is incomplete; inspect the errors above\n' >&2
  fi
  return "$rc"
}

case ${1:-} in
  up) up ;;
  status) status ;;
  inspect) inspect ;;
  down) down ;;
  -h|--help|help|'') usage ;;
  *) usage >&2; exit 2 ;;
esac
