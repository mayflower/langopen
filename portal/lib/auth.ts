import { createHash, createHmac, randomBytes, timingSafeEqual } from "crypto";

export type OIDCDiscovery = {
  authorization_endpoint: string;
  token_endpoint: string;
};

export type SessionPayload = {
  access_token?: string;
  id_token?: string;
  token_type?: string;
  expires_at?: number;
};

export function randomVerifier(): string {
  return base64url(randomBytes(64));
}

export function pkceChallenge(verifier: string): string {
  return base64url(createHash("sha256").update(verifier).digest());
}

export function base64url(value: Buffer | string): string {
  const input = typeof value === "string" ? Buffer.from(value, "utf8") : value;
  return input
    .toString("base64")
    .replace(/\+/g, "-")
    .replace(/\//g, "_")
    .replace(/=+$/g, "");
}

export function signSession(payloadB64: string, secret: string): string {
  return base64url(createHmac("sha256", secret).update(payloadB64).digest());
}

export function encodeSession(payload: SessionPayload, secret: string): string {
  const payloadB64 = base64url(JSON.stringify(payload));
  const sig = signSession(payloadB64, secret);
  return `${payloadB64}.${sig}`;
}

export function decodeSession(token: string, secret: string): SessionPayload | null {
  const [payloadB64, signature] = token.split(".");
  if (!payloadB64 || !signature) {
    return null;
  }
  const expected = signSession(payloadB64, secret);
  if (!safeEqual(signature, expected)) {
    return null;
  }
  try {
    return JSON.parse(Buffer.from(payloadB64, "base64url").toString("utf8")) as SessionPayload;
  } catch {
    return null;
  }
}

function safeEqual(a: string, b: string): boolean {
  const aa = Buffer.from(a);
  const bb = Buffer.from(b);
  if (aa.length !== bb.length) {
    return false;
  }
  return timingSafeEqual(aa, bb);
}

export async function fetchDiscovery(issuer: string): Promise<OIDCDiscovery> {
  const wellKnown = `${issuer.replace(/\/$/, "")}/.well-known/openid-configuration`;
  const resp = await fetch(wellKnown, { cache: "no-store" });
  if (!resp.ok) {
    throw new Error(`OIDC discovery failed (${resp.status})`);
  }
  const json = (await resp.json()) as Partial<OIDCDiscovery>;
  if (!json.authorization_endpoint || !json.token_endpoint) {
    throw new Error("OIDC discovery missing endpoints");
  }
  return {
    authorization_endpoint: json.authorization_endpoint,
    token_endpoint: json.token_endpoint
  };
}

export function cookieSecure(): boolean {
  return process.env.NODE_ENV === "production";
}
