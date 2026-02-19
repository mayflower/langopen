# Cluster Test Flow (data-lan-mayflower)

1. Build and push images:

```bash
./scripts/build-and-push.sh
```

2. Update image tag in:

- `deploy/helm/values-data-muc.yaml`
- `../data-cluster/langopen/values.yaml`

3. Ensure SOPS values are set in:

- `../data-cluster/langopen/charts/pre-setup/values.sops.yaml`

4. Apply ArgoCD app (after committing changes in `../argocd` + `../data-cluster`):

```bash
export KUBECONFIG=~/.kube/data-lan-mayflower
kubectl -n argocd apply -f ../argocd/data-muc/demo-apps/langopen.yaml
```

5. Run smoke test:

```bash
./scripts/cluster-smoke.sh
```

The smoke script runs all API checks from an ephemeral pod inside the cluster network (no local `port-forward` dependency).
