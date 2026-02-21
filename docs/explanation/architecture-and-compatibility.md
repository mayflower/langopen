# Explanation: Architecture and compatibility model

## System planes

LangOpen is intentionally split into operational planes so compatibility and isolation can evolve independently.

- Control plane: deployment, build, and policy orchestration (`/services/control-plane`, `/services/builder`, `/services/operator`).
- Data plane: request handling, run state, queue processing, and stream/cancel semantics (`/services/api-server`, `/services/worker`).
- Sandbox/runtime plane: execution runtime path (`/services/runtime-runner`) and Kubernetes runtime isolation choices (gVisor/Kata via Helm/runtime classes).
- Observability plane: metrics/log/trace endpoints and dashboards configured through Helm and cluster tooling.
- Portal plane: user-facing interface in `/portal` for platform workflows.

This split keeps user-facing API behavior stable while allowing infrastructure internals to change safely.

## Request and run lifecycle

A typical threaded run follows this flow:

1. Client calls the API server thread/run endpoint.
2. API server persists state and pushes a wake-up signal into Redis using configured queue/prefix keys.
3. Worker consumes wake-up events, resolves run details from Postgres, and executes via runtime mode.
4. Runtime emits events and outputs; worker updates run status and publishes stream events.
5. API server forwards stream events (including resume-aware flow using buffered event IDs where configured).
6. Cancel requests propagate through cancel key/channel semantics and are applied by the worker.

This design separates API request latency from execution latency while preserving stream semantics expected by LangGraph-compatible clients.

## Why compatibility is validated this way

Compatibility is checked on two axes because no single check catches all regressions:

- Contract parity: `/scripts/check_openapi_parity.py` compares local and upstream path/method coverage.
- Behavioral smoke validation: `/scripts/cluster-smoke.sh`, `/scripts/parity-smoke.sh`, and `/scripts/agent-compat-smoke.sh` exercise run, stream, cancel, and deployment behavior in real environments.

Contract parity catches schema drift; smoke checks catch runtime and integration drift.

## Security and runtime isolation model

LangOpen is built to run with hardened runtime defaults while still supporting iterative development:

- Runtime class defaults in Helm target gVisor-backed execution for major components.
- Worker runtime mode delegates agent execution to runtime-runner, enabling tighter execution boundaries.
- Network policy templates support deny-by-default style egress with controlled HTTPS allowances for runtime/build components.
- Operator and deployment controls provide a path toward stricter sandbox profiles (including Kata-based policies) without changing public APIs.

The intent is secure-by-default operation with explicit knobs for stricter isolation and operational tradeoffs.
