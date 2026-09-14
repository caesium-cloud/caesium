import { expect, test } from "@playwright/test";
import {
  applyAndRun,
  applyDefinitions,
  awaitRun,
  buildFanDefinition,
  failOnUnexpectedPageErrors,
  findJobByAlias,
  triggerJob,
  uniqueSuffix,
  type FixtureDefinition,
} from "./helpers/fixtures";

failOnUnexpectedPageErrors();

/**
 * Scale coverage: large DAGs, many partitions, and long logs stay reachable
 * against the live backend. Where the console genuinely implements
 * pagination/virtualization for a surface (the fanned-task partition table),
 * this file proves the DOM stays bounded and scrolling reaches every row.
 * Where a surface has no virtualization (the pipeline list, the log viewer),
 * it proves the actual reachability mechanism the product DOES ship
 * (search/filter) still surfaces every item at scale, rather than asserting
 * a mechanism that does not exist.
 *
 * The "many partitions" case below is explicitly SYNTHETIC (a mocked HTTP
 * response standing in for hundreds of real fan-out instances): the e2e
 * server runs with CAESIUM_FANOUT_MAX_PARTITIONS=8, so a REAL group this
 * large cannot be produced live. It runs against a real job/run/task
 * produced by a genuine (small) live fan-out; only the partitions LIST
 * response for that one task is replaced.
 */

test("many pipelines stay individually reachable through the pipeline filter", async ({ page, request }) => {
  const batchId = uniqueSuffix();
  const count = 24;
  const defs: FixtureDefinition[] = Array.from({ length: count }, (_, i) => ({
    apiVersion: "v1",
    kind: "Job",
    metadata: { alias: `scale-list-${batchId}-${String(i).padStart(2, "0")}` },
    trigger: { type: "cron", configuration: { cron: "0 0 1 1 *" } },
    steps: [{ name: "noop", image: "alpine:3.23", command: ["sh", "-c", "true"] }],
  })) as unknown as FixtureDefinition[];

  await applyDefinitions(request, ...defs);

  await page.goto("/jobs");
  await expect(page.getByRole("heading", { name: "Jobs", exact: true })).toBeVisible();

  const filterInput = page.getByPlaceholder("Filter pipelines…");

  // The whole batch is reachable...
  await filterInput.fill(`scale-list-${batchId}`);
  await expect(page.getByTestId("job-row")).toHaveCount(count);

  // ...and so is exactly one of them, narrowing from the full batch.
  await filterInput.fill(`scale-list-${batchId}-17`);
  await expect(page.getByTestId("job-row")).toHaveCount(1);
  await expect(page.getByTestId("job-row")).toContainText(`scale-list-${batchId}-17`);
});

test("a wide real DAG renders every node; none are silently dropped at scale", async ({ page, request }) => {
  test.slow();

  const width = 16;
  const alias = `scale-wide-dag-${uniqueSuffix()}`;
  await applyDefinitions(request, buildFanDefinition(alias, width));
  const job = await findJobByAlias(request, alias);
  await triggerJob(request, job.id);
  const run = await awaitRun(request, job.id, { status: "succeeded", timeoutMs: 90_000 });

  // root + width leaves + join
  expect(run.tasks).toHaveLength(width + 2);

  await page.goto(`/jobs/${job.id}/runs/${run.id}`);
  await expect(page.getByRole("heading", { name: /Run / })).toBeVisible({ timeout: 30_000 });

  const dagSection = page.getByTestId("run-interactive-dag-section");
  await expect(dagSection).toContainText(`${width + 2} nodes`);

  // react-flow renders every node to the DOM (it is not itself virtualized),
  // so a real count assertion is the correct proof that none were dropped.
  await expect(dagSection.locator(".react-flow__node")).toHaveCount(width + 2);

  // fitView (react-flow's Controls) must bring every node within the
  // viewport's transform, proving the far edge of a wide DAG is reachable,
  // not merely present off-screen.
  await dagSection.getByRole("button", { name: /fit view/i }).click();
  const leafNode = dagSection.locator(".react-flow__node", { hasText: "leaf-15" });
  await expect(leafNode).toBeVisible();
  await expect(leafNode).toBeInViewport();
});

test("SYNTHETIC: a fanned task's partition table stays reachable through virtualization at scale", async ({
  page,
  request,
}) => {
  test.slow();

  const { job, run } = await applyAndRun(request, "dynamic-fanout.job.yaml", { status: "succeeded" });

  const totalSynthetic = 240;
  const synthetic = Array.from({ length: totalSynthetic }, (_, i) => ({
    value: `synthetic-${i}`,
    index: i,
    status: "succeeded",
    attempt: 1,
    cache_hit: false,
    duration: "0.1s",
    task_run_id: `synthetic-task-run-${i}`,
  }));

  await page.route(`**/v1/jobs/${job.id}/runs/${run.id}/tasks/*/partitions*`, async (route) => {
    if (route.request().method() !== "GET") {
      await route.continue();
      return;
    }
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        partitions: synthetic,
        total: totalSynthetic,
        limit: 500,
        offset: 0,
        next_offset: null,
        status_counts: { succeeded: totalSynthetic },
      }),
    });
  });

  await page.goto(`/jobs/${job.id}/runs/${run.id}`);
  await expect(page.getByRole("heading", { name: /Run / })).toBeVisible();

  await page.locator(".react-flow__node", { hasText: "process-file" }).click();
  const panel = page.getByTestId("task-detail-panel");
  await expect(panel).toBeVisible();
  await panel.getByRole("button", { name: "Details" }).click();

  const table = panel.getByTestId("partition-table");
  await expect(table.getByTestId("partition-table-total")).toContainText(`×${totalSynthetic}`);

  const rows = table.getByTestId("partition-row");
  await expect(rows.first()).toBeVisible();
  const initialCount = await rows.count();
  // Bounded: the scroll viewport (max-h-48, ~30px rows, overscan 8 each
  // side) renders far fewer DOM rows than the full synthetic group — proof
  // the table virtualizes instead of mounting all `totalSynthetic` rows.
  expect(initialCount).toBeGreaterThan(0);
  expect(initialCount).toBeLessThan(totalSynthetic / 4);

  const firstIndices = await rows.evaluateAll((els) => els.map((el) => el.getAttribute("data-index")));

  // A real user scroll gesture (wheel), not a direct scrollTop write: the
  // virtualizer's own scroll listener + React re-render happen on the next
  // frame, so this must be polled rather than asserted immediately.
  const scrollContainer = table.locator("div.overflow-auto").first();
  await scrollContainer.hover();
  await expect
    .poll(
      async () => {
        await page.mouse.wheel(0, 4_000);
        const indices = await table
          .getByTestId("partition-row")
          .evaluateAll((els) => els.map((el) => el.getAttribute("data-index")));
        return indices.includes(String(totalSynthetic - 1));
      },
      { timeout: 20_000, message: "scrolling never reached the final synthetic partition" },
    )
    .toBe(true);

  const lastIndices = await table
    .getByTestId("partition-row")
    .evaluateAll((els) => els.map((el) => el.getAttribute("data-index")));

  // Different DOM rows are mounted after scrolling, and the final synthetic
  // partition (index 239) is among them — every row is reachable, not just
  // the first virtualized window.
  expect(lastIndices).not.toEqual(firstIndices);
  expect(lastIndices).toContain(String(totalSynthetic - 1));
});

test("long log output remains fully reachable through the log viewer's search filter", async ({ page, request }) => {
  test.slow();

  const alias = `scale-long-log-${uniqueSuffix()}`;
  const lineCount = 2000;
  const definition = {
    apiVersion: "v1",
    kind: "Job",
    metadata: { alias },
    trigger: { type: "cron", configuration: { cron: "0 0 1 1 *" } },
    steps: [
      {
        name: "emit",
        image: "alpine:3.23",
        command: ["sh", "-c", `i=1; while [ "$i" -le ${lineCount} ]; do echo "scale-log-line-$i"; i=$((i + 1)); done`],
      },
    ],
  } as unknown as FixtureDefinition;

  await applyDefinitions(request, definition);
  const job = await findJobByAlias(request, alias);
  await triggerJob(request, job.id);
  const run = await awaitRun(request, job.id, { status: "succeeded", timeoutMs: 60_000 });

  await page.goto(`/jobs/${job.id}/runs/${run.id}`);
  await expect(page.getByRole("heading", { name: /Run / })).toBeVisible();

  await page.locator(".react-flow__node").first().click();
  const panel = page.getByTestId("task-detail-panel");
  await expect(panel).toBeVisible();

  const logText = panel.getByTestId("task-log-plaintext");
  await expect(logText).toContainText("scale-log-line-1", { timeout: 30_000 });
  // The first and last lines of a 2000-line stream both remain reachable —
  // nothing was silently truncated in between.
  await expect(logText).toContainText(`scale-log-line-${lineCount}`, { timeout: 30_000 });

  // Reachability via the actual product affordance (the filter box), not
  // just raw DOM presence: filtering narrows to exactly the requested line.
  await panel.getByPlaceholder("Filter visible rows").fill(`scale-log-line-${lineCount - 1}`);
  await expect(logText).toContainText(`scale-log-line-${lineCount - 1}`);
  await expect(logText).not.toContainText(`scale-log-line-${lineCount - 2}`);
});
