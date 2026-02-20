import { Badge } from "../components/ui/Badge";
import { PageHeader } from "../components/ui/PageHeader";
import { StatusChip } from "../components/ui/StatusChip";
import { fetchControl, fetchData, portalRuntimeConfig } from "../lib/platform";
import { formatDateTime } from "../lib/format";

type Deployment = { id: string; updated_at?: string };
type Build = { id: string; status: string; created_at?: string };
type Assistant = { id: string };
type Thread = { id: string; updated_at?: string };
type AttentionSummary = {
  stuck_runs: number;
  pending_runs: number;
  error_runs: number;
  webhook_dead_letters: number;
};

type HealthResponse = {
  status: string;
};

export default async function Home() {
  const cfg = portalRuntimeConfig();

  let deployments: Deployment[] = [];
  let builds: Build[] = [];
  let assistants: Assistant[] = [];
  let threads: Thread[] = [];
  let summary: AttentionSummary | null = null;
  let health: HealthResponse | null = null;

  try {
    const projectQuery = `project_id=${encodeURIComponent(cfg.projectID)}`;
    [deployments, builds, assistants, threads, summary, health] = await Promise.all([
      fetchControl<Deployment[]>(`/internal/v1/deployments?${projectQuery}`),
      fetchControl<Build[]>(`/internal/v1/builds?${projectQuery}`),
      fetchData<Assistant[]>("/api/v1/assistants"),
      fetchData<Thread[]>("/api/v1/threads"),
      fetchData<AttentionSummary>("/api/v1/system/attention"),
      fetchData<HealthResponse>("/api/v1/system/health")
    ]);
  } catch {
  }

  const runningBuilds = builds.filter((b) => b.status === "running" || b.status === "queued").length;
  const latestBuild = builds[0]?.created_at ? formatDateTime(builds[0].created_at) : "-";
  const latestThread = threads[0]?.updated_at ? formatDateTime(threads[0].updated_at) : "-";

  return (
    <main className="ui-panel ui-stack">
      <PageHeader
        title="LangOpen Control Plane"
        subtitle="Budget-friendly LangGraph-compatible operations portal for deployments, builds, runtime streams, and policy management."
        actions={<Badge>Project: {cfg.projectID}</Badge>}
      />

      <section className="ui-kpi-grid">
        <article className="ui-kpi">
          <span className="ui-muted">Deployments</span>
          <p className="ui-kpi__value">{deployments.length}</p>
          <p className="ui-muted">tracked in control plane</p>
        </article>
        <article className="ui-kpi">
          <span className="ui-muted">Build queue</span>
          <p className="ui-kpi__value">{runningBuilds}</p>
          <p className="ui-muted">latest build: {latestBuild}</p>
        </article>
        <article className="ui-kpi">
          <span className="ui-muted">Assistants</span>
          <p className="ui-kpi__value">{assistants.length}</p>
          <p className="ui-muted">threads tracked: {threads.length}</p>
        </article>
        <article className="ui-kpi">
          <span className="ui-muted">Attention</span>
          <p className="ui-kpi__value">{(summary?.error_runs || 0) + (summary?.pending_runs || 0) + (summary?.stuck_runs || 0)}</p>
          <p className="ui-muted">latest thread: {latestThread}</p>
        </article>
      </section>

      <section className="ui-subpanel ui-stack">
        <div className="ui-row" style={{ justifyContent: "space-between" }}>
          <h2 style={{ margin: 0 }}>System snapshot</h2>
          <StatusChip status={health?.status || "unknown"} />
        </div>
        <div className="ui-kpi-grid">
          <article className="ui-kpi">
            <span className="ui-muted">Pending runs</span>
            <p className="ui-kpi__value">{summary?.pending_runs || 0}</p>
          </article>
          <article className="ui-kpi">
            <span className="ui-muted">Error runs</span>
            <p className="ui-kpi__value">{summary?.error_runs || 0}</p>
          </article>
          <article className="ui-kpi">
            <span className="ui-muted">Stuck runs</span>
            <p className="ui-kpi__value">{summary?.stuck_runs || 0}</p>
          </article>
          <article className="ui-kpi">
            <span className="ui-muted">Dead letters</span>
            <p className="ui-kpi__value">{summary?.webhook_dead_letters || 0}</p>
          </article>
        </div>
      </section>
    </main>
  );
}
