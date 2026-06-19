import { expect, type Page, test } from "@playwright/test";
import { applyDefinitions, loadFixtureDefinition } from "./helpers/fixtures";

test("DAG view surfaces volume mounts and workload identity from applied job definitions", async ({ page, request }) => {
  const volumeDefinition = await loadFixtureDefinition("volume-artifacts.job.yaml");
  const identityDefinition = await loadFixtureDefinition("k8s-workload-identity-volume.job.yaml");
  const volumeAlias = String(volumeDefinition.metadata?.alias);
  const identityAlias = String(identityDefinition.metadata?.alias);

  await applyDefinitions(request, volumeDefinition, identityDefinition);

  await openJobDetail(page, volumeAlias);
  const volumeBadges = page.getByTestId("runtime-volume-badge");
  await expect(volumeBadges).toHaveCount(3);
  await expect(volumeBadges).toHaveText(["1", "1", "1"]);
  await expect(page.getByTestId("runtime-identity-badge")).toHaveCount(0);

  await openJobDetail(page, identityAlias);
  const identityVolumeBadges = page.getByTestId("runtime-volume-badge");
  await expect(identityVolumeBadges).toHaveCount(2);
  await expect(identityVolumeBadges).toHaveText(["1", "1"]);

  const identityBadges = page.getByTestId("runtime-identity-badge");
  await expect(identityBadges).toHaveCount(2);
  await expect(identityBadges).toHaveText(["SA", "SA"]);
  await expect(page.locator('[data-testid="runtime-identity-badge"][title="ServiceAccount caesium-report-reader"]')).toHaveCount(1);
  await expect(page.locator('[data-testid="runtime-identity-badge"][title="ServiceAccount caesium-cloud-writer"]')).toHaveCount(1);
});

async function openJobDetail(page: Page, alias: string) {
  await page.goto("/jobs");
  const row = page.locator('[data-testid="job-row"]', { hasText: alias }).first();
  await expect(row).toBeVisible();
  await row.getByRole("link", { name: alias }).click();
  await page.waitForURL(/\/jobs\/[^/]+$/);
  await expect(page.getByRole("heading", { name: alias })).toBeVisible();
  await expect(page.locator(".react-flow__node").first()).toBeVisible();
}
