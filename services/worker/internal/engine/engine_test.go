package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRuntimeRunnerExecutorIncludesRuntimeEnv(t *testing.T) {
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/execute" {
			t.Fatalf("expected /execute, got %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "events": []any{}})
	}))
	defer server.Close()

	exec := runtimeRunnerExecutor{baseURL: server.URL, httpClient: server.Client()}
	_, err := exec.Execute(context.Background(), pendingRun{
		ID:          "run_1",
		ThreadID:    "thread_1",
		AssistantID: "asst_1",
		RuntimeEnv:  map[string]string{"OPENAI_API_KEY": "secret"},
		RuntimeMetadata: map[string]any{
			"graph_id": "agent",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	runtimeEnv, ok := payload["runtime_env"].(map[string]any)
	if !ok {
		t.Fatalf("expected runtime_env object in payload, got %#v", payload["runtime_env"])
	}
	if got := runtimeEnv["OPENAI_API_KEY"]; got != "secret" {
		t.Fatalf("expected runtime env value to be forwarded, got %#v", got)
	}
}

func TestExtractRuntimeEnvIncludesTopLevelAndRuntimeMetadata(t *testing.T) {
	out := extractRuntimeEnv(map[string]any{
		"runtime_env": map[string]any{
			"OPENAI_API_KEY": "top",
		},
		"runtime_metadata": map[string]any{
			"runtime_env": map[string]any{
				"GROQ_API_KEY": "nested",
			},
		},
	})
	if got := out["OPENAI_API_KEY"]; got != "top" {
		t.Fatalf("expected OPENAI_API_KEY=top, got %q", got)
	}
	if got := out["GROQ_API_KEY"]; got != "nested" {
		t.Fatalf("expected GROQ_API_KEY=nested, got %q", got)
	}
}
