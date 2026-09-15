import { expect, test, type APIRequestContext } from "@playwright/test";
import {
  applyAndRun,
  applyDefinitions,
  awaitRun,
  findJobByAlias,
  triggerJob,
  uniqueSuffix,
  type E2ECallbackRun,
  type E2ERun,
  type FixtureDefinition,
} from "./helpers/fixtures";

type FailedCallbackRun = E2ECallbackRun & { error: string };

test("run detail surfaces failed callback errors", async ({ page, request }) => {
  test.slow();

  const { job, run } = await applyAndRun(request, "callback-failure.job.yaml", {
    status: "succeeded",
  });
  const failedCallback = await awaitFailedCallback(request, job.id, run.id);

  await page.goto(`/jobs/${job.id}/runs/${run.id}`);
  await expect(page.getByRole("heading", { name: /Run / })).toBeVisible({ timeout: 30_000 });

  const callbacks = page.getByTestId("run-callbacks-section");
  await expect(callbacks).toBeVisible();

  const failedRow = callbacks.getByTestId("run-callback-row").filter({
    hasText: failedCallback.error,
  });
  await expect(failedRow).toBeVisible();
  await expect(failedRow).toContainText("failed");
  await expect(failedRow.getByTestId("run-callback-error")).toContainText(failedCallback.error);
});

test("watching a run complete refreshes its asynchronous callback result", async ({ page, request }) => {
  test.slow();

  const alias = `callback-watch-${uniqueSuffix()}`;
  const definition = {
    apiVersion: "v1",
    kind: "Job",
    metadata: { alias },
    trigger: { type: "cron", configuration: { cron: "0 0 1 1 *" } },
    callbacks: [{
      type: "notification",
      configuration: { url: "http://127.0.0.1:65535/hooks/caesium" },
    }],
    steps: [{
      name: "wait-for-viewer",
      image: "alpine:3.23",
      command: ["sh", "-c", "sleep 12; echo callback after viewer is open"],
    }],
  } as FixtureDefinition;

  await applyDefinitions(request, definition);
  const job = await findJobByAlias(request, alias);
  await triggerJob(request, job.id);
  const run = await awaitRun(request, job.id, { status: "running" });

  // Load the shared job/run query while the task is definitely active, then
  // navigate through the app so the terminal event reaches the same cache key
  // that RunDetail reads.
  await page.goto(`/jobs/${job.id}`);
  const runLink = page.locator(`a[href="/jobs/${job.id}/runs/${run.id}"]`);
  await expect(runLink).toBeVisible({ timeout: 30_000 });
  await expect(page.locator('[data-status="running"]')).toBeVisible();

  const initialResponse = await request.get(`/v1/jobs/${job.id}/runs/${run.id}`);
  expect(initialResponse.ok()).toBe(true);
  const initialRun = (await initialResponse.json()) as E2ERun;
  expect(initialRun.status).toBe("running");
  expect(initialRun.callbacks ?? []).toEqual([]);

  await runLink.click();
  await expect(page.getByRole("heading", { name: /Run / })).toBeVisible({ timeout: 30_000 });
  await expect(page.locator('[data-status="running"]')).toBeVisible();
  await expect(page.getByTestId("run-callbacks-section")).toHaveCount(0);

  const callbacks = page.getByTestId("run-callbacks-section");
  await expect(callbacks).toBeVisible({ timeout: 30_000 });
  const failedRow = callbacks.getByTestId("run-callback-row").filter({ hasText: "connection refused" });
  await expect(failedRow).toContainText("failed");
});

async function awaitFailedCallback(
  request: APIRequestContext,
  jobId: string,
  runId: string,
): Promise<FailedCallbackRun> {
  const deadline = Date.now() + 45_000;
  let lastCallbacks: E2ECallbackRun[] = [];

  while (Date.now() <= deadline) {
    const run = await getRun(request, jobId, runId);
    lastCallbacks = run.callbacks ?? [];
    const failed = lastCallbacks.find(
      (callback): callback is FailedCallbackRun =>
        callback.status === "failed" && typeof callback.error === "string" && callback.error.length > 0,
    );
    if (failed) return failed;

    await delay(1_000);
  }

  throw new Error(
    `timed out waiting for failed callback on run ${runId}; callbacks=${JSON.stringify(lastCallbacks)}`,
  );
}

async function getRun(request: APIRequestContext, jobId: string, runId: string): Promise<E2ERun> {
  const response = await request.get(`/v1/jobs/${jobId}/runs/${runId}`);
  if (!response.ok()) {
    throw new Error(`failed to load run ${runId}: ${response.status()} ${await response.text()}`);
  }
  return (await response.json()) as E2ERun;
}

async function delay(ms: number): Promise<void> {
  await new Promise((resolve) => setTimeout(resolve, ms));
}
