import { NextRequest, NextResponse } from "next/server";

export async function middleware(req: NextRequest) {
  const { pathname, search } = req.nextUrl;

  if (pathname.startsWith("/_next") || pathname.startsWith("/api/auth") || pathname === "/favicon.ico") {
    return NextResponse.next();
  }

  const token = req.cookies.get("langopen_session")?.value;
  if (!token || !(await isValidSessionToken(token))) {
    const login = req.nextUrl.clone();
    login.pathname = "/api/auth";
    login.search = `?next=${encodeURIComponent(pathname + search)}`;
    return NextResponse.redirect(login);
  }

  return NextResponse.next();
}

export const config = {
  matcher: ["/((?!_next/static|_next/image|favicon.ico).*)"]
};

async function isValidSessionToken(token: string): Promise<boolean> {
  const parts = token.split(".");
  if (parts.length !== 2) {
    return false;
  }
  const payloadB64 = parts[0];
  const signatureB64 = parts[1];
  if (!payloadB64 || !signatureB64) {
    return false;
  }

  const secret = process.env.PORTAL_SESSION_SECRET || "dev-session-secret";
  const expected = await hmacSHA256Base64URL(secret, payloadB64);
  if (!constantTimeEqual(signatureB64, expected)) {
    return false;
  }

  const payload = decodeBase64URL(payloadB64);
  if (!payload) {
    return false;
  }
  let parsed: { expires_at?: number };
  try {
    parsed = JSON.parse(payload) as { expires_at?: number };
  } catch {
    return false;
  }
  if (typeof parsed.expires_at === "number" && Date.now() > parsed.expires_at) {
    return false;
  }
  return true;
}

async function hmacSHA256Base64URL(secret: string, input: string): Promise<string> {
  const key = await crypto.subtle.importKey(
    "raw",
    new TextEncoder().encode(secret),
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["sign"]
  );
  const signed = await crypto.subtle.sign("HMAC", key, new TextEncoder().encode(input));
  return encodeBase64URL(new Uint8Array(signed));
}

function encodeBase64URL(bytes: Uint8Array): string {
  let binary = "";
  for (const b of bytes) {
    binary += String.fromCharCode(b);
  }
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/g, "");
}

function decodeBase64URL(input: string): string | null {
  const normalized = input.replace(/-/g, "+").replace(/_/g, "/");
  const padded = normalized + "=".repeat((4 - (normalized.length % 4)) % 4);
  try {
    return atob(padded);
  } catch {
    return null;
  }
}

function constantTimeEqual(a: string, b: string): boolean {
  if (a.length !== b.length) {
    return false;
  }
  let diff = 0;
  for (let i = 0; i < a.length; i += 1) {
    diff |= a.charCodeAt(i) ^ b.charCodeAt(i);
  }
  return diff === 0;
}
