"use client";

import { FormEvent, useMemo, useState } from "react";

type StreamEntry = {
  id: string;
  event: string;
  data: string;
};

export default function RunsPage() {
  const [threadID, setThreadID] = useState("");
  const [assistantID, setAssistantID] = useState("asst_default");
  const [runID, setRunID] = useState("");
  const [lastEventID, setLastEventID] = useState("-1");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [streamLog, setStreamLog] = useState<StreamEntry[]>([]);

  const streamText = useMemo(
    () => streamLog.map((e) => `id=${e.id} event=${e.event} data=${e.data}`).join("\n"),
    [streamLog]
  );

  async function startThreadRun(e: FormEvent) {
    e.preventDefault();
    if (!threadID) {
      setError("thread_id is required");
      return;
    }

    setError("");
    setLoading(true);
    setStreamLog([]);
    setLastEventID("-1");

    try {
      const resp = await fetch(`/api/platform/data/api/v1/threads/${encodeURIComponent(threadID)}/runs/stream`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ multitask_strategy: "enqueue", stream_resumable: true })
      });
      if (!resp.ok || !resp.body) {
        const body = await resp.text();
        throw new Error(`start stream failed (${resp.status}) ${body}`);
      }
      await consumeSSE(resp.body, (entry) => {
        setStreamLog((prev) => [...prev, entry]);
        setLastEventID(entry.id);
        const parsed = parseJSON(entry.data);
        if (!runID && parsed && typeof parsed.run_id === "string") {
          setRunID(parsed.run_id);
        }
      });
    } catch (err) {
      setError(err instanceof Error ? err.message : "start stream failed");
    } finally {
      setLoading(false);
    }
  }

  async function reconnect() {
    if (!runID) {
      setError("run_id is required for reconnect");
      return;
    }

    setError("");
    setLoading(true);
    try {
      const path = threadID
        ? `/api/platform/data/api/v1/threads/${encodeURIComponent(threadID)}/runs/${encodeURIComponent(runID)}/stream`
        : `/api/platform/data/api/v1/runs/${encodeURIComponent(runID)}/stream`;
      const resp = await fetch(path, {
        method: "GET",
        headers: { "Last-Event-ID": lastEventID || "-1" }
      });
      if (!resp.ok || !resp.body) {
        const body = await resp.text();
        throw new Error(`reconnect failed (${resp.status}) ${body}`);
      }
      await consumeSSE(resp.body, (entry) => {
        setStreamLog((prev) => [...prev, entry]);
        setLastEventID(entry.id);
      });
    } catch (err) {
      setError(err instanceof Error ? err.message : "reconnect failed");
    } finally {
      setLoading(false);
    }
  }

  async function startStatelessRun(e: FormEvent) {
    e.preventDefault();
    setError("");
    setLoading(true);
    setStreamLog([]);
    setLastEventID("-1");
    setThreadID("");

    try {
      const resp = await fetch("/api/platform/data/api/v1/runs/stream", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ assistant_id: assistantID, multitask_strategy: "enqueue", stream_resumable: true })
      });
      if (!resp.ok || !resp.body) {
        const body = await resp.text();
        throw new Error(`start stateless stream failed (${resp.status}) ${body}`);
      }
      await consumeSSE(resp.body, (entry) => {
        setStreamLog((prev) => [...prev, entry]);
        setLastEventID(entry.id);
        const parsed = parseJSON(entry.data);
        if (!runID && parsed && typeof parsed.run_id === "string") {
          setRunID(parsed.run_id);
        }
      });
    } catch (err) {
      setError(err instanceof Error ? err.message : "start stateless stream failed");
    } finally {
      setLoading(false);
    }
  }

  async function cancel(action: "interrupt" | "rollback") {
    if (!runID) {
      setError("run_id is required for cancel");
      return;
    }
    setError("");
    const path = threadID
      ? `/api/platform/data/api/v1/threads/${encodeURIComponent(threadID)}/runs/${encodeURIComponent(runID)}/cancel?action=${action}`
      : `/api/platform/data/api/v1/runs/${encodeURIComponent(runID)}/cancel?action=${action}`;

    const resp = await fetch(path, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: "{}"
    });
    if (!resp.ok) {
      const body = await resp.text();
      setError(`cancel failed (${resp.status}) ${body}`);
      return;
    }
    setStreamLog((prev) => [...prev, { id: "-", event: "cancel", data: action }]);
  }

  async function deleteRun() {
    if (!runID) {
      setError("run_id is required for delete");
      return;
    }
    setError("");
    const path = threadID
      ? `/api/platform/data/api/v1/threads/${encodeURIComponent(threadID)}/runs/${encodeURIComponent(runID)}`
      : `/api/platform/data/api/v1/runs/${encodeURIComponent(runID)}`;
    const resp = await fetch(path, { method: "DELETE" });
    if (!resp.ok) {
      const body = await resp.text();
      setError(`delete failed (${resp.status}) ${body}`);
      return;
    }
    setStreamLog((prev) => [...prev, { id: "-", event: "delete", data: runID }]);
  }

  return (
    <main className="hero">
      <h2>Runs</h2>
      <p>Live run viewer with SSE reconnect and Last-Event-ID resume.</p>

      <form onSubmit={startThreadRun} className="row">
        <input placeholder="thread_id" value={threadID} onChange={(e) => setThreadID(e.target.value)} />
        <button disabled={loading} type="submit">Start Thread Run</button>
      </form>

      <form onSubmit={startStatelessRun} className="row">
        <input placeholder="assistant_id" value={assistantID} onChange={(e) => setAssistantID(e.target.value)} />
        <button disabled={loading} type="submit">Start Stateless Run</button>
      </form>

      <div className="row">
        <input placeholder="run_id" value={runID} onChange={(e) => setRunID(e.target.value)} />
        <input placeholder="last_event_id" value={lastEventID} onChange={(e) => setLastEventID(e.target.value)} />
        <button disabled={loading} onClick={reconnect} type="button">Reconnect + Resume</button>
      </div>

      <div className="row">
        <button disabled={loading} onClick={() => cancel("interrupt")} type="button">Interrupt</button>
        <button disabled={loading} onClick={() => cancel("rollback")} type="button">Rollback</button>
        <button disabled={loading} onClick={deleteRun} type="button">Delete</button>
      </div>

      {error ? <p className="warn">{error}</p> : null}
      <pre className="card">{streamText || "No stream events yet."}</pre>
    </main>
  );
}

async function consumeSSE(stream: ReadableStream<Uint8Array>, onEvent: (entry: StreamEntry) => void) {
  const reader = stream.getReader();
  const decoder = new TextDecoder();
  let buffer = "";

  while (true) {
    const { value, done } = await reader.read();
    if (done) {
      break;
    }

    buffer += decoder.decode(value, { stream: true });

    while (true) {
      const boundary = buffer.indexOf("\n\n");
      if (boundary < 0) {
        break;
      }

      const chunk = buffer.slice(0, boundary);
      buffer = buffer.slice(boundary + 2);
      const entry = parseEvent(chunk);
      if (entry) {
        onEvent(entry);
      }
    }
  }
}

function parseEvent(chunk: string): StreamEntry | null {
  const lines = chunk.split(/\n/);
  let id = "";
  let event = "message";
  const data: string[] = [];
  for (const line of lines) {
    if (line.startsWith("id:")) {
      id = line.slice(3).trim();
    } else if (line.startsWith("event:")) {
      event = line.slice(6).trim();
    } else if (line.startsWith("data:")) {
      data.push(line.slice(5).trim());
    }
  }
  if (!id && data.length === 0) {
    return null;
  }
  return { id: id || "-", event, data: data.join("\n") };
}

function parseJSON(input: string): Record<string, unknown> | null {
  try {
    return JSON.parse(input) as Record<string, unknown>;
  } catch {
    return null;
  }
}
