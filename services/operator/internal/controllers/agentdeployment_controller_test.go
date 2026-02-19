package controllers

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"langopen.dev/operator/internal/apis/v1alpha1"
)

func TestDesiredWorkerDeploymentModeBConfiguresSandboxEnvAndRuntimeClass(t *testing.T) {
	replicas := int32(2)
	dep := &v1alpha1.AgentDeployment{}
	dep.Name = "demo"
	dep.Namespace = "langopen"
	dep.Spec.SandboxTemplate = "demo-template"

	worker := desiredWorkerDeployment(dep, "ghcr.io/mayflower/langopen/worker:test", "kata-qemu", replicas, "mode_b")

	if worker.Spec.Template.Spec.RuntimeClassName == nil || *worker.Spec.Template.Spec.RuntimeClassName != "kata-qemu" {
		t.Fatalf("expected runtimeClassName kata-qemu, got %#v", worker.Spec.Template.Spec.RuntimeClassName)
	}
	if worker.Spec.Template.Spec.AutomountServiceAccountToken == nil || *worker.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Fatalf("expected automountServiceAccountToken=false")
	}
	if got := worker.Spec.Template.Spec.Containers[0].SecurityContext; got == nil || got.RunAsNonRoot == nil || !*got.RunAsNonRoot {
		t.Fatalf("expected non-root container security context")
	}

	env := envMap(worker.Spec.Template.Spec.Containers[0].Env)
	if env["RUN_MODE"] != "mode_b" {
		t.Fatalf("expected RUN_MODE=mode_b, got %q", env["RUN_MODE"])
	}
	if env["SANDBOX_ENABLED"] != "true" {
		t.Fatalf("expected SANDBOX_ENABLED=true, got %q", env["SANDBOX_ENABLED"])
	}
	if env["SANDBOX_TEMPLATE_NAME"] != "demo-template" {
		t.Fatalf("expected SANDBOX_TEMPLATE_NAME=demo-template, got %q", env["SANDBOX_TEMPLATE_NAME"])
	}
	if env["SANDBOX_NAMESPACE"] != "langopen" {
		t.Fatalf("expected SANDBOX_NAMESPACE=langopen, got %q", env["SANDBOX_NAMESPACE"])
	}
}

func TestDesiredWorkerDeploymentModeADoesNotConfigureSandboxEnv(t *testing.T) {
	replicas := int32(1)
	dep := &v1alpha1.AgentDeployment{}
	dep.Name = "demo"
	dep.Namespace = "langopen"

	worker := desiredWorkerDeployment(dep, "ghcr.io/mayflower/langopen/worker:test", "gvisor", replicas, "mode_a")
	env := envMap(worker.Spec.Template.Spec.Containers[0].Env)

	if env["RUN_MODE"] != "mode_a" {
		t.Fatalf("expected RUN_MODE=mode_a, got %q", env["RUN_MODE"])
	}
	if _, ok := env["SANDBOX_ENABLED"]; ok {
		t.Fatalf("did not expect SANDBOX_ENABLED in mode_a")
	}
	if _, ok := env["SANDBOX_TEMPLATE_NAME"]; ok {
		t.Fatalf("did not expect SANDBOX_TEMPLATE_NAME in mode_a")
	}
}

func TestDesiredNetworkPolicySkipsInvalidCIDRs(t *testing.T) {
	dep := &v1alpha1.AgentDeployment{}
	dep.Name = "demo"
	dep.Namespace = "langopen"
	dep.Spec.EgressAllowlist = []string{"10.0.0.0/24", "not-a-cidr", "  "}

	policy := desiredNetworkPolicy(dep)
	if len(policy.Spec.PolicyTypes) != 1 || policy.Spec.PolicyTypes[0] != "Egress" {
		t.Fatalf("expected egress-only network policy, got %#v", policy.Spec.PolicyTypes)
	}
	if len(policy.Spec.Egress) < 2 {
		t.Fatalf("expected dns + valid cidr egress rules, got %d", len(policy.Spec.Egress))
	}

	foundDNS := false
	foundCIDR := false
	for _, rule := range policy.Spec.Egress {
		if len(rule.Ports) >= 2 {
			hasUDP53 := false
			hasTCP53 := false
			for _, port := range rule.Ports {
				if port.Protocol != nil && *port.Protocol == corev1.ProtocolUDP && port.Port != nil && port.Port.IntVal == 53 {
					hasUDP53 = true
				}
				if port.Protocol != nil && *port.Protocol == corev1.ProtocolTCP && port.Port != nil && port.Port.IntVal == 53 {
					hasTCP53 = true
				}
			}
			if hasUDP53 && hasTCP53 {
				foundDNS = true
			}
		}
		for _, to := range rule.To {
			if to.IPBlock != nil && to.IPBlock.CIDR == "10.0.0.0/24" {
				foundCIDR = true
			}
			if to.IPBlock != nil && to.IPBlock.CIDR == "not-a-cidr" {
				t.Fatalf("invalid cidr should have been filtered")
			}
		}
	}
	if !foundDNS {
		t.Fatalf("expected dns egress rule")
	}
	if !foundCIDR {
		t.Fatalf("expected allowlist cidr rule")
	}
}

func envMap(env []corev1.EnvVar) map[string]string {
	out := make(map[string]string, len(env))
	for _, item := range env {
		out[item.Name] = item.Value
	}
	return out
}
