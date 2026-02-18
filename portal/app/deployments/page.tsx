"use client";

import { FormEvent, useEffect, useState } from "react";

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

export default function DeploymentsPage() {
  const [deployments, setDeployments] = useState<Deployment[]>([]);
  const [error, setError] = useState("");
  const [status, setStatus] = useState("");
  const [loading, setLoading] = useState(false);

  const [repoURL, setRepoURL] = useState("https://github.com/acme/agent");
  const [gitRef, setGitRef] = useState("main");
  const [repoPath, setRepoPath] = useState(".");
  const [runtimeProfile, setRuntimeProfile] = useState("gvisor");
  const [mode, setMode] = useState("mode_a");

  const [deploymentID, setDeploymentID] = useState("");
  const [imageName, setImageName] = useState("ghcr.io/acme/agent");
  const [commitSHA, setCommitSHA] = useState("abcdef123456");

  useEffect(() => {
    void loadDeployments();
  }, []);

  async function loadDeployments() {
    try {
      const resp = await fetch("/api/platform/control/internal/v1/deployments", { cache: "no-store" });
      if (!resp.ok) {
        throw new Error(`load deployments failed (${resp.status})`);
      }
      const data = (await resp.json()) as Deployment[];
      setDeployments(data);
    } catch (err) {
      setError(err instanceof Error ? err.message : "failed to load deployments");
    }
  }

  async function createDeployment(e: FormEvent) {
    e.preventDefault();
    setLoading(true);
    setError("");
    setStatus("");
    try {
      const resp = await fetch("/api/platform/control/internal/v1/deployments", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          project_id: "proj_default",
          repo_url: repoURL,
          git_ref: gitRef,
          repo_path: repoPath,
          runtime_profile: runtimeProfile,
          mode
        })
      });
      const body = await resp.json();
      if (!resp.ok) {
        throw new Error(JSON.stringify(body));
      }
      if (typeof body.id === "string") {
        setDeploymentID(body.id);
      }
      setStatus("deployment created");
      await loadDeployments();
    } catch (err) {
      setError(err instanceof Error ? err.message : "create deployment failed");
    } finally {
      setLoading(false);
    }
  }

  async function validateSource() {
    setLoading(true);
    setError("");
    setStatus("");
    try {
      const resp = await fetch("/api/platform/control/internal/v1/sources/validate", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ repo_path: repoPath })
      });
      const body = await resp.json();
      if (!resp.ok) {
        throw new Error(JSON.stringify(body));
      }
      setStatus(`source validation status: ${JSON.stringify(body)}`);
    } catch (err) {
      setError(err instanceof Error ? err.message : "source validate failed");
    } finally {
      setLoading(false);
    }
  }

  async function triggerBuild(e: FormEvent) {
    e.preventDefault();
    if (!deploymentID) {
      setError("deployment_id is required");
      return;
    }
    setLoading(true);
    setError("");
    setStatus("");
    try {
      const resp = await fetch("/api/platform/control/internal/v1/builds", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          deployment_id: deploymentID,
          commit_sha: commitSHA,
          image_name: imageName,
          repo_path: repoPath
        })
      });
      const body = await resp.json();
      if (!resp.ok) {
        throw new Error(JSON.stringify(body));
      }
      setStatus(`build triggered: ${body.id || body.status}`);
      await loadDeployments();
    } catch (err) {
      setError(err instanceof Error ? err.message : "build trigger failed");
    } finally {
      setLoading(false);
    }
  }

  return (
    <main className="hero">
      <h2>Deployments</h2>
      <p>Git connect, source validation, and build/deploy flow.</p>

      <form onSubmit={createDeployment} className="card">
        <h3>Create Deployment</h3>
        <div className="row">
          <input value={repoURL} onChange={(e) => setRepoURL(e.target.value)} placeholder="repo_url" />
          <input value={gitRef} onChange={(e) => setGitRef(e.target.value)} placeholder="git_ref" />
          <input value={repoPath} onChange={(e) => setRepoPath(e.target.value)} placeholder="repo_path" />
        </div>
        <div className="row">
          <input value={runtimeProfile} onChange={(e) => setRuntimeProfile(e.target.value)} placeholder="runtime_profile" />
          <input value={mode} onChange={(e) => setMode(e.target.value)} placeholder="mode_a|mode_b" />
          <button disabled={loading} type="submit">Create</button>
          <button disabled={loading} type="button" onClick={validateSource}>Validate Source</button>
        </div>
      </form>

      <form onSubmit={triggerBuild} className="card">
        <h3>Trigger Build</h3>
        <div className="row">
          <input value={deploymentID} onChange={(e) => setDeploymentID(e.target.value)} placeholder="deployment_id" />
          <input value={imageName} onChange={(e) => setImageName(e.target.value)} placeholder="image_name" />
          <input value={commitSHA} onChange={(e) => setCommitSHA(e.target.value)} placeholder="commit_sha" />
          <button disabled={loading} type="submit">Build</button>
        </div>
      </form>

      {error ? <p className="warn">{error}</p> : null}
      {status ? <p className="muted">{status}</p> : null}

      {deployments.length === 0 ? <p>No deployments found.</p> : null}
      {deployments.length > 0 ? (
        <table className="table">
          <thead>
            <tr>
              <th>ID</th>
              <th>Repo</th>
              <th>Ref</th>
              <th>Digest</th>
              <th>Runtime</th>
              <th>Mode</th>
              <th>Updated</th>
            </tr>
          </thead>
          <tbody>
            {deployments.map((d) => (
              <tr key={d.id}>
                <td>{d.id}</td>
                <td>{d.repo_url}</td>
                <td>{d.git_ref}</td>
                <td>{d.current_image_digest || "-"}</td>
                <td>{d.runtime_profile}</td>
                <td>{d.mode}</td>
                <td>{new Date(d.updated_at).toLocaleString()}</td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : null}
    </main>
  );
}
