import { test as base, expect } from "@playwright/test";
import { createSessionToken } from "./helpers/session";

export const test = base.extend({
  page: async ({ page, context, baseURL }, use) => {
    const targetURL = new URL(baseURL || "http://127.0.0.1:4173");
    const token = createSessionToken();

    await fetch("http://127.0.0.1:8787/__test/reset", { method: "POST" });

    await context.addCookies([
      {
        name: "langopen_session",
        value: token,
        domain: targetURL.hostname,
        path: "/",
        httpOnly: true,
        secure: targetURL.protocol === "https:",
        sameSite: "Lax"
      }
    ]);

    await page.addInitScript(() => {
      const fixed = Date.parse("2026-02-20T08:00:00Z");
      Date.now = () => fixed;
    });

    await use(page);
  }
});

export { expect };
