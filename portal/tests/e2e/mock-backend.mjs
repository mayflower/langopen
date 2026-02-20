import http from "node:http";

const PORT = Number(process.env.MOCK_BACKEND_PORT || 8787);

let state = createInitialState();

const server = http.createServer(async (req, res) => {
  const method = req.method || "GET";
  const url = new URL(req.url || "/", `http://127.0.0.1:${PORT}`);
  const path = url.pathname;

  if (path === "/healthz") {
    return sendJSON(res, 200, { ok: true });
  }

  if (path === "/__test/reset" && method === "POST") {
    state = createInitialState();
    return sendJSON(res, 200, { ok: true });
  }

  if (path.startsWith("/internal/v1/")) {
    return handleControl(method, path, url, req, res);
  }

  if (path.startsWith("/api/v1/")) {
    return handleData(method, path, url, req, res);
  }

  return sendJSON(res, 404, { error: { code: "not_found", message: `${method} ${path} not found` } });
});

server.listen(PORT, "127.0.0.1", () => {
  console.log(`[mock-backend] listening on http://127.0.0.1:${PORT}`);
});

function createInitialState() {
  return {
    counters: {
      dep: 3,
      build: 2,
      binding: 2,
      key: 2,
      run: 3
    },
    deployments: [
      {
        id: "dep_alpha",
        project_id: "proj_default",
        repo_url: "https://github.com/mayflower/langopen",
        git_ref: "main",
        repo_path: ".",
        runtime_profile: "gvisor",
        mode: "mode_a",
        current_image_digest: "sha256:alpha",
        updated_at: "2026-02-20T07:32:55Z"
      },
      {
        id: "dep_beta",
        project_id: "proj_default",
        repo_url: "https://github.com/acme/agent",
        git_ref: "main",
        repo_path: ".",
        runtime_profile: "gvisor",
        mode: "mode_a",
        current_image_digest: "sha256:beta",
        updated_at: "2026-02-20T07:31:06Z"
      }
    ],
    builds: [
      {
        id: "build_1",
        deployment_id: "dep_alpha",
        commit_sha: "abcdef123456",
        status: "succeeded",
        image_digest: "sha256:build1",
        logs_ref: "mock://build_1",
        logs: "Build completed for dep_alpha",
        created_at: "2026-02-20T07:20:00Z"
      }
    ],
    secretBindings: [
      {
        id: "bind_1",
        deployment_id: "dep_alpha",
        secret_name: "openai-secret",
        target_key: "OPENAI_API_KEY",
        created_at: "2026-02-20T07:25:00Z"
      }
    ],
    apiKeys: [
      {
        id: "key_bootstrap",
        project_id: "proj_default",
        name: "bootstrap",
        created_at: "2026-02-19T22:04:29.287211Z",
        revoked_at: null
      }
    ],
    assistants: [
      { id: "asst_alpha", deployment_id: "dep_alpha", graph_id: "proof_agent:run", version: "v1" },
      { id: "asst_beta", deployment_id: "dep_beta", graph_id: "proof_agent:run", version: "v1" },
      { id: "asst_default", deployment_id: "dep_alpha", graph_id: "default", version: "v1" }
    ],
    threads: [
      { id: "thread_alpha", assistant_id: "asst_alpha", updated_at: "2026-02-20T06:32:57Z" },
      { id: "thread_beta", assistant_id: "asst_beta", updated_at: "2026-02-20T06:20:00Z" }
    ],
    runs: [
      {
        id: "run_error_seed",
        thread_id: "thread_alpha",
        assistant_id: "asst_alpha",
        status: "error",
        created_at: "2026-02-20T06:00:00Z",
        updated_at: "2026-02-20T06:00:30Z"
      },
      {
        id: "run_pending_seed",
        thread_id: "thread_beta",
        assistant_id: "asst_beta",
        status: "pending",
        created_at: "2026-02-20T06:10:00Z",
        updated_at: "2026-02-20T06:10:30Z"
      }
    ]
  };
}

async function handleControl(method, path, url, req, res) {
  if (method === "GET" && path === "/internal/v1/deployments") {
    return sendJSON(res, 200, state.deployments);
  }

  if (method === "POST" && path === "/internal/v1/deployments") {
    const body = await readJSON(req);
    const id = `dep_mock_${state.counters.dep++}`;
    const row = {
      id,
      project_id: body.project_id || "proj_default",
      repo_url: body.repo_url || "https://github.com/acme/agent",
      git_ref: body.git_ref || "main",
      repo_path: body.repo_path || ".",
      runtime_profile: body.runtime_profile || "gvisor",
      mode: body.mode || "mode_a",
      current_image_digest: "",
      updated_at: "2026-02-20T08:00:00Z"
    };
    state.deployments.unshift(row);
    state.assistants.unshift({ id: `asst_${id}`, deployment_id: id, graph_id: "proof_agent:run", version: "v1" });
    return sendJSON(res, 200, row);
  }

  const depMatch = path.match(/^\/internal\/v1\/deployments\/([^/]+)$/);
  if (depMatch) {
    const depID = decodeURIComponent(depMatch[1]);

    if (method === "PATCH") {
      const body = await readJSON(req);
      state.deployments = state.deployments.map((dep) =>
        dep.id === depID
          ? {
              ...dep,
              repo_url: body.repo_url || dep.repo_url,
              git_ref: body.git_ref || dep.git_ref,
              repo_path: body.repo_path || dep.repo_path,
              runtime_profile: body.runtime_profile || dep.runtime_profile,
              mode: body.mode || dep.mode,
              updated_at: "2026-02-20T08:01:00Z"
            }
          : dep
      );
      return sendJSON(res, 200, { id: depID });
    }

    if (method === "DELETE") {
      state.deployments = state.deployments.filter((dep) => dep.id !== depID);
      state.secretBindings = state.secretBindings.filter((binding) => binding.deployment_id !== depID);
      state.assistants = state.assistants.filter((assistant) => assistant.deployment_id !== depID);
      return sendJSON(res, 200, { ok: true });
    }
  }

  if (method === "POST" && path === "/internal/v1/sources/validate") {
    return sendJSON(res, 200, { valid: true, checks: ["langgraph.json", "requirements.txt"] });
  }

  if (method === "GET" && path === "/internal/v1/builds") {
    return sendJSON(res, 200, state.builds);
  }

  if (method === "POST" && path === "/internal/v1/builds") {
    const body = await readJSON(req);
    const id = `build_${state.counters.build++}`;
    const row = {
      id,
      deployment_id: body.deployment_id || "dep_alpha",
      commit_sha: body.commit_sha || "abcdef123456",
      status: "queued",
      image_digest: `sha256:${id}`,
      logs_ref: `mock://${id}`,
      logs: `Build queued for ${body.deployment_id || "dep_alpha"}\nImage: ${body.image_name || "ghcr.io/acme/agent"}`,
      created_at: "2026-02-20T08:02:00Z"
    };
    state.builds.unshift(row);
    return sendJSON(res, 200, row);
  }

  const logsMatch = path.match(/^\/internal\/v1\/builds\/([^/]+)\/logs$/);
  if (method === "GET" && logsMatch) {
    const id = decodeURIComponent(logsMatch[1]);
    const row = state.builds.find((build) => build.id === id);
    if (!row) {
      return sendJSON(res, 404, { error: { code: "build_not_found", message: "Build not found" } });
    }
    return sendJSON(res, 200, { logs: row.logs, logs_ref: row.logs_ref });
  }

  if (method === "GET" && path === "/internal/v1/secrets/bindings") {
    const deploymentID = url.searchParams.get("deployment_id") || "";
    const rows = state.secretBindings.filter((binding) => binding.deployment_id === deploymentID);
    return sendJSON(res, 200, rows);
  }

  if (method === "POST" && path === "/internal/v1/secrets/bind") {
    const body = await readJSON(req);
    const row = {
      id: `bind_${state.counters.binding++}`,
      deployment_id: body.deployment_id,
      secret_name: body.secret_name,
      target_key: body.target_key,
      created_at: "2026-02-20T08:03:00Z"
    };
    state.secretBindings.unshift(row);
    return sendJSON(res, 200, row);
  }

  const bindingMatch = path.match(/^\/internal\/v1\/secrets\/bindings\/([^/]+)$/);
  if (method === "DELETE" && bindingMatch) {
    const id = decodeURIComponent(bindingMatch[1]);
    state.secretBindings = state.secretBindings.filter((binding) => binding.id !== id);
    return sendJSON(res, 200, { ok: true });
  }

  if (method === "POST" && path === "/internal/v1/policies/runtime") {
    const body = await readJSON(req);
    state.deployments = state.deployments.map((dep) =>
      dep.id === body.deployment_id
        ? {
            ...dep,
            runtime_profile: body.runtime_profile || dep.runtime_profile,
            mode: body.mode || dep.mode,
            updated_at: "2026-02-20T08:04:00Z"
          }
        : dep
    );
    return sendJSON(res, 200, { ok: true });
  }

  if (method === "GET" && path === "/internal/v1/api-keys") {
    return sendJSON(res, 200, state.apiKeys);
  }

  if (method === "POST" && path === "/internal/v1/api-keys") {
    const body = await readJSON(req);
    const id = `key_${state.counters.key++}`;
    const row = {
      id,
      project_id: body.project_id || "proj_default",
      name: body.name || "portal",
      created_at: "2026-02-20T08:05:00Z",
      revoked_at: null
    };
    state.apiKeys.unshift(row);
    return sendJSON(res, 200, { key: `sk_test_${id}` });
  }

  const keyMatch = path.match(/^\/internal\/v1\/api-keys\/([^/]+)\/revoke$/);
  if (method === "POST" && keyMatch) {
    const id = decodeURIComponent(keyMatch[1]);
    state.apiKeys = state.apiKeys.map((item) =>
      item.id === id
        ? {
            ...item,
            revoked_at: "2026-02-20T08:06:00Z"
          }
        : item
    );
    return sendJSON(res, 200, { ok: true });
  }

  return sendJSON(res, 404, { error: { code: "control_not_found", message: `${method} ${path} not found` } });
}

async function handleData(method, path, url, req, res) {
  if (method === "GET" && path === "/api/v1/assistants") {
    return sendJSON(res, 200, state.assistants);
  }

  if (method === "GET" && path === "/api/v1/threads") {
    return sendJSON(res, 200, state.threads);
  }

  if (method === "GET" && path === "/api/v1/system/health") {
    return sendJSON(res, 200, { status: "ok" });
  }

  if (method === "GET" && path === "/api/v1/system/attention") {
    const pending_runs = state.runs.filter((run) => run.status === "pending").length;
    const error_runs = state.runs.filter((run) => run.status === "error").length;
    const stuck_runs = state.runs.filter((run) => run.status === "stuck").length;
    return sendJSON(res, 200, {
      stuck_runs,
      pending_runs,
      error_runs,
      webhook_dead_letters: 0,
      stuck_threshold_seconds: 300
    });
  }

  const threadRunStreamMatch = path.match(/^\/api\/v1\/threads\/([^/]+)\/runs\/stream$/);
  if (method === "POST" && threadRunStreamMatch) {
    const threadID = decodeURIComponent(threadRunStreamMatch[1]);
    const thread = state.threads.find((item) => item.id === threadID);
    const assistantID = thread?.assistant_id || "asst_default";
    return sendSSE(res, makeRunStream(threadID, assistantID));
  }

  if (method === "POST" && path === "/api/v1/runs/stream") {
    const body = await readJSON(req);
    return sendSSE(res, makeRunStream("", body.assistant_id || "asst_default"));
  }

  const reconnectThreadMatch = path.match(/^\/api\/v1\/threads\/([^/]+)\/runs\/([^/]+)\/stream$/);
  if (method === "GET" && reconnectThreadMatch) {
    const runID = decodeURIComponent(reconnectThreadMatch[2]);
    return sendSSE(res, makeReconnectStream(runID, req.headers["last-event-id"]));
  }

  const reconnectRunMatch = path.match(/^\/api\/v1\/runs\/([^/]+)\/stream$/);
  if (method === "GET" && reconnectRunMatch) {
    const runID = decodeURIComponent(reconnectRunMatch[1]);
    return sendSSE(res, makeReconnectStream(runID, req.headers["last-event-id"]));
  }

  const cancelThreadMatch = path.match(/^\/api\/v1\/threads\/([^/]+)\/runs\/([^/]+)\/cancel$/);
  if (method === "POST" && cancelThreadMatch) {
    const action = url.searchParams.get("action") || "interrupt";
    return sendJSON(res, 200, { action });
  }

  const cancelRunMatch = path.match(/^\/api\/v1\/runs\/([^/]+)\/cancel$/);
  if (method === "POST" && cancelRunMatch) {
    const action = url.searchParams.get("action") || "interrupt";
    return sendJSON(res, 200, { action });
  }

  const deleteThreadRunMatch = path.match(/^\/api\/v1\/threads\/([^/]+)\/runs\/([^/]+)$/);
  if (method === "DELETE" && deleteThreadRunMatch) {
    const runID = decodeURIComponent(deleteThreadRunMatch[2]);
    state.runs = state.runs.filter((run) => run.id !== runID);
    return sendJSON(res, 200, { ok: true });
  }

  const deleteRunMatch = path.match(/^\/api\/v1\/runs\/([^/]+)$/);
  if (method === "DELETE" && deleteRunMatch) {
    const runID = decodeURIComponent(deleteRunMatch[1]);
    state.runs = state.runs.filter((run) => run.id !== runID);
    return sendJSON(res, 200, { ok: true });
  }

  return sendJSON(res, 404, { error: { code: "data_not_found", message: `${method} ${path} not found` } });
}

function makeRunStream(threadID, assistantID) {
  const runID = `run_mock_${state.counters.run++}`;
  state.runs.unshift({
    id: runID,
    thread_id: threadID,
    assistant_id: assistantID,
    status: "success",
    created_at: "2026-02-20T08:07:00Z",
    updated_at: "2026-02-20T08:07:10Z"
  });
  return [
    `id: 1\nevent: run_queued\ndata: ${JSON.stringify({ run_id: runID, status: "queued" })}\n`,
    `id: 2\nevent: token\ndata: ${JSON.stringify({ text: "mock token stream" })}\n`,
    `id: 3\nevent: run_completed\ndata: ${JSON.stringify({ run_id: runID, status: "success" })}\n`
  ].join("\n");
}

function makeReconnectStream(runID, lastEventID) {
  const startID = Number.parseInt(String(lastEventID || "-1"), 10);
  if (Number.isFinite(startID) && startID >= 3) {
    return "";
  }
  return [
    `id: 3\nevent: run_completed\ndata: ${JSON.stringify({ run_id: runID, status: "success" })}\n`
  ].join("\n");
}

function sendJSON(res, status, body) {
  res.statusCode = status;
  res.setHeader("content-type", "application/json");
  res.end(JSON.stringify(body));
}

function sendSSE(res, payload) {
  res.statusCode = 200;
  res.setHeader("content-type", "text/event-stream; charset=utf-8");
  res.setHeader("cache-control", "no-cache");
  res.end(payload);
}

async function readJSON(req) {
  const chunks = [];
  for await (const chunk of req) {
    chunks.push(chunk);
  }
  if (chunks.length === 0) {
    return {};
  }
  const raw = Buffer.concat(chunks).toString("utf8");
  if (!raw) {
    return {};
  }
  try {
    return JSON.parse(raw);
  } catch {
    return {};
  }
}
