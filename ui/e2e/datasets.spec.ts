import { expect, test } from "@playwright/test";

test("dataset board omits per-row hold reads and fetches full selected detail", async ({ page, request }) => {
  const name = `dataset-board-${Date.now().toString(36)}`;
  const datasetPath = `/v1/datasets/_/${name}`;
  const advanced = await request.post(`${datasetPath}/advance`, {
    data: { watermark: "2026-09-09T00:00:00Z" },
  });
  expect(advanced.ok(), await advanced.text()).toBeTruthy();

  const metadataRead = page.waitForResponse((response) => {
    const url = new URL(response.url());
    return url.pathname === datasetPath && url.searchParams.get("include_hold") === "false";
  });
  await page.goto("/datasets");
  expect((await metadataRead).ok()).toBeTruthy();
  const row = page.getByTestId("dataset-row").filter({ hasText: name });
  await expect(row).toBeVisible();

  // A separate cache key must ensure metadata cannot satisfy the full read.
  const fullRead = page.waitForResponse((response) => {
    const url = new URL(response.url());
    return url.pathname === datasetPath && !url.searchParams.has("include_hold");
  });
  await row.click();
  expect((await fullRead).ok()).toBeTruthy();
  await expect(page.getByTestId("dataset-detail-panel")).toContainText(name);
});
