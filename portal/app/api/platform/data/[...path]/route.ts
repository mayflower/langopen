import { NextRequest, NextResponse } from "next/server";

const BASE = process.env.PLATFORM_DATA_API_BASE_URL || "http://langopen-api-server";
const API_KEY = process.env.PLATFORM_API_KEY || process.env.BOOTSTRAP_API_KEY || "";

export async function GET(req: NextRequest, ctx: { params: Promise<{ path: string[] }> }) {
  return proxy(req, ctx);
}

export async function POST(req: NextRequest, ctx: { params: Promise<{ path: string[] }> }) {
  return proxy(req, ctx);
}

export async function PUT(req: NextRequest, ctx: { params: Promise<{ path: string[] }> }) {
  return proxy(req, ctx);
}

export async function PATCH(req: NextRequest, ctx: { params: Promise<{ path: string[] }> }) {
  return proxy(req, ctx);
}

export async function DELETE(req: NextRequest, ctx: { params: Promise<{ path: string[] }> }) {
  return proxy(req, ctx);
}

export async function HEAD(req: NextRequest, ctx: { params: Promise<{ path: string[] }> }) {
  return proxy(req, ctx);
}

async function proxy(req: NextRequest, ctx: { params: Promise<{ path: string[] }> }) {
  if (!API_KEY) {
    return NextResponse.json(
      { error: "missing_platform_api_key", message: "Set PLATFORM_API_KEY or BOOTSTRAP_API_KEY" },
      { status: 500 }
    );
  }

  const { path } = await ctx.params;
  const upstream = new URL(path.join("/"), ensureSlash(BASE));
  upstream.search = req.nextUrl.search;

  const headers = new Headers();
  headers.set("X-Api-Key", API_KEY);

  const passthrough = ["accept", "content-type", "last-event-id", "cache-control", "x-auth-scheme"];
  passthrough.forEach((key) => {
    const value = req.headers.get(key);
    if (value) {
      headers.set(key, value);
    }
  });

  const method = req.method;
  const body = method === "GET" || method === "HEAD" ? undefined : await req.arrayBuffer();

  const resp = await fetch(upstream.toString(), {
    method,
    headers,
    body,
    redirect: "manual",
    cache: "no-store"
  });

  const outHeaders = new Headers();
  ["content-type", "cache-control", "connection"].forEach((name) => {
    const value = resp.headers.get(name);
    if (value) {
      outHeaders.set(name, value);
    }
  });
  outHeaders.set("x-proxied-by", "langopen-portal");

  return new NextResponse(resp.body, {
    status: resp.status,
    headers: outHeaders
  });
}

function ensureSlash(input: string): string {
  return input.endsWith("/") ? input : `${input}/`;
}
