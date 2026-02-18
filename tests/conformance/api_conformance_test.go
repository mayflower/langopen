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
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/docs status = %d", resp.StatusCode)
	}

	resp, err = client.Get(ts.URL + "/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/openapi.json status = %d", resp.StatusCode)
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
	a2aResp.Body.Close()

	mcpResp := doJSON(t, client, http.MethodPost, ts.URL+"/mcp", map[string]any{"jsonrpc": "2.0", "id": "1", "method": "initialize"}, "test-key")
	if mcpResp.StatusCode != http.StatusOK {
		t.Fatalf("mcp status=%d", mcpResp.StatusCode)
	}
	mcpResp.Body.Close()
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
