package integration

import (
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
}
