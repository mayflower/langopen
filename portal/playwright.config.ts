import { defineConfig, devices } from "@playwright/test";

const PORT = 4173;
const MOCK_PORT = 8787;

export default defineConfig({
  testDir: "./tests/e2e",
  fullyParallel: false,
  retries: process.env.CI ? 2 : 0,
  workers: process.env.CI ? 1 : undefined,
  reporter: [["list"], ["html", { open: "never" }]],
  use: {
    baseURL: `http://127.0.0.1:${PORT}`,
    trace: "on-first-retry",
    screenshot: "only-on-failure",
    video: "retain-on-failure"
  },
  webServer: [
    {
      command: `node tests/e2e/mock-backend.mjs`,
      url: `http://127.0.0.1:${MOCK_PORT}/healthz`,
      reuseExistingServer: !process.env.CI,
      timeout: 120000
    },
    {
      command: `PORTAL_SESSION_SECRET=dev-session-secret PLATFORM_API_KEY=sk_test_playwright PLATFORM_DATA_API_BASE_URL=http://127.0.0.1:${MOCK_PORT} PLATFORM_CONTROL_API_BASE_URL=http://127.0.0.1:${MOCK_PORT} npm run dev -- --hostname 127.0.0.1 --port ${PORT}`,
      url: `http://127.0.0.1:${PORT}/api/healthz`,
      reuseExistingServer: !process.env.CI,
      timeout: 120000
    }
  ],
  projects: [
    {
      name: "chromium",
      use: { ...devices["Desktop Chrome"], viewport: { width: 1440, height: 960 } }
    }
  ]
});
