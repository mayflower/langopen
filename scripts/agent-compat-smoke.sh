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
WINDOW_START="2025-01-01"
WINDOW_END="2026-12-31"

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
  local value
  value="$(kubectl -n "$NAMESPACE" get secret langopen-runtime-secrets -o jsonpath="{.data.${key}}" 2>/dev/null | base64 --decode || true)"
  if [[ -n "$value" ]]; then
    printf '%s' "$value"
    return 0
  fi

  case "$key" in
    ELASTICSEARCH_USER)
      printf '%s' "elastic"
      ;;
    ELASTICSEARCH_PASSWORD)
      kubectl -n eck-stack get secret elasticsearch-es-elastic-user -o jsonpath='{.data.elastic}' 2>/dev/null | base64 --decode || true
      ;;
    ELASTICSEARCH_URL)
      printf '%s' "https://elasticsearch-es-http.eck-stack.svc.cluster.local:9200"
      ;;
  esac
}

assert_fixture_commit_window() {
  local repo="$1"
  local sha="$2"
  local date
  date="$(curl -fsSL "https://api.github.com/repos/${repo}/commits/${sha}" | jq -r '.commit.committer.date // empty')"
  [[ -n "$date" ]] || return 1
  local day="${date%%T*}"
  [[ "$day" > "$WINDOW_END" || "$day" < "$WINDOW_START" ]] && return 1
  return 0
}

require_cmd kubectl
require_cmd jq
require_cmd curl

BOOTSTRAP_KEY="$(secret_value BOOTSTRAP_API_KEY)"
[[ -n "$BOOTSTRAP_KEY" ]] || fail "BOOTSTRAP_API_KEY is empty in secret/langopen-runtime-secrets"

WORKER_EXECUTOR="$(kubectl -n "$NAMESPACE" get deploy -l app.kubernetes.io/component=worker -o jsonpath='{.items[0].spec.template.spec.containers[0].env[?(@.name=="LANGOPEN_EXECUTOR")].value}')"
[[ "$WORKER_EXECUTOR" == "runtime" ]] || fail "expected LANGOPEN_EXECUTOR=runtime, got '$WORKER_EXECUTOR'"

fixtures=(
  "new-langgraph-project|langchain-ai/new-langgraph-project|e7d94c8a78401574fb29e261d6b0acfc9ed3f1e3|agent|.|OPENAI_API_KEY|{}|compat smoke: reply with OK"
  "react-agent|langchain-ai/react-agent|0e68628a2217072a79edbc22d0b9efbb13112da5|agent|.|OPENAI_API_KEY|{\"model\":\"openai:gpt-4o-mini\",\"max_search_results\":1,\"system_prompt\":\"You are a concise assistant. Reply in one short sentence and avoid tool use unless required.\"}|compat smoke: reply with OK and avoid tools"
  "retrieval-agent-template|langchain-ai/retrieval-agent-template|062c3400e51a3b25351a7dfe8f0446a1d4713fb8|retrieval_graph|.|OPENAI_API_KEY,ELASTICSEARCH_URL,ELASTICSEARCH_USER,ELASTICSEARCH_PASSWORD|{\"user_id\":\"compat-smoke\",\"retriever_provider\":\"elastic-local\",\"embedding_model\":\"openai/text-embedding-3-small\",\"query_model\":\"openai:gpt-4o-mini\",\"response_model\":\"openai:gpt-4o-mini\",\"search_kwargs\":{\"k\":1}}|compat smoke: answer from retrieved context or say no results"
  "agent-inbox-example|langchain-ai/agent-inbox-langgraph-example|f56c3326c7cc6652e56431240b669e5ca4c72f96|agent|.|OPENAI_API_KEY|{}|compat smoke: reply with OK"
  "oap-tools-agent|langchain-ai/oap-langgraph-tools-agent|8bf78d70671f1a6e34fe545066ae6fd47a4687be|agent|.|OPENAI_API_KEY|{\"model_name\":\"openai:gpt-4o-mini\",\"temperature\":0,\"max_tokens\":128}|compat smoke: reply with OK and no tools"
)

for item in "${fixtures[@]}"; do
  IFS='|' read -r name repo sha graph repo_path required_env config_json prompt_text <<<"$item"
  if ! assert_fixture_commit_window "$repo" "$sha"; then
    fail "fixture ${name} commit ${sha} for ${repo} is outside ${WINDOW_START}..${WINDOW_END} or cannot be verified"
  fi
  echo "[compat] fixture ${name} (${repo}@${sha}) required env: ${required_env}"
done

kubectl -n "$NAMESPACE" run "$SMOKE_POD_NAME" --image="$SMOKE_POD_IMAGE" --restart=Never --command -- sleep 1800 >/dev/null
kubectl -n "$NAMESPACE" wait --for=condition=Ready "pod/$SMOKE_POD_NAME" --timeout=120s >/dev/null

run_agent_case() {
  local name="$1"
  local repo="$2"
  local sha="$3"
  local graph_id="$4"
  local repo_path="$5"
  local required_env_csv="$6"
  local config_json="$7"
  local prompt_text="$8"
  local repo_url="https://github.com/${repo}"

  echo "[compat] deploying ${name} (${repo_url} ${repo_path}#${sha})"

  local dep_json dep_id
  dep_json="$(kcurl -H "X-Api-Key: ${BOOTSTRAP_KEY}" -H "Content-Type: application/json" -d "{\"project_id\":\"proj_default\",\"repo_url\":\"${repo_url}\",\"git_ref\":\"${sha}\",\"repo_path\":\"${repo_path}\"}" "$CONTROL_BASE/internal/v1/deployments")"
  dep_id="$(printf '%s' "$dep_json" | jq -r '.id // empty')"
  [[ -n "$dep_id" ]] || { CASE_ERROR="create_deployment:$(printf '%s' "$dep_json" | tr '\n' ' ')"; return 1; }

  local env_key env_val
  IFS=',' read -ra env_keys <<<"$required_env_csv"
  for env_key in "${env_keys[@]}"; do
    env_key="${env_key// /}"
    [[ -n "$env_key" ]] || continue
    env_val="$(secret_value "$env_key")"
    if [[ -z "$env_val" ]]; then
      CASE_ERROR="missing_secret:${env_key}"
      return 1
    fi
    local var_json
    var_json="$(kcurl -H "X-Api-Key: ${BOOTSTRAP_KEY}" -H "Content-Type: application/json" -X PUT \
      -d "{\"project_id\":\"proj_default\",\"key\":\"${env_key}\",\"value\":$(jq -Rn --arg v "$env_val" '$v'),\"is_secret\":true}" \
      "$CONTROL_BASE/internal/v1/deployments/${dep_id}/variables")"
    local var_key
    var_key="$(printf '%s' "$var_json" | jq -r '.key // empty')"
    [[ -n "$var_key" ]] || { CASE_ERROR="upsert_variable:$(printf '%s' "$var_json" | tr '\n' ' ')"; return 1; }
  done

  local asst_json asst_id
  asst_json="$(kcurl -H "X-Api-Key: ${BOOTSTRAP_KEY}" -H "Content-Type: application/json" -d "{\"deployment_id\":\"${dep_id}\",\"graph_id\":\"${graph_id}\"}" "$API_BASE/api/v1/assistants")"
  asst_id="$(printf '%s' "$asst_json" | jq -r '.id // empty')"
  [[ -n "$asst_id" ]] || { CASE_ERROR="create_assistant:$(printf '%s' "$asst_json" | tr '\n' ' ')"; return 1; }

  local run_json status run_id err_type err_msg run_payload
  run_payload="$(jq -cn \
    --arg assistant_id "$asst_id" \
    --arg prompt "$prompt_text" \
    --argjson cfg "$config_json" \
    '{assistant_id:$assistant_id,input:{messages:[{role:"user",content:$prompt}]},configurable:$cfg}')"
  run_json="$(kcurl -H "X-Api-Key: ${BOOTSTRAP_KEY}" -H "Content-Type: application/json" -d "$run_payload" "$API_BASE/api/v1/runs/wait?timeout_seconds=${RUN_TIMEOUT_SECONDS}")"
  status="$(printf '%s' "$run_json" | jq -r '.status // empty')"
  if [[ "$status" != "success" ]]; then
    run_id="$(printf '%s' "$run_json" | jq -r '.id // ""')"
    err_type="$(printf '%s' "$run_json" | jq -r '.error.type // ""')"
    err_msg="$(printf '%s' "$run_json" | jq -r '.error.message // ""')"
    CASE_ERROR="run_failed:status=${status} run_id=${run_id} type=${err_type} message=${err_msg}"
    return 1
  fi

  echo "[compat] success ${name}"
  return 0
}

failures=()
for item in "${fixtures[@]}"; do
  CASE_ERROR=""
  IFS='|' read -r name repo sha graph repo_path required_env config_json prompt_text <<<"$item"
  if ! run_agent_case "$name" "$repo" "$sha" "$graph" "$repo_path" "$required_env" "$config_json" "$prompt_text"; then
    failures+=("${name}|${repo}|${sha}|${CASE_ERROR:-unknown}")
  fi
done

if [[ ${#failures[@]} -gt 0 ]]; then
  echo "[compat] failed fixtures:"
  for entry in "${failures[@]}"; do
    IFS='|' read -r name repo sha reason <<<"$entry"
    echo "  - ${name} (${repo}@${sha}) :: ${reason}"
  done
  exit 1
fi

echo "agent compatibility smoke complete"
