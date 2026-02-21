# LangOpen Documentation

LangOpen is a Kubernetes-native platform. This documentation is organized with Diataxis for platform operators and portal users.

## Choose by intent

- Tutorials: follow a full onboarding path in [Deploy LangOpen on Kubernetes and use the Portal](/docs/tutorials/deploy-langopen-on-kubernetes-and-use-portal.md).
- How-to guides: execute operational procedures with [Deploy and run cluster smoke tests](/docs/how-to/deploy-and-run-cluster-smoke-tests.md).
- Reference: look up exact interfaces in [Platform components and interfaces](/docs/reference/platform-reference.md).
- Explanation: understand architecture choices in [Architecture and compatibility model](/docs/explanation/architecture-and-compatibility.md).

## Quick repo pointers

- Project overview and entrypoint: [/README.md](/README.md)
- API contract source: [/contracts/openapi/agent-server.json](/contracts/openapi/agent-server.json)
- Cluster flow runbook: [/deploy/cluster/README.md](/deploy/cluster/README.md)
- Helm chart and values: [/deploy/helm](/deploy/helm)
- Portal implementation: [/portal](/portal)

## Conventions

- Docs are Markdown-first and versioned in this repository.
- Runtime truth lives in source files (services, Helm templates/values, contracts, and scripts).
- For operational behavior, prefer code and scripts over prose if they conflict.
