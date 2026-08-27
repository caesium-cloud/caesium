import { expect, test } from "@playwright/test";
import { applyAndRun } from "./helpers/fixtures";

test("fanned step renders as one DAG node with a partition table", async ({ page, request }) => {
  test.slow();

  const { job, run } = await applyAndRun(request, "dynamic-fanout.job.yaml", {
    status: "succeeded",
  });

  await page.goto(`/jobs/${job.id}/runs/${run.id}`);
  await expect(page.getByRole("heading", { name: /Run / })).toBeVisible({ timeout: 30_000 });

  // The fixture has exactly 3 steps (list-files, process-file, publish). Dynamic
  // fan-out must materialize as ONE DAG node per step, never one node per partition.
  const dagNodes = page.locator(".react-flow__node");
  await expect(dagNodes).toHaveCount(3);

  const fannedNode = page.locator(".react-flow__node", { hasText: "process-file" });
  await expect(fannedNode).toBeVisible();

  const badge = fannedNode.getByTestId("fanout-badge");
  await expect(badge).toBeVisible();
  await expect(badge).toHaveText("×3");

  await fannedNode.click();

  const panel = page.getByTestId("task-detail-panel");
  await expect(panel).toBeVisible();
  await panel.getByRole("button", { name: "Details" }).click();

  const partitionTable = page.getByTestId("partition-table");
  await expect(partitionTable).toBeVisible();

  const rows = partitionTable.getByTestId("partition-row");
  await expect(rows).toHaveCount(3);

  const rowValues = await rows.allTextContents();
  for (const value of ["alpha", "bravo", "charlie"]) {
    expect(rowValues.some((row) => row.includes(value))).toBe(true);
  }
});
