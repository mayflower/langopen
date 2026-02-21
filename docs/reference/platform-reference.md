# Reference: Platform components and interfaces

## Repository map

| Path | Responsibility |
| --- | --- |
| `/services/api-server` | Data-plane API, runs/threads, SSE stream, `/docs`, `/openapi.json`. |
| `/services/worker` | Queue-driven execution orchestration, cancel semantics, webhook/cron loops. |
| `/services/runtime-runner` | Python execution runtime invoked by worker in runtime executor mode. |
| `/services/control-plane` | Deployment/build/API-key/policy endpoints for platform operations. |
| `/services/builder` | Source validation and build orchestration API surface. |
| `/services/operator` | `AgentDeployment` reconciliation in Kubernetes. |
| `/portal` | Next.js operations portal (auth, proxy routes, UI pages). |
| `/deploy/helm` | Helm chart, templates, and values for all components. |
| `/contracts/openapi` | Local and upstream API contract artifacts for parity checks. |

## Kubernetes service defaults

| Component | Service name (Helm release `langopen`) | Container port | Key source |
| --- | --- | --- | --- |
| API server | `langopen-api-server` | `8080` | `/services/api-server/cmd/api-server/main.go` |
| Control plane | `langopen-control-plane` | `8081` | `/services/control-plane/cmd/control-plane/main.go` |
| Builder | `langopen-builder` | `8082` | `/services/builder/cmd/builder/main.go` |
| Runtime runner | `langopen-runtime-runner` | `8083` | `/services/runtime-runner/Dockerfile` |
| Portal | `langopen-portal` | `3000` | `/portal/Dockerfile` |
| Worker metrics | worker pod endpoint | `9091` | `/services/worker/cmd/worker/main.go` |

## Portal routes and user pages

| Path | Purpose | Backend dependency |
| --- | --- | --- |
| `/` | Control-plane summary dashboard. | Control + data APIs |
| `/deployments` | Deployment CRUD, source validation, build trigger, secrets, runtime policy. | Control API |
| `/builds` | Build status and logs view. | Control API |
| `/api-keys` | Project API key creation and revocation. | Control API |
| `/assistants` | Assistant listing and details. | Data API |
| `/threads` | Thread inspection and handoff to runs. | Data API |
| `/runs` | Run stream/reconnect/cancel/delete operations. | Data API |
| `/attention` | Ops attention metrics and Grafana deep links. | Data + control APIs |

## Portal auth and proxy environment

| Variable | Required | Purpose |
| --- | --- | --- |
| `OPENAUTH_ISSUER_URL` | Yes | OIDC discovery issuer for login flow. |
| `OPENAUTH_CLIENT_ID` | Yes | Portal OIDC client ID. |
| `OPENAUTH_REDIRECT_URI` | Yes | Callback URI handled by `/api/auth/callback`. |
| `OPENAUTH_CLIENT_SECRET` | Optional | Used when OIDC provider requires confidential client auth. |
| `PORTAL_SESSION_SECRET` | Yes | HMAC signing secret for `langopen_session` cookie. |
| `PLATFORM_API_KEY` | Yes (recommended) | API key used by portal proxy routes to call control/data APIs. |
| `PLATFORM_DATA_API_BASE_URL` | Yes | Base URL for data-plane proxy target. |
| `PLATFORM_CONTROL_API_BASE_URL` | Yes | Base URL for control-plane proxy target. |
| `GRAFANA_*` URLs | Optional | Deep links shown on Attention page. |

## API contracts and parity tooling

- Local JSON contract: [/contracts/openapi/agent-server.json](/contracts/openapi/agent-server.json)
- Local YAML contract: [/contracts/openapi/agent-server.yaml](/contracts/openapi/agent-server.yaml)
- Upstream baseline: [/contracts/openapi/upstream-agent-server-v0.7.5.json](/contracts/openapi/upstream-agent-server-v0.7.5.json)
- Parity checker: [/scripts/check_openapi_parity.py](/scripts/check_openapi_parity.py)

## Operational scripts

| Script | Purpose |
| --- | --- |
| `/scripts/build-and-push.sh` | Build and push all component images. |
| `/scripts/cluster-smoke.sh` | Core in-cluster smoke checks (health, docs, runs, SSE, cancel, control-plane calls). |
| `/scripts/parity-smoke.sh` | Higher-level parity checks across data and control plane flows. |
| `/scripts/agent-compat-smoke.sh` | Compatibility runs against external agent repositories. |
