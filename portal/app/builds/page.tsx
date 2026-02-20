"use client";

import { useEffect, useMemo, useState } from "react";
import { Button } from "../../components/ui/Button";
import { CodeBlock } from "../../components/ui/CodeBlock";
import { DataTable, type ColumnDef } from "../../components/ui/DataTable";
import { EmptyState } from "../../components/ui/EmptyState";
import { ErrorState } from "../../components/ui/ErrorState";
import { Field } from "../../components/ui/Field";
import { InlineAlert } from "../../components/ui/InlineAlert";
import { PageHeader } from "../../components/ui/PageHeader";
import { Select } from "../../components/ui/Select";
import { SplitPane } from "../../components/ui/SplitPane";
import { StatusChip } from "../../components/ui/StatusChip";
import { Toolbar } from "../../components/ui/Toolbar";
import { fetchPortalJSON, type PortalError } from "../../lib/client-api";
import { compactId, formatDateTime } from "../../lib/format";
import { useTableQueryState } from "../../lib/table-query";
import { initialDataState } from "../../lib/ui-types";

type Build = {
  id: string;
  deployment_id: string;
  commit_sha: string;
  status: string;
  image_digest?: string;
  logs_ref?: string;
  created_at: string;
};

const DEFAULT_PROJECT = "proj_default";

export default function BuildsPage() {
  const [projectID, setProjectID] = useState(DEFAULT_PROJECT);
  const [state, setState] = useState(initialDataState<Build>());
  const [selectedBuildID, setSelectedBuildID] = useState("");
  const [logs, setLogs] = useState("");
  const [logsLoading, setLogsLoading] = useState(false);
  const [loadError, setLoadError] = useState<PortalError | null>(null);

  const { state: table, setState: setTable } = useTableQueryState({ sort: "created_at", order: "desc", page: 1, pageSize: 10 });

  useEffect(() => {
    void loadBuilds();
  }, [projectID]);

  const selectedBuild = useMemo(() => state.items.find((item) => item.id === selectedBuildID) || null, [selectedBuildID, state.items]);

  const visible = useMemo(() => {
    const q = table.q.trim().toLowerCase();
    const filtered = q
      ? state.items.filter((item) => [item.id, item.deployment_id, item.commit_sha, item.status].join(" ").toLowerCase().includes(q))
      : state.items;

    const sorted = [...filtered].sort((a, b) => {
      const key = table.sort || "created_at";
      const av = `${(a as Record<string, string | undefined>)[key] || ""}`;
      const bv = `${(b as Record<string, string | undefined>)[key] || ""}`;
      const cmp = av.localeCompare(bv);
      return table.order === "asc" ? cmp : -cmp;
    });

    const start = (table.page - 1) * table.pageSize;
    return {
      total: sorted.length,
      rows: sorted.slice(start, start + table.pageSize)
    };
  }, [state.items, table]);

  async function loadBuilds() {
    setState((prev) => ({ ...prev, loading: true, error: "" }));
    setLoadError(null);
    try {
      const rows = await fetchPortalJSON<Build[]>(`/api/platform/control/internal/v1/builds?project_id=${encodeURIComponent(projectID)}`);
      setState({ loading: false, error: "", items: rows });
      if (rows.length > 0 && !rows.some((row) => row.id === selectedBuildID)) {
        setSelectedBuildID(rows[0].id);
      }
      if (rows.length === 0) {
        setSelectedBuildID("");
        setLogs("");
      }
    } catch (err) {
      const normalized = err as PortalError;
      setState((prev) => ({ ...prev, loading: false, error: normalized.message }));
      setLoadError(normalized);
    }
  }

  async function loadBuildLogs(buildID: string) {
    setLogsLoading(true);
    setSelectedBuildID(buildID);
    try {
      const body = await fetchPortalJSON<{ logs?: string; logs_ref?: string }>(
        `/api/platform/control/internal/v1/builds/${encodeURIComponent(buildID)}/logs?project_id=${encodeURIComponent(projectID)}`
      );
      setLogs(body.logs || body.logs_ref || "No logs available yet.");
    } catch (err) {
      const normalized = err as PortalError;
      setLogs(`${normalized.title}\n${normalized.message}`);
    } finally {
      setLogsLoading(false);
    }
  }

  const columns: ColumnDef<Build>[] = [
    { key: "id", header: "Build", render: (row) => <code>{compactId(row.id, 10)}</code> },
    { key: "deployment", header: "Deployment", render: (row) => <code>{compactId(row.deployment_id, 10)}</code> },
    { key: "commit", header: "Commit", render: (row) => <code>{compactId(row.commit_sha, 8)}</code> },
    { key: "status", header: "Status", render: (row) => <StatusChip status={row.status} /> },
    { key: "digest", header: "Digest", render: (row) => <code>{compactId(row.image_digest || "-", 12)}</code> },
    { key: "created", header: "Created", render: (row) => formatDateTime(row.created_at) },
    {
      key: "action",
      header: "Logs",
      render: (row) => (
        <Button
          type="button"
          variant="secondary"
          onClick={(e) => {
            e.stopPropagation();
            void loadBuildLogs(row.id);
          }}
          busy={logsLoading && selectedBuildID === row.id}
        >
          View
        </Button>
      )
    }
  ];

  return (
    <main className="ui-panel ui-stack">
      <PageHeader title="Builds" subtitle="BuildKit jobs with commit-tagged images, status visibility, and inline logs." />

      <Toolbar>
        <Field id="build-project" label="Project" value={projectID} onChange={(e) => setProjectID(e.target.value)} />
        <Field id="build-search" label="Search" value={table.q} onChange={(e) => setTable({ q: e.target.value, page: 1 })} placeholder="id, deployment, commit" />
        <Select id="build-sort" label="Sort" value={table.sort} onChange={(e) => setTable({ sort: e.target.value })}>
          <option value="created_at">Created</option>
          <option value="status">Status</option>
          <option value="deployment_id">Deployment</option>
        </Select>
        <Select id="build-order" label="Order" value={table.order} onChange={(e) => setTable({ order: e.target.value as "asc" | "desc" })}>
          <option value="desc">Descending</option>
          <option value="asc">Ascending</option>
        </Select>
      </Toolbar>

      <div className="ui-row" style={{ justifyContent: "space-between" }}>
        <span className="ui-muted">{visible.total} build(s)</span>
        <div className="ui-row">
          <Button type="button" variant="secondary" onClick={() => void loadBuilds()}>
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
          retry={() => void loadBuilds()}
          actionLabel={loadError.actionLabel}
          actionHref={loadError.actionHref}
        />
      ) : null}

      {!loadError && state.loading ? <InlineAlert type="info">Loading builds...</InlineAlert> : null}

      {!loadError && !state.loading && visible.rows.length === 0 ? (
        <EmptyState title="No builds yet" description="Trigger a build from the deployments page and it will appear here." />
      ) : null}

      {!loadError && visible.rows.length > 0 ? (
        <SplitPane
          left={
            <DataTable
              columns={columns}
              rows={visible.rows}
              onRowClick={(row) => {
                setSelectedBuildID(row.id);
                void loadBuildLogs(row.id);
              }}
              selectedRow={(row) => row.id === selectedBuildID}
            />
          }
          right={
            <section className="ui-stack">
              <div className="ui-subpanel ui-stack">
                <h2 style={{ margin: 0 }}>Build details</h2>
                {selectedBuild ? (
                  <>
                    <div className="ui-row" style={{ justifyContent: "space-between" }}>
                      <span className="ui-muted">{selectedBuild.id}</span>
                      <StatusChip status={selectedBuild.status} />
                    </div>
                    <p className="ui-muted">Deployment: {selectedBuild.deployment_id}</p>
                    <p className="ui-muted">Commit: {selectedBuild.commit_sha}</p>
                    <p className="ui-muted">Created: {formatDateTime(selectedBuild.created_at)}</p>
                    <p className="ui-muted">Digest: {selectedBuild.image_digest || "-"}</p>
                  </>
                ) : (
                  <EmptyState title="Select a build" description="Choose a build row to inspect details and logs." />
                )}
              </div>
              <CodeBlock title={selectedBuildID ? `Logs: ${selectedBuildID}` : "Logs"}>
                {logsLoading ? "Loading logs..." : logs || "Select a build to view logs."}
              </CodeBlock>
            </section>
          }
        />
      ) : null}
    </main>
  );
}
