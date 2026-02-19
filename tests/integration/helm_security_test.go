package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestHelmDefaultsEnforceSecurity(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed")
	}

	root := filepath.Join("..", "..")
	cmd := exec.Command("helm", "template", "langopen", filepath.Join(root, "deploy", "helm"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v output=%s", err, string(out))
	}
	rendered := string(out)

	if !strings.Contains(rendered, "kind: NetworkPolicy") || !strings.Contains(rendered, "default-deny-egress") {
		t.Fatalf("expected default deny egress network policy in rendered chart")
	}
	if strings.Contains(rendered, "automountServiceAccountToken: true") {
		t.Fatalf("security regression: found automountServiceAccountToken=true")
	}

	runtimeClassCount := strings.Count(rendered, "runtimeClassName: gvisor")
	if runtimeClassCount < 4 {
		t.Fatalf("expected runtimeClassName gvisor for runtime pods, got count=%d", runtimeClassCount)
	}
	if !strings.Contains(rendered, "langopen-grafana-dashboard") {
		t.Fatalf("expected grafana dashboard configmap in rendered chart")
	}
}

func TestBuilderHasJobRBAC(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed")
	}

	root := filepath.Join("..", "..")
	cmd := exec.Command("helm", "template", "langopen", filepath.Join(root, "deploy", "helm"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v output=%s", err, string(out))
	}
	rendered := string(out)

	if !strings.Contains(rendered, "kind: Role") || !strings.Contains(rendered, "resources: [\"jobs\"]") {
		t.Fatalf("expected builder job role permissions in rendered chart")
	}
	if !strings.Contains(rendered, "kind: RoleBinding") || !strings.Contains(rendered, "-builder") {
		t.Fatalf("expected builder role binding in rendered chart")
	}
	if !strings.Contains(rendered, "name: WORKER_METRICS_ADDR") || !strings.Contains(rendered, "containerPort: 9091") {
		t.Fatalf("expected worker metrics endpoint wiring in rendered chart")
	}
	if !strings.Contains(rendered, "apiGroups: [\"extensions.agents.x-k8s.io\"]") ||
		!strings.Contains(rendered, "resources: [\"sandboxtemplates\", \"sandboxwarmpools\", \"sandboxclaims\"]") {
		t.Fatalf("expected operator RBAC permissions for sandbox CRDs")
	}
}

func TestObservabilityAlertRulesCoverage(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed")
	}

	root := filepath.Join("..", "..")
	cmd := exec.Command("helm", "template", "langopen", filepath.Join(root, "deploy", "helm"), "--set", "observability.alerts.enabled=true")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v output=%s", err, string(out))
	}
	rendered := string(out)

	requiredAlerts := []string{
		"LangOpenStuckRuns",
		"LangOpenBuildFailures",
		"LangOpenWebhookDeadLetters",
		"LangOpenWarmPoolDepleted",
		"LangOpenBackendUnavailable",
	}
	for _, alertName := range requiredAlerts {
		if !strings.Contains(rendered, "alert: "+alertName) {
			t.Fatalf("missing required alert rule %s", alertName)
		}
	}
}

func TestAgentDeploymentCRDIncludesIngressFields(t *testing.T) {
	root := filepath.Join("..", "..")
	paths := []string{
		filepath.Join(root, "deploy", "k8s", "crds", "agentdeployment.yaml"),
		filepath.Join(root, "deploy", "helm", "crds", "agentdeployment.yaml"),
	}
	required := []string{
		"ingressEnabled:",
		"ingressHost:",
		"ingressClassName:",
		"ingressTLSSecretRef:",
	}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		content := string(raw)
		for _, needle := range required {
			if !strings.Contains(content, needle) {
				t.Fatalf("missing %q in %s", needle, p)
			}
		}
	}
}
