import { expect, test, type APIRequestContext } from "@playwright/test";
import { applyAndRun, type E2ECallbackRun, type E2ERun } from "./helpers/fixtures";

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
