import { createHmac } from "node:crypto";

type SessionPayload = {
  token_type?: string;
  expires_at?: number;
};

export function createSessionToken(payload?: SessionPayload, secret = "dev-session-secret") {
  const body: SessionPayload = {
    token_type: "Bearer",
    expires_at: Date.now() + 60 * 60 * 1000,
    ...(payload || {})
  };
  const payloadB64 = base64url(Buffer.from(JSON.stringify(body), "utf8"));
  const sig = base64url(createHmac("sha256", secret).update(payloadB64).digest());
  return `${payloadB64}.${sig}`;
}

function base64url(input: Buffer) {
  return input
    .toString("base64")
    .replace(/\+/g, "-")
    .replace(/\//g, "_")
    .replace(/=+$/g, "");
}
