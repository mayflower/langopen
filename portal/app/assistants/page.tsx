import { fetchData } from "../../lib/platform";

type Assistant = {
  id: string;
  deployment_id: string;
  graph_id: string;
  version: string;
};

export default async function AssistantsPage() {
  let assistants: Assistant[] = [];
  let error = "";
  try {
    assistants = await fetchData<Assistant[]>("/api/v1/assistants");
  } catch (err) {
    error = err instanceof Error ? err.message : "failed to load assistants";
  }

  return (
    <main className="hero">
      <h2>Assistants</h2>
      <p>Graph versions and deployment bindings.</p>
      {error ? <p className="warn">{error}</p> : null}
      {!error && assistants.length === 0 ? <p>No assistants found.</p> : null}
      {assistants.length > 0 ? (
        <table className="table">
          <thead>
            <tr>
              <th>ID</th>
              <th>Deployment</th>
              <th>Graph</th>
              <th>Version</th>
            </tr>
          </thead>
          <tbody>
            {assistants.map((a) => (
              <tr key={a.id}>
                <td>{a.id}</td>
                <td>{a.deployment_id}</td>
                <td>{a.graph_id}</td>
                <td>{a.version}</td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : null}
    </main>
  );
}
