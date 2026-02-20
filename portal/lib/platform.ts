const DATA_BASE = process.env.PLATFORM_DATA_API_BASE_URL || "http://langopen-api-server";
const CONTROL_BASE = process.env.PLATFORM_CONTROL_API_BASE_URL || "http://langopen-control-plane";
const API_KEY = process.env.PLATFORM_API_KEY || process.env.BOOTSTRAP_API_KEY || "";
const PORTAL_PROJECT_ID = process.env.PORTAL_PROJECT_ID || "proj_default";

type FetchOptions = {
  method?: string;
  body?: unknown;
  headers?: Record<string, string>;
};

async function request(base: string, path: string, opts: FetchOptions = {}): Promise<Response> {
  const url = new URL(path, ensureSlash(base));
  const headers = new Headers(opts.headers || {});
  if (API_KEY && !headers.has("X-Api-Key")) {
    headers.set("X-Api-Key", API_KEY);
  }
  if (!headers.has("Content-Type") && opts.body !== undefined) {
    headers.set("Content-Type", "application/json");
  }

  return fetch(url.toString(), {
    method: opts.method || "GET",
    headers,
    body: opts.body === undefined ? undefined : JSON.stringify(opts.body),
    cache: "no-store"
  });
}

export async function fetchData<T>(path: string): Promise<T> {
  const resp = await request(DATA_BASE, path);
  if (!resp.ok) {
    throw await toFriendlyError(resp, "Data API request failed.");
  }
  return (await resp.json()) as T;
}

export async function fetchControl<T>(path: string): Promise<T> {
  const resp = await request(CONTROL_BASE, path);
  if (!resp.ok) {
    throw await toFriendlyError(resp, "Control API request failed.");
  }
  return (await resp.json()) as T;
}

export function portalRuntimeConfig() {
  const grafanaBase = trimTrailingSlash(process.env.GRAFANA_BASE_URL || "");
  const defaultExplore = grafanaBase ? `${grafanaBase}/explore` : "";
  const defaultTempo = grafanaBase ? `${grafanaBase}/explore?left=%7B%22datasource%22:%22Tempo%22%7D` : "";
  const defaultDashboard = grafanaBase ? `${grafanaBase}/d/langopen-overview/langopen-overview` : "";
  return {
    dataBase: DATA_BASE,
    controlBase: CONTROL_BASE,
    projectID: PORTAL_PROJECT_ID,
    hasAPIKey: API_KEY.length > 0,
    grafanaBase,
    grafanaExploreUrl: process.env.GRAFANA_EXPLORE_URL || defaultExplore,
    grafanaTempoUrl: process.env.GRAFANA_TEMPO_URL || defaultTempo,
    grafanaDashboardUrl: process.env.GRAFANA_DASHBOARD_URL || defaultDashboard
  };
}

function ensureSlash(input: string): string {
  return input.endsWith("/") ? input : `${input}/`;
}

function trimTrailingSlash(input: string): string {
  if (!input) {
    return "";
  }
  return input.endsWith("/") ? input.slice(0, -1) : input;
}

async function toFriendlyError(resp: Response, fallback: string): Promise<Error> {
  const text = await resp.text();
  if (resp.status === 401 || resp.status === 403) {
    return new Error("Your session is missing or expired. Re-authenticate to continue.");
  }
  if (!text) {
    return new Error(fallback);
  }
  try {
    const parsed = JSON.parse(text) as { error?: { message?: string }; message?: string };
    return new Error(parsed.error?.message || parsed.message || fallback);
  } catch {
    return new Error(text);
  }
}
