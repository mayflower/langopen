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
    [health, summary, builds] = await Promise.all([
      fetchData<HealthResponse>("/api/v1/system/health"),
      fetchData<AttentionSummary>("/api/v1/system/attention"),
      fetchControl<Build[]>("/internal/v1/builds")
    ]);
  } catch (err) {
    healthErr = err instanceof Error ? err.message : "failed to load health";
  }

  const buildFailures = builds.filter((b) => b.status === "failed").length;
  const degraded = health?.status && health.status !== "ok";

  return (
    <main className="hero">
      <h2>Needs Attention</h2>
      {healthErr ? <p className="warn">{healthErr}</p> : null}
      <ul>
        <li>system health: {health?.status || "unknown"}</li>
        <li>build failures: {buildFailures}</li>
        <li>degraded dependencies: {degraded ? "yes" : "no"}</li>
        <li>stuck runs (&gt; {summary?.stuck_threshold_seconds ?? 300}s): {summary?.stuck_runs ?? 0}</li>
        <li>pending runs: {summary?.pending_runs ?? 0}</li>
        <li>error runs: {summary?.error_runs ?? 0}</li>
        <li>webhook dead letters: {summary?.webhook_dead_letters ?? 0}</li>
      </ul>
      <p>
        Deep links:{" "}
        {cfg.grafanaExploreUrl ? <a href={cfg.grafanaExploreUrl} target="_blank" rel="noreferrer">Explore</a> : "set GRAFANA_*_URL"}
        {" | "}
        {cfg.grafanaTempoUrl ? <a href={cfg.grafanaTempoUrl} target="_blank" rel="noreferrer">Tempo</a> : "set GRAFANA_*_URL"}
        {" | "}
        {cfg.grafanaDashboardUrl ? <a href={cfg.grafanaDashboardUrl} target="_blank" rel="noreferrer">Dashboard</a> : "set GRAFANA_*_URL"}
      </p>
    </main>
  );
}
