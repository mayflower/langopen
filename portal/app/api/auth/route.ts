import { NextRequest, NextResponse } from "next/server";
import { cookieSecure, fetchDiscovery, pkceChallenge, randomVerifier } from "../../../lib/auth";

export async function GET(req: NextRequest) {
  const issuer = process.env.OPENAUTH_ISSUER_URL;
  const clientId = process.env.OPENAUTH_CLIENT_ID;
  const redirectUri = process.env.OPENAUTH_REDIRECT_URI;

  if (!issuer || !clientId || !redirectUri) {
    return NextResponse.json(
      {
        error: "missing_openauth_config",
        message: "Set OPENAUTH_ISSUER_URL, OPENAUTH_CLIENT_ID, OPENAUTH_REDIRECT_URI"
      },
      { status: 500 }
    );
  }

  let authorizationEndpoint: string;
  try {
    const discovery = await fetchDiscovery(issuer);
    authorizationEndpoint = discovery.authorization_endpoint;
  } catch (err) {
    return NextResponse.json(
      {
        error: "oidc_discovery_failed",
        message: err instanceof Error ? err.message : "discovery failed"
      },
      { status: 502 }
    );
  }

  const state = crypto.randomUUID();
  const verifier = randomVerifier();
  const challenge = pkceChallenge(verifier);
  const nextPath = sanitizeNextPath(req.nextUrl.searchParams.get("next"));

  const url = new URL(authorizationEndpoint);
  url.searchParams.set("response_type", "code");
  url.searchParams.set("client_id", clientId);
  url.searchParams.set("redirect_uri", redirectUri);
  url.searchParams.set("scope", "openid profile email");
  url.searchParams.set("state", state);
  url.searchParams.set("code_challenge_method", "S256");
  url.searchParams.set("code_challenge", challenge);

  const secure = cookieSecure();
  const res = NextResponse.redirect(url.toString());
  res.cookies.set("oidc_state", state, { httpOnly: true, sameSite: "lax", secure, path: "/" });
  res.cookies.set("oidc_verifier", verifier, { httpOnly: true, sameSite: "lax", secure, path: "/" });
  res.cookies.set("oidc_next", nextPath, { httpOnly: true, sameSite: "lax", secure, path: "/" });
  return res;
}

function sanitizeNextPath(nextPath: string | null): string {
  if (!nextPath || !nextPath.startsWith("/") || nextPath.startsWith("//")) {
    return "/";
  }
  return nextPath;
}
