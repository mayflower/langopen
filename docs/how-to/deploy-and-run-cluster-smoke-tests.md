# How-to: Deploy and run cluster smoke tests

## When to use this guide

Use this guide after image updates or Helm changes to verify LangOpen behavior in Kubernetes before promoting changes.

## Prerequisites

- `kubectl` and `jq` installed.
- Access to your container registry.
- Cluster access to the LangOpen namespace.
- Runtime secrets present in `langopen-runtime-secrets` (at least `BOOTSTRAP_API_KEY`; provider keys required for compatibility smoke cases).

## Build and push images

```bash
./scripts/build-and-push.sh
```

Update image tag references in your deployment values after push.

## Apply GitOps and Argo changes

Follow the canonical cluster flow in [/deploy/cluster/README.md](/deploy/cluster/README.md):

1. Update values in GitOps repositories.
2. Apply ArgoCD application manifest.
3. Wait until all LangOpen deployments are healthy.

## Run smoke suites

```bash
export KUBECONFIG=~/.kube/data-lan-mayflower
```

1. Core smoke:

```bash
./scripts/cluster-smoke.sh
```

2. Parity smoke:

```bash
./scripts/parity-smoke.sh
```

3. Agent compatibility smoke:

```bash
./scripts/agent-compat-smoke.sh
```

## Confirm portal health after smoke

- Open **Attention** in Portal and verify no unexpected new failure signals.
- Check **Builds** and **Runs** pages for recently generated errors.

## Troubleshooting

- Missing secret values: ensure required keys exist in `secret/langopen-runtime-secrets` in the target namespace.
- Service mismatch: verify release name and expected services (`langopen-api-server`, `langopen-control-plane`, `langopen-portal`).
- Worker execution mode mismatch: `parity-smoke` expects `LANGOPEN_EXECUTOR=runtime`.
- Portal authentication errors: verify OpenAuth env values and callback URL alignment.
