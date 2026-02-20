import { Button } from "../../components/ui/Button";
import { PageHeader } from "../../components/ui/PageHeader";
import { StatusChip } from "../../components/ui/StatusChip";
import { fetchControl, fetchData, portalRuntimeConfig } from "../../lib/platform";

type Build = { status: string };

type HealthResponse = {
  status: string;
  checks?: Record<string, { status: string; error?: string }>;
};

type AttentionSummary = {
  stuck_runs: number;
  pending_runs: number;
  error_runs: number;
  webhook_dead_letters: number;
  stuck_threshold_seconds: number;
};

export default async function AttentionPage() {
  const cfg = portalRuntimeConfig();

  let health: HealthResponse | null = null;
  let summary: AttentionSummary | null = null;
  let builds: Build[] = [];
  let healthErr = "";

  try {
    const projectQuery = `project_id=${encodeURIComponent(cfg.projectID)}`;
    [health, summary, builds] = await Promise.all([
      fetchData<HealthResponse>("/api/v1/system/health"),
      fetchData<AttentionSummary>("/api/v1/system/attention"),
      fetchControl<Build[]>(`/internal/v1/builds?${projectQuery}`)
    ]);
  } catch (err) {
    healthErr = err instanceof Error ? err.message : "Failed to load health.";
  }

  const buildFailures = builds.filter((b) => b.status === "failed").length;
  const cards = [
    {
      label: "Build failures",
      value: buildFailures,
      severity: buildFailures > 0 ? "error" : "ok",
      detail: "Failed builds in project scope"
    },
    {
      label: "Stuck runs",
      value: summary?.stuck_runs ?? 0,
      severity: (summary?.stuck_runs ?? 0) > 0 ? "warning" : "ok",
      detail: `Threshold: > ${summary?.stuck_threshold_seconds ?? 300}s`
    },
    {
      label: "Pending runs",
      value: summary?.pending_runs ?? 0,
      severity: (summary?.pending_runs ?? 0) > 5 ? "warning" : "ok",
      detail: "Queued and pending execution"
    },
    {
      label: "Error runs",
      value: summary?.error_runs ?? 0,
      severity: (summary?.error_runs ?? 0) > 0 ? "error" : "ok",
      detail: "Terminal run failures"
    },
    {
      label: "Webhook dead letters",
      value: summary?.webhook_dead_letters ?? 0,
      severity: (summary?.webhook_dead_letters ?? 0) > 0 ? "error" : "ok",
      detail: "Undeliverable webhook attempts"
    }
  ];

  return (
    <main className="ui-panel ui-stack">
      <PageHeader title="Needs Attention" subtitle="Operational signals across runs, builds, dependencies, and webhook delivery." />

      {healthErr ? (
        <section className="ui-error" role="alert">
          <h3>Unable to load attention metrics</h3>
          <p>{healthErr}</p>
          <div className="ui-error__actions">
            <a className="ui-button ui-button--primary" href="/api/auth?next=/attention">
              Re-authenticate
            </a>
          </div>
        </section>
      ) : null}

      {!healthErr ? (
        <>
          <section className="ui-subpanel ui-stack">
            <div className="ui-row" style={{ justifyContent: "space-between" }}>
              <h2 style={{ margin: 0 }}>System health</h2>
              <StatusChip status={health?.status || "unknown"} />
            </div>
            <div className="ui-kpi-grid">
              {cards.map((card) => (
                <article key={card.label} className="ui-kpi">
                  <span className="ui-muted">{card.label}</span>
                  <p className="ui-kpi__value">{card.value}</p>
                  <p className="ui-muted">{card.detail}</p>
                  <StatusChip status={card.severity} />
                </article>
              ))}
            </div>
          </section>

          <section className="ui-subpanel ui-stack">
            <h2 style={{ margin: 0 }}>Operational deep links</h2>
            <p className="ui-muted">Jump into Grafana Explore, Tempo, and dashboard context for diagnostics.</p>
            <div className="ui-row">
              {cfg.grafanaExploreUrl ? (
                <a className="ui-button ui-button--secondary" href={cfg.grafanaExploreUrl} target="_blank" rel="noreferrer">
                  Open Explore
                </a>
              ) : (
                <Button type="button" variant="ghost" disabled>
                  Set `GRAFANA_EXPLORE_URL`
                </Button>
              )}
              {cfg.grafanaTempoUrl ? (
                <a className="ui-button ui-button--secondary" href={cfg.grafanaTempoUrl} target="_blank" rel="noreferrer">
                  Open Tempo
                </a>
              ) : (
                <Button type="button" variant="ghost" disabled>
                  Set `GRAFANA_TEMPO_URL`
                </Button>
              )}
              {cfg.grafanaDashboardUrl ? (
                <a className="ui-button ui-button--primary" href={cfg.grafanaDashboardUrl} target="_blank" rel="noreferrer">
                  Open Dashboard
                </a>
              ) : (
                <Button type="button" variant="ghost" disabled>
                  Set `GRAFANA_DASHBOARD_URL`
                </Button>
              )}
            </div>
          </section>
        </>
      ) : null}
    </main>
  );
}
