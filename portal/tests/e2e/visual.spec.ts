import { test, expect } from "./fixtures";

const pages = [
  { path: "/", name: "home" },
  { path: "/deployments", name: "deployments" },
  { path: "/builds", name: "builds" },
  { path: "/api-keys", name: "api-keys" },
  { path: "/assistants", name: "assistants" },
  { path: "/threads", name: "threads" },
  { path: "/runs", name: "runs" },
  { path: "/attention", name: "attention" }
] as const;

test.describe("Portal visual regression", () => {
  for (const entry of pages) {
    test(`${entry.name} page`, async ({ page }) => {
      await page.goto(entry.path);
      await expect(page.locator("main")).toBeVisible();
      await page.waitForTimeout(200);
      await expect(page).toHaveScreenshot(`${entry.name}.png`, {
        fullPage: true,
        animations: "disabled"
      });
    });
  }
});
