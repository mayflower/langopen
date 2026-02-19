package conformance

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	apiserver "langopen.dev/api-server/server"
)

func newClientServer(t *testing.T) (*httptest.Server, *http.Client) {
	t.Helper()
	h, err := apiserver.NewHandler(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts, ts.Client()
}

func TestDocsAndOpenAPI(t *testing.T) {
	ts, client := newClientServer(t)

	resp, err := client.Get(ts.URL + "/docs")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/docs status = %d", resp.StatusCode)
	}
	docsBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(docsBody), "swagger-ui") {
		t.Fatalf("/docs did not render OpenAPI UI content")
	}

	resp, err = client.Get(ts.URL + "/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/openapi.json status = %d", resp.StatusCode)
	}
	var spec map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&spec); err != nil {
		t.Fatalf("decode openapi: %v", err)
	}
	paths, _ := spec["paths"].(map[string]any)
	required := []string{
		"/assistants",
		"/assistants/search",
		"/assistants/{assistant_id}",
		"/threads",
		"/threads/search",
		"/threads/{thread_id}",
		"/threads/{thread_id}/history",
		"/threads/{thread_id}/state",
		"/threads/{thread_id}/runs",
		"/threads/{thread_id}/runs/wait",
		"/threads/{thread_id}/runs/{run_id}",
		"/threads/{thread_id}/runs/stream",
		"/threads/{thread_id}/runs/{run_id}/stream",
		"/runs",
		"/runs/wait",
		"/runs/stream",
		"/runs/{run_id}",
		"/runs/{run_id}/wait",
		"/store/items",
		"/crons",
		"/a2a/{assistant_id}",
		"/a2a/{assistant_id}/.well-known/agent-card.json",
		"/mcp",
		"/system/attention",
	}
	for _, path := range required {
		if _, ok := paths[path]; !ok {
			t.Fatalf("openapi missing required path %s", path)
		}
	}
}

func TestDataPlaneRBAC(t *testing.T) {
	ts, client := newClientServer(t)

	viewerCreateReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/threads", bytes.NewReader([]byte(`{}`)))
	viewerCreateReq.Header.Set("Content-Type", "application/json")
	viewerCreateReq.Header.Set("X-Api-Key", "test-key")
	viewerCreateReq.Header.Set("X-Project-Role", "viewer")
	viewerCreateResp, err := client.Do(viewerCreateReq)
	if err != nil {
		t.Fatal(err)
	}
	if viewerCreateResp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(viewerCreateResp.Body)
		t.Fatalf("expected 403 for viewer thread create, got %d body=%s", viewerCreateResp.StatusCode, string(body))
	}
	viewerCreateResp.Body.Close()

	viewerListReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/threads", nil)
	viewerListReq.Header.Set("X-Api-Key", "test-key")
	viewerListReq.Header.Set("X-Project-Role", "viewer")
	viewerListResp, err := client.Do(viewerListReq)
	if err != nil {
		t.Fatal(err)
	}
	if viewerListResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(viewerListResp.Body)
		t.Fatalf("expected 200 for viewer thread list, got %d body=%s", viewerListResp.StatusCode, string(body))
	}
	viewerListResp.Body.Close()

	viewerMCPReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/mcp", bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":"1","method":"initialize"}`)))
	viewerMCPReq.Header.Set("Content-Type", "application/json")
	viewerMCPReq.Header.Set("X-Api-Key", "test-key")
	viewerMCPReq.Header.Set("X-Project-Role", "viewer")
	viewerMCPResp, err := client.Do(viewerMCPReq)
	if err != nil {
		t.Fatal(err)
	}
	if viewerMCPResp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(viewerMCPResp.Body)
		t.Fatalf("expected 403 for viewer mcp post, got %d body=%s", viewerMCPResp.StatusCode, string(body))
	}
	viewerMCPResp.Body.Close()
}

func TestAuthSchemeCompatibility(t *testing.T) {
	ts, client := newClientServer(t)

	validReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/system", nil)
	validReq.Header.Set("X-Api-Key", "test-key")
	validReq.Header.Set("X-Auth-Scheme", "langsmith-api-key")
	validResp, err := client.Do(validReq)
	if err != nil {
		t.Fatal(err)
	}
	if validResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(validResp.Body)
		t.Fatalf("expected 200 for compatible auth scheme, got %d body=%s", validResp.StatusCode, string(body))
	}
	validResp.Body.Close()

	invalidReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/system", nil)
	invalidReq.Header.Set("X-Api-Key", "test-key")
	invalidReq.Header.Set("X-Auth-Scheme", "bearer")
	invalidResp, err := client.Do(invalidReq)
	if err != nil {
		t.Fatal(err)
	}
	if invalidResp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(invalidResp.Body)
		t.Fatalf("expected 400 for invalid auth scheme, got %d body=%s", invalidResp.StatusCode, string(body))
	}
	invalidResp.Body.Close()
}

func TestAssistantsAndThreadsCRUD(t *testing.T) {
	ts, client := newClientServer(t)

	createAssistantResp := doJSON(t, client, http.MethodPost, ts.URL+"/api/v1/assistants", map[string]any{
		"deployment_id": "dep_default",
		"graph_id":      "graph_1",
		"config":        map[string]any{"mode": "test"},
		"version":       "v1",
	}, "test-key")
	if createAssistantResp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(createAssistantResp.Body)
		t.Fatalf("create assistant status=%d body=%s", createAssistantResp.StatusCode, string(body))
	}
	var assistant map[string]any
	if err := json.NewDecoder(createAssistantResp.Body).Decode(&assistant); err != nil {
		t.Fatal(err)
	}
	createAssistantResp.Body.Close()
	assistantID, _ := assistant["id"].(string)
	if assistantID == "" {
		t.Fatal("missing assistant id")
	}

	getAssistantReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/assistants/"+assistantID, nil)
	getAssistantReq.Header.Set("X-Api-Key", "test-key")
	getAssistantResp, err := client.Do(getAssistantReq)
	if err != nil {
		t.Fatal(err)
	}
	if getAssistantResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(getAssistantResp.Body)
		t.Fatalf("get assistant status=%d body=%s", getAssistantResp.StatusCode, string(body))
	}
	getAssistantResp.Body.Close()

	updateAssistantResp := doJSON(t, client, http.MethodPatch, ts.URL+"/api/v1/assistants/"+assistantID, map[string]any{
		"graph_id": "graph_2",
		"version":  "v2",
		"config":   map[string]any{"mode": "prod"},
	}, "test-key")
	if updateAssistantResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(updateAssistantResp.Body)
		t.Fatalf("update assistant status=%d body=%s", updateAssistantResp.StatusCode, string(body))
	}
	var updatedAssistant map[string]any
	if err := json.NewDecoder(updateAssistantResp.Body).Decode(&updatedAssistant); err != nil {
		t.Fatal(err)
	}
	updateAssistantResp.Body.Close()
	if got, _ := updatedAssistant["graph_id"].(string); got != "graph_2" {
		t.Fatalf("expected graph_id=graph_2 got %q", got)
	}

	createThreadResp := doJSON(t, client, http.MethodPost, ts.URL+"/api/v1/threads", map[string]any{
		"assistant_id": assistantID,
		"metadata":     map[string]any{"source": "conformance"},
	}, "test-key")
	if createThreadResp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(createThreadResp.Body)
		t.Fatalf("create thread status=%d body=%s", createThreadResp.StatusCode, string(body))
	}
	var thread map[string]any
	if err := json.NewDecoder(createThreadResp.Body).Decode(&thread); err != nil {
		t.Fatal(err)
	}
	createThreadResp.Body.Close()
	threadID, _ := thread["id"].(string)
	if threadID == "" {
		t.Fatal("missing thread id")
	}

	getThreadReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/threads/"+threadID, nil)
	getThreadReq.Header.Set("X-Api-Key", "test-key")
	getThreadResp, err := client.Do(getThreadReq)
	if err != nil {
		t.Fatal(err)
	}
	if getThreadResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(getThreadResp.Body)
		t.Fatalf("get thread status=%d body=%s", getThreadResp.StatusCode, string(body))
	}
	getThreadResp.Body.Close()

	patchThreadResp := doJSON(t, client, http.MethodPatch, ts.URL+"/api/v1/threads/"+threadID, map[string]any{
		"metadata": map[string]any{"source": "updated"},
	}, "test-key")
	if patchThreadResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(patchThreadResp.Body)
		t.Fatalf("patch thread status=%d body=%s", patchThreadResp.StatusCode, string(body))
	}
	var patchedThread map[string]any
	if err := json.NewDecoder(patchThreadResp.Body).Decode(&patchedThread); err != nil {
		t.Fatal(err)
	}
	patchThreadResp.Body.Close()
	metadata, _ := patchedThread["metadata"].(map[string]any)
	if got, _ := metadata["source"].(string); got != "updated" {
		t.Fatalf("expected metadata.source=updated got %q", got)
	}

	deleteThreadReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/threads/"+threadID, nil)
	deleteThreadReq.Header.Set("X-Api-Key", "test-key")
	deleteThreadResp, err := client.Do(deleteThreadReq)
	if err != nil {
		t.Fatal(err)
	}
	if deleteThreadResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(deleteThreadResp.Body)
		t.Fatalf("delete thread status=%d body=%s", deleteThreadResp.StatusCode, string(body))
	}
	deleteThreadResp.Body.Close()

	deleteAssistantReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/assistants/"+assistantID, nil)
	deleteAssistantReq.Header.Set("X-Api-Key", "test-key")
	deleteAssistantResp, err := client.Do(deleteAssistantReq)
	if err != nil {
		t.Fatal(err)
	}
	if deleteAssistantResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(deleteAssistantResp.Body)
		t.Fatalf("delete assistant status=%d body=%s", deleteAssistantResp.StatusCode, string(body))
	}
	deleteAssistantResp.Body.Close()
}

func TestCompatibilityRootPaths(t *testing.T) {
	ts, client := newClientServer(t)

	createThreadResp := doJSON(t, client, http.MethodPost, ts.URL+"/threads", map[string]any{}, "test-key")
	if createThreadResp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(createThreadResp.Body)
		t.Fatalf("root create thread status=%d body=%s", createThreadResp.StatusCode, string(body))
	}
	var thread map[string]any
	if err := json.NewDecoder(createThreadResp.Body).Decode(&thread); err != nil {
		t.Fatal(err)
	}
	createThreadResp.Body.Close()
	threadID, _ := thread["id"].(string)
	if threadID == "" {
		t.Fatal("missing thread id")
	}

	getThreadReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/threads/"+threadID, nil)
	getThreadReq.Header.Set("X-Api-Key", "test-key")
	getThreadResp, err := client.Do(getThreadReq)
	if err != nil {
		t.Fatal(err)
	}
	if getThreadResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(getThreadResp.Body)
		t.Fatalf("root get thread status=%d body=%s", getThreadResp.StatusCode, string(body))
	}
	getThreadResp.Body.Close()

	rootStreamResp := doJSON(t, client, http.MethodPost, ts.URL+"/threads/"+threadID+"/runs/stream", map[string]any{}, "test-key")
	if rootStreamResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(rootStreamResp.Body)
		t.Fatalf("root run stream status=%d body=%s", rootStreamResp.StatusCode, string(body))
	}
	runID := findRunID(t, rootStreamResp.Body)
	rootStreamResp.Body.Close()
	if runID == "" {
		t.Fatal("root run stream missing run id")
	}

	rootCreateRunResp := doJSON(t, client, http.MethodPost, ts.URL+"/threads/"+threadID+"/runs", map[string]any{}, "test-key")
	if rootCreateRunResp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(rootCreateRunResp.Body)
		t.Fatalf("root run create status=%d body=%s", rootCreateRunResp.StatusCode, string(body))
	}
	rootCreateRunResp.Body.Close()

	rootCreateStatelessResp := doJSON(t, client, http.MethodPost, ts.URL+"/runs", map[string]any{}, "test-key")
	if rootCreateStatelessResp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(rootCreateStatelessResp.Body)
		t.Fatalf("root stateless run create status=%d body=%s", rootCreateStatelessResp.StatusCode, string(body))
	}
	rootCreateStatelessResp.Body.Close()
}

func TestRunCreateAndListNonStream(t *testing.T) {
	ts, client := newClientServer(t)

	threadResp := doJSON(t, client, http.MethodPost, ts.URL+"/api/v1/threads", map[string]any{}, "test-key")
	defer threadResp.Body.Close()
	var thread struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(threadResp.Body).Decode(&thread); err != nil {
		t.Fatal(err)
	}

	createThreadRunResp := doJSON(t, client, http.MethodPost, ts.URL+"/api/v1/threads/"+thread.ID+"/runs", map[string]any{
		"multitask_strategy": "enqueue",
	}, "test-key")
	if createThreadRunResp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(createThreadRunResp.Body)
		t.Fatalf("create thread run status=%d body=%s", createThreadRunResp.StatusCode, string(body))
	}
	var threadRun map[string]any
	if err := json.NewDecoder(createThreadRunResp.Body).Decode(&threadRun); err != nil {
		t.Fatal(err)
	}
	createThreadRunResp.Body.Close()
	threadRunID, _ := threadRun["id"].(string)
	if threadRunID == "" {
		t.Fatal("missing thread run id")
	}

	getThreadRunReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/threads/"+thread.ID+"/runs/"+threadRunID, nil)
	getThreadRunReq.Header.Set("X-Api-Key", "test-key")
	getThreadRunResp, err := client.Do(getThreadRunReq)
	if err != nil {
		t.Fatal(err)
	}
	if getThreadRunResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(getThreadRunResp.Body)
		t.Fatalf("get thread run status=%d body=%s", getThreadRunResp.StatusCode, string(body))
	}
	getThreadRunResp.Body.Close()

	deleteThreadRunReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/threads/"+thread.ID+"/runs/"+threadRunID, nil)
	deleteThreadRunReq.Header.Set("X-Api-Key", "test-key")
	deleteThreadRunResp, err := client.Do(deleteThreadRunReq)
	if err != nil {
		t.Fatal(err)
	}
	if deleteThreadRunResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(deleteThreadRunResp.Body)
		t.Fatalf("delete thread run status=%d body=%s", deleteThreadRunResp.StatusCode, string(body))
	}
	deleteThreadRunResp.Body.Close()

	getDeletedThreadRunReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/threads/"+thread.ID+"/runs/"+threadRunID, nil)
	getDeletedThreadRunReq.Header.Set("X-Api-Key", "test-key")
	getDeletedThreadRunResp, err := client.Do(getDeletedThreadRunReq)
	if err != nil {
		t.Fatal(err)
	}
	if getDeletedThreadRunResp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(getDeletedThreadRunResp.Body)
		t.Fatalf("expected 404 for deleted thread run, got %d body=%s", getDeletedThreadRunResp.StatusCode, string(body))
	}
	getDeletedThreadRunResp.Body.Close()

	listThreadRunsReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/threads/"+thread.ID+"/runs", nil)
	listThreadRunsReq.Header.Set("X-Api-Key", "test-key")
	listThreadRunsResp, err := client.Do(listThreadRunsReq)
	if err != nil {
		t.Fatal(err)
	}
	if listThreadRunsResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(listThreadRunsResp.Body)
		t.Fatalf("list thread runs status=%d body=%s", listThreadRunsResp.StatusCode, string(body))
	}
	var listedThreadRuns []map[string]any
	if err := json.NewDecoder(listThreadRunsResp.Body).Decode(&listedThreadRuns); err != nil {
		t.Fatal(err)
	}
	listThreadRunsResp.Body.Close()
	foundThreadRun := false
	for _, item := range listedThreadRuns {
		if id, _ := item["id"].(string); id == threadRunID {
			foundThreadRun = true
			break
		}
	}
	if foundThreadRun {
		t.Fatalf("deleted thread run %s should not appear in thread run list", threadRunID)
	}

	createStatelessResp := doJSON(t, client, http.MethodPost, ts.URL+"/api/v1/runs", map[string]any{
		"assistant_id": "asst_default",
	}, "test-key")
	if createStatelessResp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(createStatelessResp.Body)
		t.Fatalf("create stateless run status=%d body=%s", createStatelessResp.StatusCode, string(body))
	}
	var stateless map[string]any
	if err := json.NewDecoder(createStatelessResp.Body).Decode(&stateless); err != nil {
		t.Fatal(err)
	}
	createStatelessResp.Body.Close()
	statelessRunID, _ := stateless["id"].(string)
	if statelessRunID == "" {
		t.Fatal("missing stateless run id")
	}

	deleteStatelessReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/runs/"+statelessRunID, nil)
	deleteStatelessReq.Header.Set("X-Api-Key", "test-key")
	deleteStatelessResp, err := client.Do(deleteStatelessReq)
	if err != nil {
		t.Fatal(err)
	}
	if deleteStatelessResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(deleteStatelessResp.Body)
		t.Fatalf("delete stateless run status=%d body=%s", deleteStatelessResp.StatusCode, string(body))
	}
	deleteStatelessResp.Body.Close()

	getDeletedStatelessReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/runs/"+statelessRunID, nil)
	getDeletedStatelessReq.Header.Set("X-Api-Key", "test-key")
	getDeletedStatelessResp, err := client.Do(getDeletedStatelessReq)
	if err != nil {
		t.Fatal(err)
	}
	if getDeletedStatelessResp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(getDeletedStatelessResp.Body)
		t.Fatalf("expected 404 for deleted stateless run, got %d body=%s", getDeletedStatelessResp.StatusCode, string(body))
	}
	getDeletedStatelessResp.Body.Close()

	listRunsReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/runs", nil)
	listRunsReq.Header.Set("X-Api-Key", "test-key")
	listRunsResp, err := client.Do(listRunsReq)
	if err != nil {
		t.Fatal(err)
	}
	if listRunsResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(listRunsResp.Body)
		t.Fatalf("list runs status=%d body=%s", listRunsResp.StatusCode, string(body))
	}
	var listedRuns []map[string]any
	if err := json.NewDecoder(listRunsResp.Body).Decode(&listedRuns); err != nil {
		t.Fatal(err)
	}
	listRunsResp.Body.Close()
	foundThread := false
	foundStateless := false
	for _, item := range listedRuns {
		id, _ := item["id"].(string)
		if id == threadRunID {
			foundThread = true
		}
		if id == statelessRunID {
			foundStateless = true
		}
	}
	if foundThread || foundStateless {
		t.Fatalf("deleted runs should not appear in list, foundThread=%v foundStateless=%v", foundThread, foundStateless)
	}
}

func TestSearchWaitAndThreadState(t *testing.T) {
	ts, client := newClientServer(t)

	createAssistantResp := doJSON(t, client, http.MethodPost, ts.URL+"/api/v1/assistants", map[string]any{
		"deployment_id": "dep_default",
		"graph_id":      "graph_search",
		"version":       "v-search",
	}, "test-key")
	if createAssistantResp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(createAssistantResp.Body)
		t.Fatalf("create assistant status=%d body=%s", createAssistantResp.StatusCode, string(body))
	}
	var assistant map[string]any
	if err := json.NewDecoder(createAssistantResp.Body).Decode(&assistant); err != nil {
		t.Fatal(err)
	}
	createAssistantResp.Body.Close()
	assistantID, _ := assistant["id"].(string)
	if assistantID == "" {
		t.Fatal("missing assistant id")
	}

	searchAssistantsResp := doJSON(t, client, http.MethodPost, ts.URL+"/api/v1/assistants/search", map[string]any{
		"query": "graph_search",
	}, "test-key")
	if searchAssistantsResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(searchAssistantsResp.Body)
		t.Fatalf("assistants/search status=%d body=%s", searchAssistantsResp.StatusCode, string(body))
	}
	var assistantSearch map[string]any
	if err := json.NewDecoder(searchAssistantsResp.Body).Decode(&assistantSearch); err != nil {
		t.Fatal(err)
	}
	searchAssistantsResp.Body.Close()
	if items, _ := assistantSearch["items"].([]any); len(items) == 0 {
		t.Fatalf("assistants/search expected at least one item, got %#v", assistantSearch)
	}

	createThreadResp := doJSON(t, client, http.MethodPost, ts.URL+"/api/v1/threads", map[string]any{
		"assistant_id": assistantID,
		"metadata":     map[string]any{"source": "parity"},
	}, "test-key")
	if createThreadResp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(createThreadResp.Body)
		t.Fatalf("create thread status=%d body=%s", createThreadResp.StatusCode, string(body))
	}
	var thread map[string]any
	if err := json.NewDecoder(createThreadResp.Body).Decode(&thread); err != nil {
		t.Fatal(err)
	}
	createThreadResp.Body.Close()
	threadID, _ := thread["id"].(string)
	if threadID == "" {
		t.Fatal("missing thread id")
	}

	searchThreadsResp := doJSON(t, client, http.MethodPost, ts.URL+"/api/v1/threads/search", map[string]any{
		"query":        threadID,
		"assistant_id": assistantID,
	}, "test-key")
	if searchThreadsResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(searchThreadsResp.Body)
		t.Fatalf("threads/search status=%d body=%s", searchThreadsResp.StatusCode, string(body))
	}
	var threadSearch map[string]any
	if err := json.NewDecoder(searchThreadsResp.Body).Decode(&threadSearch); err != nil {
		t.Fatal(err)
	}
	searchThreadsResp.Body.Close()
	if items, _ := threadSearch["items"].([]any); len(items) == 0 {
		t.Fatalf("threads/search expected at least one item, got %#v", threadSearch)
	}

	updateStateResp := doJSON(t, client, http.MethodPost, ts.URL+"/api/v1/threads/"+threadID+"/state", map[string]any{
		"checkpoint_id": "cp_1",
		"values":        map[string]any{"step": "draft"},
		"metadata":      map[string]any{"source": "manual"},
		"reason":        "initial",
	}, "test-key")
	if updateStateResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(updateStateResp.Body)
		t.Fatalf("thread state update status=%d body=%s", updateStateResp.StatusCode, string(body))
	}
	updateStateResp.Body.Close()

	historyReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/threads/"+threadID+"/history", nil)
	historyReq.Header.Set("X-Api-Key", "test-key")
	historyResp, err := client.Do(historyReq)
	if err != nil {
		t.Fatal(err)
	}
	if historyResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(historyResp.Body)
		t.Fatalf("thread history status=%d body=%s", historyResp.StatusCode, string(body))
	}
	var historyBody map[string]any
	if err := json.NewDecoder(historyResp.Body).Decode(&historyBody); err != nil {
		t.Fatal(err)
	}
	historyResp.Body.Close()
	if items, _ := historyBody["items"].([]any); len(items) == 0 {
		t.Fatalf("thread history expected at least one item, got %#v", historyBody)
	}

	threadWaitResp := doJSON(t, client, http.MethodPost, ts.URL+"/api/v1/threads/"+threadID+"/runs/wait", map[string]any{
		"assistant_id": assistantID,
	}, "test-key")
	if threadWaitResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(threadWaitResp.Body)
		t.Fatalf("thread runs/wait status=%d body=%s", threadWaitResp.StatusCode, string(body))
	}
	var waitedThreadRun map[string]any
	if err := json.NewDecoder(threadWaitResp.Body).Decode(&waitedThreadRun); err != nil {
		t.Fatal(err)
	}
	threadWaitResp.Body.Close()
	if status, _ := waitedThreadRun["status"].(string); status != "success" {
		t.Fatalf("thread wait expected success, got %q", status)
	}

	statelessWaitResp := doJSON(t, client, http.MethodPost, ts.URL+"/api/v1/runs/wait", map[string]any{
		"assistant_id": assistantID,
	}, "test-key")
	if statelessWaitResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(statelessWaitResp.Body)
		t.Fatalf("runs/wait status=%d body=%s", statelessWaitResp.StatusCode, string(body))
	}
	var waitedRun map[string]any
	if err := json.NewDecoder(statelessWaitResp.Body).Decode(&waitedRun); err != nil {
		t.Fatal(err)
	}
	statelessWaitResp.Body.Close()
	if status, _ := waitedRun["status"].(string); status != "success" {
		t.Fatalf("runs/wait expected success, got %q", status)
	}

	createRunResp := doJSON(t, client, http.MethodPost, ts.URL+"/api/v1/runs", map[string]any{
		"assistant_id": assistantID,
	}, "test-key")
	if createRunResp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(createRunResp.Body)
		t.Fatalf("create run status=%d body=%s", createRunResp.StatusCode, string(body))
	}
	var createdRun map[string]any
	if err := json.NewDecoder(createRunResp.Body).Decode(&createdRun); err != nil {
		t.Fatal(err)
	}
	createRunResp.Body.Close()
	runID, _ := createdRun["id"].(string)
	if runID == "" {
		t.Fatal("missing run id")
	}

	waitExistingResp := doJSON(t, client, http.MethodPost, ts.URL+"/api/v1/runs/"+runID+"/wait", map[string]any{}, "test-key")
	if waitExistingResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(waitExistingResp.Body)
		t.Fatalf("runs/{run_id}/wait status=%d body=%s", waitExistingResp.StatusCode, string(body))
	}
	var waitedExisting map[string]any
	if err := json.NewDecoder(waitExistingResp.Body).Decode(&waitedExisting); err != nil {
		t.Fatal(err)
	}
	waitExistingResp.Body.Close()
	if status, _ := waitedExisting["status"].(string); status != "success" {
		t.Fatalf("runs/{run_id}/wait expected success, got %q", status)
	}
}

func TestRunStreamResumeAndReplay(t *testing.T) {
	ts, client := newClientServer(t)

	threadResp := doJSON(t, client, http.MethodPost, ts.URL+"/api/v1/threads", map[string]any{}, "test-key")
	defer threadResp.Body.Close()
	var thread struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(threadResp.Body).Decode(&thread); err != nil {
		t.Fatal(err)
	}

	streamResp := doJSON(t, client, http.MethodPost, ts.URL+"/api/v1/threads/"+thread.ID+"/runs/stream", map[string]any{}, "test-key")
	defer streamResp.Body.Close()
	runID := findRunID(t, streamResp.Body)
	if runID == "" {
		t.Fatal("run id not found in stream")
	}

	replayReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/threads/"+thread.ID+"/runs/"+runID+"/stream", nil)
	replayReq.Header.Set("X-Api-Key", "test-key")
	replayReq.Header.Set("Last-Event-ID", "-1")
	replayResp, err := client.Do(replayReq)
	if err != nil {
		t.Fatal(err)
	}
	defer replayResp.Body.Close()
	data, _ := io.ReadAll(replayResp.Body)
	if !strings.Contains(string(data), "id: 1") {
		t.Fatalf("expected replay to include id 1, got: %s", string(data))
	}

	resumeReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/threads/"+thread.ID+"/runs/"+runID+"/stream", nil)
	resumeReq.Header.Set("X-Api-Key", "test-key")
	resumeReq.Header.Set("Last-Event-ID", "1")
	resumeResp, err := client.Do(resumeReq)
	if err != nil {
		t.Fatal(err)
	}
	defer resumeResp.Body.Close()
	resumed, _ := io.ReadAll(resumeResp.Body)
	if strings.Contains(string(resumed), "id: 1") {
		t.Fatalf("expected resume-from-1 to skip id 1, got: %s", string(resumed))
	}
	if !strings.Contains(string(resumed), "id: 2") {
		t.Fatalf("expected resume-from-1 to include id 2, got: %s", string(resumed))
	}

	otherThreadResp := doJSON(t, client, http.MethodPost, ts.URL+"/api/v1/threads", map[string]any{}, "test-key")
	defer otherThreadResp.Body.Close()
	var otherThread struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(otherThreadResp.Body).Decode(&otherThread); err != nil {
		t.Fatal(err)
	}

	wrongThreadJoinReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/threads/"+otherThread.ID+"/runs/"+runID+"/stream", nil)
	wrongThreadJoinReq.Header.Set("X-Api-Key", "test-key")
	wrongThreadJoinResp, err := client.Do(wrongThreadJoinReq)
	if err != nil {
		t.Fatal(err)
	}
	if wrongThreadJoinResp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(wrongThreadJoinResp.Body)
		t.Fatalf("expected 404 for wrong-thread stream join, got %d body=%s", wrongThreadJoinResp.StatusCode, string(body))
	}
	wrongThreadJoinResp.Body.Close()

	missingRunJoinReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/runs/run_missing/stream", nil)
	missingRunJoinReq.Header.Set("X-Api-Key", "test-key")
	missingRunJoinResp, err := client.Do(missingRunJoinReq)
	if err != nil {
		t.Fatal(err)
	}
	if missingRunJoinResp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(missingRunJoinResp.Body)
		t.Fatalf("expected 404 for missing run stream join, got %d body=%s", missingRunJoinResp.StatusCode, string(body))
	}
	missingRunJoinResp.Body.Close()
}

func TestCancelInterruptAndRollback(t *testing.T) {
	ts, client := newClientServer(t)

	threadResp := doJSON(t, client, http.MethodPost, ts.URL+"/api/v1/threads", map[string]any{}, "test-key")
	defer threadResp.Body.Close()
	var thread struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(threadResp.Body).Decode(&thread)

	streamResp := doJSON(t, client, http.MethodPost, ts.URL+"/api/v1/threads/"+thread.ID+"/runs/stream", map[string]any{}, "test-key")
	defer streamResp.Body.Close()
	runID := findRunID(t, streamResp.Body)

	interrupt := doJSON(t, client, http.MethodPost, ts.URL+"/api/v1/threads/"+thread.ID+"/runs/"+runID+"/cancel?action=interrupt", map[string]any{}, "test-key")
	if interrupt.StatusCode != http.StatusOK {
		t.Fatalf("interrupt status=%d", interrupt.StatusCode)
	}
	interrupt.Body.Close()

	getInterruptedReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/runs/"+runID, nil)
	getInterruptedReq.Header.Set("X-Api-Key", "test-key")
	getInterruptedResp, err := client.Do(getInterruptedReq)
	if err != nil {
		t.Fatal(err)
	}
	if getInterruptedResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(getInterruptedResp.Body)
		t.Fatalf("get interrupted run status=%d body=%s", getInterruptedResp.StatusCode, string(body))
	}
	var interruptedRun map[string]any
	if err := json.NewDecoder(getInterruptedResp.Body).Decode(&interruptedRun); err != nil {
		t.Fatal(err)
	}
	getInterruptedResp.Body.Close()
	if status, _ := interruptedRun["status"].(string); status != "interrupted" {
		t.Fatalf("expected interrupted status, got %q", status)
	}

	streamResp2 := doJSON(t, client, http.MethodPost, ts.URL+"/api/v1/threads/"+thread.ID+"/runs/stream", map[string]any{}, "test-key")
	defer streamResp2.Body.Close()
	runID2 := findRunID(t, streamResp2.Body)

	rollback := doJSON(t, client, http.MethodPost, ts.URL+"/api/v1/threads/"+thread.ID+"/runs/"+runID2+"/cancel?action=rollback", map[string]any{}, "test-key")
	if rollback.StatusCode != http.StatusOK {
		t.Fatalf("rollback status=%d", rollback.StatusCode)
	}
	rollback.Body.Close()

	getRolledBackReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/runs/"+runID2, nil)
	getRolledBackReq.Header.Set("X-Api-Key", "test-key")
	getRolledBackResp, err := client.Do(getRolledBackReq)
	if err != nil {
		t.Fatal(err)
	}
	if getRolledBackResp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(getRolledBackResp.Body)
		t.Fatalf("expected 404 after rollback, got %d body=%s", getRolledBackResp.StatusCode, string(body))
	}
	getRolledBackResp.Body.Close()
}

func TestA2AAndMCP(t *testing.T) {
	ts, client := newClientServer(t)

	a2aBody := map[string]any{"jsonrpc": "2.0", "id": "1", "method": "message/send", "params": map[string]any{"contextId": "thread_x"}}
	a2aResp := doJSON(t, client, http.MethodPost, ts.URL+"/a2a/asst_1", a2aBody, "test-key")
	if a2aResp.StatusCode != http.StatusOK {
		t.Fatalf("a2a status=%d", a2aResp.StatusCode)
	}
	var a2aResult map[string]any
	if err := json.NewDecoder(a2aResp.Body).Decode(&a2aResult); err != nil {
		t.Fatal(err)
	}
	resultObj, _ := a2aResult["result"].(map[string]any)
	if got, _ := resultObj["thread_id"].(string); got != "thread_x" {
		t.Fatalf("expected thread_id=thread_x, got %q", got)
	}
	taskID, _ := resultObj["task_id"].(string)
	if taskID == "" {
		t.Fatalf("expected task_id in message/send result, got %#v", resultObj)
	}
	a2aResp.Body.Close()

	a2aStreamBody := map[string]any{"jsonrpc": "2.0", "id": "stream1", "method": "message/stream", "params": map[string]any{"message": map[string]any{"contextId": "thread_x"}}}
	a2aStreamResp := doJSON(t, client, http.MethodPost, ts.URL+"/a2a/asst_1", a2aStreamBody, "test-key")
	if a2aStreamResp.StatusCode != http.StatusOK {
		t.Fatalf("a2a stream status=%d", a2aStreamResp.StatusCode)
	}
	streamBytes, _ := io.ReadAll(a2aStreamResp.Body)
	a2aStreamResp.Body.Close()
	if !strings.Contains(string(streamBytes), "event: message_received") {
		t.Fatalf("expected a2a stream event, got: %s", string(streamBytes))
	}

	a2aCancelBody := map[string]any{"jsonrpc": "2.0", "id": "cancel1", "method": "tasks/cancel", "params": map[string]any{"taskId": taskID}}
	a2aCancelResp := doJSON(t, client, http.MethodPost, ts.URL+"/a2a/asst_1", a2aCancelBody, "test-key")
	if a2aCancelResp.StatusCode != http.StatusOK {
		t.Fatalf("a2a cancel status=%d", a2aCancelResp.StatusCode)
	}
	var a2aCancelResult map[string]any
	if err := json.NewDecoder(a2aCancelResp.Body).Decode(&a2aCancelResult); err != nil {
		t.Fatal(err)
	}
	a2aCancelResp.Body.Close()
	cancelResultObj, _ := a2aCancelResult["result"].(map[string]any)
	if got, _ := cancelResultObj["status"].(string); got != "cancelled" {
		t.Fatalf("expected tasks/cancel to set cancelled status, got %#v", a2aCancelResult)
	}

	a2aGetBody := map[string]any{"jsonrpc": "2.0", "id": "get1", "method": "tasks/get", "params": map[string]any{"taskId": taskID}}
	a2aGetResp := doJSON(t, client, http.MethodPost, ts.URL+"/a2a/asst_1", a2aGetBody, "test-key")
	if a2aGetResp.StatusCode != http.StatusOK {
		t.Fatalf("a2a tasks/get status=%d", a2aGetResp.StatusCode)
	}
	var a2aGetResult map[string]any
	if err := json.NewDecoder(a2aGetResp.Body).Decode(&a2aGetResult); err != nil {
		t.Fatal(err)
	}
	a2aGetResp.Body.Close()
	getResultObj, _ := a2aGetResult["result"].(map[string]any)
	if got, _ := getResultObj["task_id"].(string); got != taskID {
		t.Fatalf("expected task_id %q, got %q", taskID, got)
	}

	a2aMissingGetBody := map[string]any{"jsonrpc": "2.0", "id": "get-missing", "method": "tasks/get", "params": map[string]any{"taskId": "task_missing"}}
	a2aMissingGetResp := doJSON(t, client, http.MethodPost, ts.URL+"/a2a/asst_1", a2aMissingGetBody, "test-key")
	if a2aMissingGetResp.StatusCode != http.StatusOK {
		t.Fatalf("a2a tasks/get missing status=%d", a2aMissingGetResp.StatusCode)
	}
	var a2aMissingGetResult map[string]any
	if err := json.NewDecoder(a2aMissingGetResp.Body).Decode(&a2aMissingGetResult); err != nil {
		t.Fatal(err)
	}
	a2aMissingGetResp.Body.Close()
	if errObj, ok := a2aMissingGetResult["error"].(map[string]any); !ok {
		t.Fatalf("expected missing task error response, got %#v", a2aMissingGetResult)
	} else if code, ok := errObj["code"].(float64); !ok || int(code) != -32004 {
		t.Fatalf("unexpected missing task error code: %#v", errObj["code"])
	}

	agentCardReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/a2a/asst_1/.well-known/agent-card.json", nil)
	agentCardReq.Header.Set("X-Api-Key", "test-key")
	agentCardResp, err := client.Do(agentCardReq)
	if err != nil {
		t.Fatal(err)
	}
	if agentCardResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(agentCardResp.Body)
		t.Fatalf("agent-card status=%d body=%s", agentCardResp.StatusCode, string(body))
	}
	agentCardResp.Body.Close()

	mcpResp := doJSON(t, client, http.MethodPost, ts.URL+"/mcp", map[string]any{"jsonrpc": "2.0", "id": "1", "method": "initialize"}, "test-key")
	if mcpResp.StatusCode != http.StatusOK {
		t.Fatalf("mcp status=%d", mcpResp.StatusCode)
	}
	var mcpResult map[string]any
	if err := json.NewDecoder(mcpResp.Body).Decode(&mcpResult); err != nil {
		t.Fatal(err)
	}
	mcpResp.Body.Close()

	mcpTerminate := doJSON(t, client, http.MethodPost, ts.URL+"/mcp", map[string]any{"jsonrpc": "2.0", "id": "2", "method": "session/terminate"}, "test-key")
	if mcpTerminate.StatusCode != http.StatusOK {
		t.Fatalf("mcp terminate status=%d", mcpTerminate.StatusCode)
	}
	var terminateResult map[string]any
	if err := json.NewDecoder(mcpTerminate.Body).Decode(&terminateResult); err != nil {
		t.Fatal(err)
	}
	termObj, _ := terminateResult["result"].(map[string]any)
	if compatibility, _ := termObj["compatibility"].(string); compatibility != "no-op" {
		t.Fatalf("expected compatibility no-op, got %q", compatibility)
	}
	mcpTerminate.Body.Close()

	a2aV1Resp := doJSON(t, client, http.MethodPost, ts.URL+"/api/v1/a2a/asst_1", a2aBody, "test-key")
	if a2aV1Resp.StatusCode != http.StatusOK {
		t.Fatalf("api/v1 a2a status=%d", a2aV1Resp.StatusCode)
	}
	var a2aV1Result map[string]any
	if err := json.NewDecoder(a2aV1Resp.Body).Decode(&a2aV1Result); err != nil {
		t.Fatal(err)
	}
	a2aV1Resp.Body.Close()
	a2aV1Obj, _ := a2aV1Result["result"].(map[string]any)
	if got, _ := a2aV1Obj["thread_id"].(string); got != "thread_x" {
		t.Fatalf("expected api/v1 thread_id=thread_x, got %q", got)
	}

	mcpV1Resp := doJSON(t, client, http.MethodPost, ts.URL+"/api/v1/mcp", map[string]any{"jsonrpc": "2.0", "id": "v1", "method": "initialize"}, "test-key")
	if mcpV1Resp.StatusCode != http.StatusOK {
		t.Fatalf("api/v1 mcp status=%d", mcpV1Resp.StatusCode)
	}
	var mcpV1Result map[string]any
	if err := json.NewDecoder(mcpV1Resp.Body).Decode(&mcpV1Result); err != nil {
		t.Fatal(err)
	}
	mcpV1Resp.Body.Close()
	if _, ok := mcpV1Result["result"].(map[string]any); !ok {
		t.Fatalf("expected api/v1 mcp result object, got %#v", mcpV1Result)
	}
}

func TestStoreStatelessRunsAndSystem(t *testing.T) {
	ts, client := newClientServer(t)

	systemReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/system", nil)
	systemReq.Header.Set("X-Api-Key", "test-key")
	systemResp, err := client.Do(systemReq)
	if err != nil {
		t.Fatal(err)
	}
	if systemResp.StatusCode != http.StatusOK {
		t.Fatalf("system status=%d", systemResp.StatusCode)
	}
	systemResp.Body.Close()

	healthReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/system/health", nil)
	healthReq.Header.Set("X-Api-Key", "test-key")
	healthResp, err := client.Do(healthReq)
	if err != nil {
		t.Fatal(err)
	}
	if healthResp.StatusCode != http.StatusOK {
		t.Fatalf("system health status=%d", healthResp.StatusCode)
	}
	healthResp.Body.Close()

	attentionReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/system/attention", nil)
	attentionReq.Header.Set("X-Api-Key", "test-key")
	attentionResp, err := client.Do(attentionReq)
	if err != nil {
		t.Fatal(err)
	}
	if attentionResp.StatusCode != http.StatusOK {
		t.Fatalf("system attention status=%d", attentionResp.StatusCode)
	}
	attentionResp.Body.Close()

	putResp := doJSON(t, client, http.MethodPost, ts.URL+"/api/v1/store/items", map[string]any{
		"namespace": "ns_conformance",
		"key":       "k1",
		"value":     map[string]any{"hello": "world"},
		"metadata":  map[string]any{"source": "test"},
	}, "test-key")
	if putResp.StatusCode != http.StatusCreated {
		t.Fatalf("store put status=%d", putResp.StatusCode)
	}
	putResp.Body.Close()

	getReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/store/items/ns_conformance/k1", nil)
	getReq.Header.Set("X-Api-Key", "test-key")
	getResp, err := client.Do(getReq)
	if err != nil {
		t.Fatal(err)
	}
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("store get status=%d", getResp.StatusCode)
	}
	getResp.Body.Close()

	listReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/store/items?namespace=ns_conformance", nil)
	listReq.Header.Set("X-Api-Key", "test-key")
	listResp, err := client.Do(listReq)
	if err != nil {
		t.Fatal(err)
	}
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("store list status=%d", listResp.StatusCode)
	}
	var listed []map[string]any
	if err := json.NewDecoder(listResp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	listResp.Body.Close()
	if len(listed) == 0 {
		t.Fatalf("store list empty")
	}

	deleteReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/store/items/ns_conformance/k1", nil)
	deleteReq.Header.Set("X-Api-Key", "test-key")
	deleteResp, err := client.Do(deleteReq)
	if err != nil {
		t.Fatal(err)
	}
	if deleteResp.StatusCode != http.StatusOK {
		t.Fatalf("store delete status=%d", deleteResp.StatusCode)
	}
	deleteResp.Body.Close()

	statelessResp := doJSON(t, client, http.MethodPost, ts.URL+"/api/v1/runs/stream", map[string]any{}, "test-key")
	if statelessResp.StatusCode != http.StatusOK {
		t.Fatalf("stateless run stream status=%d", statelessResp.StatusCode)
	}
	runID := findRunID(t, statelessResp.Body)
	statelessResp.Body.Close()
	if runID == "" {
		t.Fatal("stateless run id not found")
	}

	replayReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/runs/"+runID+"/stream", nil)
	replayReq.Header.Set("X-Api-Key", "test-key")
	replayReq.Header.Set("Last-Event-ID", "-1")
	replayResp, err := client.Do(replayReq)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(replayResp.Body)
	replayResp.Body.Close()
	if !strings.Contains(string(data), "id: 1") {
		t.Fatalf("stateless replay missing id 1: %s", string(data))
	}

	cancelResp := doJSON(t, client, http.MethodPost, ts.URL+"/api/v1/runs/"+runID+"/cancel?action=rollback", map[string]any{}, "test-key")
	if cancelResp.StatusCode != http.StatusOK {
		t.Fatalf("stateless rollback status=%d", cancelResp.StatusCode)
	}
	cancelResp.Body.Close()

	getRunReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/runs/"+runID, nil)
	getRunReq.Header.Set("X-Api-Key", "test-key")
	getRunResp, err := client.Do(getRunReq)
	if err != nil {
		t.Fatal(err)
	}
	if getRunResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected run deleted after rollback, status=%d", getRunResp.StatusCode)
	}
	getRunResp.Body.Close()
}

func TestCronsCRUD(t *testing.T) {
	ts, client := newClientServer(t)

	createResp := doJSON(t, client, http.MethodPost, ts.URL+"/api/v1/crons", map[string]any{
		"assistant_id": "asst_default",
		"schedule":     "*/5 * * * *",
		"enabled":      true,
	}, "test-key")
	if createResp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(createResp.Body)
		t.Fatalf("create cron status=%d body=%s", createResp.StatusCode, string(body))
	}
	var created map[string]any
	if err := json.NewDecoder(createResp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	createResp.Body.Close()
	cronID, _ := created["id"].(string)
	if cronID == "" {
		t.Fatal("missing cron id")
	}

	getReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/crons/"+cronID, nil)
	getReq.Header.Set("X-Api-Key", "test-key")
	getResp, err := client.Do(getReq)
	if err != nil {
		t.Fatal(err)
	}
	if getResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(getResp.Body)
		t.Fatalf("get cron status=%d body=%s", getResp.StatusCode, string(body))
	}
	getResp.Body.Close()

	patchResp := doJSON(t, client, http.MethodPatch, ts.URL+"/api/v1/crons/"+cronID, map[string]any{
		"schedule": "*/10 * * * *",
		"enabled":  false,
	}, "test-key")
	if patchResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(patchResp.Body)
		t.Fatalf("patch cron status=%d body=%s", patchResp.StatusCode, string(body))
	}
	var patched map[string]any
	if err := json.NewDecoder(patchResp.Body).Decode(&patched); err != nil {
		t.Fatal(err)
	}
	patchResp.Body.Close()
	if got, _ := patched["schedule"].(string); got != "*/10 * * * *" {
		t.Fatalf("expected updated schedule, got %q", got)
	}
	if enabled, ok := patched["enabled"].(bool); !ok || enabled {
		t.Fatalf("expected enabled=false, got %#v", patched["enabled"])
	}

	listReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/crons", nil)
	listReq.Header.Set("X-Api-Key", "test-key")
	listResp, err := client.Do(listReq)
	if err != nil {
		t.Fatal(err)
	}
	if listResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(listResp.Body)
		t.Fatalf("list crons status=%d body=%s", listResp.StatusCode, string(body))
	}
	var listed []map[string]any
	if err := json.NewDecoder(listResp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	listResp.Body.Close()
	found := false
	for _, item := range listed {
		if id, _ := item["id"].(string); id == cronID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("cron %s not found in list", cronID)
	}

	deleteReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/crons/"+cronID, nil)
	deleteReq.Header.Set("X-Api-Key", "test-key")
	deleteResp, err := client.Do(deleteReq)
	if err != nil {
		t.Fatal(err)
	}
	if deleteResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(deleteResp.Body)
		t.Fatalf("delete cron status=%d body=%s", deleteResp.StatusCode, string(body))
	}
	deleteResp.Body.Close()

	getAfterDeleteReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/crons/"+cronID, nil)
	getAfterDeleteReq.Header.Set("X-Api-Key", "test-key")
	getAfterDeleteResp, err := client.Do(getAfterDeleteReq)
	if err != nil {
		t.Fatal(err)
	}
	if getAfterDeleteResp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(getAfterDeleteResp.Body)
		t.Fatalf("expected 404 after delete, got %d body=%s", getAfterDeleteResp.StatusCode, string(body))
	}
	getAfterDeleteResp.Body.Close()
}

func doJSON(t *testing.T, client *http.Client, method, url string, body map[string]any, apiKey string) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", apiKey)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func findRunID(t *testing.T, body io.Reader) string {
	t.Helper()
	s := bufio.NewScanner(body)
	for s.Scan() {
		line := s.Text()
		if strings.Contains(line, `"id":"run_`) {
			idx := strings.Index(line, `"id":"`)
			if idx < 0 {
				continue
			}
			part := line[idx+6:]
			end := strings.Index(part, `"`)
			if end < 0 {
				continue
			}
			return part[:end]
		}
	}
	if err := s.Err(); err != nil && err != os.ErrClosed {
		t.Fatalf("scan stream: %v", err)
	}
	return ""
}
