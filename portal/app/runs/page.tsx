"use client";

import { FormEvent, useEffect, useMemo, useState } from "react";
import { Button } from "../../components/ui/Button";
import { CodeBlock } from "../../components/ui/CodeBlock";
import { EmptyState } from "../../components/ui/EmptyState";
import { ErrorState } from "../../components/ui/ErrorState";
import { Field } from "../../components/ui/Field";
import { InlineAlert } from "../../components/ui/InlineAlert";
import { PageHeader } from "../../components/ui/PageHeader";
import { Select } from "../../components/ui/Select";
import { SplitPane } from "../../components/ui/SplitPane";
import { StatusChip } from "../../components/ui/StatusChip";
import { ToastRegion, useToastRegion } from "../../components/ui/ToastRegion";
import { fetchPortalJSON, type PortalError } from "../../lib/client-api";
import { formatDateTime } from "../../lib/format";

type Thread = {
  id: string;
  assistant_id: string;
  updated_at: string;
};

type Assistant = {
  id: string;
  deployment_id: string;
  graph_id: string;
};

type StreamEntry = {
  id: string;
  event: string;
  data: string;
  ts: string;
};

export default function RunsPage() {
  const { toasts, push, remove } = useToastRegion();

  const [threads, setThreads] = useState<Thread[]>([]);
  const [assistants, setAssistants] = useState<Assistant[]>([]);
  const [loadError, setLoadError] = useState<PortalError | null>(null);

  const [threadID, setThreadID] = useState("");
  const [assistantID, setAssistantID] = useState("asst_default");

  const [runID, setRunID] = useState("");
  const [lastEventID, setLastEventID] = useState("-1");
  const [advancedOpen, setAdvancedOpen] = useState(false);

  const [entries, setEntries] = useState<StreamEntry[]>([]);
  const [streaming, setStreaming] = useState(false);
  const [actionMessage, setActionMessage] = useState("");

  useEffect(() => {
    void loadOptions();
  }, []);

  useEffect(() => {
    if (typeof window === "undefined") {
      return;
    }
    const qs = new URLSearchParams(window.location.search);
    const thread = qs.get("thread_id");
    const assistant = qs.get("assistant_id");
    if (thread) {
      setThreadID(thread);
    }
    if (assistant) {
      setAssistantID(assistant);
    }
  }, []);

  const timeline = useMemo(() => [...entries].reverse(), [entries]);
  const latestEvent = entries[entries.length - 1];

  async function loadOptions() {
    setLoadError(null);
    try {
      const [threadRows, assistantRows] = await Promise.all([
        fetchPortalJSON<Thread[]>("/api/platform/data/api/v1/threads"),
        fetchPortalJSON<Assistant[]>("/api/platform/data/api/v1/assistants")
      ]);
      setThreads(threadRows);
      setAssistants(assistantRows);
      if (!threadID && threadRows.length > 0) {
        setThreadID(threadRows[0].id);
      }
      if ((!assistantID || assistantID === "asst_default") && assistantRows.length > 0) {
        setAssistantID(assistantRows[0].id);
      }
    } catch (err) {
      setLoadError(err as PortalError);
    }
  }

  async function startThreadRun(e: FormEvent) {
    e.preventDefault();
    if (!threadID) {
      push("warning", "Select a thread first.");
      return;
    }
    setEntries([]);
    setLastEventID("-1");
    setActionMessage("Starting thread run...");

    try {
      const resp = await fetch(`/api/platform/data/api/v1/threads/${encodeURIComponent(threadID)}/runs/stream`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ multitask_strategy: "enqueue", stream_resumable: true })
      });
      if (!resp.ok || !resp.body) {
        throw await toPortalError(resp, "Unable to start thread run.");
      }
      setStreaming(true);
      await consumeSSE(resp.body, (entry) => {
        appendEntry(entry);
      });
      setActionMessage("Stream closed.");
    } catch (err) {
      const normalized = err as PortalError;
      setActionMessage(normalized.message);
      push("error", normalized.message);
    } finally {
      setStreaming(false);
    }
  }

  async function startStatelessRun(e: FormEvent) {
    e.preventDefault();
    if (!assistantID) {
      push("warning", "Select an assistant first.");
      return;
    }
    setEntries([]);
    setLastEventID("-1");
    setActionMessage("Starting stateless run...");

    try {
      const resp = await fetch("/api/platform/data/api/v1/runs/stream", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ assistant_id: assistantID, multitask_strategy: "enqueue", stream_resumable: true })
      });
      if (!resp.ok || !resp.body) {
        throw await toPortalError(resp, "Unable to start stateless run.");
      }
      setStreaming(true);
      await consumeSSE(resp.body, (entry) => {
        appendEntry(entry);
      });
      setActionMessage("Stream closed.");
    } catch (err) {
      const normalized = err as PortalError;
      setActionMessage(normalized.message);
      push("error", normalized.message);
    } finally {
      setStreaming(false);
    }
  }

  async function reconnect() {
    if (!runID) {
      push("warning", "Run ID is required for reconnect.");
      return;
    }

    setActionMessage("Reconnecting stream...");
    try {
      const path = threadID
        ? `/api/platform/data/api/v1/threads/${encodeURIComponent(threadID)}/runs/${encodeURIComponent(runID)}/stream`
        : `/api/platform/data/api/v1/runs/${encodeURIComponent(runID)}/stream`;
      const resp = await fetch(path, { headers: { "Last-Event-ID": lastEventID || "-1" } });
      if (!resp.ok || !resp.body) {
        throw await toPortalError(resp, "Unable to reconnect stream.");
      }
      setStreaming(true);
      await consumeSSE(resp.body, (entry) => appendEntry(entry));
      setActionMessage("Reconnected stream closed.");
    } catch (err) {
      const normalized = err as PortalError;
      setActionMessage(normalized.message);
      push("error", normalized.message);
    } finally {
      setStreaming(false);
    }
  }

  async function cancel(action: "interrupt" | "rollback") {
    if (!runID) {
      push("warning", "Run ID is required.");
      return;
    }
    const path = threadID
      ? `/api/platform/data/api/v1/threads/${encodeURIComponent(threadID)}/runs/${encodeURIComponent(runID)}/cancel?action=${action}`
      : `/api/platform/data/api/v1/runs/${encodeURIComponent(runID)}/cancel?action=${action}`;

    try {
      const resp = await fetch(path, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: "{}"
      });
      if (!resp.ok) {
        throw await toPortalError(resp, "Cancel failed.");
      }
      appendEntry({ id: "-", event: "cancel", data: action });
      push("success", `Run ${action} request sent.`);
    } catch (err) {
      const normalized = err as PortalError;
      push("error", normalized.message);
    }
  }

  async function deleteRun() {
    if (!runID) {
      push("warning", "Run ID is required.");
      return;
    }
    const path = threadID
      ? `/api/platform/data/api/v1/threads/${encodeURIComponent(threadID)}/runs/${encodeURIComponent(runID)}`
      : `/api/platform/data/api/v1/runs/${encodeURIComponent(runID)}`;

    try {
      const resp = await fetch(path, { method: "DELETE" });
      if (!resp.ok) {
        throw await toPortalError(resp, "Delete run failed.");
      }
      appendEntry({ id: "-", event: "delete", data: runID });
      push("success", "Run deleted.");
    } catch (err) {
      const normalized = err as PortalError;
      push("error", normalized.message);
    }
  }

  function appendEntry(entry: { id: string; event: string; data: string }) {
    setEntries((prev) => [...prev, { ...entry, ts: new Date().toISOString() }]);
    setLastEventID(entry.id);
    const parsed = parseJSON(entry.data);
    if (parsed && typeof parsed.run_id === "string") {
      setRunID(parsed.run_id);
    }
  }

  return (
    <main className="ui-panel ui-stack">
      <PageHeader title="Runs" subtitle="Run-centric operations with live stream timeline, reconnect controls, and safe cancellation actions." />

      {loadError ? (
        <ErrorState
          title={loadError.title}
          message={loadError.message}
          retry={() => void loadOptions()}
          actionLabel={loadError.actionLabel}
          actionHref={loadError.actionHref}
        />
      ) : null}

      {!loadError ? (
        <SplitPane
          left={
            <section className="ui-subpanel ui-stack">
              <h2 style={{ margin: 0 }}>Run controls</h2>
              <form className="ui-stack" onSubmit={startThreadRun}>
                <Select id="run-thread" label="Thread" value={threadID} onChange={(e) => setThreadID(e.target.value)}>
                  <option value="">Select thread</option>
                  {threads.map((thread) => (
                    <option key={thread.id} value={thread.id}>
                      {thread.id}
                    </option>
                  ))}
                </Select>
                <Button type="submit" variant="primary" busy={streaming}>
                  Start thread run
                </Button>
              </form>

              <form className="ui-stack" onSubmit={startStatelessRun}>
                <Select id="run-assistant" label="Assistant" value={assistantID} onChange={(e) => setAssistantID(e.target.value)}>
                  <option value="">Select assistant</option>
                  {assistants.map((assistant) => (
                    <option key={assistant.id} value={assistant.id}>
                      {assistant.id} ({assistant.graph_id})
                    </option>
                  ))}
                </Select>
                <Button type="submit" variant="secondary" busy={streaming}>
                  Start stateless run
                </Button>
              </form>

              <Field id="run-id" label="Run ID" value={runID} onChange={(e) => setRunID(e.target.value)} hint="Auto-populated from stream events." />

              <details open={advancedOpen} onToggle={(e) => setAdvancedOpen((e.currentTarget as HTMLDetailsElement).open)}>
                <summary style={{ cursor: "pointer", marginBottom: 8 }}>Advanced stream controls</summary>
                <div className="ui-stack" style={{ marginTop: 12 }}>
                  <Field id="run-last-event" label="Last-Event-ID" value={lastEventID} onChange={(e) => setLastEventID(e.target.value)} />
                  <Button type="button" variant="ghost" onClick={() => void reconnect()} busy={streaming}>
                    Reconnect + resume
                  </Button>
                </div>
              </details>

              <div className="ui-row">
                <Button type="button" variant="secondary" onClick={() => void cancel("interrupt")}>
                  Interrupt
                </Button>
                <Button type="button" variant="secondary" onClick={() => void cancel("rollback")}>
                  Rollback
                </Button>
                <Button type="button" variant="danger" onClick={() => void deleteRun()}>
                  Delete run
                </Button>
              </div>

              {actionMessage ? <InlineAlert type="info">{actionMessage}</InlineAlert> : null}
            </section>
          }
          right={
            <section className="ui-subpanel ui-stack">
              <div className="ui-row" style={{ justifyContent: "space-between" }}>
                <h2 style={{ margin: 0 }}>Run timeline</h2>
                <StatusChip status={streaming ? "running" : latestEvent?.event || "idle"} />
              </div>

              {entries.length === 0 ? (
                <EmptyState title="No events yet" description="Start a run to see stream events and checkpoint progress." />
              ) : (
                <>
                  <CodeBlock title="Raw stream events">
                    {entries.map((entry) => `id=${entry.id} event=${entry.event} data=${entry.data}`).join("\n")}
                  </CodeBlock>
                  <div className="ui-table-wrap">
                    <table className="ui-table ui-table--dense">
                      <thead>
                        <tr>
                          <th>Time</th>
                          <th>Event</th>
                          <th>Event ID</th>
                          <th>Data</th>
                        </tr>
                      </thead>
                      <tbody>
                        {timeline.map((entry, idx) => (
                          <tr key={`${entry.id}-${idx}`}>
                            <td>{formatDateTime(entry.ts)}</td>
                            <td><StatusChip status={entry.event} /></td>
                            <td><code>{entry.id}</code></td>
                            <td><code>{entry.data}</code></td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                </>
              )}
            </section>
          }
        />
      ) : null}

      <ToastRegion toasts={toasts} remove={remove} />
    </main>
  );
}

async function consumeSSE(stream: ReadableStream<Uint8Array>, onEvent: (entry: { id: string; event: string; data: string }) => void) {
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

  // Some upstreams close without a final blank-line delimiter. Parse trailing chunk on EOF.
  const tail = buffer.trim();
  if (tail) {
    const entry = parseEvent(tail);
    if (entry) {
      onEvent(entry);
    }
  }
}

function parseEvent(chunk: string): { id: string; event: string; data: string } | null {
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

async function toPortalError(resp: Response, fallback: string): Promise<PortalError> {
  const text = await resp.text();
  try {
    const parsed = JSON.parse(text) as { error?: { title?: string; message?: string; action_label?: string; action_href?: string } };
    return {
      status: resp.status,
      title: parsed.error?.title || "Request failed",
      message: parsed.error?.message || fallback,
      actionLabel: parsed.error?.action_label,
      actionHref: parsed.error?.action_href
    };
  } catch {
    return { status: resp.status, title: "Request failed", message: text || fallback };
  }
}
