import { NextRequest, NextResponse } from "next/server";
import { decodeSession } from "../../../../lib/auth";

export async function GET(req: NextRequest) {
  const raw = req.cookies.get("langopen_session")?.value;
  if (!raw) {
    return NextResponse.json({ authenticated: false }, { status: 401 });
  }

  const secret = process.env.PORTAL_SESSION_SECRET || "dev-session-secret";
  const session = decodeSession(raw, secret);
  if (!session) {
    return NextResponse.json({ authenticated: false, error: "invalid_session" }, { status: 401 });
  }

  if (session.expires_at && Date.now() > session.expires_at) {
    return NextResponse.json({ authenticated: false, error: "session_expired" }, { status: 401 });
  }

  return NextResponse.json({
    authenticated: true,
    token_type: session.token_type,
    expires_at: session.expires_at
  });
}
