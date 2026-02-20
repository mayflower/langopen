import { NextRequest, NextResponse } from "next/server";
import { toNormalizedError } from "../../../../../lib/errors";

const BASE = process.env.PLATFORM_CONTROL_API_BASE_URL || "http://langopen-control-plane";
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
  const { path } = await ctx.params;
  const upstream = new URL(path.join("/"), ensureSlash(BASE));
  upstream.search = req.nextUrl.search;

  const headers = new Headers();
  if (API_KEY) {
    headers.set("X-Api-Key", API_KEY);
  }

  const passthrough = ["accept", "content-type", "cache-control"];
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

  if (!resp.ok) {
    const normalized = await toNormalizedError(resp, "Control API request failed.");
    return NextResponse.json(
      {
        error: {
          code: normalized.code,
          title: normalized.title,
          message: normalized.message,
          action_label: normalized.actionLabel,
          action_href: normalized.actionHref
        }
      },
      { status: resp.status }
    );
  }

  const outHeaders = new Headers();
  ["content-type", "cache-control", "connection", "x-request-id"].forEach((name) => {
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
