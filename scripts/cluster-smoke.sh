#!/usr/bin/env bash
set -euo pipefail

export KUBECONFIG="${KUBECONFIG:-$HOME/.kube/data-lan-mayflower}"
NAMESPACE="${NAMESPACE:-langopen}"
API_SERVICE="${API_SERVICE:-langopen-api-server}"
CONTROL_PLANE_SERVICE="${CONTROL_PLANE_SERVICE:-langopen-control-plane}"
SMOKE_POD_IMAGE="${SMOKE_POD_IMAGE:-curlimages/curl:8.12.1}"
SMOKE_POD_NAME="${SMOKE_POD_NAME:-langopen-smoke-$(date +%s)}"
API_BASE="http://${API_SERVICE}"
CONTROL_BASE="http://${CONTROL_PLANE_SERVICE}"

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required command: $1" >&2
    exit 1
  fi
}

fail() {
  echo "smoke failed: $*" >&2
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

kubectl -n "$NAMESPACE" get deploy

BOOTSTRAP_KEY="$(kubectl -n "$NAMESPACE" get secret langopen-runtime-secrets -o jsonpath='{.data.BOOTSTRAP_API_KEY}' | base64 --decode)"
[[ -n "$BOOTSTRAP_KEY" ]] || fail "BOOTSTRAP_API_KEY is empty in secret/langopen-runtime-secrets"

kubectl -n "$NAMESPACE" run "$SMOKE_POD_NAME" --image="$SMOKE_POD_IMAGE" --restart=Never --command -- sleep 600 >/dev/null
kubectl -n "$NAMESPACE" wait --for=condition=Ready "pod/$SMOKE_POD_NAME" --timeout=120s >/dev/null

HEALTHZ="$(kcurl "$API_BASE/healthz")"
[[ "$HEALTHZ" == "ok" ]] || fail "healthz response was '$HEALTHZ'"
kcurl -H "X-Api-Key: ${BOOTSTRAP_KEY}" "$API_BASE/docs" >/dev/null
OPENAPI_HEAD="$(kcurl -H "X-Api-Key: ${BOOTSTRAP_KEY}" "$API_BASE/openapi.json" | head -c 1)"
[[ "$OPENAPI_HEAD" == "{" ]] || fail "openapi.json was not JSON"

THREAD_JSON="$(kcurl -H "X-Api-Key: ${BOOTSTRAP_KEY}" -H "Content-Type: application/json" -d '{}' "$API_BASE/api/v1/threads")"
THREAD_ID="$(printf '%s' "$THREAD_JSON" | jq -r '.id')"
[[ -n "$THREAD_ID" && "$THREAD_ID" != "null" ]] || fail "thread id missing in response: $THREAD_JSON"
echo "created thread: $THREAD_ID"

STREAM_OUTPUT="$(kcurl -N --max-time 20 -H "X-Api-Key: ${BOOTSTRAP_KEY}" -H "Content-Type: application/json" -d '{"multitask_strategy":"enqueue","stream_resumable":true}' "$API_BASE/api/v1/threads/${THREAD_ID}/runs/stream")"
printf '%s\n' "$STREAM_OUTPUT" | grep -q "event: run_queued" || fail "create stream missing run_queued"
printf '%s\n' "$STREAM_OUTPUT" | grep -Eq "event: run_(completed|interrupted|failed)" || fail "create stream missing terminal event"

RUN_ID="$(printf '%s\n' "$STREAM_OUTPUT" | sed -n 's/^data: //p' | jq -r 'select(.run_id != null) | .run_id' | head -n 1)"
[[ -n "$RUN_ID" ]] || fail "could not extract run_id from stream output"
echo "created run: $RUN_ID"

REPLAY_ALL="$(kcurl -N --max-time 20 -H "X-Api-Key: ${BOOTSTRAP_KEY}" -H "Last-Event-ID: -1" "$API_BASE/api/v1/threads/${THREAD_ID}/runs/${RUN_ID}/stream")"
printf '%s\n' "$REPLAY_ALL" | grep -q "id: 1" || fail "replay from -1 missing first event id"
printf '%s\n' "$REPLAY_ALL" | grep -q "event: run_queued" || fail "replay from -1 missing queued event"

REPLAY_AFTER_ONE="$(kcurl -N --max-time 20 -H "X-Api-Key: ${BOOTSTRAP_KEY}" -H "Last-Event-ID: 1" "$API_BASE/api/v1/threads/${THREAD_ID}/runs/${RUN_ID}/stream")"
if printf '%s\n' "$REPLAY_AFTER_ONE" | grep -q "event: run_queued"; then
  fail "replay from Last-Event-ID: 1 still included run_queued"
fi
printf '%s\n' "$REPLAY_AFTER_ONE" | grep -Eq "event: (token|run_(completed|interrupted|failed))" || fail "replay from Last-Event-ID: 1 missing subsequent events"

INTERRUPT_JSON="$(kcurl -X POST -H "X-Api-Key: ${BOOTSTRAP_KEY}" -H "Content-Type: application/json" -d '{}' "$API_BASE/api/v1/threads/${THREAD_ID}/runs/${RUN_ID}/cancel?action=interrupt")"
[[ "$(printf '%s' "$INTERRUPT_JSON" | jq -r '.action')" == "interrupt" ]] || fail "interrupt action failed: $INTERRUPT_JSON"

ROLLBACK_JSON="$(kcurl -X POST -H "X-Api-Key: ${BOOTSTRAP_KEY}" -H "Content-Type: application/json" -d '{}' "$API_BASE/api/v1/threads/${THREAD_ID}/runs/${RUN_ID}/cancel?action=rollback")"
[[ "$(printf '%s' "$ROLLBACK_JSON" | jq -r '.action')" == "rollback" ]] || fail "rollback action failed: $ROLLBACK_JSON"

RUN_STATUS_CODE="$(kcurl -o /dev/null -w '%{http_code}' -H "X-Api-Key: ${BOOTSTRAP_KEY}" "$API_BASE/api/v1/runs/${RUN_ID}")"
[[ "$RUN_STATUS_CODE" == "404" ]] || fail "run should be deleted after rollback, got status $RUN_STATUS_CODE"

A2A_JSON="$(kcurl -H "X-Api-Key: ${BOOTSTRAP_KEY}" -H "Content-Type: application/json" -d '{"jsonrpc":"2.0","id":"1","method":"message/send","params":{"contextId":"'"$THREAD_ID"'"}}' "$API_BASE/a2a/asst_default")"
[[ "$(printf '%s' "$A2A_JSON" | jq -r '.jsonrpc')" == "2.0" ]] || fail "a2a response invalid: $A2A_JSON"

MCP_JSON="$(kcurl -H "X-Api-Key: ${BOOTSTRAP_KEY}" -H "Content-Type: application/json" -d '{"jsonrpc":"2.0","id":"1","method":"initialize","params":{}}' "$API_BASE/mcp")"
[[ "$(printf '%s' "$MCP_JSON" | jq -r '.jsonrpc')" == "2.0" ]] || fail "mcp response invalid: $MCP_JSON"

CP_DEPLOY_JSON="$(kcurl -H "Content-Type: application/json" -d '{"project_id":"proj_default","repo_url":"https://github.com/acme/agent","git_ref":"main","repo_path":"/"}' "$CONTROL_BASE/internal/v1/deployments")"
CP_DEPLOY_ID="$(printf '%s' "$CP_DEPLOY_JSON" | jq -r '.id')"
[[ -n "$CP_DEPLOY_ID" && "$CP_DEPLOY_ID" != "null" ]] || fail "control-plane deployment id missing: $CP_DEPLOY_JSON"
echo "control-plane deployment: $CP_DEPLOY_ID"

CP_BUILD_JSON="$(kcurl -H "Content-Type: application/json" -d '{"deployment_id":"'"$CP_DEPLOY_ID"'","commit_sha":"abcdef123456","image_name":"ghcr.io/acme/agent"}' "$CONTROL_BASE/internal/v1/builds")"
CP_BUILD_STATUS="$(printf '%s' "$CP_BUILD_JSON" | jq -r '.status')"
[[ "$CP_BUILD_STATUS" == "succeeded" || "$CP_BUILD_STATUS" == "queued" ]] || fail "control-plane build trigger failed: $CP_BUILD_JSON"

echo "smoke test complete"
