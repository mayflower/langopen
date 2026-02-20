"use client";

import { sanitizeMessage } from "./errors";

type ProxyErrorBody = {
  error?: {
    title?: string;
    message?: string;
    action_label?: string;
    action_href?: string;
  };
  message?: string;
};

export type PortalError = {
  title: string;
  message: string;
  actionLabel?: string;
  actionHref?: string;
  status: number;
};

export async function fetchPortalJSON<T>(input: RequestInfo | URL, init?: RequestInit): Promise<T> {
  const resp = await fetch(input, { cache: "no-store", ...(init || {}) });
  if (!resp.ok) {
    throw await toPortalError(resp, "Request failed.");
  }
  return (await resp.json()) as T;
}

export async function toPortalError(resp: Response, fallback: string): Promise<PortalError> {
  const text = await resp.text();
  const parsed = parseProxyError(text);

  return {
    status: resp.status,
    title: parsed.error?.title || (resp.status === 401 || resp.status === 403 ? "Authentication required" : "Request failed"),
    message: sanitizeMessage(parsed.error?.message || parsed.message || text || fallback, fallback),
    actionLabel: parsed.error?.action_label,
    actionHref: parsed.error?.action_href
  };
}

function parseProxyError(input: string): ProxyErrorBody {
  if (!input) {
    return {};
  }
  try {
    return JSON.parse(input) as ProxyErrorBody;
  } catch {
    return { message: input };
  }
}
