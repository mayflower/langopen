# Tutorial: Deploy LangOpen on Kubernetes and use the Portal

## What you will build

You will deploy LangOpen into a Kubernetes namespace, expose the Portal, sign in through OpenAuth/OIDC, and perform the first operator workflow from the UI.

## Prerequisites

- A Kubernetes cluster and `kubectl` context.
- `helm` available locally.
- Reachable Postgres and Redis for platform state/queueing.
- Container images available in your registry (`ghcr.io/mayflower/langopen/*` or your fork).
- Portal auth configuration:
  - `OPENAUTH_ISSUER_URL`
  - `OPENAUTH_CLIENT_ID`
  - `OPENAUTH_REDIRECT_URI`
  - `PORTAL_SESSION_SECRET`
- A platform API key for portal proxying (`PLATFORM_API_KEY` or `BOOTSTRAP_API_KEY`).

## Step 1: Prepare Helm values

Start from `/deploy/helm/values.yaml` and create an environment override file.

Set at minimum:

- `env.postgresDsn`
- `env.redisAddr`
- `env.bootstrapApiKey` (or provide via `global.envFromSecret`)
- `portal.env.openauthIssuerUrl`
- `portal.env.openauthClientId`
- `portal.env.openauthRedirectUri`
- `portal.env.portalSessionSecret`
- `portal.env.platformApiKey`

Reference example: `/deploy/helm/values-data-muc.yaml`.

## Step 2: Install or upgrade LangOpen

```bash
helm upgrade --install langopen ./deploy/helm \
  --namespace langopen \
  --create-namespace \
  -f /path/to/your-values.yaml
```

Wait for workloads to become ready:

```bash
kubectl -n langopen get deploy,pod,svc
```

## Step 3: Verify platform services

```bash
kubectl -n langopen get svc langopen-api-server langopen-control-plane langopen-portal
```

Optional API health check via port-forward:

```bash
kubectl -n langopen port-forward svc/langopen-api-server 8080:80
curl -sS http://localhost:8080/healthz
```

## Step 4: Open and authenticate in Portal

Open the portal through ingress or port-forward:

```bash
kubectl -n langopen port-forward svc/langopen-portal 3000:80
```

Visit `http://localhost:3000` and complete the OpenAuth redirect flow (`/api/auth`).

## Step 5: Run the first portal workflow

1. Open **Deployments** and create or select a deployment (`repo_url`, `git_ref`, `repo_path`, runtime profile, mode).
2. Use **Deployments -> Build** (or **Builds**) to trigger and inspect build status/logs.
3. Use **API Keys** to create project-scoped keys for SDK/automation callers.
4. Use **Attention** for operational signals (stuck runs, error runs, dead letters).
5. Use **Threads** and **Runs** to observe and control stream/cancel behavior for existing assistants.

Note: the Runs UI consumes assistants exposed by the data plane. Ensure assistants are present in the target project before expecting run execution success.

## Step 6: Continue with operational validation

Run the smoke suites in [/docs/how-to/deploy-and-run-cluster-smoke-tests.md](/docs/how-to/deploy-and-run-cluster-smoke-tests.md).
