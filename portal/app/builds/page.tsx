"use client";

import { useEffect, useState } from "react";

type Build = {
  id: string;
  deployment_id: string;
  commit_sha: string;
  status: string;
  image_digest?: string;
  logs_ref?: string;
  created_at: string;
};

export default function BuildsPage() {
  const [builds, setBuilds] = useState<Build[]>([]);
  const [selectedBuildID, setSelectedBuildID] = useState("");
  const [logs, setLogs] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    void loadBuilds();
  }, []);

  async function loadBuilds() {
    setLoading(true);
    setError("");
    try {
      const resp = await fetch("/api/platform/control/internal/v1/builds", { cache: "no-store" });
      if (!resp.ok) {
        throw new Error(`load builds failed (${resp.status})`);
      }
      const data = (await resp.json()) as Build[];
      setBuilds(data);
    } catch (err) {
      setError(err instanceof Error ? err.message : "failed to load builds");
    } finally {
      setLoading(false);
    }
  }

  async function loadBuildLogs(buildID: string) {
    setLoading(true);
    setError("");
    setSelectedBuildID(buildID);
    setLogs("");
    try {
      const resp = await fetch(`/api/platform/control/internal/v1/builds/${encodeURIComponent(buildID)}/logs`, { cache: "no-store" });
      if (!resp.ok) {
        const body = await resp.text();
        throw new Error(`load build logs failed (${resp.status}) ${body}`);
      }
      const body = (await resp.json()) as { logs?: string; logs_ref?: string };
      setLogs(body.logs || body.logs_ref || "no logs");
    } catch (err) {
      setError(err instanceof Error ? err.message : "failed to load build logs");
    } finally {
      setLoading(false);
    }
  }

  return (
    <main className="hero">
      <h2>Builds</h2>
      <p>BuildKit rootless jobs with commit-tagged image digests and inline log view.</p>
      <div className="row">
        <button disabled={loading} type="button" onClick={() => void loadBuilds()}>Refresh</button>
      </div>
      {error ? <p className="warn">{error}</p> : null}
      {!error && builds.length === 0 ? <p>No builds found.</p> : null}
      {builds.length > 0 ? (
        <table className="table">
          <thead>
            <tr>
              <th>Build</th>
              <th>Deployment</th>
              <th>Commit</th>
              <th>Status</th>
              <th>Digest</th>
              <th>Logs</th>
            </tr>
          </thead>
          <tbody>
            {builds.map((b) => (
              <tr key={b.id}>
                <td>{b.id}</td>
                <td>{b.deployment_id}</td>
                <td>{b.commit_sha}</td>
                <td>{b.status}</td>
                <td>{b.image_digest || "-"}</td>
                <td>
                  <button disabled={loading} type="button" onClick={() => void loadBuildLogs(b.id)}>View</button>
                  {b.logs_ref ? <> ({b.logs_ref})</> : null}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : null}
      <pre className="card">{selectedBuildID ? `build=${selectedBuildID}\n${logs || "loading..."}` : "Select a build to view logs."}</pre>
    </main>
  );
}
