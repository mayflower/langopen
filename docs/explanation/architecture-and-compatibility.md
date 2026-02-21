# Explanation: Architecture and compatibility model

## Kubernetes-first platform model

LangOpen is designed as a Kubernetes-native control/data/runtime platform, not as a standalone single-process app. Core assumptions in code and deployment artifacts rely on:

- multi-service composition (API server, worker, control plane, builder, runtime-runner, portal),
- Kubernetes networking/service discovery,
- Helm-managed runtime policy and security defaults,
- in-cluster operational checks.

That is why operational documentation should start from cluster deployment and portal access, not local single-binary execution.

## System planes and users

LangOpen separates concerns by plane and by user intent:

- Control plane: deployment lifecycle, builds, API keys, policy changes.
- Data plane: assistants, threads, runs, SSE stream and cancel semantics.
- Runtime plane: worker orchestration and runtime-runner execution.
- Portal plane: operator-facing web workflows and diagnostics.
- Observability plane: metrics/logs/traces and Grafana links.

Portal users are operators and platform engineers coordinating deployments, builds, key rotation, and run diagnostics.

## Request and run lifecycle

A run request crosses multiple components:

1. Client or portal sends run request to API server.
2. API server persists run state and emits queue wake-up signals.
3. Worker consumes pending runs and executes through runtime-runner (runtime executor mode).
4. Runtime-runner resolves graph target and dependencies, executes graph, and returns output/events.
5. Worker updates terminal status and publishes stream events.
6. API server serves live/reconnect stream and cancel handling.

This decomposition keeps API behavior compatible while execution and isolation remain controllable at platform level.

## Why compatibility checks are split

LangOpen validates compatibility through both static and behavioral checks:

- Static contract parity (`check_openapi_parity.py`) catches path/method drift.
- Behavioral smoke suites catch execution, stream, cancel, and deployment regressions in a real cluster.

Both are required because API shape parity does not guarantee runtime semantics.

## Portal as control surface

The portal is not a cosmetic dashboard. It is the primary control surface for platform users:

- Deployments page drives source/build/policy actions.
- Builds page tracks build outcome and logs.
- API Keys page handles project-scoped credential lifecycle.
- Threads/Runs pages provide run operations and stream diagnostics.
- Attention page provides aggregated operational signals and Grafana deep links.

That user journey should be explicit in documentation because it defines day-to-day platform operation.
