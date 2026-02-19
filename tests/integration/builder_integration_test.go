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
	"strings"
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

func TestValidateRejectsInvalidGraphTarget(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "graph.py"), []byte("graph = object()\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "requirements.txt"), []byte("langgraph\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	langgraphJSON := map[string]any{
		"graphs":       map[string]string{"default": "graph.py"},
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
	if resp.StatusCode != http.StatusUnprocessableEntity {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 422, got %d body=%s", resp.StatusCode, string(body))
	}
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result["valid"] != false {
		t.Fatalf("expected valid=false, got %#v", result["valid"])
	}
	errorsList, _ := result["errors"].([]any)
	found := false
	for _, item := range errorsList {
		if msg, _ := item.(string); strings.Contains(msg, "invalid target") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected invalid target error, got %#v", result["errors"])
	}
}

func TestValidateRejectsDependencyTraversal(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "graph.py"), []byte("graph = object()\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	langgraphJSON := map[string]any{
		"graphs":       map[string]string{"default": "graph.py:graph"},
		"dependencies": []string{"../outside"},
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
	if resp.StatusCode != http.StatusUnprocessableEntity {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 422, got %d body=%s", resp.StatusCode, string(body))
	}
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result["valid"] != false {
		t.Fatalf("expected valid=false, got %#v", result["valid"])
	}
	errorsList, _ := result["errors"].([]any)
	found := false
	for _, item := range errorsList {
		if msg, _ := item.(string); strings.Contains(msg, "dependency path must be within repo root") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected dependency traversal error, got %#v", result["errors"])
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

func TestControlPlaneDeploymentGetAndUpdate(t *testing.T) {
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
	var created map[string]any
	if err := json.NewDecoder(createResp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	createResp.Body.Close()
	depID, _ := created["id"].(string)
	if depID == "" {
		t.Fatal("missing deployment id")
	}

	getReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/internal/v1/deployments/"+depID, nil)
	getResp, err := http.DefaultClient.Do(getReq)
	if err != nil {
		t.Fatal(err)
	}
	if getResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(getResp.Body)
		t.Fatalf("expected 200 for get deployment, got %d body=%s", getResp.StatusCode, string(b))
	}
	getResp.Body.Close()

	updateResp := doControlPlaneReqWithHeaders(t, http.MethodPatch, ts.URL+"/internal/v1/deployments/"+depID, map[string]any{
		"git_ref":         "release/v1",
		"runtime_profile": "kata-qemu",
		"mode":            "mode_b",
	}, map[string]string{})
	if updateResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(updateResp.Body)
		t.Fatalf("expected 200 for update deployment, got %d body=%s", updateResp.StatusCode, string(b))
	}
	var updated map[string]any
	if err := json.NewDecoder(updateResp.Body).Decode(&updated); err != nil {
		t.Fatal(err)
	}
	updateResp.Body.Close()
	if got, _ := updated["git_ref"].(string); got != "release/v1" {
		t.Fatalf("expected git_ref release/v1, got %q", got)
	}
	if got, _ := updated["runtime_profile"].(string); got != "kata-qemu" {
		t.Fatalf("expected runtime_profile kata-qemu, got %q", got)
	}
	if got, _ := updated["mode"].(string); got != "mode_b" {
		t.Fatalf("expected mode mode_b, got %q", got)
	}

	deleteReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/internal/v1/deployments/"+depID, nil)
	deleteResp, err := http.DefaultClient.Do(deleteReq)
	if err != nil {
		t.Fatal(err)
	}
	if deleteResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(deleteResp.Body)
		t.Fatalf("expected 200 for delete deployment, got %d body=%s", deleteResp.StatusCode, string(b))
	}
	deleteResp.Body.Close()

	getAfterDeleteReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/internal/v1/deployments/"+depID, nil)
	getAfterDeleteResp, err := http.DefaultClient.Do(getAfterDeleteReq)
	if err != nil {
		t.Fatal(err)
	}
	if getAfterDeleteResp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(getAfterDeleteResp.Body)
		t.Fatalf("expected 404 after delete, got %d body=%s", getAfterDeleteResp.StatusCode, string(b))
	}
	getAfterDeleteResp.Body.Close()
}

func TestControlPlaneListFiltersByProject(t *testing.T) {
	t.Setenv("POSTGRES_DSN", "")
	t.Setenv("BUILDER_URL", "http://example.invalid")

	h := controlplaneapi.NewHandler(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	ts := httptest.NewServer(h)
	defer ts.Close()

	createDep := func(projectID string) string {
		resp := doControlPlaneReq(t, ts.URL+"/internal/v1/deployments", map[string]any{
			"project_id": projectID,
			"repo_url":   "https://github.com/acme/agent",
			"git_ref":    "main",
			"repo_path":  ".",
		})
		if resp.StatusCode != http.StatusCreated {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 201, got %d body=%s", resp.StatusCode, string(b))
		}
		var created map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		depID, _ := created["id"].(string)
		if depID == "" {
			t.Fatal("missing deployment id")
		}
		return depID
	}

	depA := createDep("proj_a")
	depB := createDep("proj_b")

	buildA := doControlPlaneReq(t, ts.URL+"/internal/v1/builds", map[string]any{
		"deployment_id": depA,
		"commit_sha":    "aaaa1111",
		"image_name":    "ghcr.io/acme/agent-a",
	})
	if buildA.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(buildA.Body)
		t.Fatalf("expected 202 for build A, got %d body=%s", buildA.StatusCode, string(b))
	}
	var buildAObj map[string]any
	if err := json.NewDecoder(buildA.Body).Decode(&buildAObj); err != nil {
		t.Fatal(err)
	}
	buildA.Body.Close()
	buildAID, _ := buildAObj["id"].(string)
	if buildAID == "" {
		t.Fatal("missing build A id")
	}

	buildB := doControlPlaneReq(t, ts.URL+"/internal/v1/builds", map[string]any{
		"deployment_id": depB,
		"commit_sha":    "bbbb2222",
		"image_name":    "ghcr.io/acme/agent-b",
	})
	if buildB.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(buildB.Body)
		t.Fatalf("expected 202 for build B, got %d body=%s", buildB.StatusCode, string(b))
	}
	var buildBObj map[string]any
	if err := json.NewDecoder(buildB.Body).Decode(&buildBObj); err != nil {
		t.Fatal(err)
	}
	buildB.Body.Close()
	buildBID, _ := buildBObj["id"].(string)
	if buildBID == "" {
		t.Fatal("missing build B id")
	}

	listDepsReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/internal/v1/deployments?project_id=proj_a", nil)
	listDepsResp, err := http.DefaultClient.Do(listDepsReq)
	if err != nil {
		t.Fatal(err)
	}
	if listDepsResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(listDepsResp.Body)
		t.Fatalf("expected 200 for deployment list filter, got %d body=%s", listDepsResp.StatusCode, string(b))
	}
	var deps []map[string]any
	if err := json.NewDecoder(listDepsResp.Body).Decode(&deps); err != nil {
		t.Fatal(err)
	}
	listDepsResp.Body.Close()
	if len(deps) != 1 {
		t.Fatalf("expected 1 deployment for proj_a, got %d", len(deps))
	}
	if id, _ := deps[0]["id"].(string); id != depA {
		t.Fatalf("expected deployment %s for proj_a, got %s", depA, id)
	}

	listBuildsReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/internal/v1/builds?project_id=proj_a", nil)
	listBuildsResp, err := http.DefaultClient.Do(listBuildsReq)
	if err != nil {
		t.Fatal(err)
	}
	if listBuildsResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(listBuildsResp.Body)
		t.Fatalf("expected 200 for build list filter, got %d body=%s", listBuildsResp.StatusCode, string(b))
	}
	var builds []map[string]any
	if err := json.NewDecoder(listBuildsResp.Body).Decode(&builds); err != nil {
		t.Fatal(err)
	}
	listBuildsResp.Body.Close()
	if len(builds) == 0 {
		t.Fatalf("expected at least one build for proj_a")
	}
	for _, build := range builds {
		if gotDep, _ := build["deployment_id"].(string); gotDep != depA {
			t.Fatalf("expected only deployment %s builds for proj_a filter, got deployment_id=%s", depA, gotDep)
		}
	}

	getAllowedReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/internal/v1/builds/"+buildAID+"?project_id=proj_a", nil)
	getAllowedResp, err := http.DefaultClient.Do(getAllowedReq)
	if err != nil {
		t.Fatal(err)
	}
	if getAllowedResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(getAllowedResp.Body)
		t.Fatalf("expected 200 for project-scoped build get, got %d body=%s", getAllowedResp.StatusCode, string(b))
	}
	getAllowedResp.Body.Close()

	getDeniedReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/internal/v1/builds/"+buildBID+"?project_id=proj_a", nil)
	getDeniedResp, err := http.DefaultClient.Do(getDeniedReq)
	if err != nil {
		t.Fatal(err)
	}
	if getDeniedResp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(getDeniedResp.Body)
		t.Fatalf("expected 404 for cross-project build get, got %d body=%s", getDeniedResp.StatusCode, string(b))
	}
	getDeniedResp.Body.Close()

	logsDeniedReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/internal/v1/builds/"+buildBID+"/logs?project_id=proj_a", nil)
	logsDeniedResp, err := http.DefaultClient.Do(logsDeniedReq)
	if err != nil {
		t.Fatal(err)
	}
	if logsDeniedResp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(logsDeniedResp.Body)
		t.Fatalf("expected 404 for cross-project build logs, got %d body=%s", logsDeniedResp.StatusCode, string(b))
	}
	logsDeniedResp.Body.Close()
}

func TestControlPlaneDeploymentProjectScopeForItemEndpoints(t *testing.T) {
	t.Setenv("POSTGRES_DSN", "")
	t.Setenv("BUILDER_URL", "http://example.invalid")

	h := controlplaneapi.NewHandler(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	ts := httptest.NewServer(h)
	defer ts.Close()

	createResp := doControlPlaneReq(t, ts.URL+"/internal/v1/deployments", map[string]any{
		"project_id": "proj_a",
		"repo_url":   "https://github.com/acme/agent",
		"git_ref":    "main",
		"repo_path":  ".",
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
	depID, _ := created["id"].(string)
	if depID == "" {
		t.Fatal("missing deployment id")
	}

	getDeniedReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/internal/v1/deployments/"+depID+"?project_id=proj_b", nil)
	getDeniedResp, err := http.DefaultClient.Do(getDeniedReq)
	if err != nil {
		t.Fatal(err)
	}
	if getDeniedResp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(getDeniedResp.Body)
		t.Fatalf("expected 404 for cross-project deployment get, got %d body=%s", getDeniedResp.StatusCode, string(b))
	}
	getDeniedResp.Body.Close()

	updateDeniedResp := doControlPlaneReqWithHeaders(t, http.MethodPatch, ts.URL+"/internal/v1/deployments/"+depID+"?project_id=proj_b", map[string]any{
		"git_ref": "release/v2",
	}, map[string]string{})
	if updateDeniedResp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(updateDeniedResp.Body)
		t.Fatalf("expected 404 for cross-project deployment update, got %d body=%s", updateDeniedResp.StatusCode, string(b))
	}
	updateDeniedResp.Body.Close()

	deleteDeniedReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/internal/v1/deployments/"+depID+"?project_id=proj_b", nil)
	deleteDeniedResp, err := http.DefaultClient.Do(deleteDeniedReq)
	if err != nil {
		t.Fatal(err)
	}
	if deleteDeniedResp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(deleteDeniedResp.Body)
		t.Fatalf("expected 404 for cross-project deployment delete, got %d body=%s", deleteDeniedResp.StatusCode, string(b))
	}
	deleteDeniedResp.Body.Close()

	getAllowedReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/internal/v1/deployments/"+depID+"?project_id=proj_a", nil)
	getAllowedResp, err := http.DefaultClient.Do(getAllowedReq)
	if err != nil {
		t.Fatal(err)
	}
	if getAllowedResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(getAllowedResp.Body)
		t.Fatalf("expected 200 for same-project deployment get, got %d body=%s", getAllowedResp.StatusCode, string(b))
	}
	getAllowedResp.Body.Close()
}

func TestControlPlaneAuditFilters(t *testing.T) {
	t.Setenv("POSTGRES_DSN", "")
	t.Setenv("BUILDER_URL", "http://example.invalid")

	h := controlplaneapi.NewHandler(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	ts := httptest.NewServer(h)
	defer ts.Close()

	create := func(projectID string) {
		resp := doControlPlaneReq(t, ts.URL+"/internal/v1/deployments", map[string]any{
			"project_id": projectID,
			"repo_url":   "https://github.com/acme/agent",
			"git_ref":    "main",
			"repo_path":  ".",
		})
		if resp.StatusCode != http.StatusCreated {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 201, got %d body=%s", resp.StatusCode, string(b))
		}
		resp.Body.Close()
	}
	create("proj_a")
	create("proj_b")

	byEventReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/internal/v1/audit?event=deployment.created", nil)
	byEventResp, err := http.DefaultClient.Do(byEventReq)
	if err != nil {
		t.Fatal(err)
	}
	if byEventResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(byEventResp.Body)
		t.Fatalf("expected 200 for audit event filter, got %d body=%s", byEventResp.StatusCode, string(b))
	}
	var eventItems []map[string]any
	if err := json.NewDecoder(byEventResp.Body).Decode(&eventItems); err != nil {
		t.Fatal(err)
	}
	byEventResp.Body.Close()
	if len(eventItems) < 2 {
		t.Fatalf("expected at least 2 deployment.created audit events, got %d", len(eventItems))
	}
	for _, item := range eventItems {
		if ev, _ := item["event"].(string); ev != "deployment.created" {
			t.Fatalf("unexpected event in filtered audit list: %q", ev)
		}
	}

	byProjectReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/internal/v1/audit?project_id=proj_a", nil)
	byProjectResp, err := http.DefaultClient.Do(byProjectReq)
	if err != nil {
		t.Fatal(err)
	}
	if byProjectResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(byProjectResp.Body)
		t.Fatalf("expected 200 for audit project filter, got %d body=%s", byProjectResp.StatusCode, string(b))
	}
	var projectItems []map[string]any
	if err := json.NewDecoder(byProjectResp.Body).Decode(&projectItems); err != nil {
		t.Fatal(err)
	}
	byProjectResp.Body.Close()
	if len(projectItems) == 0 {
		t.Fatalf("expected at least one audit record for proj_a")
	}
	for _, item := range projectItems {
		if pid, _ := item["project_id"].(string); pid != "proj_a" {
			t.Fatalf("expected project_id proj_a in filtered audit list, got %q", pid)
		}
	}
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

func TestControlPlaneBuildLogsFallbackWhenBuilderUnavailable(t *testing.T) {
	t.Setenv("POSTGRES_DSN", "")
	t.Setenv("BUILDER_URL", "http://127.0.0.1:1")

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
	logsRef, _ := build["logs_ref"].(string)
	if buildID == "" || logsRef == "" {
		t.Fatalf("expected build id and logs_ref in fallback response, got %#v", build)
	}

	logsReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/internal/v1/builds/"+buildID+"/logs", nil)
	logsResp, err := http.DefaultClient.Do(logsReq)
	if err != nil {
		t.Fatal(err)
	}
	if logsResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(logsResp.Body)
		t.Fatalf("expected 200 for fallback logs response, got %d body=%s", logsResp.StatusCode, string(b))
	}
	var logsBody map[string]any
	if err := json.NewDecoder(logsResp.Body).Decode(&logsBody); err != nil {
		t.Fatal(err)
	}
	logsResp.Body.Close()
	if gotBuildID, _ := logsBody["build_id"].(string); gotBuildID != buildID {
		t.Fatalf("expected build_id %s, got %q", buildID, gotBuildID)
	}
	if gotLogsRef, _ := logsBody["logs_ref"].(string); gotLogsRef == "" {
		t.Fatalf("expected non-empty logs_ref in fallback logs response")
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
	bindingID, _ := bindings[0]["id"].(string)
	if bindingID == "" {
		t.Fatal("missing binding id")
	}

	deleteReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/internal/v1/secrets/bindings/"+bindingID, nil)
	deleteResp, err := http.DefaultClient.Do(deleteReq)
	if err != nil {
		t.Fatal(err)
	}
	if deleteResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(deleteResp.Body)
		t.Fatalf("expected 200, got %d body=%s", deleteResp.StatusCode, string(b))
	}
	deleteResp.Body.Close()

	listAfterReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/internal/v1/secrets/bindings?deployment_id="+depID, nil)
	listAfterResp, err := http.DefaultClient.Do(listAfterReq)
	if err != nil {
		t.Fatal(err)
	}
	if listAfterResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(listAfterResp.Body)
		t.Fatalf("expected 200, got %d body=%s", listAfterResp.StatusCode, string(b))
	}
	var after []map[string]any
	if err := json.NewDecoder(listAfterResp.Body).Decode(&after); err != nil {
		t.Fatal(err)
	}
	listAfterResp.Body.Close()
	if len(after) != 0 {
		t.Fatalf("expected no bindings after delete, got %d", len(after))
	}
}

func TestControlPlaneSecretBindingProjectScope(t *testing.T) {
	t.Setenv("POSTGRES_DSN", "")
	t.Setenv("BUILDER_URL", "http://example.invalid")

	h := controlplaneapi.NewHandler(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	ts := httptest.NewServer(h)
	defer ts.Close()

	createResp := doControlPlaneReq(t, ts.URL+"/internal/v1/deployments", map[string]any{
		"project_id": "proj_a",
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

	bindDeniedResp := doControlPlaneReq(t, ts.URL+"/internal/v1/secrets/bind", map[string]any{
		"deployment_id": depID,
		"project_id":    "proj_b",
		"secret_name":   "openai-secret",
		"target_key":    "OPENAI_API_KEY",
	})
	if bindDeniedResp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(bindDeniedResp.Body)
		t.Fatalf("expected 404 for cross-project bind, got %d body=%s", bindDeniedResp.StatusCode, string(b))
	}
	bindDeniedResp.Body.Close()

	bindAllowedResp := doControlPlaneReq(t, ts.URL+"/internal/v1/secrets/bind", map[string]any{
		"deployment_id": depID,
		"project_id":    "proj_a",
		"secret_name":   "openai-secret",
		"target_key":    "OPENAI_API_KEY",
	})
	if bindAllowedResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(bindAllowedResp.Body)
		t.Fatalf("expected 200 for same-project bind, got %d body=%s", bindAllowedResp.StatusCode, string(b))
	}
	bindAllowedResp.Body.Close()

	listDeniedReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/internal/v1/secrets/bindings?deployment_id="+depID+"&project_id=proj_b", nil)
	listDeniedResp, err := http.DefaultClient.Do(listDeniedReq)
	if err != nil {
		t.Fatal(err)
	}
	if listDeniedResp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(listDeniedResp.Body)
		t.Fatalf("expected 404 for cross-project list, got %d body=%s", listDeniedResp.StatusCode, string(b))
	}
	listDeniedResp.Body.Close()

	listAllowedReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/internal/v1/secrets/bindings?deployment_id="+depID+"&project_id=proj_a", nil)
	listAllowedResp, err := http.DefaultClient.Do(listAllowedReq)
	if err != nil {
		t.Fatal(err)
	}
	if listAllowedResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(listAllowedResp.Body)
		t.Fatalf("expected 200 for same-project list, got %d body=%s", listAllowedResp.StatusCode, string(b))
	}
	var bindings []map[string]any
	if err := json.NewDecoder(listAllowedResp.Body).Decode(&bindings); err != nil {
		t.Fatal(err)
	}
	listAllowedResp.Body.Close()
	if len(bindings) == 0 {
		t.Fatal("expected at least one binding")
	}
	bindingID, _ := bindings[0]["id"].(string)
	if bindingID == "" {
		t.Fatal("missing binding id")
	}

	deleteDeniedReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/internal/v1/secrets/bindings/"+bindingID+"?project_id=proj_b", nil)
	deleteDeniedResp, err := http.DefaultClient.Do(deleteDeniedReq)
	if err != nil {
		t.Fatal(err)
	}
	if deleteDeniedResp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(deleteDeniedResp.Body)
		t.Fatalf("expected 404 for cross-project delete, got %d body=%s", deleteDeniedResp.StatusCode, string(b))
	}
	deleteDeniedResp.Body.Close()

	deleteAllowedReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/internal/v1/secrets/bindings/"+bindingID+"?project_id=proj_a", nil)
	deleteAllowedResp, err := http.DefaultClient.Do(deleteAllowedReq)
	if err != nil {
		t.Fatal(err)
	}
	if deleteAllowedResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(deleteAllowedResp.Body)
		t.Fatalf("expected 200 for same-project delete, got %d body=%s", deleteAllowedResp.StatusCode, string(b))
	}
	deleteAllowedResp.Body.Close()
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

func TestControlPlaneRuntimePolicyProjectScope(t *testing.T) {
	t.Setenv("POSTGRES_DSN", "")
	t.Setenv("BUILDER_URL", "http://example.invalid")

	h := controlplaneapi.NewHandler(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	ts := httptest.NewServer(h)
	defer ts.Close()

	createResp := doControlPlaneReq(t, ts.URL+"/internal/v1/deployments", map[string]any{
		"project_id": "proj_a",
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

	deniedResp := doControlPlaneReq(t, ts.URL+"/internal/v1/policies/runtime", map[string]any{
		"deployment_id":   depID,
		"project_id":      "proj_b",
		"runtime_profile": "kata-qemu",
		"mode":            "mode_b",
	})
	if deniedResp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(deniedResp.Body)
		t.Fatalf("expected 404 for cross-project runtime policy update, got %d body=%s", deniedResp.StatusCode, string(b))
	}
	deniedResp.Body.Close()

	allowedResp := doControlPlaneReq(t, ts.URL+"/internal/v1/policies/runtime", map[string]any{
		"deployment_id":   depID,
		"project_id":      "proj_a",
		"runtime_profile": "kata-qemu",
		"mode":            "mode_b",
	})
	if allowedResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(allowedResp.Body)
		t.Fatalf("expected 200 for same-project runtime policy update, got %d body=%s", allowedResp.StatusCode, string(b))
	}
	allowedResp.Body.Close()
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
