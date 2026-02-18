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
		"/assistants/{assistant_id}",
		"/threads",
		"/threads/{thread_id}",
		"/threads/{thread_id}/runs/stream",
		"/threads/{thread_id}/runs/{run_id}/stream",
		"/runs/stream",
		"/runs/{run_id}",
		"/store/items",
		"/crons",
		"/a2a/{assistant_id}",
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

	rollback := doJSON(t, client, http.MethodPost, ts.URL+"/api/v1/threads/"+thread.ID+"/runs/"+runID+"/cancel?action=rollback", map[string]any{}, "test-key")
	if rollback.StatusCode != http.StatusOK {
		t.Fatalf("rollback status=%d", rollback.StatusCode)
	}
	rollback.Body.Close()
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

	a2aCancelBody := map[string]any{"jsonrpc": "2.0", "id": "cancel1", "method": "tasks/cancel", "params": map[string]any{"taskId": "task_x"}}
	a2aCancelResp := doJSON(t, client, http.MethodPost, ts.URL+"/a2a/asst_1", a2aCancelBody, "test-key")
	if a2aCancelResp.StatusCode != http.StatusOK {
		t.Fatalf("a2a cancel status=%d", a2aCancelResp.StatusCode)
	}
	var a2aCancelResult map[string]any
	if err := json.NewDecoder(a2aCancelResp.Body).Decode(&a2aCancelResult); err != nil {
		t.Fatal(err)
	}
	a2aCancelResp.Body.Close()
	if errObj, ok := a2aCancelResult["error"].(map[string]any); !ok {
		t.Fatalf("expected tasks/cancel error response, got %#v", a2aCancelResult)
	} else if code, ok := errObj["code"].(float64); !ok || int(code) != -32001 {
		t.Fatalf("unexpected tasks/cancel error code: %#v", errObj["code"])
	}

	a2aGetBody := map[string]any{"jsonrpc": "2.0", "id": "get1", "method": "tasks/get", "params": map[string]any{"taskId": "task_x"}}
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
	if got, _ := getResultObj["task_id"].(string); got != "task_x" {
		t.Fatalf("expected task_id task_x, got %q", got)
	}

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
