import { fetchData } from "../../lib/platform";

type Thread = {
  id: string;
  assistant_id: string;
  updated_at: string;
};

export default async function ThreadsPage() {
  let threads: Thread[] = [];
  let error = "";
  try {
    threads = await fetchData<Thread[]>("/api/v1/threads");
  } catch (err) {
    error = err instanceof Error ? err.message : "failed to load threads";
  }

  return (
    <main className="hero">
      <h2>Threads</h2>
      <p>Recent thread snapshots and metadata.</p>
      {error ? <p className="warn">{error}</p> : null}
      {!error && threads.length === 0 ? <p>No threads found.</p> : null}
      {threads.length > 0 ? (
        <table className="table">
          <thead>
            <tr>
              <th>ID</th>
              <th>Assistant</th>
              <th>Updated</th>
            </tr>
          </thead>
          <tbody>
            {threads.map((t) => (
              <tr key={t.id}>
                <td>{t.id}</td>
                <td>{t.assistant_id}</td>
                <td>{new Date(t.updated_at).toLocaleString()}</td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : null}
    </main>
  );
}
