import { test, expect } from "./fixtures";

test.describe("Portal E2E flows", () => {
  test("deployment lifecycle flow", async ({ page }) => {
    await page.goto("/deployments");
    await expect(page.getByRole("heading", { level: 1, name: "Deployments" })).toBeVisible();

    await page.getByRole("button", { name: "Create deployment" }).first().click();
    const createDialog = page.getByRole("dialog", { name: "Create deployment" });
    await createDialog.getByLabel("Repository URL").fill("https://github.com/acme/new-agent");
    await createDialog.getByLabel("Git ref").fill("main");
    await createDialog.getByRole("button", { name: "Create deployment" }).click();

    await expect(page.getByText("Deployment created.").first()).toBeVisible();
    await expect(page.getByText("https://github.com/acme/new-agent").first()).toBeVisible();

    await page.getByRole("tab", { name: "Build" }).click();
    await page.getByRole("button", { name: "Trigger build" }).click();
    await expect(page.getByText("Build queued.").first()).toBeVisible();

    await page.getByRole("tab", { name: "Variables" }).click();
    await page.getByLabel("Key", { exact: true }).fill("GROQ_API_KEY");
    await page.getByLabel("Value").fill("gsk_test");
    await page.getByRole("button", { name: "Save variable" }).click();
    await expect(page.getByText("Saved variable GROQ_API_KEY.").first()).toBeVisible();
    await expect(page.getByText("GROQ_API_KEY").first()).toBeVisible();

    await page.getByRole("tab", { name: "Secrets (legacy)" }).click();
    await page.getByLabel("Secret name").fill("anthropic-secret");
    await page.getByLabel("Target env key").fill("ANTHROPIC_API_KEY");
    await page.getByRole("button", { name: "Bind secret" }).click();
    await expect(page.getByText("Secret bound to deployment.").first()).toBeVisible();
    await expect(page.getByText("anthropic-secret").first()).toBeVisible();

    await page.getByRole("tab", { name: "Policy" }).click();
    await page.getByLabel("Execution mode").selectOption("mode_b");
    await page.getByRole("button", { name: "Apply policy" }).click();
    await expect(page.getByText("Runtime policy applied.").first()).toBeVisible();
  });

  test("build log inspection flow", async ({ page }) => {
    await page.goto("/builds");
    await expect(page.getByRole("heading", { name: "Builds" })).toBeVisible();

    await page.getByRole("button", { name: "View" }).first().click();
    await expect(page.getByText("Build completed for dep_alpha")).toBeVisible();
  });

  test("api key create and revoke flow", async ({ page }) => {
    await page.goto("/api-keys");
    await expect(page.getByRole("heading", { name: "API Keys" })).toBeVisible();

    await page.getByRole("button", { name: "Create key" }).first().click();
    const createDialog = page.getByRole("dialog", { name: "Create key" });
    await createDialog.getByLabel("Name").fill("integration-test-key");
    await createDialog.getByRole("button", { name: "Create key" }).click();

    await expect(page.getByText("One-time key reveal").first()).toBeVisible();
    await expect(page.getByText("API key created. Copy it now; it will not be shown again.").first()).toBeVisible();

    await page.getByRole("button", { name: "Revoke" }).first().click();
    await page.getByRole("dialog", { name: "Revoke API key" }).getByRole("button", { name: "Revoke" }).click();
    await expect(page.getByText("API key revoked.").first()).toBeVisible();
  });

  test("run stream flow with cancel semantics", async ({ page }) => {
    await page.goto("/runs");
    await expect(page.getByRole("heading", { name: "Runs" })).toBeVisible();

    await page.getByRole("button", { name: "Start stateless run" }).click();
    await expect(page.getByText("run_completed").first()).toBeVisible();

    await page.getByRole("button", { name: "Interrupt" }).click();
    await page.getByRole("button", { name: "Rollback" }).click();
    await expect(page.getByText("Run interrupt request sent.").first()).toBeVisible();
    await expect(page.getByText("Run rollback request sent.").first()).toBeVisible();
  });
});
