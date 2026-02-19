import { NextRequest, NextResponse } from "next/server";

export async function POST(req: NextRequest) {
  const response = NextResponse.json({ ok: true });
  response.cookies.delete("langopen_session");
  response.cookies.delete("oidc_state");
  response.cookies.delete("oidc_verifier");
  response.cookies.delete("oidc_next");
  return response;
}
