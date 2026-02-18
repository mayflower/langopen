const DATA_BASE = process.env.PLATFORM_DATA_API_BASE_URL || "http://langopen-api-server";
const CONTROL_BASE = process.env.PLATFORM_CONTROL_API_BASE_URL || "http://langopen-control-plane";
const API_KEY = process.env.PLATFORM_API_KEY || process.env.BOOTSTRAP_API_KEY || "";

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
    throw new Error(`data api ${path} failed (${resp.status})`);
  }
  return (await resp.json()) as T;
}

export async function fetchControl<T>(path: string): Promise<T> {
  const resp = await request(CONTROL_BASE, path);
  if (!resp.ok) {
    throw new Error(`control api ${path} failed (${resp.status})`);
  }
  return (await resp.json()) as T;
}

export function portalRuntimeConfig() {
  return {
    dataBase: DATA_BASE,
    controlBase: CONTROL_BASE,
    hasAPIKey: API_KEY.length > 0,
    grafanaBase: process.env.GRAFANA_BASE_URL || ""
  };
}

function ensureSlash(input: string): string {
  return input.endsWith("/") ? input : `${input}/`;
}
