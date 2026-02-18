import { fetchControl, fetchData, portalRuntimeConfig } from "../../lib/platform";

type Build = { status: string };

type HealthResponse = {
  status: string;
  checks?: Record<string, { status: string; error?: string }>;
};

export default async function AttentionPage() {
  const cfg = portalRuntimeConfig();

  let health: HealthResponse | null = null;
  let builds: Build[] = [];
  let healthErr = "";

  try {
    [health, builds] = await Promise.all([
      fetchData<HealthResponse>("/api/v1/system/health"),
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
