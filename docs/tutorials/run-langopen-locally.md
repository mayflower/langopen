# Tutorial: Run LangOpen locally

## What you will build

In this tutorial you will run LangOpen services locally, verify that core endpoints respond, and understand where to continue for deployment and architecture details.

## Prerequisites

- Go 1.25+ with workspace support (`go.work`).
- Node.js and npm for the portal.
- Optional for full end-to-end behavior: reachable Postgres and Redis via `POSTGRES_DSN` and `REDIS_ADDR`.

## Step 1: Run tests

From the repository root:

```bash
go test ./...
```

This validates all Go modules in the workspace before starting services.

## Step 2: Start core services

Use the same startup commands as the root README.

```bash
# Run API server
(cd services/api-server && go run ./cmd/api-server)

# Run control plane
(cd services/control-plane && go run ./cmd/control-plane)

# Run builder
(cd services/builder && go run ./cmd/builder)

# Run worker
(cd services/worker && go run ./cmd/worker)

# Run portal
(cd portal && npm install && npm run dev)
```

Run each command in its own terminal session.

## Step 3: Verify health and docs endpoints

With services running:

1. Check API server health: `curl -sS http://localhost:8080/healthz`
2. Open API docs UI: `http://localhost:8080/docs`
3. Check control plane health: `curl -sS http://localhost:8081/healthz`
4. Check builder health: `curl -sS http://localhost:8082/healthz`
5. Check worker metrics endpoint: `curl -sS http://localhost:9091/metrics | head`

Expected health responses are `ok`.

## Step 4: What to explore next

- For cluster operations and smoke suites, continue with [/docs/how-to/deploy-and-run-cluster-smoke-tests.md](/docs/how-to/deploy-and-run-cluster-smoke-tests.md).
- For lookup details (services, env defaults, contracts), use [/docs/reference/platform-reference.md](/docs/reference/platform-reference.md).
- For architecture and compatibility rationale, read [/docs/explanation/architecture-and-compatibility.md](/docs/explanation/architecture-and-compatibility.md).
