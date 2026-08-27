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

test("a fanned task's partition table exposes a per-instance Logs action", async ({ page, request }) => {
  test.slow();

  const { job, run } = await applyAndRun(request, "dynamic-fanout.job.yaml", {
    status: "succeeded",
  });

  await page.goto(`/jobs/${job.id}/runs/${run.id}`);
  await expect(page.getByRole("heading", { name: /Run / })).toBeVisible({ timeout: 30_000 });

  await page.locator(".react-flow__node", { hasText: "process-file" }).click();
  const panel = page.getByTestId("task-detail-panel");
  await expect(panel).toBeVisible();
  await panel.getByRole("button", { name: "Details" }).click();

  const rows = panel.getByTestId("partition-table").getByTestId("partition-row");
  await expect(rows).toHaveCount(3);

  // Every instance needs a route to its own log, not only failed ones — the
  // table used to expose Retry alone, so a succeeded partition's output was
  // unreachable from the UI.
  await expect(panel.getByTestId("partition-logs-button")).toHaveCount(3);

  // Partition rows are ordered by partition_index, which is the producer's
  // emission order: alpha=0, bravo=1, charlie=2.
  await rows.nth(2).getByTestId("partition-logs-button").click();

  // The action selects that instance and switches to the Logs tab.
  await expect(panel.getByTestId("log-partition-select")).toBeVisible();
  await expect(panel.getByTestId("task-log-plaintext")).toContainText("processing charlie", {
    timeout: 30_000,
  });
});

test("each partition's Logs tab streams that instance's own container output", async ({
  page,
  request,
}) => {
  test.slow();

  const { job, run } = await applyAndRun(request, "dynamic-fanout.job.yaml", {
    status: "succeeded",
  });

  await page.goto(`/jobs/${job.id}/runs/${run.id}`);
  await expect(page.getByRole("heading", { name: /Run / })).toBeVisible({ timeout: 30_000 });

  await page.locator(".react-flow__node", { hasText: "process-file" }).click();
  const panel = page.getByTestId("task-detail-panel");
  await expect(panel).toBeVisible();

  // The Logs tab is the default tab, and a fanned task must offer an instance
  // picker there: `task_id` alone names N containers, so the backend answers
  // 400 rather than guessing, and the panel used to send exactly that request.
  const picker = panel.getByTestId("log-partition-select");
  await expect(picker).toBeVisible();
  await expect(picker.locator("option")).toHaveCount(3);

  const logText = panel.getByTestId("task-log-plaintext");

  // The fixture's process-file echoes `processing $CAESIUM_PARTITION`, so the
  // log text is the instance's own identity. Options are ordered by
  // partition_index: alpha=0, bravo=1, charlie=2.
  await picker.selectOption({ index: 0 });
  await expect(logText).toContainText("processing alpha", { timeout: 30_000 });
  await expect(logText).not.toContainText("processing bravo");

  await picker.selectOption({ index: 1 });
  await expect(logText).toContainText("processing bravo", { timeout: 30_000 });
  // The load-bearing assertion: two partitions must show DISTINCT logs. Serving
  // one arbitrary instance's container for the whole group is the defect this
  // covers, and it would leave alpha's line on screen here.
  await expect(logText).not.toContainText("processing alpha");
});
