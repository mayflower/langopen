#!/usr/bin/env bash
set -euo pipefail

export KUBECONFIG="${KUBECONFIG:-$HOME/.kube/data-lan-mayflower}"
NAMESPACE="${NAMESPACE:-langopen}"
API_SERVICE="${API_SERVICE:-langopen-api-server}"
CONTROL_PLANE_SERVICE="${CONTROL_PLANE_SERVICE:-langopen-control-plane}"
SMOKE_POD_IMAGE="${SMOKE_POD_IMAGE:-curlimages/curl:8.12.1}"
SMOKE_POD_NAME="${SMOKE_POD_NAME:-langopen-parity-smoke-$(date +%s)}"
API_BASE="http://${API_SERVICE}"
CONTROL_BASE="http://${CONTROL_PLANE_SERVICE}"

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required command: $1" >&2
    exit 1
  fi
}

fail() {
  echo "parity smoke failed: $*" >&2
  exit 1
}

cleanup() {
  kubectl -n "$NAMESPACE" delete pod "$SMOKE_POD_NAME" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT

kcurl() {
  kubectl -n "$NAMESPACE" exec "$SMOKE_POD_NAME" -- curl -sS "$@"
}

require_cmd kubectl
require_cmd jq

BOOTSTRAP_KEY="$(kubectl -n "$NAMESPACE" get secret langopen-runtime-secrets -o jsonpath='{.data.BOOTSTRAP_API_KEY}' | base64 --decode)"
[[ -n "$BOOTSTRAP_KEY" ]] || fail "BOOTSTRAP_API_KEY is empty in secret/langopen-runtime-secrets"

kubectl -n "$NAMESPACE" run "$SMOKE_POD_NAME" --image="$SMOKE_POD_IMAGE" --restart=Never --command -- sleep 600 >/dev/null
kubectl -n "$NAMESPACE" wait --for=condition=Ready "pod/$SMOKE_POD_NAME" --timeout=120s >/dev/null

WORKER_EXECUTOR="$(kubectl -n "$NAMESPACE" get deploy -l app.kubernetes.io/component=worker -o jsonpath='{.items[0].spec.template.spec.containers[0].env[?(@.name=="LANGOPEN_EXECUTOR")].value}')"
[[ "$WORKER_EXECUTOR" == "runtime" ]] || fail "expected LANGOPEN_EXECUTOR=runtime, got '$WORKER_EXECUTOR'"
kubectl -n "$NAMESPACE" get deploy -l app.kubernetes.io/component=runtime-runner >/dev/null

ASSISTANT_JSON="$(kcurl -H "X-Api-Key: ${BOOTSTRAP_KEY}" -H "Content-Type: application/json" -d '{"deployment_id":"dep_default","graph_id":"graph_parity"}' "$API_BASE/api/v1/assistants")"
ASSISTANT_ID="$(printf '%s' "$ASSISTANT_JSON" | jq -r '.id')"
[[ -n "$ASSISTANT_ID" && "$ASSISTANT_ID" != "null" ]] || fail "assistant create failed: $ASSISTANT_JSON"

THREAD_JSON="$(kcurl -H "X-Api-Key: ${BOOTSTRAP_KEY}" -H "Content-Type: application/json" -d '{"assistant_id":"'"$ASSISTANT_ID"'","metadata":{"source":"parity"}}' "$API_BASE/api/v1/threads")"
THREAD_ID="$(printf '%s' "$THREAD_JSON" | jq -r '.id')"
[[ -n "$THREAD_ID" && "$THREAD_ID" != "null" ]] || fail "thread create failed: $THREAD_JSON"

ASSIST_SEARCH="$(kcurl -H "X-Api-Key: ${BOOTSTRAP_KEY}" -H "Content-Type: application/json" -d '{"query":"graph_parity"}' "$API_BASE/api/v1/assistants/search")"
[[ "$(printf '%s' "$ASSIST_SEARCH" | jq -r '.total')" -ge 1 ]] || fail "assistants/search returned no items: $ASSIST_SEARCH"

THREAD_SEARCH="$(kcurl -H "X-Api-Key: ${BOOTSTRAP_KEY}" -H "Content-Type: application/json" -d '{"assistant_id":"'"$ASSISTANT_ID"'"}' "$API_BASE/api/v1/threads/search")"
[[ "$(printf '%s' "$THREAD_SEARCH" | jq -r '.total')" -ge 1 ]] || fail "threads/search returned no items: $THREAD_SEARCH"

STATE_JSON="$(kcurl -H "X-Api-Key: ${BOOTSTRAP_KEY}" -H "Content-Type: application/json" -d '{"checkpoint_id":"cp_smoke","values":{"stage":"draft"},"reason":"smoke"}' "$API_BASE/api/v1/threads/$THREAD_ID/state")"
[[ "$(printf '%s' "$STATE_JSON" | jq -r '.thread_id')" == "$THREAD_ID" ]] || fail "thread state update failed: $STATE_JSON"

HISTORY_JSON="$(kcurl -H "X-Api-Key: ${BOOTSTRAP_KEY}" "$API_BASE/api/v1/threads/$THREAD_ID/history")"
[[ "$(printf '%s' "$HISTORY_JSON" | jq -r '.items | length')" -ge 1 ]] || fail "thread history empty: $HISTORY_JSON"

WAIT_THREAD_JSON="$(kcurl -H "X-Api-Key: ${BOOTSTRAP_KEY}" -H "Content-Type: application/json" -d '{"assistant_id":"'"$ASSISTANT_ID"'"}' "$API_BASE/api/v1/threads/$THREAD_ID/runs/wait")"
[[ "$(printf '%s' "$WAIT_THREAD_JSON" | jq -r '.status')" == "success" ]] || fail "thread run wait failed: $WAIT_THREAD_JSON"

WAIT_STATELESS_JSON="$(kcurl -H "X-Api-Key: ${BOOTSTRAP_KEY}" -H "Content-Type: application/json" -d '{"assistant_id":"'"$ASSISTANT_ID"'"}' "$API_BASE/api/v1/runs/wait")"
RUN_ID="$(printf '%s' "$WAIT_STATELESS_JSON" | jq -r '.id')"
[[ "$(printf '%s' "$WAIT_STATELESS_JSON" | jq -r '.status')" == "success" ]] || fail "stateless wait failed: $WAIT_STATELESS_JSON"

WAIT_EXISTING_JSON="$(kcurl -H "X-Api-Key: ${BOOTSTRAP_KEY}" -H "Content-Type: application/json" -d '{}' "$API_BASE/api/v1/runs/$RUN_ID/wait")"
[[ "$(printf '%s' "$WAIT_EXISTING_JSON" | jq -r '.status')" == "success" ]] || fail "wait existing failed: $WAIT_EXISTING_JSON"

AGENT_CARD="$(kcurl -H "X-Api-Key: ${BOOTSTRAP_KEY}" "$API_BASE/api/v1/a2a/$ASSISTANT_ID/.well-known/agent-card.json")"
[[ "$(printf '%s' "$AGENT_CARD" | jq -r '.id')" == "$ASSISTANT_ID" ]] || fail "agent card lookup failed: $AGENT_CARD"

DEPLOY_JSON="$(kcurl -H "X-Api-Key: ${BOOTSTRAP_KEY}" -H "Content-Type: application/json" -d '{"project_id":"proj_default","repo_url":"https://github.com/mayflower/langopen","git_ref":"main","repo_path":"/"}' "$CONTROL_BASE/v2/deployments")"
DEPLOY_ID="$(printf '%s' "$DEPLOY_JSON" | jq -r '.id')"
[[ -n "$DEPLOY_ID" && "$DEPLOY_ID" != "null" ]] || fail "v2 deployment create failed: $DEPLOY_JSON"

ROLLBACK_JSON="$(kcurl -H "X-Api-Key: ${BOOTSTRAP_KEY}" -H "Content-Type: application/json" -d '{"image_digest":"ghcr.io/mayflower/langopen/api-server:latest"}' "$CONTROL_BASE/v2/deployments/$DEPLOY_ID/rollback")"
[[ "$(printf '%s' "$ROLLBACK_JSON" | jq -r '.id')" == "$DEPLOY_ID" ]] || fail "v2 rollback failed: $ROLLBACK_JSON"

REV_JSON="$(kcurl -H "X-Api-Key: ${BOOTSTRAP_KEY}" "$CONTROL_BASE/v2/deployments/$DEPLOY_ID/revisions")"
[[ "$(printf '%s' "$REV_JSON" | jq -r '.total')" -ge 1 ]] || fail "v2 revisions empty: $REV_JSON"

GH_AUTH_JSON="$(kcurl -H "X-Api-Key: ${BOOTSTRAP_KEY}" "$CONTROL_BASE/v1/integrations/github/auth")"
[[ "$(printf '%s' "$GH_AUTH_JSON" | jq -r '.provider')" == "github" ]] || fail "github auth metadata failed: $GH_AUTH_JSON"

echo "parity smoke test complete"
