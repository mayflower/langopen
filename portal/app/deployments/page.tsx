import { fetchControl } from "../../lib/platform";

type Deployment = {
  id: string;
  project_id: string;
  repo_url: string;
  git_ref: string;
  runtime_profile: string;
  mode: string;
  current_image_digest?: string;
  updated_at: string;
};

export default async function DeploymentsPage() {
  let deployments: Deployment[] = [];
  let error = "";
  try {
    deployments = await fetchControl<Deployment[]>("/internal/v1/deployments");
  } catch (err) {
    error = err instanceof Error ? err.message : "failed to load deployments";
  }

  return (
    <main className="hero">
      <h2>Deployments</h2>
      {error ? <p className="warn">{error}</p> : null}
      {!error && deployments.length === 0 ? <p>No deployments found.</p> : null}
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
