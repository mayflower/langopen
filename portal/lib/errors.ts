export type NormalizedError = {
  code: string;
  title: string;
  message: string;
  actionLabel?: string;
  actionHref?: string;
};

type ApiEnvelope = {
  error?: {
    code?: string;
    message?: string;
  };
  message?: string;
};

export async function toNormalizedError(resp: Response, fallback: string): Promise<NormalizedError> {
  const text = await resp.text();
  const envelope = parseEnvelope(text);
  const code = envelope.error?.code || `http_${resp.status}`;
  const sourceMessage = envelope.error?.message || envelope.message || text || fallback;

  if (resp.status === 401 || resp.status === 403) {
    return {
      code,
      title: "Authentication required",
      message: "Your session is missing or expired. Re-authenticate to continue.",
      actionLabel: "Sign in",
      actionHref: "/api/auth?next=/"
    };
  }

  if (resp.status >= 500) {
    return {
      code,
      title: "Service unavailable",
      message: "LangOpen is temporarily unavailable. Retry in a few seconds."
    };
  }

  return {
    code,
    title: "Request failed",
    message: sanitizeMessage(sourceMessage, fallback)
  };
}

export function normalizeClientError(err: unknown, fallback: string): string {
  if (err instanceof Error && err.message) {
    return sanitizeMessage(err.message, fallback);
  }
  return fallback;
}

export function sanitizeMessage(message: string, fallback: string): string {
  const trimmed = message.trim();
  if (!trimmed) {
    return fallback;
  }
  if (trimmed.startsWith("{") && trimmed.endsWith("}")) {
    const parsed = parseEnvelope(trimmed);
    if (parsed.error?.message) {
      return parsed.error.message;
    }
    if (parsed.message) {
      return parsed.message;
    }
  }
  return trimmed;
}

function parseEnvelope(input: string): ApiEnvelope {
  if (!input) {
    return {};
  }
  try {
    return JSON.parse(input) as ApiEnvelope;
  } catch {
    return { message: input };
  }
}
