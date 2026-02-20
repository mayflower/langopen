import { NextRequest, NextResponse } from "next/server";
import { cookieSecure, encodeSession, fetchDiscovery } from "../../../../lib/auth";

export async function GET(req: NextRequest) {
  const issuer = process.env.OPENAUTH_ISSUER_URL;
  const clientId = process.env.OPENAUTH_CLIENT_ID;
  const clientSecret = process.env.OPENAUTH_CLIENT_SECRET;
  const redirectUri = process.env.OPENAUTH_REDIRECT_URI;
  const sessionSecret = process.env.PORTAL_SESSION_SECRET || "dev-session-secret";

  if (!issuer || !clientId || !redirectUri) {
    return NextResponse.json(
      {
        error: "missing_openauth_config",
        message: "Set OPENAUTH_ISSUER_URL, OPENAUTH_CLIENT_ID, OPENAUTH_REDIRECT_URI"
      },
      { status: 500 }
    );
  }

  const state = req.nextUrl.searchParams.get("state");
  const code = req.nextUrl.searchParams.get("code");
  const storedState = req.cookies.get("oidc_state")?.value;
  const verifier = req.cookies.get("oidc_verifier")?.value;
  const nextPath = sanitizeNextPath(req.cookies.get("oidc_next")?.value);

  if (!state || !code || !storedState || !verifier || state !== storedState) {
    return NextResponse.json({ error: "invalid_callback", message: "state or code validation failed" }, { status: 400 });
  }

  let tokenEndpoint: string;
  try {
    const discovery = await fetchDiscovery(issuer);
    tokenEndpoint = discovery.token_endpoint;
  } catch (err) {
    return NextResponse.json(
      {
        error: "oidc_discovery_failed",
        message: err instanceof Error ? err.message : "discovery failed"
      },
      { status: 502 }
    );
  }

  const form = new URLSearchParams();
  form.set("grant_type", "authorization_code");
  form.set("code", code);
  form.set("redirect_uri", redirectUri);
  form.set("client_id", clientId);
  form.set("code_verifier", verifier);
  if (clientSecret) {
    form.set("client_secret", clientSecret);
  }

  const tokenResp = await fetch(tokenEndpoint, {
    method: "POST",
    headers: { "content-type": "application/x-www-form-urlencoded" },
    body: form,
    cache: "no-store"
  });

  if (!tokenResp.ok) {
    const errorText = await tokenResp.text();
    return NextResponse.json(
      { error: "token_exchange_failed", message: errorText || `status ${tokenResp.status}` },
      { status: 502 }
    );
  }

  const tokenJSON = (await tokenResp.json()) as {
    access_token?: string;
    id_token?: string;
    token_type?: string;
    expires_in?: number;
  };

  const expiresAt = Date.now() + (tokenJSON.expires_in ?? 3600) * 1000;
  const session = encodeSession(
    {
      access_token: tokenJSON.access_token,
      id_token: tokenJSON.id_token,
      token_type: tokenJSON.token_type,
      expires_at: expiresAt
    },
    sessionSecret
  );

  const secure = cookieSecure();
  const res = NextResponse.redirect(new URL(nextPath, req.url));
  res.cookies.set("langopen_session", session, {
    httpOnly: true,
    sameSite: "lax",
    secure,
    path: "/",
    expires: new Date(expiresAt)
  });
  res.cookies.delete("oidc_state");
  res.cookies.delete("oidc_verifier");
  res.cookies.delete("oidc_next");
  return res;
}

function sanitizeNextPath(nextPath: string | undefined): string {
  if (!nextPath || !nextPath.startsWith("/") || nextPath.startsWith("//")) {
    return "/";
  }
  return nextPath;
}
