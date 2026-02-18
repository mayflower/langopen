import { fetchControl } from "../../lib/platform";

type Build = {
  id: string;
  deployment_id: string;
  commit_sha: string;
  status: string;
  image_digest?: string;
  logs_ref?: string;
  created_at: string;
};

export default async function BuildsPage() {
  let builds: Build[] = [];
  let error = "";
  try {
    builds = await fetchControl<Build[]>("/internal/v1/builds");
  } catch (err) {
    error = err instanceof Error ? err.message : "failed to load builds";
  }

  return (
    <main className="hero">
      <h2>Builds</h2>
      <p>BuildKit rootless jobs with commit-tagged image digests.</p>
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
                <td>{b.logs_ref || "-"}</td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : null}
    </main>
  );
}
