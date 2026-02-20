import { describe, expect, it } from "vitest";
import { normalizeClientError, sanitizeMessage, toNormalizedError } from "../../lib/errors";

describe("errors", () => {
  it("maps 401 to auth guidance", async () => {
    const resp = new Response(JSON.stringify({ error: { code: "missing_api_key", message: "x" } }), {
      status: 401,
      headers: { "content-type": "application/json" }
    });

    const normalized = await toNormalizedError(resp, "fallback");
    expect(normalized.title).toBe("Authentication required");
    expect(normalized.actionHref).toBe("/api/auth?next=/");
  });

  it("sanitizes json envelope messages", () => {
    const message = sanitizeMessage('{"error":{"message":"bad input"}}', "fallback");
    expect(message).toBe("bad input");
  });

  it("normalizes unknown errors", () => {
    expect(normalizeClientError(null, "fallback")).toBe("fallback");
  });
});
