"use client";

import { useEffect, useMemo, useState } from "react";
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
import { compactId } from "../../lib/format";
import { useTableQueryState } from "../../lib/table-query";
import { initialDataState } from "../../lib/ui-types";

type Assistant = {
  id: string;
  deployment_id: string;
  graph_id: string;
  version: string;
};

export default function AssistantsPage() {
  const [state, setState] = useState(initialDataState<Assistant>());
  const [loadError, setLoadError] = useState<PortalError | null>(null);
  const [selectedID, setSelectedID] = useState("");

  const { state: table, setState: setTable } = useTableQueryState({ sort: "id", order: "asc", page: 1, pageSize: 12 });

  useEffect(() => {
    void loadAssistants();
  }, []);

  const selected = useMemo(() => state.items.find((item) => item.id === selectedID) || null, [selectedID, state.items]);

  const visible = useMemo(() => {
    const q = table.q.trim().toLowerCase();
    const filtered = q
      ? state.items.filter((item) => [item.id, item.deployment_id, item.graph_id, item.version].join(" ").toLowerCase().includes(q))
      : state.items;

    const sorted = [...filtered].sort((a, b) => {
      const key = table.sort || "id";
      const av = `${(a as Record<string, string | undefined>)[key] || ""}`;
      const bv = `${(b as Record<string, string | undefined>)[key] || ""}`;
      const cmp = av.localeCompare(bv);
      return table.order === "asc" ? cmp : -cmp;
    });

    const start = (table.page - 1) * table.pageSize;
    return { total: sorted.length, rows: sorted.slice(start, start + table.pageSize) };
  }, [state.items, table]);

  async function loadAssistants() {
    setState((prev) => ({ ...prev, loading: true, error: "" }));
    setLoadError(null);
    try {
      const rows = await fetchPortalJSON<Assistant[]>("/api/platform/data/api/v1/assistants");
      setState({ loading: false, error: "", items: rows });
      if (rows.length > 0) {
        setSelectedID((prev) => prev || rows[0].id);
      }
    } catch (err) {
      const normalized = err as PortalError;
      setState((prev) => ({ ...prev, loading: false, error: normalized.message }));
      setLoadError(normalized);
    }
  }

  const columns: ColumnDef<Assistant>[] = [
    { key: "id", header: "Assistant", render: (row) => <code>{compactId(row.id, 10)}</code> },
    { key: "deployment", header: "Deployment", render: (row) => <code>{compactId(row.deployment_id, 10)}</code> },
    { key: "graph", header: "Graph", render: (row) => row.graph_id },
    { key: "version", header: "Version", render: (row) => row.version }
  ];

  return (
    <main className="ui-panel ui-stack">
      <PageHeader title="Assistants" subtitle="Graph versions, deployment bindings, and quick details for active assistant definitions." />

      <Toolbar>
        <Field id="asst-search" label="Search" value={table.q} onChange={(e) => setTable({ q: e.target.value, page: 1 })} placeholder="assistant, deployment, graph" />
        <Select id="asst-sort" label="Sort" value={table.sort} onChange={(e) => setTable({ sort: e.target.value })}>
          <option value="id">Assistant</option>
          <option value="deployment_id">Deployment</option>
          <option value="graph_id">Graph</option>
          <option value="version">Version</option>
        </Select>
        <Select id="asst-order" label="Order" value={table.order} onChange={(e) => setTable({ order: e.target.value as "asc" | "desc" })}>
          <option value="asc">Ascending</option>
          <option value="desc">Descending</option>
        </Select>
      </Toolbar>

      <div className="ui-row" style={{ justifyContent: "space-between" }}>
        <span className="ui-muted">{visible.total} assistant(s)</span>
        <div className="ui-row">
          <button className="ui-button ui-button--secondary" type="button" onClick={() => void loadAssistants()}>
            Refresh
          </button>
          <button className="ui-button ui-button--ghost" type="button" onClick={() => setTable({ page: Math.max(1, table.page - 1) })} disabled={table.page <= 1}>
            Prev
          </button>
          <span className="ui-muted">Page {table.page}</span>
          <button
            className="ui-button ui-button--ghost"
            type="button"
            onClick={() => setTable({ page: table.page + 1 })}
            disabled={table.page * table.pageSize >= visible.total}
          >
            Next
          </button>
        </div>
      </div>

      {loadError ? (
        <ErrorState
          title={loadError.title}
          message={loadError.message}
          retry={() => void loadAssistants()}
          actionLabel={loadError.actionLabel}
          actionHref={loadError.actionHref}
        />
      ) : null}

      {!loadError && state.loading ? <InlineAlert type="info">Loading assistants...</InlineAlert> : null}

      {!loadError && !state.loading && visible.rows.length === 0 ? (
        <EmptyState title="No assistants found" description="Create or deploy a graph to register assistants." />
      ) : null}

      {!loadError && visible.rows.length > 0 ? (
        <SplitPane
          left={
            <DataTable
              columns={columns}
              rows={visible.rows}
              onRowClick={(row) => setSelectedID(row.id)}
              selectedRow={(row) => row.id === selectedID}
            />
          }
          right={
            <section className="ui-subpanel ui-stack">
              <h2 style={{ margin: 0 }}>Assistant details</h2>
              {selected ? (
                <>
                  <p className="ui-muted">ID: {selected.id}</p>
                  <p className="ui-muted">Deployment: {selected.deployment_id}</p>
                  <p className="ui-muted">Graph: {selected.graph_id}</p>
                  <p className="ui-muted">Version: {selected.version}</p>
                </>
              ) : (
                <EmptyState title="Select an assistant" description="Choose a row to inspect details." />
              )}
            </section>
          }
        />
      ) : null}
    </main>
  );
}
