"use client";

import { FormEvent, useEffect, useMemo, useState } from "react";
import { Button } from "../../components/ui/Button";
import { ConfirmDialog } from "../../components/ui/ConfirmDialog";
import { DataTable, type ColumnDef } from "../../components/ui/DataTable";
import { EmptyState } from "../../components/ui/EmptyState";
import { ErrorState } from "../../components/ui/ErrorState";
import { Field } from "../../components/ui/Field";
import { InlineAlert } from "../../components/ui/InlineAlert";
import { PageHeader } from "../../components/ui/PageHeader";
import { Select } from "../../components/ui/Select";
import { StatusChip } from "../../components/ui/StatusChip";
import { ToastRegion, useToastRegion } from "../../components/ui/ToastRegion";
import { Toolbar } from "../../components/ui/Toolbar";
import { fetchPortalJSON, type PortalError } from "../../lib/client-api";
import { compactId, formatDateTime } from "../../lib/format";
import { useTableQueryState } from "../../lib/table-query";
import { initialActionState, initialDataState } from "../../lib/ui-types";

type APIKey = {
  id: string;
  project_id: string;
  name: string;
  created_at: string;
  revoked_at?: string;
};

const DEFAULT_PROJECT = "proj_default";

export default function APIKeysPage() {
  const [projectID, setProjectID] = useState(DEFAULT_PROJECT);
  const [name, setName] = useState("portal");
  const [state, setState] = useState(initialDataState<APIKey>());
  const [loadError, setLoadError] = useState<PortalError | null>(null);
  const [createdKey, setCreatedKey] = useState("");
  const [showCreate, setShowCreate] = useState(false);
  const [action, setAction] = useState(initialActionState());
  const [confirmRevoke, setConfirmRevoke] = useState<string>("");

  const { state: table, setState: setTable } = useTableQueryState({ sort: "created_at", order: "desc", page: 1, pageSize: 12 });
  const { toasts, push, remove } = useToastRegion();

  useEffect(() => {
    void loadKeys();
  }, [projectID]);

  const visible = useMemo(() => {
    const q = table.q.trim().toLowerCase();
    const filtered = q
      ? state.items.filter((item) => [item.id, item.name, item.project_id].join(" ").toLowerCase().includes(q))
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

  async function loadKeys() {
    setState((prev) => ({ ...prev, loading: true, error: "" }));
    setLoadError(null);
    try {
      const rows = await fetchPortalJSON<APIKey[]>(`/api/platform/control/internal/v1/api-keys?project_id=${encodeURIComponent(projectID)}`);
      setState({ loading: false, error: "", items: rows });
    } catch (err) {
      const normalized = err as PortalError;
      setState((prev) => ({ ...prev, loading: false, error: normalized.message }));
      setLoadError(normalized);
    }
  }

  async function createKey(e: FormEvent) {
    e.preventDefault();
    setAction({ phase: "pending", message: "Creating API key..." });
    try {
      const created = await fetchPortalJSON<{ key?: string }>("/api/platform/control/internal/v1/api-keys", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ project_id: projectID, name })
      });
      setCreatedKey(created.key || "");
      setShowCreate(false);
      setAction({ phase: "success", message: "API key created." });
      push("success", "API key created. Copy it now; it will not be shown again.");
      await loadKeys();
    } catch (err) {
      const normalized = err as PortalError;
      setAction({ phase: "error", message: normalized.message });
      push("error", normalized.message);
    }
  }

  async function revokeKey() {
    if (!confirmRevoke) {
      return;
    }
    setAction({ phase: "pending", message: "Revoking key..." });
    try {
      await fetchPortalJSON(
        `/api/platform/control/internal/v1/api-keys/${encodeURIComponent(confirmRevoke)}/revoke?project_id=${encodeURIComponent(projectID)}`,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: "{}"
        }
      );
      setAction({ phase: "success", message: "API key revoked." });
      push("success", "API key revoked.");
      await loadKeys();
    } catch (err) {
      const normalized = err as PortalError;
      setAction({ phase: "error", message: normalized.message });
      push("error", normalized.message);
    } finally {
      setConfirmRevoke("");
    }
  }

  async function copyCreatedKey() {
    if (!createdKey) {
      return;
    }
    try {
      await navigator.clipboard.writeText(createdKey);
      push("success", "Copied API key.");
    } catch {
      push("warning", "Clipboard copy failed. Copy manually.");
    }
  }

  const columns: ColumnDef<APIKey>[] = [
    { key: "id", header: "ID", render: (row) => <code>{compactId(row.id, 10)}</code> },
    { key: "name", header: "Name", render: (row) => row.name },
    { key: "project", header: "Project", render: (row) => row.project_id },
    { key: "created", header: "Created", render: (row) => formatDateTime(row.created_at) },
    {
      key: "status",
      header: "Status",
      render: (row) => <StatusChip status={row.revoked_at ? "revoked" : "active"} />
    },
    {
      key: "action",
      header: "Action",
      render: (row) =>
        row.revoked_at ? (
          <span className="ui-muted">-</span>
        ) : (
          <Button
            type="button"
            variant="danger"
            onClick={(e) => {
              e.stopPropagation();
              setConfirmRevoke(row.id);
            }}
          >
            Revoke
          </Button>
        )
    }
  ];

  return (
    <main className="ui-panel ui-stack">
      <PageHeader
        title="API Keys"
        subtitle="Manage per-project keys with one-time reveal, rotation, and revoke controls."
        actions={
          <Button type="button" variant="primary" onClick={() => setShowCreate(true)}>
            Create key
          </Button>
        }
      />

      {createdKey ? (
        <InlineAlert type="warning" title="One-time key reveal">
          <code style={{ marginRight: 8 }}>{createdKey}</code>
          <Button type="button" variant="secondary" onClick={() => void copyCreatedKey()}>
            Copy
          </Button>
        </InlineAlert>
      ) : null}

      <Toolbar>
        <Field id="key-project" label="Project" value={projectID} onChange={(e) => setProjectID(e.target.value)} />
        <Field id="key-search" label="Search" value={table.q} onChange={(e) => setTable({ q: e.target.value, page: 1 })} placeholder="id, name, project" />
        <Select id="key-sort" label="Sort" value={table.sort} onChange={(e) => setTable({ sort: e.target.value })}>
          <option value="created_at">Created</option>
          <option value="name">Name</option>
          <option value="project_id">Project</option>
        </Select>
        <Select id="key-order" label="Order" value={table.order} onChange={(e) => setTable({ order: e.target.value as "asc" | "desc" })}>
          <option value="desc">Descending</option>
          <option value="asc">Ascending</option>
        </Select>
      </Toolbar>

      <div className="ui-row" style={{ justifyContent: "space-between" }}>
        <span className="ui-muted">{visible.total} key(s)</span>
        <div className="ui-row">
          <Button type="button" variant="secondary" onClick={() => void loadKeys()}>
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
          retry={() => void loadKeys()}
          actionLabel={loadError.actionLabel}
          actionHref={loadError.actionHref}
        />
      ) : null}

      {!loadError && state.loading ? <InlineAlert type="info">Loading API keys...</InlineAlert> : null}

      {!loadError && !state.loading && visible.rows.length === 0 ? (
        <EmptyState title="No keys found" description="Create an API key to grant project-scoped access." />
      ) : null}

      {!loadError && visible.rows.length > 0 ? <DataTable columns={columns} rows={visible.rows} /> : null}

      {action.message ? (
        <InlineAlert type={action.phase === "error" ? "error" : action.phase === "success" ? "success" : "info"}>{action.message}</InlineAlert>
      ) : null}

      {showCreate ? (
        <div className="ui-modal-backdrop" role="dialog" aria-modal="true" aria-label="Create key">
          <form className="ui-modal ui-stack" onSubmit={createKey}>
            <h3>Create API key</h3>
            <p className="ui-muted">Keys are scoped to a single project and shown only once.</p>
            <Toolbar>
              <Field id="create-key-project" label="Project" value={projectID} onChange={(e) => setProjectID(e.target.value)} required />
              <Field id="create-key-name" label="Name" value={name} onChange={(e) => setName(e.target.value)} required />
            </Toolbar>
            <div className="ui-modal__actions">
              <Button type="button" variant="secondary" onClick={() => setShowCreate(false)}>
                Cancel
              </Button>
              <Button type="submit" variant="primary" busy={action.phase === "pending"}>
                Create key
              </Button>
            </div>
          </form>
        </div>
      ) : null}

      <ConfirmDialog
        open={Boolean(confirmRevoke)}
        title="Revoke API key"
        message="This key will no longer authenticate requests. Continue?"
        confirmLabel="Revoke"
        danger
        onCancel={() => setConfirmRevoke("")}
        onConfirm={() => void revokeKey()}
      />

      <ToastRegion toasts={toasts} remove={remove} />
    </main>
  );
}
