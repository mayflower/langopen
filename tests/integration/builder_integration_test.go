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
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/internal/v1/builds/trigger":
			writeJSONResp(t, w, http.StatusAccepted, map[string]any{
				"id":            "build_mock_1",
				"deployment_id": "dep_mock_1",
				"status":        "succeeded",
				"commit_sha":    "abc123",
				"image_digest":  "sha256:deadbeef",
				"logs_ref":      "inline://builds/build_mock_1",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/internal/v1/builds/build_mock_1":
			writeJSONResp(t, w, http.StatusOK, map[string]any{
				"id":            "build_mock_1",
				"deployment_id": "dep_mock_1",
				"status":        "succeeded",
				"commit_sha":    "abc123",
				"image_digest":  "sha256:deadbeef",
				"logs_ref":      "inline://builds/build_mock_1",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/internal/v1/builds/build_mock_1/logs":
			writeJSONResp(t, w, http.StatusOK, map[string]any{
				"build_id": "build_mock_1",
				"logs":     "line1\nline2",
			})
		default:
			http.NotFound(w, r)
		}
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
	if buildResp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(buildResp.Body)
		t.Fatalf("expected 202, got %d body=%s", buildResp.StatusCode, string(b))
	}
	var build map[string]any
	if err := json.NewDecoder(buildResp.Body).Decode(&build); err != nil {
		t.Fatal(err)
	}
	buildResp.Body.Close()

	buildID, _ := build["id"].(string)
	if buildID == "" {
		t.Fatal("missing build id")
	}

	statusReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/internal/v1/builds/"+buildID, nil)
	statusResp, err := http.DefaultClient.Do(statusReq)
	if err != nil {
		t.Fatal(err)
	}
	if statusResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(statusResp.Body)
		t.Fatalf("expected 200 for build status, got %d body=%s", statusResp.StatusCode, string(b))
	}
	statusResp.Body.Close()

	logsReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/internal/v1/builds/"+buildID+"/logs", nil)
	logsResp, err := http.DefaultClient.Do(logsReq)
	if err != nil {
		t.Fatal(err)
	}
	if logsResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(logsResp.Body)
		t.Fatalf("expected 200 for build logs, got %d body=%s", logsResp.StatusCode, string(b))
	}
	logsResp.Body.Close()
}

func TestControlPlaneSourceValidate(t *testing.T) {
	t.Setenv("POSTGRES_DSN", "")

	builderMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/internal/v1/builds/validate" {
			http.NotFound(w, r)
			return
		}
		writeJSONResp(t, w, http.StatusOK, map[string]any{
			"valid":    true,
			"errors":   []string{},
			"warnings": []string{},
		})
	}))
	defer builderMock.Close()
	t.Setenv("BUILDER_URL", builderMock.URL)

	h := controlplaneapi.NewHandler(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	ts := httptest.NewServer(h)
	defer ts.Close()

	resp := doControlPlaneReq(t, ts.URL+"/internal/v1/sources/validate", map[string]any{
		"repo_path": ".",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(b))
	}
}

func TestControlPlaneAPIKeyLifecycle(t *testing.T) {
	t.Setenv("POSTGRES_DSN", "")
	t.Setenv("BUILDER_URL", "http://example.invalid")

	h := controlplaneapi.NewHandler(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	ts := httptest.NewServer(h)
	defer ts.Close()

	createResp := doControlPlaneReq(t, ts.URL+"/internal/v1/api-keys", map[string]any{
		"project_id": "proj_default",
		"name":       "integration",
	})
	if createResp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(createResp.Body)
		t.Fatalf("expected 201, got %d body=%s", createResp.StatusCode, string(b))
	}
	var created map[string]any
	if err := json.NewDecoder(createResp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	createResp.Body.Close()
	keyID, _ := created["id"].(string)
	rawKey, _ := created["key"].(string)
	if keyID == "" || rawKey == "" {
		t.Fatalf("missing key id or plaintext key in create response: %#v", created)
	}

	listReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/internal/v1/api-keys?project_id=proj_default", nil)
	listResp, err := http.DefaultClient.Do(listReq)
	if err != nil {
		t.Fatal(err)
	}
	if listResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(listResp.Body)
		t.Fatalf("expected 200, got %d body=%s", listResp.StatusCode, string(b))
	}
	var listed []map[string]any
	if err := json.NewDecoder(listResp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	listResp.Body.Close()
	found := false
	for _, item := range listed {
		if id, _ := item["id"].(string); id == keyID {
			if _, hasSecret := item["key"]; hasSecret {
				t.Fatalf("list response must not include plaintext key")
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("created key %s not found in list", keyID)
	}

	revokeResp := doControlPlaneReq(t, ts.URL+"/internal/v1/api-keys/"+keyID+"/revoke", map[string]any{})
	if revokeResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(revokeResp.Body)
		t.Fatalf("expected 200, got %d body=%s", revokeResp.StatusCode, string(b))
	}
	revokeResp.Body.Close()
}

func TestControlPlaneSecretBindingLifecycle(t *testing.T) {
	t.Setenv("POSTGRES_DSN", "")
	t.Setenv("BUILDER_URL", "http://example.invalid")

	h := controlplaneapi.NewHandler(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	ts := httptest.NewServer(h)
	defer ts.Close()

	createResp := doControlPlaneReq(t, ts.URL+"/internal/v1/deployments", map[string]any{
		"project_id": "proj_default",
		"repo_url":   "https://github.com/acme/agent",
		"git_ref":    "main",
		"repo_path":  ".",
	})
	if createResp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(createResp.Body)
		t.Fatalf("expected 201, got %d body=%s", createResp.StatusCode, string(b))
	}
	var dep map[string]any
	if err := json.NewDecoder(createResp.Body).Decode(&dep); err != nil {
		t.Fatal(err)
	}
	createResp.Body.Close()
	depID, _ := dep["id"].(string)
	if depID == "" {
		t.Fatal("missing deployment id")
	}

	bindResp := doControlPlaneReq(t, ts.URL+"/internal/v1/secrets/bind", map[string]any{
		"deployment_id": depID,
		"secret_name":   "openai-secret",
		"target_key":    "OPENAI_API_KEY",
	})
	if bindResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(bindResp.Body)
		t.Fatalf("expected 200, got %d body=%s", bindResp.StatusCode, string(b))
	}
	bindResp.Body.Close()

	listReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/internal/v1/secrets/bindings?deployment_id="+depID, nil)
	listResp, err := http.DefaultClient.Do(listReq)
	if err != nil {
		t.Fatal(err)
	}
	if listResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(listResp.Body)
		t.Fatalf("expected 200, got %d body=%s", listResp.StatusCode, string(b))
	}
	var bindings []map[string]any
	if err := json.NewDecoder(listResp.Body).Decode(&bindings); err != nil {
		t.Fatal(err)
	}
	listResp.Body.Close()
	if len(bindings) == 0 {
		t.Fatal("expected at least one secret binding")
	}
}

func TestControlPlaneRBAC(t *testing.T) {
	t.Setenv("POSTGRES_DSN", "")
	t.Setenv("BUILDER_URL", "http://example.invalid")

	h := controlplaneapi.NewHandler(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	ts := httptest.NewServer(h)
	defer ts.Close()

	viewerCreate := doControlPlaneReqWithHeaders(t, http.MethodPost, ts.URL+"/internal/v1/deployments", map[string]any{
		"project_id": "proj_default",
		"repo_url":   "https://github.com/acme/agent",
		"git_ref":    "main",
		"repo_path":  ".",
	}, map[string]string{"X-Project-Role": "viewer"})
	if viewerCreate.StatusCode != http.StatusForbidden {
		b, _ := io.ReadAll(viewerCreate.Body)
		t.Fatalf("expected 403 for viewer deploy create, got %d body=%s", viewerCreate.StatusCode, string(b))
	}
	viewerCreate.Body.Close()

	viewerList := doControlPlaneReqWithHeaders(t, http.MethodGet, ts.URL+"/internal/v1/deployments", nil, map[string]string{"X-Project-Role": "viewer"})
	if viewerList.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(viewerList.Body)
		t.Fatalf("expected 200 for viewer deployments list, got %d body=%s", viewerList.StatusCode, string(b))
	}
	viewerList.Body.Close()

	developerKeyCreate := doControlPlaneReqWithHeaders(t, http.MethodPost, ts.URL+"/internal/v1/api-keys", map[string]any{
		"project_id": "proj_default",
		"name":       "dev-try",
	}, map[string]string{"X-Project-Role": "developer"})
	if developerKeyCreate.StatusCode != http.StatusForbidden {
		b, _ := io.ReadAll(developerKeyCreate.Body)
		t.Fatalf("expected 403 for developer api-key create, got %d body=%s", developerKeyCreate.StatusCode, string(b))
	}
	developerKeyCreate.Body.Close()
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

	secretBindingsRaw, err := os.ReadFile(filepath.Join(root, "db", "migrations", "002_secret_bindings.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(secretBindingsRaw, []byte("deployment_secret_bindings")) {
		t.Fatal("missing deployment_secret_bindings migration")
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

func doControlPlaneReqWithHeaders(t *testing.T, method, url string, body map[string]any, headers map[string]string) *http.Response {
	t.Helper()
	var payload io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		payload = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, payload)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
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
