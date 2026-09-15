import { expect, test } from "@playwright/test";
import { applyAndRun, failOnUnexpectedPageErrors } from "./helpers/fixtures";

// A disposed terminal must not leave an asynchronous xterm callback behind.
// This guard turns the Viewport/renderer error that prompted this regression
// into a browser-test failure instead of letting a screenshot hide it.
failOnUnexpectedPageErrors();

test("switching and closing the log panel does not leave a disposed terminal callback", async ({
  page,
  request,
}) => {
  test.slow();

  const { job, run } = await applyAndRun(request, "dynamic-fanout.job.yaml", {
    status: "succeeded",
  });

  await page.goto(`/jobs/${job.id}/runs/${run.id}`);
  await expect(page.getByRole("heading", { name: /Run / })).toBeVisible({ timeout: 30_000 });

  const fannedNode = page.locator(".react-flow__node", { hasText: "process-file" });
  const panel = page.getByTestId("task-detail-panel");

  // This is the production race: opening the default Logs tab queues xterm
  // work, while switching tabs immediately unmounts it. Escape runs the same
  // panel close path as the header control, and visibility assertions wait for
  // that transition without an arbitrary delay.
  for (let attempt = 0; attempt < 3; attempt += 1) {
    await fannedNode.click();
    await expect(panel).toBeVisible();
    await panel.getByRole("button", { name: "Details" }).click();
    await expect(panel.getByTestId("partition-table")).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(panel).not.toBeVisible();
  }

  // The maintained terminal still renders retained output and its search path
  // after the rapid lifecycle transitions above.
  await fannedNode.click();
  await expect(panel).toBeVisible();
  const logText = panel.getByTestId("task-log-plaintext");
  await expect(logText).toContainText("processing alpha", { timeout: 30_000 });

  // Changing the instance must replace the terminal buffer. A no-op picker
  // would leave alpha visible after bravo has been selected.
  const picker = panel.getByTestId("log-partition-select");
  await picker.selectOption({ index: 1 });
  await expect(logText).toContainText("processing bravo", { timeout: 30_000 });
  await expect(logText).not.toContainText("processing alpha");

  // Filter the log that is actually loaded, so this assertion fails if the
  // log-search control stops applying its predicate.
  await panel.getByPlaceholder("Filter visible rows").fill("not-a-bravo-log-line");
  await expect(logText).not.toContainText("processing bravo");
});
