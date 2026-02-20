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
import { SplitPane } from "../../components/ui/SplitPane";
import { StatusChip } from "../../components/ui/StatusChip";
import { ToastRegion, useToastRegion } from "../../components/ui/ToastRegion";
import { Toolbar } from "../../components/ui/Toolbar";
import { fetchPortalJSON, toPortalError, type PortalError } from "../../lib/client-api";
import { compactId, formatDateTime } from "../../lib/format";
import { useTableQueryState } from "../../lib/table-query";
import { initialActionState, initialDataState } from "../../lib/ui-types";

type Deployment = {
  id: string;
  project_id: string;
  repo_url: string;
  git_ref: string;
  repo_path: string;
  runtime_profile: string;
  mode: string;
  current_image_digest?: string;
  updated_at: string;
};

type SecretBinding = {
  id: string;
  deployment_id: string;
  secret_name: string;
  target_key?: string;
  created_at: string;
};

type DetailTab = "overview" | "source" | "build" | "secrets" | "policy";

const DEFAULT_PROJECT = "proj_default";

export default function DeploymentsPage() {
  const [projectID, setProjectID] = useState(DEFAULT_PROJECT);
  const [state, setState] = useState(initialDataState<Deployment>());
  const [selectedDeploymentID, setSelectedDeploymentID] = useState("");
  const [loadError, setLoadError] = useState<PortalError | null>(null);

  const [tab, setTab] = useState<DetailTab>("overview");
  const [showCreate, setShowCreate] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState<string>("");

  const [repoURL, setRepoURL] = useState("https://github.com/acme/agent");
  const [gitRef, setGitRef] = useState("main");
  const [repoPath, setRepoPath] = useState(".");
  const [runtimeProfile, setRuntimeProfile] = useState("gvisor");
  const [mode, setMode] = useState("mode_a");

  const [imageName, setImageName] = useState("ghcr.io/acme/agent");
  const [commitSHA, setCommitSHA] = useState("abcdef123456");

  const [secretName, setSecretName] = useState("openai-secret");
  const [targetKey, setTargetKey] = useState("OPENAI_API_KEY");
  const [bindings, setBindings] = useState(initialDataState<SecretBinding>());

  const [action, setAction] = useState(initialActionState());
  const [deletingBindingID, setDeletingBindingID] = useState("");
  const { state: table, setState: setTable } = useTableQueryState({ sort: "updated_at", order: "desc", page: 1, pageSize: 8 });
  const { toasts, push, remove } = useToastRegion();

  useEffect(() => {
    void loadDeployments();
  }, [projectID]);

  useEffect(() => {
    if (!selectedDeploymentID) {
      setBindings(initialDataState<SecretBinding>());
      return;
    }
    void loadBindings(selectedDeploymentID);
  }, [selectedDeploymentID, projectID]);

  const selectedDeployment = useMemo(
    () => state.items.find((item) => item.id === selectedDeploymentID) || null,
    [state.items, selectedDeploymentID]
  );

  const visibleDeployments = useMemo(() => {
    const q = table.q.trim().toLowerCase();
    const filtered = q
      ? state.items.filter((item) => [item.id, item.repo_url, item.git_ref, item.mode].join(" ").toLowerCase().includes(q))
      : state.items;

    const sorted = [...filtered].sort((a, b) => {
      const key = table.sort || "updated_at";
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

  async function loadDeployments() {
    setState((prev) => ({ ...prev, loading: true, error: "" }));
    setLoadError(null);
    try {
      const rows = await fetchPortalJSON<Deployment[]>(`/api/platform/control/internal/v1/deployments?project_id=${encodeURIComponent(projectID)}`);
      setState({ loading: false, error: "", items: rows });
      if (rows.length > 0 && !rows.some((row) => row.id === selectedDeploymentID)) {
        setSelectedDeploymentID(rows[0].id);
      }
      if (rows.length === 0) {
        setSelectedDeploymentID("");
      }
    } catch (err) {
      const normalized = err as PortalError;
      setState((prev) => ({ ...prev, loading: false, error: normalized.message }));
      setLoadError(normalized);
    }
  }

  async function loadBindings(deploymentID: string) {
    setBindings((prev) => ({ ...prev, loading: true, error: "" }));
    try {
      const rows = await fetchPortalJSON<SecretBinding[]>(
        `/api/platform/control/internal/v1/secrets/bindings?deployment_id=${encodeURIComponent(deploymentID)}&project_id=${encodeURIComponent(projectID)}`
      );
      setBindings({ loading: false, error: "", items: rows });
    } catch (err) {
      const normalized = err as PortalError;
      setBindings((prev) => ({ ...prev, loading: false, error: normalized.message }));
    }
  }

  async function createDeployment(e: FormEvent) {
    e.preventDefault();
    setAction({ phase: "pending", message: "Creating deployment..." });
    try {
      const created = await fetchPortalJSON<Deployment>("/api/platform/control/internal/v1/deployments", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          project_id: projectID,
          repo_url: repoURL,
          git_ref: gitRef,
          repo_path: repoPath,
          runtime_profile: runtimeProfile,
          mode
        })
      });
      setSelectedDeploymentID(created.id);
      setShowCreate(false);
      setAction({ phase: "success", message: "Deployment created." });
      push("success", "Deployment created.");
      await loadDeployments();
    } catch (err) {
      const normalized = err as PortalError;
      setAction({ phase: "error", message: normalized.message });
      push("error", normalized.message);
    }
  }

  async function updateSource(e: FormEvent) {
    e.preventDefault();
    if (!selectedDeploymentID) {
      push("warning", "Select a deployment first.");
      return;
    }
    setAction({ phase: "pending", message: "Updating source settings..." });
    try {
      await fetchPortalJSON(`/api/platform/control/internal/v1/deployments/${encodeURIComponent(selectedDeploymentID)}?project_id=${encodeURIComponent(projectID)}`, {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ repo_url: repoURL, git_ref: gitRef, repo_path: repoPath })
      });
      setAction({ phase: "success", message: "Source settings saved." });
      push("success", "Deployment source updated.");
      await loadDeployments();
    } catch (err) {
      const normalized = err as PortalError;
      setAction({ phase: "error", message: normalized.message });
      push("error", normalized.message);
    }
  }

  async function validateSource() {
    setAction({ phase: "pending", message: "Validating source..." });
    try {
      await fetchPortalJSON("/api/platform/control/internal/v1/sources/validate", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ repo_path: repoPath })
      });
      setAction({ phase: "success", message: "Source validation passed." });
      push("success", "Source validation passed.");
    } catch (err) {
      const normalized = err as PortalError;
      setAction({ phase: "error", message: normalized.message });
      push("error", normalized.message);
    }
  }

  async function triggerBuild(e: FormEvent) {
    e.preventDefault();
    if (!selectedDeploymentID) {
      push("warning", "Select a deployment first.");
      return;
    }
    setAction({ phase: "pending", message: "Triggering build..." });
    try {
      await fetchPortalJSON("/api/platform/control/internal/v1/builds", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          deployment_id: selectedDeploymentID,
          commit_sha: commitSHA,
          image_name: imageName,
          repo_path: repoPath
        })
      });
      setAction({ phase: "success", message: "Build queued." });
      push("success", "Build queued.");
    } catch (err) {
      const normalized = err as PortalError;
      setAction({ phase: "error", message: normalized.message });
      push("error", normalized.message);
    }
  }

  async function bindSecret(e: FormEvent) {
    e.preventDefault();
    if (!selectedDeploymentID) {
      push("warning", "Select a deployment first.");
      return;
    }
    setAction({ phase: "pending", message: "Binding secret..." });
    try {
      await fetchPortalJSON("/api/platform/control/internal/v1/secrets/bind", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          deployment_id: selectedDeploymentID,
          project_id: projectID,
          secret_name: secretName,
          target_key: targetKey
        })
      });
      setAction({ phase: "success", message: "Secret bound." });
      push("success", "Secret bound to deployment.");
      await loadBindings(selectedDeploymentID);
    } catch (err) {
      const normalized = err as PortalError;
      setAction({ phase: "error", message: normalized.message });
      push("error", normalized.message);
    }
  }

  async function unbindSecret(bindingID: string) {
    setDeletingBindingID(bindingID);
    try {
      await fetchPortalJSON(
        `/api/platform/control/internal/v1/secrets/bindings/${encodeURIComponent(bindingID)}?project_id=${encodeURIComponent(projectID)}`,
        { method: "DELETE" }
      );
      push("success", "Secret unbound.");
      if (selectedDeploymentID) {
        await loadBindings(selectedDeploymentID);
      }
    } catch (err) {
      const normalized = err as PortalError;
      push("error", normalized.message);
    } finally {
      setDeletingBindingID("");
    }
  }

  async function applyPolicy(e: FormEvent) {
    e.preventDefault();
    if (!selectedDeploymentID) {
      push("warning", "Select a deployment first.");
      return;
    }
    setAction({ phase: "pending", message: "Applying runtime policy..." });
    try {
      await fetchPortalJSON("/api/platform/control/internal/v1/policies/runtime", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          deployment_id: selectedDeploymentID,
          project_id: projectID,
          runtime_profile: runtimeProfile,
          mode
        })
      });
      setAction({ phase: "success", message: "Runtime policy applied." });
      push("success", "Runtime policy applied.");
      await loadDeployments();
    } catch (err) {
      const normalized = err as PortalError;
      setAction({ phase: "error", message: normalized.message });
      push("error", normalized.message);
    }
  }

  async function deleteDeployment() {
    if (!confirmDelete) {
      return;
    }
    try {
      await fetchPortalJSON(
        `/api/platform/control/internal/v1/deployments/${encodeURIComponent(confirmDelete)}?project_id=${encodeURIComponent(projectID)}`,
        { method: "DELETE" }
      );
      push("success", "Deployment deleted.");
      if (selectedDeploymentID === confirmDelete) {
        setSelectedDeploymentID("");
      }
      await loadDeployments();
    } catch (err) {
      const normalized = err as PortalError;
      push("error", normalized.message);
    } finally {
      setConfirmDelete("");
    }
  }

  const columns: ColumnDef<Deployment>[] = [
    { key: "id", header: "Deployment", render: (row) => <code title={row.id}>{compactId(row.id, 10)}</code> },
    { key: "repo_url", header: "Repository", render: (row) => <span>{row.repo_url}</span> },
    { key: "git_ref", header: "Ref", render: (row) => row.git_ref },
    { key: "current_image_digest", header: "Image", render: (row) => <code>{compactId(row.current_image_digest || "-", 12)}</code> },
    { key: "runtime", header: "Runtime", render: (row) => <StatusChip status={row.runtime_profile || "unknown"} /> },
    { key: "mode", header: "Mode", render: (row) => row.mode },
    { key: "updated_at", header: "Updated", render: (row) => formatDateTime(row.updated_at) }
  ];

  return (
    <main className="ui-panel ui-stack">
      <PageHeader
        title="Deployments"
        subtitle="Selection-first workflow for source updates, builds, secret bindings, and runtime policy operations."
        actions={
          <Button type="button" variant="primary" onClick={() => setShowCreate(true)}>
            Create deployment
          </Button>
        }
      />

      <Toolbar>
        <Field id="dep-project" label="Project" value={projectID} onChange={(e) => setProjectID(e.target.value)} />
        <Field
          id="dep-search"
          label="Search"
          value={table.q}
          onChange={(e) => setTable({ q: e.target.value, page: 1 })}
          placeholder="Search by id, repo, ref, mode"
        />
        <Select id="dep-sort" label="Sort" value={table.sort} onChange={(e) => setTable({ sort: e.target.value })}>
          <option value="updated_at">Updated</option>
          <option value="git_ref">Ref</option>
          <option value="repo_url">Repository</option>
          <option value="mode">Mode</option>
        </Select>
        <Select id="dep-order" label="Order" value={table.order} onChange={(e) => setTable({ order: e.target.value as "asc" | "desc" })}>
          <option value="desc">Descending</option>
          <option value="asc">Ascending</option>
        </Select>
      </Toolbar>

      <div className="ui-row" style={{ justifyContent: "space-between" }}>
        <span className="ui-muted">{visibleDeployments.total} deployment(s)</span>
        <div className="ui-row">
          <Button type="button" variant="ghost" onClick={() => setTable({ page: Math.max(1, table.page - 1) })} disabled={table.page <= 1}>
            Prev
          </Button>
          <span className="ui-muted">Page {table.page}</span>
          <Button
            type="button"
            variant="ghost"
            onClick={() => setTable({ page: table.page + 1 })}
            disabled={table.page * table.pageSize >= visibleDeployments.total}
          >
            Next
          </Button>
        </div>
      </div>

      {loadError ? (
        <ErrorState
          title={loadError.title}
          message={loadError.message}
          retry={() => void loadDeployments()}
          actionHref={loadError.actionHref}
          actionLabel={loadError.actionLabel}
        />
      ) : null}

      {!loadError && state.loading ? <InlineAlert type="info">Loading deployments...</InlineAlert> : null}
      {!loadError && !state.loading && visibleDeployments.rows.length === 0 ? (
        <EmptyState
          title="No deployments yet"
          description="Create a deployment to connect a repository and start build/deploy operations."
          action={
            <Button type="button" variant="primary" onClick={() => setShowCreate(true)}>
              Create deployment
            </Button>
          }
        />
      ) : null}

      {!loadError && visibleDeployments.rows.length > 0 ? (
        <SplitPane
          left={
            <DataTable
              columns={columns}
              rows={visibleDeployments.rows}
              onRowClick={(row) => setSelectedDeploymentID(row.id)}
              selectedRow={(row) => row.id === selectedDeploymentID}
            />
          }
          right={
            <section className="ui-subpanel ui-stack">
              <div className="ui-row" style={{ justifyContent: "space-between" }}>
                <h2 style={{ margin: 0 }}>{selectedDeployment ? compactId(selectedDeployment.id, 14) : "Select a deployment"}</h2>
                {selectedDeployment ? <StatusChip status={selectedDeployment.runtime_profile} /> : null}
              </div>

              <div className="ui-tabs" role="tablist" aria-label="Deployment detail tabs">
                {([
                  ["overview", "Overview"],
                  ["source", "Source"],
                  ["build", "Build"],
                  ["secrets", "Secrets"],
                  ["policy", "Policy"]
                ] as [DetailTab, string][]).map(([value, label]) => (
                  <button
                    key={value}
                    type="button"
                    role="tab"
                    aria-selected={tab === value}
                    className={tab === value ? "is-active" : ""}
                    onClick={() => setTab(value)}
                  >
                    {label}
                  </button>
                ))}
              </div>

              {selectedDeployment ? (
                <>
                  {tab === "overview" ? (
                    <section className="ui-stack">
                      <InlineAlert type="info">Select tabs to update source, trigger builds, bind secrets, or apply runtime policy.</InlineAlert>
                      <div className="ui-kpi-grid">
                        <article className="ui-kpi">
                          <span className="ui-muted">Repository</span>
                          <p>{selectedDeployment.repo_url}</p>
                        </article>
                        <article className="ui-kpi">
                          <span className="ui-muted">Ref</span>
                          <p>{selectedDeployment.git_ref}</p>
                        </article>
                        <article className="ui-kpi">
                          <span className="ui-muted">Mode</span>
                          <p>{selectedDeployment.mode}</p>
                        </article>
                        <article className="ui-kpi">
                          <span className="ui-muted">Updated</span>
                          <p>{formatDateTime(selectedDeployment.updated_at)}</p>
                        </article>
                      </div>
                      <div className="ui-right">
                        <Button type="button" variant="danger" onClick={() => setConfirmDelete(selectedDeployment.id)}>
                          Delete deployment
                        </Button>
                      </div>
                    </section>
                  ) : null}

                  {tab === "source" ? (
                    <form className="ui-stack" onSubmit={updateSource}>
                      <Toolbar>
                        <Field id="dep-source-repo" label="Repository URL" value={repoURL} onChange={(e) => setRepoURL(e.target.value)} required />
                        <Field id="dep-source-ref" label="Git ref" value={gitRef} onChange={(e) => setGitRef(e.target.value)} required />
                        <Field id="dep-source-path" label="Repo path" value={repoPath} onChange={(e) => setRepoPath(e.target.value)} required />
                      </Toolbar>
                      <div className="ui-row">
                        <Button type="submit" variant="primary" busy={action.phase === "pending"}>
                          Save source
                        </Button>
                        <Button type="button" variant="secondary" onClick={() => void validateSource()}>
                          Validate source
                        </Button>
                      </div>
                    </form>
                  ) : null}

                  {tab === "build" ? (
                    <form className="ui-stack" onSubmit={triggerBuild}>
                      <Toolbar>
                        <Field id="dep-build-image" label="Image name" value={imageName} onChange={(e) => setImageName(e.target.value)} required />
                        <Field id="dep-build-commit" label="Commit SHA" value={commitSHA} onChange={(e) => setCommitSHA(e.target.value)} required />
                      </Toolbar>
                      <div className="ui-row">
                        <Button type="submit" variant="primary" busy={action.phase === "pending"}>
                          Trigger build
                        </Button>
                      </div>
                    </form>
                  ) : null}

                  {tab === "secrets" ? (
                    <section className="ui-stack">
                      <form className="ui-stack" onSubmit={bindSecret}>
                        <Toolbar>
                          <Field id="dep-secret-name" label="Secret name" value={secretName} onChange={(e) => setSecretName(e.target.value)} required />
                          <Field id="dep-secret-target" label="Target env key" value={targetKey} onChange={(e) => setTargetKey(e.target.value)} required />
                        </Toolbar>
                        <div className="ui-row">
                          <Button type="submit" variant="primary" busy={action.phase === "pending"}>
                            Bind secret
                          </Button>
                          <Button type="button" variant="secondary" onClick={() => void loadBindings(selectedDeployment.id)}>
                            Refresh
                          </Button>
                        </div>
                      </form>
                      {bindings.error ? <InlineAlert type="error">{bindings.error}</InlineAlert> : null}
                      {bindings.loading ? <InlineAlert type="info">Loading bindings...</InlineAlert> : null}
                      {!bindings.loading && bindings.items.length === 0 ? (
                        <EmptyState title="No bindings" description="No secrets are bound to this deployment yet." />
                      ) : null}
                      {bindings.items.length > 0 ? (
                        <DataTable
                          dense
                          rows={bindings.items}
                          columns={[
                            { key: "id", header: "Binding", render: (row) => <code>{compactId(row.id, 10)}</code> },
                            { key: "secret", header: "Secret", render: (row) => row.secret_name },
                            { key: "target", header: "Target", render: (row) => row.target_key || "-" },
                            { key: "created", header: "Created", render: (row) => formatDateTime(row.created_at) },
                            {
                              key: "action",
                              header: "Action",
                              render: (row) => (
                                <Button
                                  type="button"
                                  variant="ghost"
                                  onClick={(e) => {
                                    e.stopPropagation();
                                    void unbindSecret(row.id);
                                  }}
                                  disabled={deletingBindingID === row.id}
                                >
                                  Unbind
                                </Button>
                              )
                            }
                          ]}
                        />
                      ) : null}
                    </section>
                  ) : null}

                  {tab === "policy" ? (
                    <form className="ui-stack" onSubmit={applyPolicy}>
                      <Toolbar>
                        <Select id="dep-policy-runtime" label="Runtime profile" value={runtimeProfile} onChange={(e) => setRuntimeProfile(e.target.value)}>
                          <option value="gvisor">gvisor</option>
                          <option value="kata-default">kata-default</option>
                        </Select>
                        <Select id="dep-policy-mode" label="Execution mode" value={mode} onChange={(e) => setMode(e.target.value)}>
                          <option value="mode_a">mode_a</option>
                          <option value="mode_b">mode_b</option>
                        </Select>
                      </Toolbar>
                      <div className="ui-row">
                        <Button type="submit" variant="primary" busy={action.phase === "pending"}>
                          Apply policy
                        </Button>
                      </div>
                    </form>
                  ) : null}
                </>
              ) : (
                <EmptyState title="No deployment selected" description="Select a deployment on the left to manage source, builds, secrets, and policy." />
              )}

              {action.message ? (
                <InlineAlert type={action.phase === "error" ? "error" : action.phase === "success" ? "success" : "info"}>{action.message}</InlineAlert>
              ) : null}
            </section>
          }
        />
      ) : null}

      <ConfirmDialog
        open={Boolean(confirmDelete)}
        title="Delete deployment"
        message="This removes deployment metadata and may disrupt runs. Continue?"
        confirmLabel="Delete"
        danger
        onCancel={() => setConfirmDelete("")}
        onConfirm={() => void deleteDeployment()}
      />

      {showCreate ? (
        <div className="ui-modal-backdrop" role="dialog" aria-modal="true" aria-label="Create deployment">
          <form className="ui-modal ui-stack" onSubmit={createDeployment}>
            <h3>Create deployment</h3>
            <p className="ui-muted">Step 1: Connect source. Step 2: Set runtime defaults. Step 3: Create.</p>
            <Toolbar>
              <Field id="dep-create-project" label="Project" value={projectID} onChange={(e) => setProjectID(e.target.value)} required />
              <Field id="dep-create-repo" label="Repository URL" value={repoURL} onChange={(e) => setRepoURL(e.target.value)} required />
              <Field id="dep-create-ref" label="Git ref" value={gitRef} onChange={(e) => setGitRef(e.target.value)} required />
              <Field id="dep-create-path" label="Repo path" value={repoPath} onChange={(e) => setRepoPath(e.target.value)} required />
              <Select id="dep-create-runtime" label="Runtime profile" value={runtimeProfile} onChange={(e) => setRuntimeProfile(e.target.value)}>
                <option value="gvisor">gvisor</option>
                <option value="kata-default">kata-default</option>
              </Select>
              <Select id="dep-create-mode" label="Mode" value={mode} onChange={(e) => setMode(e.target.value)}>
                <option value="mode_a">mode_a</option>
                <option value="mode_b">mode_b</option>
              </Select>
            </Toolbar>
            <div className="ui-modal__actions">
              <Button type="button" variant="secondary" onClick={() => setShowCreate(false)}>
                Cancel
              </Button>
              <Button type="submit" variant="primary" busy={action.phase === "pending"}>
                Create deployment
              </Button>
            </div>
          </form>
        </div>
      ) : null}

      <ToastRegion toasts={toasts} remove={remove} />
    </main>
  );
}
