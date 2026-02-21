#!/usr/bin/env bash
set -euo pipefail

export KUBECONFIG="${KUBECONFIG:-$HOME/.kube/data-lan-mayflower}"
NAMESPACE="${NAMESPACE:-langopen}"
API_SERVICE="${API_SERVICE:-langopen-api-server}"
CONTROL_PLANE_SERVICE="${CONTROL_PLANE_SERVICE:-langopen-control-plane}"
SMOKE_POD_IMAGE="${SMOKE_POD_IMAGE:-curlimages/curl:8.12.1}"
SMOKE_POD_NAME="${SMOKE_POD_NAME:-langopen-agent-compat-smoke-$(date +%s)}"
API_BASE="http://${API_SERVICE}"
CONTROL_BASE="http://${CONTROL_PLANE_SERVICE}"
RUN_TIMEOUT_SECONDS="${RUN_TIMEOUT_SECONDS:-420}"

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required command: $1" >&2
    exit 1
  fi
}

fail() {
  echo "agent compat smoke failed: $*" >&2
  exit 1
}

cleanup() {
  kubectl -n "$NAMESPACE" delete pod "$SMOKE_POD_NAME" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT

kcurl() {
  kubectl -n "$NAMESPACE" exec "$SMOKE_POD_NAME" -- curl -sS "$@"
}

secret_value() {
  local key="$1"
  kubectl -n "$NAMESPACE" get secret langopen-runtime-secrets -o jsonpath="{.data.${key}}" 2>/dev/null | base64 --decode || true
}

require_cmd kubectl
require_cmd jq

BOOTSTRAP_KEY="$(secret_value BOOTSTRAP_API_KEY)"
[[ -n "$BOOTSTRAP_KEY" ]] || fail "BOOTSTRAP_API_KEY is empty in secret/langopen-runtime-secrets"

REQUIRED_PROVIDER_KEYS=(OPENAI_API_KEY GROQ_API_KEY)
for provider_key in "${REQUIRED_PROVIDER_KEYS[@]}"; do
  value="$(secret_value "$provider_key")"
  [[ -n "$value" ]] || fail "required provider key ${provider_key} missing in secret/langopen-runtime-secrets"
done

kubectl -n "$NAMESPACE" run "$SMOKE_POD_NAME" --image="$SMOKE_POD_IMAGE" --restart=Never --command -- sleep 1800 >/dev/null
kubectl -n "$NAMESPACE" wait --for=condition=Ready "pod/$SMOKE_POD_NAME" --timeout=120s >/dev/null

WORKER_EXECUTOR="$(kubectl -n "$NAMESPACE" get deploy -l app.kubernetes.io/component=worker -o jsonpath='{.items[0].spec.template.spec.containers[0].env[?(@.name=="LANGOPEN_EXECUTOR")].value}')"
[[ "$WORKER_EXECUTOR" == "runtime" ]] || fail "expected LANGOPEN_EXECUTOR=runtime, got '$WORKER_EXECUTOR'"

RUNTIME_POLICY_JSON="$(kubectl -n "$NAMESPACE" get netpol langopen-runtime-egress-https -o json 2>/dev/null || true)"
[[ -n "$RUNTIME_POLICY_JSON" ]] || fail "runtime HTTPS egress network policy not found"

agents=(
  "proof|https://github.com/mayflower/langopen|main|examples/python_proof_agent|proof_agent:run"
  "staminna|https://github.com/staminna/RAG-PDF-LangGraph-plus-LangChain|main|.|rag"
  "d-hackmt|https://github.com/d-hackmt/Ai-Blog-Generation|main|.|blog_generator_agent"
  "lolloberga|https://github.com/lolloberga/langgraph_multi-agent_supervisor|main|.|agent"
  "flave1|https://github.com/Flave1/robot_trader|main|.|market_watch_dog_agent"
)

run_agent_case() {
  local name="$1"
  local repo_url="$2"
  local git_ref="$3"
  local repo_path="$4"
  local graph_id="$5"

  echo "[compat] deploying ${name} (${repo_url} ${repo_path}#${git_ref})"

  local dep_json
  dep_json="$(kcurl -H "X-Api-Key: ${BOOTSTRAP_KEY}" -H "Content-Type: application/json" -d "{\"project_id\":\"proj_default\",\"repo_url\":\"${repo_url}\",\"git_ref\":\"${git_ref}\",\"repo_path\":\"${repo_path}\"}" "$CONTROL_BASE/internal/v1/deployments")"
  local dep_id
  dep_id="$(printf '%s' "$dep_json" | jq -r '.id')"
  [[ -n "$dep_id" && "$dep_id" != "null" ]] || fail "deployment create failed for ${name}: ${dep_json}"

  local asst_json
  asst_json="$(kcurl -H "X-Api-Key: ${BOOTSTRAP_KEY}" -H "Content-Type: application/json" -d "{\"deployment_id\":\"${dep_id}\",\"graph_id\":\"${graph_id}\"}" "$API_BASE/api/v1/assistants")"
  local asst_id
  asst_id="$(printf '%s' "$asst_json" | jq -r '.id')"
  [[ -n "$asst_id" && "$asst_id" != "null" ]] || fail "assistant create failed for ${name}: ${asst_json}"

  local run_json
  run_json="$(kcurl -H "X-Api-Key: ${BOOTSTRAP_KEY}" -H "Content-Type: application/json" -d "{\"assistant_id\":\"${asst_id}\",\"input\":{\"messages\":[{\"role\":\"user\",\"content\":\"compat smoke ${name}\"}]}}" "$API_BASE/api/v1/runs/wait?timeout_seconds=${RUN_TIMEOUT_SECONDS}")"
  local status
  status="$(printf '%s' "$run_json" | jq -r '.status')"
  if [[ "$status" != "success" ]]; then
    local run_id
    run_id="$(printf '%s' "$run_json" | jq -r '.id')"
    local err_type
    err_type="$(printf '%s' "$run_json" | jq -r '.error.type // ""')"
    local err_msg
    err_msg="$(printf '%s' "$run_json" | jq -r '.error.message // ""')"
    fail "run failed for ${name}: status=${status} run_id=${run_id} error_type=${err_type} error_message=${err_msg} payload=${run_json}"
  fi

  echo "[compat] success ${name}"
}

for item in "${agents[@]}"; do
  IFS='|' read -r name repo_url git_ref repo_path graph_id <<<"$item"
  run_agent_case "$name" "$repo_url" "$git_ref" "$repo_path" "$graph_id"
done

echo "agent compatibility smoke complete"
