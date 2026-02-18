import { fetchControl, fetchData, portalRuntimeConfig } from "../lib/platform";

type Deployment = { id: string };
type Build = { id: string; status: string };
type Assistant = { id: string };
type Thread = { id: string };

export default async function Home() {
  const cfg = portalRuntimeConfig();

  let deployments: Deployment[] = [];
  let builds: Build[] = [];
  let assistants: Assistant[] = [];
  let threads: Thread[] = [];

  try {
    [deployments, builds, assistants, threads] = await Promise.all([
      fetchControl<Deployment[]>("/internal/v1/deployments"),
      fetchControl<Build[]>("/internal/v1/builds"),
      fetchData<Assistant[]>("/api/v1/assistants"),
      fetchData<Thread[]>("/api/v1/threads")
    ]);
  } catch {
  }

  const runningBuilds = builds.filter((b) => b.status === "running" || b.status === "queued").length;

  return (
    <main>
      <section className="hero">
        <h1>LangOpen Platform</h1>
        <p>Drop-in LangGraph-compatible runtime with Kubernetes sandboxing and operator-grade observability.</p>
        <p className="muted">Data API: {cfg.dataBase} | Control API: {cfg.controlBase} | API key loaded: {String(cfg.hasAPIKey)}</p>
      </section>
      <section className="grid">
        <article className="card">
          <div className="badge">Deployments</div>
          <p className="kpi">{deployments.length}</p>
          <p>Current deployment records in control-plane.</p>
        </article>
        <article className="card">
          <div className="badge">Builds</div>
          <p className="kpi">{builds.length}</p>
          <p>{runningBuilds} queued/running builds.</p>
        </article>
        <article className="card">
          <div className="badge">Assistants</div>
          <p className="kpi">{assistants.length}</p>
          <p>{threads.length} threads currently tracked.</p>
        </article>
      </section>
    </main>
  );
}
