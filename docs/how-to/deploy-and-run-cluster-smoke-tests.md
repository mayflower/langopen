# How-to: Deploy and run cluster smoke tests

## When to use this guide

Use this when you need to publish a new image set to your cluster environment and verify platform behavior with the existing smoke suites.

## Prerequisites

- `kubectl` and `jq` installed locally.
- Access to push images to the configured registry.
- Cluster access and namespace permissions.
- Runtime secret values set in `secret/langopen-runtime-secrets` (at minimum `BOOTSTRAP_API_KEY`; provider keys required for compatibility smoke).

## Build and push images

From repository root:

```bash
./scripts/build-and-push.sh
```

This builds and pushes all component images and prints the tag to apply in your deployment values.

## Apply GitOps and Argo changes

Use the cluster flow in [/deploy/cluster/README.md](/deploy/cluster/README.md) for the exact data-lan-mayflower sequence.

At minimum:

1. Update image tags in GitOps value files.
2. Apply the ArgoCD app manifest for LangOpen.
3. Wait for deployments to become ready before running smoke tests.

## Run smoke suites

Set kubeconfig if needed:

```bash
export KUBECONFIG=~/.kube/data-lan-mayflower
```

Run the suites from least to most exhaustive:

1. Base cluster smoke:

```bash
./scripts/cluster-smoke.sh
```

2. API parity smoke:

```bash
./scripts/parity-smoke.sh
```

3. Agent compatibility smoke:

```bash
./scripts/agent-compat-smoke.sh
```

## Troubleshooting

- Missing secrets: if smoke reports missing `BOOTSTRAP_API_KEY` or provider keys, populate `langopen-runtime-secrets` in the target namespace.
- Service name mismatch: verify Helm release naming and service defaults (`langopen-api-server`, `langopen-control-plane`) before rerunning.
- Executor mode mismatch: `parity-smoke` expects worker `LANGOPEN_EXECUTOR=runtime`; confirm deployment env values and redeploy if needed.
