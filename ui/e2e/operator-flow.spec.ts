import fs from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { expect, test } from "@playwright/test";
import { parseAllDocuments } from "yaml";

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);

async function loadFixtureDefinition() {
  const fixturePath = path.resolve(__dirname, "../../docs/examples/log-streaming.job.yaml");
  const yaml = await fs.readFile(fixturePath, "utf8");
  const docs = parseAllDocuments(yaml);
  const definition = docs[0]?.toJS() as Record<string, any> | undefined;
  if (!definition) {
    throw new Error("failed to parse log-streaming fixture");
  }

  const suffix = Date.now().toString(36);
  definition.metadata = {
    ...(definition.metadata ?? {}),
    alias: `log-streaming-e2e-${suffix}`,
  };

  if (definition.trigger?.configuration) {
    definition.trigger.configuration.path = `/hooks/demo/log-streaming-${suffix}`;
  }

  return definition;
}

test("operator can trigger a run, watch it update live, and inspect retained logs", async ({ page, request }) => {
  test.slow();

  const definition = await loadFixtureDefinition();
  const alias = String(definition.metadata.alias);

  const applyResponse = await request.post("/v1/jobdefs/apply", {
    data: { definitions: [definition] },
  });
  expect(applyResponse.ok()).toBeTruthy();

  await page.goto("/jobs");

  const row = page.locator("tr", { hasText: alias }).first();
  await expect(row).toBeVisible();

  await row.locator('button[title="Trigger run"]').click();

  await page.waitForURL(/\/jobs\/[^/]+\/runs\/[^/]+$/);
  await expect(page.getByRole("heading", { name: /Run / })).toBeVisible();
  await expect(page.getByText("running", { exact: true }).first()).toBeVisible();

  let node = page.locator(".react-flow__node").first();
  await expect(node).toBeVisible();
  await node.click();

  await expect(page.getByTestId("task-detail-panel")).toBeVisible();
  await expect(page.getByText("Live", { exact: true })).toBeVisible();
  await expect(page.getByTestId("task-log-plaintext")).toContainText("Starting log streaming showcase");

  await expect(page.getByText("succeeded", { exact: true }).first()).toBeVisible({ timeout: 90_000 });

  await page.reload();
  node = page.locator(".react-flow__node").first();
  await expect(node).toBeVisible();
  await node.click();

  await expect(page.getByText("Retained", { exact: true })).toBeVisible({ timeout: 30_000 });
  await expect(page.getByTestId("task-log-plaintext")).toContainText(
    "stream complete: retained logs should remain available after cleanup",
  );
});
