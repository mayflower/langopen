package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	builderapi "langopen.dev/builder/api"
	controlplaneapi "langopen.dev/control-plane/httpapi"
)

func TestValidateLanggraphAndRequirements(t *testing.T) {
	tmp := t.TempDir()
	graphFile := filepath.Join(tmp, "graph.py")
	if err := os.WriteFile(graphFile, []byte("graph = object()\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "requirements.txt"), []byte("langgraph\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	langgraphJSON := map[string]any{
		"graphs":       map[string]string{"default": "graph.py:graph"},
		"dependencies": []string{"."},
	}
	b, _ := json.Marshal(langgraphJSON)
	if err := os.WriteFile(filepath.Join(tmp, "langgraph.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	h := builderapi.NewHandler(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	ts := httptest.NewServer(h)
	defer ts.Close()

	resp := doBuilderReq(t, ts.URL+"/internal/v1/builds/validate", map[string]any{"repo_path": tmp})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
}

func TestBuildKitJobEndpoint(t *testing.T) {
	h := builderapi.NewHandler(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	ts := httptest.NewServer(h)
	defer ts.Close()

	resp := doBuilderReq(t, ts.URL+"/internal/v1/builds/job", map[string]any{
		"repo_url":   "https://github.com/acme/agent",
		"git_ref":    "main",
		"image_name": "ghcr.io/acme/agent",
		"commit_sha": "abcdef1234567890",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(b))
	}
}

func TestBuildTriggerStatusAndLogs(t *testing.T) {
	h := builderapi.NewHandler(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	ts := httptest.NewServer(h)
	defer ts.Close()

	triggerResp := doBuilderReq(t, ts.URL+"/internal/v1/builds/trigger", map[string]any{
		"deployment_id": "dep_1",
		"repo_url":      "https://github.com/acme/agent",
		"git_ref":       "main",
		"image_name":    "ghcr.io/acme/agent",
		"commit_sha":    "abcdef1234567890",
	})
	defer triggerResp.Body.Close()
	if triggerResp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(triggerResp.Body)
		t.Fatalf("expected 202, got %d body=%s", triggerResp.StatusCode, string(b))
	}
	var triggered map[string]any
	if err := json.NewDecoder(triggerResp.Body).Decode(&triggered); err != nil {
		t.Fatal(err)
	}
	buildID, _ := triggered["id"].(string)
	if buildID == "" {
		t.Fatal("missing build id")
	}

	statusReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/internal/v1/builds/"+buildID, nil)
	statusResp, err := http.DefaultClient.Do(statusReq)
	if err != nil {
		t.Fatal(err)
	}
	defer statusResp.Body.Close()
	if statusResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(statusResp.Body)
		t.Fatalf("expected 200 on build status, got %d body=%s", statusResp.StatusCode, string(body))
	}

	logsReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/internal/v1/builds/"+buildID+"/logs", nil)
	logsResp, err := http.DefaultClient.Do(logsReq)
	if err != nil {
		t.Fatal(err)
	}
	defer logsResp.Body.Close()
	if logsResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(logsResp.Body)
		t.Fatalf("expected 200 on build logs, got %d body=%s", logsResp.StatusCode, string(body))
	}
}

func TestControlPlaneDeploymentAndBuildTrigger(t *testing.T) {
	t.Setenv("POSTGRES_DSN", "")

	builderMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/internal/v1/builds/trigger" {
			http.NotFound(w, r)
			return
		}
		writeJSONResp(t, w, http.StatusAccepted, map[string]any{
			"id":            "build_mock_1",
			"deployment_id": "dep_mock_1",
			"status":        "succeeded",
			"commit_sha":    "abc123",
			"image_digest":  "sha256:deadbeef",
			"logs_ref":      "inline://builds/build_mock_1",
		})
	}))
	defer builderMock.Close()
	t.Setenv("BUILDER_URL", builderMock.URL)

	h := controlplaneapi.NewHandler(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	ts := httptest.NewServer(h)
	defer ts.Close()

	createResp := doControlPlaneReq(t, ts.URL+"/internal/v1/deployments", map[string]any{
		"project_id": "proj_default",
		"repo_url":   "https://github.com/acme/agent",
		"git_ref":    "main",
		"repo_path":  ".",
	})
	defer createResp.Body.Close()
	if createResp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(createResp.Body)
		t.Fatalf("expected 201, got %d body=%s", createResp.StatusCode, string(b))
	}
	var dep map[string]any
	if err := json.NewDecoder(createResp.Body).Decode(&dep); err != nil {
		t.Fatal(err)
	}
	depID, _ := dep["id"].(string)
	if depID == "" {
		t.Fatal("missing deployment id")
	}

	buildResp := doControlPlaneReq(t, ts.URL+"/internal/v1/builds", map[string]any{
		"deployment_id": depID,
		"commit_sha":    "abc123",
		"image_name":    "ghcr.io/acme/agent",
		"repo_path":     ".",
	})
	defer buildResp.Body.Close()
	if buildResp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(buildResp.Body)
		t.Fatalf("expected 202, got %d body=%s", buildResp.StatusCode, string(b))
	}
}

func TestMigrationContainsRequiredIndexes(t *testing.T) {
	root := filepath.Join("..", "..")
	raw, err := os.ReadFile(filepath.Join(root, "db", "migrations", "001_init.sql"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	required := []string{
		"idx_runs_status_created_at",
		"idx_runs_thread_status",
		"idx_threads_updated_at",
		"idx_deployments_project",
		"idx_api_keys_project_revoked",
	}
	for _, idx := range required {
		if !bytes.Contains(raw, []byte(idx)) {
			t.Fatalf("missing required index %s", idx)
		}
	}
	if len(content) == 0 {
		t.Fatal("migration file is empty")
	}
}

func doBuilderReq(t *testing.T, url string, body map[string]any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func doControlPlaneReq(t *testing.T, url string, body map[string]any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func writeJSONResp(t *testing.T, w http.ResponseWriter, status int, payload map[string]any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}
