"use client";

import Link from "next/link";
import { useEffect, useMemo, useState } from "react";
import { Button } from "../../components/ui/Button";
import { DataTable, type ColumnDef } from "../../components/ui/DataTable";
import { EmptyState } from "../../components/ui/EmptyState";
import { ErrorState } from "../../components/ui/ErrorState";
import { Field } from "../../components/ui/Field";
import { InlineAlert } from "../../components/ui/InlineAlert";
import { PageHeader } from "../../components/ui/PageHeader";
import { Select } from "../../components/ui/Select";
import { SplitPane } from "../../components/ui/SplitPane";
import { Toolbar } from "../../components/ui/Toolbar";
import { fetchPortalJSON, type PortalError } from "../../lib/client-api";
import { compactId, formatDateTime, formatRelativeTime } from "../../lib/format";
import { useTableQueryState } from "../../lib/table-query";
import { initialDataState } from "../../lib/ui-types";

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

export default function ThreadsPage() {
  const [threads, setThreads] = useState(initialDataState<Thread>());
  const [assistants, setAssistants] = useState<Assistant[]>([]);
  const [loadError, setLoadError] = useState<PortalError | null>(null);
  const [assistantFilter, setAssistantFilter] = useState("");
  const [selectedThreadID, setSelectedThreadID] = useState("");

  const { state: table, setState: setTable } = useTableQueryState({ sort: "updated_at", order: "desc", page: 1, pageSize: 12 });

  useEffect(() => {
    void loadData();
  }, []);

  const selected = useMemo(() => threads.items.find((item) => item.id === selectedThreadID) || null, [selectedThreadID, threads.items]);

  const visible = useMemo(() => {
    const q = table.q.trim().toLowerCase();
    const filteredByAssistant = assistantFilter ? threads.items.filter((item) => item.assistant_id === assistantFilter) : threads.items;
    const filtered = q
      ? filteredByAssistant.filter((item) => [item.id, item.assistant_id, item.updated_at].join(" ").toLowerCase().includes(q))
      : filteredByAssistant;

    const sorted = [...filtered].sort((a, b) => {
      const key = table.sort || "updated_at";
      const av = `${(a as Record<string, string | undefined>)[key] || ""}`;
      const bv = `${(b as Record<string, string | undefined>)[key] || ""}`;
      const cmp = av.localeCompare(bv);
      return table.order === "asc" ? cmp : -cmp;
    });

    const start = (table.page - 1) * table.pageSize;
    return { total: sorted.length, rows: sorted.slice(start, start + table.pageSize) };
  }, [threads.items, table, assistantFilter]);

  async function loadData() {
    setThreads((prev) => ({ ...prev, loading: true, error: "" }));
    setLoadError(null);
    try {
      const [threadRows, assistantRows] = await Promise.all([
        fetchPortalJSON<Thread[]>("/api/platform/data/api/v1/threads"),
        fetchPortalJSON<Assistant[]>("/api/platform/data/api/v1/assistants")
      ]);
      setThreads({ loading: false, error: "", items: threadRows });
      setAssistants(assistantRows);
      if (threadRows.length > 0) {
        setSelectedThreadID((prev) => prev || threadRows[0].id);
      }
    } catch (err) {
      const normalized = err as PortalError;
      setThreads((prev) => ({ ...prev, loading: false, error: normalized.message }));
      setLoadError(normalized);
    }
  }

  const columns: ColumnDef<Thread>[] = [
    { key: "id", header: "Thread", render: (row) => <code>{compactId(row.id, 10)}</code> },
    { key: "assistant", header: "Assistant", render: (row) => <code>{compactId(row.assistant_id, 10)}</code> },
    {
      key: "updated",
      header: "Updated",
      render: (row) => (
        <div>
          <div>{formatRelativeTime(row.updated_at)}</div>
          <div className="ui-muted">{formatDateTime(row.updated_at)}</div>
        </div>
      )
    },
    {
      key: "action",
      header: "Run",
      render: (row) => (
        <Link className="ui-button ui-button--secondary" href={`/runs?thread_id=${encodeURIComponent(row.id)}&assistant_id=${encodeURIComponent(row.assistant_id)}`}>
          Open run viewer
        </Link>
      )
    }
  ];

  return (
    <main className="ui-panel ui-stack">
      <PageHeader title="Threads" subtitle="Thread activity with assistant filters and direct handoff to run operations." />

      <Toolbar>
        <Field id="thread-search" label="Search" value={table.q} onChange={(e) => setTable({ q: e.target.value, page: 1 })} placeholder="thread id, assistant" />
        <Select id="thread-assistant" label="Assistant filter" value={assistantFilter} onChange={(e) => setAssistantFilter(e.target.value)}>
          <option value="">All assistants</option>
          {assistants.map((item) => (
            <option key={item.id} value={item.id}>
              {item.id}
            </option>
          ))}
        </Select>
        <Select id="thread-sort" label="Sort" value={table.sort} onChange={(e) => setTable({ sort: e.target.value })}>
          <option value="updated_at">Updated</option>
          <option value="id">Thread</option>
          <option value="assistant_id">Assistant</option>
        </Select>
        <Select id="thread-order" label="Order" value={table.order} onChange={(e) => setTable({ order: e.target.value as "asc" | "desc" })}>
          <option value="desc">Descending</option>
          <option value="asc">Ascending</option>
        </Select>
      </Toolbar>

      <div className="ui-row" style={{ justifyContent: "space-between" }}>
        <span className="ui-muted">{visible.total} thread(s)</span>
        <div className="ui-row">
          <Button type="button" variant="secondary" onClick={() => void loadData()}>
            Refresh
          </Button>
          <Button type="button" variant="ghost" onClick={() => setTable({ page: Math.max(1, table.page - 1) })} disabled={table.page <= 1}>
            Prev
          </Button>
          <span className="ui-muted">Page {table.page}</span>
          <Button
            type="button"
            variant="ghost"
            onClick={() => setTable({ page: table.page + 1 })}
            disabled={table.page * table.pageSize >= visible.total}
          >
            Next
          </Button>
        </div>
      </div>

      {loadError ? (
        <ErrorState
          title={loadError.title}
          message={loadError.message}
          retry={() => void loadData()}
          actionLabel={loadError.actionLabel}
          actionHref={loadError.actionHref}
        />
      ) : null}

      {!loadError && threads.loading ? <InlineAlert type="info">Loading threads...</InlineAlert> : null}

      {!loadError && !threads.loading && visible.rows.length === 0 ? (
        <EmptyState title="No threads found" description="Runs will create thread records that appear here." />
      ) : null}

      {!loadError && visible.rows.length > 0 ? (
        <SplitPane
          left={
            <DataTable
              columns={columns}
              rows={visible.rows}
              onRowClick={(row) => setSelectedThreadID(row.id)}
              selectedRow={(row) => row.id === selectedThreadID}
            />
          }
          right={
            <section className="ui-subpanel ui-stack">
              <h2 style={{ margin: 0 }}>Thread details</h2>
              {selected ? (
                <>
                  <p className="ui-muted">Thread: {selected.id}</p>
                  <p className="ui-muted">Assistant: {selected.assistant_id}</p>
                  <p className="ui-muted">Updated: {formatDateTime(selected.updated_at)}</p>
                  <div className="ui-row">
                    <Link className="ui-button ui-button--primary" href={`/runs?thread_id=${encodeURIComponent(selected.id)}&assistant_id=${encodeURIComponent(selected.assistant_id)}`}>
                      Start/monitor runs
                    </Link>
                  </div>
                </>
              ) : (
                <EmptyState title="Select a thread" description="Choose a row to inspect and open run controls." />
              )}
            </section>
          }
        />
      ) : null}
    </main>
  );
}
