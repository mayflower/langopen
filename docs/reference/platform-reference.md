# Reference: Platform components and interfaces

## Repository map

| Path | Responsibility |
| --- | --- |
| `/services/api-server` | Data-plane API, SSE stream, run/threads endpoints, docs and OpenAPI serving. |
| `/services/worker` | Queue worker, run execution orchestration, cancellation, streaming relay. |
| `/services/control-plane` | Internal deployment/build/policy APIs and integrations. |
| `/services/builder` | Build and source validation APIs for `langgraph.json` and dependency inputs. |
| `/services/operator` | Kubernetes controller for `AgentDeployment` reconciliation. |
| `/services/runtime-runner` | Python runtime executor used by worker runtime mode. |
| `/portal` | Next.js user-facing web portal. |
| `/pkg/contracts` | Shared DTOs, enums, and error contract definitions. |
| `/db/migrations` | Postgres schema and migration files. |
| `/deploy/helm` | Helm chart templates and values for all components. |
| `/tests/conformance` | API behavior conformance tests. |
| `/tests/integration` | Integration tests for builder and Helm/security constraints. |

## Service defaults

| Component | Bind env var (default) | Default bind/port | Key path |
| --- | --- | --- | --- |
| API server | `API_SERVER_ADDR` (`:8080`) | `http://localhost:8080` | `/services/api-server/cmd/api-server/main.go` |
| Control plane | `CONTROL_PLANE_ADDR` (`:8081`) | `http://localhost:8081` | `/services/control-plane/cmd/control-plane/main.go` |
| Builder | `BUILDER_ADDR` (`:8082`) | `http://localhost:8082` | `/services/builder/cmd/builder/main.go` |
| Worker metrics | `WORKER_METRICS_ADDR` (`:9091`) | `http://localhost:9091/metrics` | `/services/worker/cmd/worker/main.go` |
| Runtime runner | container `CMD --port 8083` | `http://localhost:8083` | `/services/runtime-runner/Dockerfile` |
| Operator probes | fixed in manager options | health probe `:8081` | `/services/operator/cmd/operator/main.go` |
| Portal | Next.js start + container `EXPOSE 3000` | `http://localhost:3000` | `/portal/Dockerfile` |

## Helm values and deployment knobs

Use [/deploy/helm/values.yaml](/deploy/helm/values.yaml) as the primary deployment configuration reference.

High-impact knobs include:

- Global image repository/tag and pull policy (`global.image.*`).
- Runtime and sandbox behavior (`worker.executor`, `worker.runMode`, `worker.sandboxEnabled`).
- Component exposure and replica counts (`apiServer`, `controlPlane`, `builder`, `portal`).
- Runtime-runner compatibility profile and Python versions (`runtimeRunner.*`).
- Network policy egress allowances (`networkPolicy.*`).

## API contracts

- Primary local API contract: [/contracts/openapi/agent-server.json](/contracts/openapi/agent-server.json)
- YAML-form contract variant: [/contracts/openapi/agent-server.yaml](/contracts/openapi/agent-server.yaml)
- Upstream comparison baseline: [/contracts/openapi/upstream-agent-server-v0.7.5.json](/contracts/openapi/upstream-agent-server-v0.7.5.json)
- Parity check script: [/scripts/check_openapi_parity.py](/scripts/check_openapi_parity.py)

## Operational scripts

| Script | Purpose |
| --- | --- |
| `/scripts/build-and-push.sh` | Build and push all component container images. |
| `/scripts/cluster-smoke.sh` | Validate health, API behavior, SSE resume, cancel semantics, and control-plane basics in-cluster. |
| `/scripts/parity-smoke.sh` | Validate runtime execution mode plus API/control-plane parity scenarios. |
| `/scripts/agent-compat-smoke.sh` | Validate deployment and execution against multiple external agent repositories. |
| `/scripts/check_openapi_parity.py` | Compare local OpenAPI paths/methods to upstream baseline and emit parity report JSON. |
