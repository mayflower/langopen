"use client";

import { FormEvent, useEffect, useState } from "react";

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
  const [keys, setKeys] = useState<APIKey[]>([]);
  const [createdKey, setCreatedKey] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    void reload(DEFAULT_PROJECT);
  }, []);

  async function reload(project: string) {
    setLoading(true);
    setError("");
    try {
      const resp = await fetch(`/api/platform/control/internal/v1/api-keys?project_id=${encodeURIComponent(project)}`, { cache: "no-store" });
      if (!resp.ok) {
        throw new Error(`list failed (${resp.status})`);
      }
      const data = (await resp.json()) as APIKey[];
      setKeys(data);
    } catch (err) {
      setError(err instanceof Error ? err.message : "failed to list api keys");
    } finally {
      setLoading(false);
    }
  }

  async function createKey(e: FormEvent) {
    e.preventDefault();
    setCreatedKey("");
    setError("");
    try {
      const resp = await fetch("/api/platform/control/internal/v1/api-keys", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ project_id: projectID, name })
      });
      if (!resp.ok) {
        const body = await resp.text();
        throw new Error(`create failed (${resp.status}) ${body}`);
      }
      const data = (await resp.json()) as { key?: string };
      if (data.key) {
        setCreatedKey(data.key);
      }
      await reload(projectID);
    } catch (err) {
      setError(err instanceof Error ? err.message : "failed to create api key");
    }
  }

  async function revoke(id: string) {
    setError("");
    try {
      const resp = await fetch(`/api/platform/control/internal/v1/api-keys/${encodeURIComponent(id)}/revoke?project_id=${encodeURIComponent(projectID)}`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: "{}"
      });
      if (!resp.ok) {
        const body = await resp.text();
        throw new Error(`revoke failed (${resp.status}) ${body}`);
      }
      await reload(projectID);
    } catch (err) {
      setError(err instanceof Error ? err.message : "failed to revoke api key");
    }
  }

  return (
    <main className="hero">
      <h2>API Keys</h2>
      <p>Per-project API key lifecycle: create, list, revoke.</p>

      <form className="row" onSubmit={createKey}>
        <input value={projectID} onChange={(e) => setProjectID(e.target.value)} placeholder="project_id" />
        <input value={name} onChange={(e) => setName(e.target.value)} placeholder="key_name" />
        <button type="submit">Create Key</button>
        <button type="button" onClick={() => void reload(projectID)} disabled={loading}>Refresh</button>
      </form>

      {createdKey ? <p className="warn">New key (shown once): <code>{createdKey}</code></p> : null}
      {error ? <p className="warn">{error}</p> : null}

      {keys.length === 0 ? <p>No API keys found.</p> : (
        <table className="table">
          <thead>
            <tr>
              <th>ID</th>
              <th>Name</th>
              <th>Project</th>
              <th>Created</th>
              <th>Status</th>
              <th>Action</th>
            </tr>
          </thead>
          <tbody>
            {keys.map((k) => (
              <tr key={k.id}>
                <td>{k.id}</td>
                <td>{k.name}</td>
                <td>{k.project_id}</td>
                <td>{k.created_at}</td>
                <td>{k.revoked_at ? "revoked" : "active"}</td>
                <td>
                  {!k.revoked_at ? <button type="button" onClick={() => void revoke(k.id)}>Revoke</button> : "-"}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </main>
  );
}
