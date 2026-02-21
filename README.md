# LangOpen

LangOpen is a Kubernetes-native, budget-friendly LangGraph-compatible server.

It implements LangGraph-compatible APIs and runtime semantics so teams can run agent infrastructure on their own cluster with lower cost and full control.

## Layout

- `services/api-server`: Data-plane API with SSE, cancel semantics, A2A, MCP endpoints.
- `services/worker`: Queue worker loop with Redis wakeup + cancel signal scaffolding.
- `services/control-plane`: Deployment/build/policy internal APIs.
- `services/builder`: `langgraph.json` + `requirements.txt` validator and BuildKit job spec API.
- `services/operator`: Kubernetes controller for `AgentDeployment`.
- `pkg/contracts`: Shared enums, DTOs, error envelope.
- `pkg/observability`: Request/correlation middleware.
- `portal`: Next.js user portal skeleton.
- `db/migrations`: Postgres schema and required indexes.
- `deploy/helm`: Helm chart scaffold.
- `deploy/k8s/crds`: CRDs for platform + sandbox integration stubs.
- `tests/conformance`: API behavior tests.
- `tests/integration`: Builder and migration integration tests.

## Documentation (Diátaxis)

- Docs index: [`/docs/README.md`](/docs/README.md)
- Tutorial: [`/docs/tutorials/deploy-langopen-on-kubernetes-and-use-portal.md`](/docs/tutorials/deploy-langopen-on-kubernetes-and-use-portal.md)
- How-to: [`/docs/how-to/deploy-and-run-cluster-smoke-tests.md`](/docs/how-to/deploy-and-run-cluster-smoke-tests.md)
- Reference: [`/docs/reference/platform-reference.md`](/docs/reference/platform-reference.md)
- Explanation: [`/docs/explanation/architecture-and-compatibility.md`](/docs/explanation/architecture-and-compatibility.md)

## Developer Entrypoints

LangOpen runtime operation is Kubernetes-first. The commands below are useful for code-level development and debugging, not as a full production-equivalent platform run.

```bash
# Go tests across modules in workspace
go test ./...

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

## Cluster Test (data-lan-mayflower)

```bash
export KUBECONFIG=~/.kube/data-lan-mayflower
./scripts/build-and-push.sh
kubectl -n argocd apply -f ../argocd/data-muc/demo-apps/langopen.yaml
./scripts/cluster-smoke.sh
```

Canonical operator flow: see [`/deploy/cluster/README.md`](/deploy/cluster/README.md) and [`/docs/how-to/deploy-and-run-cluster-smoke-tests.md`](/docs/how-to/deploy-and-run-cluster-smoke-tests.md).

Related GitOps files:

- `../argocd/data-muc/demo-apps/langopen.yaml`
- `../data-cluster/langopen/values.yaml`
- `../data-cluster/langopen/charts/pre-setup/values.sops.yaml`

## License

MIT. See `LICENSE`.
