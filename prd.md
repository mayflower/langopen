# PRD: Open, Kubernetes-Native LangGraph Platform Replacement

**Document status:** Draft (implementation-ready)
**Version:** 0.9
**Date:** 2026-02-16
**Audience:** Engineering (Codex CLI), Product, DevOps/SRE, Security
**Goal:** Specify a fully functional, plug-and-replace, open-source platform compatible with the commercial **LangGraph Platform / LangSmith Agent Server** APIs and deployment workflow.

---

## 1. Summary

Build an open-source, Kubernetes-native platform that:

1. Is a **drop-in replacement** for the commercial LangGraph Platform runtime from a client/API perspective (LangGraph SDK compatibility, same endpoint behavior, SSE streaming + resume, cancel semantics, A2A and MCP endpoints, `/docs` OpenAPI UI). ([docs.langchain.com][1])
2. Uses **Kubernetes Agent Sandbox** as the secure execution layer for agent code, backed by **gVisor** and **Kata Containers** runtimes. ([Agent Sandbox][2])
3. Uses **Grafana Stack** (Prometheus + Loki + Tempo + Alerting) as the operator observability backend, but provides an **own user-facing frontend** with **OpenAuth** (OAuth2/OIDC) login. ([OpenAuth][3])
4. Implements the same “upload/deploy” strategy as the original: **pull from Git**, require **`langgraph.json`** + **`requirements.txt`**, build immutable images, deploy. ([docs.langchain.com][4])

---

## 2. Goals

### 2.1 Product goals

* **Compatibility:** Existing LangGraph SDK clients can switch `base_url` to this platform without code changes (or with minimal auth header config).
* **Security-first execution:** All untrusted agent execution happens inside hardened sandboxes (gVisor/Kata) with strict NetworkPolicies and least privilege.
* **Self-service deployment:** Users can deploy from Git using `langgraph.json` + `requirements.txt` with a “build → deploy → run” workflow.
* **Operational clarity:** Operators can see “what is happening now” (running/pending/stuck runs, failing deployments, webhook failures, sandbox pool depletion) and get alerts.
* **Multi-tenancy:** Organizations/Projects are first-class; RBAC is enforced for portal and API usage.
* **Kubernetes-native:** Installable via Helm; optional Kubernetes Operator for reconciliation.

### 2.2 Success metrics (MVP)

* **API parity:** ≥ 95% of endpoints and fields covered by automated conformance tests; critical endpoints 100% (Runs, Threads, Streaming, Cancel, Assistants, Store, Crons, `/docs`). ([docs.langchain.com][1])
* **SSE streaming correctness:** Resume works via `Last-Event-ID` when `stream_resumable=true`. ([docs.langchain.com][5])
* **Sandbox isolation:** 100% of run execution occurs in pods with `runtimeClassName` set to `gvisor` or `kata-*` according to policy.
* **Build reliability:** ≥ 99% successful builds for valid repos; clear diagnostics for invalid `langgraph.json` / missing dependencies.

---

## 3. Non-goals (MVP)

* Reimplementing the full commercial **LangSmith Studio** UI feature set (we provide our own portal + Grafana for ops; optional deep links to logs/traces).
* Providing an LLM provider marketplace or managed secrets vault beyond Kubernetes Secrets integration.
* Supporting every possible dependency format initially (MVP: `requirements.txt` only; `pyproject.toml` is a planned extension).

---

## 4. Personas & user stories

### 4.1 Personas

1. **App Developer (User):** Deploys and iterates on agents; needs Git-based build and easy run monitoring.
2. **Platform Operator (SRE):** Runs the platform; needs dashboards, alerts, debug tooling, and predictable resource usage.
3. **Security Engineer:** Needs isolation, network egress controls, supply chain integrity, and audit logs.
4. **Admin:** Manages orgs/projects/users, quotas, and policies.

### 4.2 Core user stories

* As a developer, I connect a Git repo and deploy an agent by providing `langgraph.json` + `requirements.txt`. ([docs.langchain.com][6])
* As a developer, I can run threads and stream tokens/events live; if my connection drops I can resume the stream. ([docs.langchain.com][7])
* As a developer, I can cancel a run with `interrupt` or `rollback`. ([docs.langchain.com][8])
* As an operator, I can see backlog, stuck runs, sandbox pool health, webhook failures, and DB/Redis health in Grafana.
* As a security engineer, I can enforce that all execution uses gVisor by default and Kata for high-security workloads, with deny-by-default egress.

---

## 5. Product scope

### 5.1 Components

**Control Plane**

* User Portal (web UI)
* Auth (OpenAuth OAuth2/OIDC)
* Git integration + deployment objects
* Build service (in-cluster builds)
* Deployment reconciler (Operator/controller)
* Policy engine (sandbox/runtime/network/quota)

**Data Plane**

* Agent Server API (LangGraph Platform compatible)
* Queue worker(s)
* Postgres (durable state)
* Redis (wakeups, pubsub streaming, cancel signaling, resumable stream buffer) ([docs.langchain.com][9])

**Sandbox Plane**

* Kubernetes Agent Sandbox CRDs (Sandbox, SandboxTemplate, SandboxClaim, SandboxWarmPool) ([GitHub][10])
* RuntimeClasses: `gvisor`, `kata-*` (cluster-configurable)

**Observability Plane**

* Prometheus (metrics)
* Loki (logs)
* Tempo (traces)
* Grafana (dashboards, alerting)
* Optional: OpenTelemetry collector

---

## 6. Compatibility requirements (Plug-and-replace)

### 6.1 External API parity

The platform MUST expose an Agent Server API with:

* `/docs` OpenAPI UI (per deployment) and `/openapi.json` ([docs.langchain.com][1])
* Endpoints for Assistants, Threads, Thread Runs, Stateless Runs, Store, Crons, A2A, MCP, System endpoints ([docs.langchain.com][1])
* SSE streaming endpoints and behavior:

  * Create+Stream and Join-Stream
  * Resume via `Last-Event-ID` when `stream_resumable=true`
  * `Last-Event-ID: -1` to replay all events ([docs.langchain.com][5])

### 6.2 Required run semantics

* Run status values: `pending`, `running`, `error`, `success`, `timeout`, `interrupted` ([docs.langchain.com][11])
* Concurrency strategy on same thread: `reject`, `rollback`, `interrupt`, `enqueue` ([docs.langchain.com][11])
* Cancel semantics:

  * `action=interrupt|rollback`
  * rollback deletes run + checkpoints ([docs.langchain.com][8])

### 6.3 Redis “data plane” semantics

Redis MUST be used as described (for drop-in behavioral compatibility):

1. **Redis list** only stores a sentinel “wake up” value; run details are fetched from Postgres by workers. ([docs.langchain.com][9])
2. **Cancel signaling:** Redis string + Redis PubSub channel to notify the correct worker. ([docs.langchain.com][9])
3. **Streaming output:** Redis PubSub channel; server subscribes and forwards to SSE; events are not persisted in Redis unless resumable stream is enabled. ([docs.langchain.com][9])

---

## 7. Deployment workflow requirements (Git → langgraph.json + requirements.txt)

### 7.1 Repository requirements (MVP)

* Repo MUST contain:

  * `langgraph.json` (path configurable; default repo root) ([docs.langchain.com][4])
  * A dependency directory specified in `langgraph.json.dependencies` that contains `requirements.txt` (typically `"./"` for repo root). ([docs.langchain.com][6])

### 7.2 `langgraph.json` interpretation (MVP subset)

* Must parse at minimum:

  * `graphs`: map `graph_id -> "path/to/file.py:graph_var_or_factory"` ([docs.langchain.com][6])
  * `dependencies`: list of dependency sources (directory paths supported) ([docs.langchain.com][6])
  * `env`: optional `.env` path OR inline env map (portal should prefer secrets integration)
* Validation must fail fast with actionable errors:

  * Missing file, invalid JSON, missing graphs, invalid graph import path.

### 7.3 Build pipeline requirements

* Build MUST be executed inside sandboxed build pods (prefer Kata for builds).
* Builds produce immutable images tagged by commit SHA and stored by digest.
* Build logs are persisted and searchable (Loki labels include org/project/deployment/build_id).

### 7.4 Deployment rollout

* Each deployment results in:

  * Data-plane API Service (Agent Server)
  * Worker pool
  * Optional warm pool of sandboxes for low latency (SandboxWarmPool) ([GitHub][10])
* Rollback: ability to roll back to previous image digest.

---

## 8. Execution isolation requirements (Agent Sandbox + gVisor/Kata)

### 8.1 Supported runtime profiles

* **Default profile:** gVisor
* **High security profile:** Kata (e.g., `kata-qemu`) ([GitHub][12])

### 8.2 Agent Sandbox integration

The platform MUST support Agent Sandbox CRDs:

* `SandboxTemplate` for standard execution templates
* `SandboxClaim` to allocate sandboxes per run/thread
* `SandboxWarmPool` for pre-warmed sandboxes ([GitHub][10])

### 8.3 Execution modes

Support two execution modes (configurable per deployment/assistant):

**Mode A: Worker-in-sandbox (default MVP)**

* Worker pods run agent code directly, but the pods themselves are in gVisor/Kata runtimeClass.

**Mode B: Run-in-sandbox (advanced, planned but spec’d now)**

* Worker orchestrates; each run executes in a dedicated Sandbox allocated from a WarmPool.
* Enables per-thread persistent volume and stable runtime identity.

### 8.4 Network security defaults

* Deny-by-default egress for sandboxes, with explicit allowlist (LLM provider endpoints, Git, artifact registry).
* No Kubernetes API access from sandbox workloads (disable SA token mount unless explicitly needed).
* Enforce Pod Security Standards: non-root, drop caps, readOnlyRootFS (where feasible).

---

## 9. Observability requirements (Grafana Stack + own Portal)

### 9.1 Operator observability (Grafana)

Expose:

* Prometheus metrics (API latency, run queue depth, worker concurrency, sandbox pool health)
* Loki logs (API, worker, build, sandbox runtime logs)
* Tempo traces (OTEL instrumentation across API → worker → sandbox runner)

### 9.2 User-facing Portal (custom frontend)

Portal provides curated operational and developer UX:

**Core views**

* Deployments: status, versions, commit SHAs, image digests, runtime profile (gVisor/Kata)
* Assistants: versions, config, graph schema (where available)
* Threads: list, state snapshots, history
* Runs: live running list, status timeline, stream viewer (SSE)
* “Needs attention” inbox:

  * stuck runs (running > SLA)
  * failing builds
  * failing webhooks
  * sandbox warm pool depleted
  * elevated error rate

**Deep links**

* “Open logs” (Grafana Explore with Loki query prefilled)
* “Open trace” (Tempo trace view)
* “Open metrics panel” (Grafana dashboard filtered)

### 9.3 Correlation model (mandatory)

All logs/metrics/traces must carry:

* `org_id`, `project_id`, `deployment_id`, `assistant_id`, `thread_id`, `run_id`, `sandbox_id`, `build_id`

---

## 10. Authentication & authorization requirements

### 10.1 Portal login

* Use OpenAuth for OAuth2/OIDC login flows (Authorization Code + PKCE).
* Support common upstream IdPs via OIDC/OAuth2 provider config. ([OpenAuth][3])

### 10.2 API auth (data plane)

* MUST support API key authentication compatible with commercial usage patterns:

  * `X-Api-Key` accepted for Agent Server requests ([docs.langchain.com][1])
* MUST support optional `X-Auth-Scheme: langsmith-api-key` for compatibility with Agent Builder / PAT flows. ([docs.langchain.com][13])
* For self-hosted, allow custom auth middleware and RBAC mapping (org/project scoping). ([docs.langchain.com][14])

### 10.3 RBAC

Roles per project:

* `viewer`: read-only (runs, logs links)
* `developer`: deploy/build, run agents
* `operator`: manage quotas/policies, view all metrics
* `admin`: org management, identity, billing/quota (if any)

---

## 11. Functional requirements by subsystem

### 11.1 Control Plane API (internal)

Provide an internal API (not public LangGraph API) for portal actions:

* Git repo connection management (OAuth app / GitHub App / deploy key)
* Deployment creation and updates
* Build triggers, build logs, build artifacts
* Policy configuration (runtime profile, egress allowlist)
* Secrets binding (Kubernetes Secret references)
* Audit log events

### 11.2 Builder

* Inputs: repo URL, ref (branch/tag/SHA), path, build config
* Outputs: image digest, SBOM (optional), provenance (optional)
* Security:

  * sandboxed build environment
  * no privileged pods
  * restricted egress
* Caching:

  * dependency cache by hash of `requirements.txt`
  * image layer cache if available

### 11.3 Deployer / Operator (reconciler)

* Watches `AgentDeployment` CRD (recommended) or internal DB records.
* Ensures desired state:

  * API deployment + service + ingress
  * Worker deployment(s)
  * Agent Sandbox templates/pools if enabled
  * NetworkPolicies
  * HPA resources (optional)

### 11.4 Data Plane: Agent Server

* Implements external endpoints and semantics described in Sections 6–7
* Must provide `/docs` OpenAPI and match the referenced API groups ([docs.langchain.com][1])
* Must support “stream resumable” with TTL backing store when enabled ([docs.langchain.com][5])

### 11.5 Queue Worker

* Fetches runnable runs from Postgres; uses Redis wakeups + pubsub for cancel/streaming ([docs.langchain.com][9])
* Enforces per-thread single concurrency plus `multitask_strategy` behavior ([docs.langchain.com][11])
* Emits:

  * streaming events (redis pubsub)
  * structured logs
  * traces

### 11.6 A2A endpoint

* Provide `/a2a/{assistant_id}` JSON-RPC 2.0 with methods:

  * `message/send`, `message/stream` (SSE), `tasks/get`, `tasks/cancel` (returns error if unsupported) ([docs.langchain.com][15])
* Map `message.contextId` ↔ `thread_id` ([docs.langchain.com][15])

### 11.7 MCP endpoint

* Provide `/mcp` implementing Streamable HTTP transport
* Must be stateless; “terminate session” is a no-op (for compatibility) ([docs.langchain.com][16])
* Must align with Streamable HTTP transport rules (headers, optional session IDs) ([modelcontextprotocol.io][17])

---

## 12. Data model (minimum viable)

### 12.1 Core entities

* **Organization**
* **Project**
* **User**
* **Deployment**

  * git repo/ref/path
  * build config
  * runtime policy (gVisor/Kata)
  * network policy profile
  * current image digest
* **Build**

  * deployment_id, status, logs pointer, image digest, timestamps
* **Assistant**

  * deployment_id, graph_id, config, version(s)
* **Thread**

  * assistant_id, metadata, TTL policy, created_at
* **Run**

  * thread_id (nullable), assistant_id, status, multitask_strategy, timestamps, webhook, stream_resumable flag
* **Checkpoint**

  * run_id/thread_id, state snapshot, created_at
* **StoreItem**

  * namespace, key, value, metadata
* **WebhookDelivery**

  * run_id, url, status, attempts, last_error

### 12.2 Required indexes

* Runs by status + created_at (queue scanning)
* Runs by thread_id + status (single concurrency)
* Threads by updated_at (UX)
* Deployments by project_id

---

## 13. UX requirements (Portal)

### 13.1 IA / Navigation

* Projects

  * Deployments

    * Overview
    * Builds
    * Assistants
    * Runs (live + history)
    * Threads
    * Settings (runtime profile, env/secrets, webhooks)
  * Attention (inbox)

### 13.2 Run viewer

* Live SSE event stream with:

  * reconnect + resume (if enabled)
  * copyable `run_id`, `thread_id`, `assistant_id`
  * “Cancel” button with interrupt/rollback

### 13.3 Deployment wizard

* Connect Git
* Select ref + path
* Detect `langgraph.json`
* Validate graphs & dependencies
* Trigger build + show logs
* Deploy + show endpoints and sample curl/SDK snippet

---

## 14. SRE / Operations requirements

### 14.1 Installation

* Helm charts:

  * platform core (control plane + data plane)
  * optional: builder
  * optional: operator
  * observability stack (or integrate with existing Grafana stack)

### 14.2 Scaling

* API deployment: scale by CPU/RPS
* Worker pool: scale by backlog depth (pending runs count) and CPU
* Sandbox warm pools: maintain configured buffer per deployment

### 14.3 Backup & retention

* Postgres backups required
* Configurable retention:

  * runs, checkpoints, logs references, build logs
* TTL for resumable streaming buffers

---

## 15. Security requirements

### 15.1 Isolation

* Enforce `runtimeClassName` for sandboxed execution
* Default to gVisor; allow Kata where nodes support it ([GitHub][12])

### 15.2 Supply chain

* Pin base images
* Record image digests
* Optional SBOM generation and vulnerability scan integration

### 15.3 Secrets

* Secrets must not be stored in repo
* Support binding Kubernetes Secrets to deployments (env injection)

### 15.4 Audit logging

* Record:

  * logins, deployments, builds, policy changes
  * run cancellations, webhook configuration changes

---

## 16. Acceptance criteria (Definition of Done)

### 16.1 Compatibility test suite

Automated conformance tests must validate:

* `/docs` and OpenAPI availability ([docs.langchain.com][1])
* Runs/Threads/Assistants CRUD and behavior
* Streaming correctness and resume:

  * `stream_resumable=true` + `Last-Event-ID` resume + `-1` replay ([docs.langchain.com][5])
* Cancel semantics interrupt vs rollback ([docs.langchain.com][8])
* Redis signaling behavior (wakeup sentinel, cancel, pubsub streaming) ([docs.langchain.com][9])
* A2A methods and mapping rules ([docs.langchain.com][15])
* MCP endpoint exists and is stateless; terminate session no-op ([docs.langchain.com][16])

### 16.2 Security checks

* Run execution pods always have `runtimeClassName` set (policy enforced)
* NetworkPolicies block egress by default
* Builder pods are non-privileged and sandboxed

### 16.3 Operational checks

* Grafana dashboards load and show core SLO indicators
* Alerts fire for:

  * stuck runs
  * build failures
  * webhook delivery failures
  * warm pool depletion
  * DB/Redis connectivity issues

---

## 17. Milestones (suggested)

### Milestone 1 — MVP “Compatible Data Plane”

* Agent Server endpoints: Assistants/Threads/Runs/Store/System
* SSE streaming + resume
* Cancel semantics
* Postgres + Redis data plane wiring

### Milestone 2 — Git Build & Deploy

* Portal + OpenAuth login
* Git integration
* Build service producing images from `langgraph.json` + `requirements.txt`
* Deployment reconciler + runtimeClass selection (gVisor default)

### Milestone 3 — Agent Sandbox + Warm Pools

* SandboxTemplate/Claim/WarmPool integration
* Kata profile support (node pool)
* Per-deployment sandbox policy & egress allowlists

### Milestone 4 — A2A + MCP + Crons + Webhooks

* A2A JSON-RPC + SSE
* MCP `/mcp` Streamable HTTP
* Cron scheduling
* Webhook outbox + retries

---

## 18. Open questions (engineering decisions to lock early)

1. **Execution mode default:** Worker-in-sandbox (Mode A) only for MVP, or enable Run-in-sandbox (Mode B) early?
2. **Builder tooling:** BuildKit rootless vs Kaniko (both viable; pick one for MVP).
3. **API key model:** Per project vs per user; mapping to RBAC.
4. **Multi-cluster support:** Single cluster only (MVP) vs remote data plane clusters.

---

## 19. References (behavioral contracts and upstream semantics)

* Agent Server API reference availability at `/docs` ([docs.langchain.com][1])
* Create Run (stream output) fields and streaming options ([docs.langchain.com][7])
* Join Run Stream and `Last-Event-ID` semantics (including `-1`) ([docs.langchain.com][5])
* Run status values and `multitask_strategy` options ([docs.langchain.com][11])
* Cancel run semantics (`interrupt` vs `rollback`) ([docs.langchain.com][8])
* LangGraph app structure and deployment config concepts (`langgraph.json`, dependencies, graphs, env) ([docs.langchain.com][4])
* LangSmith data plane: Redis wakeup sentinel, cancel signaling, streaming pubsub ([docs.langchain.com][9])
* Agent Sandbox overview and CRD primitives (Template/Claim/WarmPool) ([Agent Sandbox][2])
* OpenAuth OAuth2/OIDC provider reference ([OpenAuth][3])
* A2A endpoint and supported methods mapping ([docs.langchain.com][18])
* MCP endpoint in Agent Server and stateless note ([docs.langchain.com][16])

[1]: https://docs.langchain.com/langsmith/server-api-ref?utm_source=chatgpt.com "Agent Server API reference for LangSmith Deployment"
[2]: https://agent-sandbox.sigs.k8s.io/?utm_source=chatgpt.com "Agent Sandbox - Kubernetes"
[3]: https://openauth.js.org/docs/provider/oidc/?utm_source=chatgpt.com "OidcProvider"
[4]: https://docs.langchain.com/langsmith/application-structure?utm_source=chatgpt.com "Application structure - Docs by LangChain"
[5]: https://docs.langchain.com/langsmith/agent-server-api/thread-runs/join-run-stream?utm_source=chatgpt.com "Join Run Stream - Docs by LangChain"
[6]: https://docs.langchain.com/langsmith/setup-app-requirements-txt?utm_source=chatgpt.com "How to set up an application with requirements.txt"
[7]: https://docs.langchain.com/langsmith/agent-server-api/thread-runs/create-run-stream-output?utm_source=chatgpt.com "Create Run, Stream Output - Docs by LangChain"
[8]: https://docs.langchain.com/langsmith/agent-server-api/thread-runs/cancel-run?utm_source=chatgpt.com "Cancel Run - Docs by LangChain"
[9]: https://docs.langchain.com/langsmith/data-plane?utm_source=chatgpt.com "LangSmith data plane"
[10]: https://github.com/kubernetes-sigs/agent-sandbox?utm_source=chatgpt.com "kubernetes-sigs/agent-sandbox"
[11]: https://docs.langchain.com/langsmith/agent-server-api/thread-runs/get-run?utm_source=chatgpt.com "Get Run - Docs by LangChain"
[12]: https://github.com/kubernetes-sigs/agent-sandbox/issues/262?utm_source=chatgpt.com "feat: Default runtimeClassName to gvisor · Issue #262"
[13]: https://docs.langchain.com/langsmith/agent-builder-code?utm_source=chatgpt.com "Call agents from code - Docs by LangChain"
[14]: https://docs.langchain.com/langsmith/auth?utm_source=chatgpt.com "Authentication & access control - Docs by LangChain"
[15]: https://docs.langchain.com/langsmith/agent-server-api/a2a/a2a-json-rpc?utm_source=chatgpt.com "A2A JSON-RPC - Docs by LangChain"
[16]: https://docs.langchain.com/langsmith/server-mcp?utm_source=chatgpt.com "MCP endpoint in Agent Server - Docs by LangChain"
[17]: https://modelcontextprotocol.io/specification/2025-06-18/basic/transports?utm_source=chatgpt.com "Transports"
[18]: https://docs.langchain.com/langsmith/server-a2a?utm_source=chatgpt.com "A2A endpoint in Agent Server"

