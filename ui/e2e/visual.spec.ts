import { expect, test, type Locator, type Page } from "@playwright/test";
import {
  applyAndRun,
  applyDefinitions,
  failOnUnexpectedPageErrors,
  findJobByAlias,
  loadFixtureDefinition,
} from "./helpers/fixtures";

failOnUnexpectedPageErrors();

/**
 * Deterministic visual regression coverage.
 *
 * CRITICAL: these snapshots are generated and compared per-platform (Playwright's
 * default `toHaveScreenshot` naming appends the OS, e.g. `-linux.png` /
 * `-darwin.png`). CI runs on ubuntu-24.04 ("noble") and only ever compares
 * against `-linux.png` baselines. Any baseline committed here MUST be
 * generated inside a Linux container running the exact `@playwright/test`
 * version pinned in package-lock.json (e.g.
 * `mcr.microsoft.com/playwright:v<version>-noble`) — a baseline generated on
 * a developer's macOS/Windows machine names a DIFFERENT file and would never
 * be consulted by CI, silently leaving the linux baseline missing (which
 * fails the very first CI run, not passes it).
 *
 * Determinism strategy:
 *  - Fixed viewport for this whole file (`test.use` below).
 *  - Google Fonts network requests are blocked so the browser always falls
 *    back to the same platform font stack declared in src/index.css, instead
 *    of racing a network font swap.
 *  - `animations: "disabled"` freezes/finishes CSS transitions and
 *    animations before the pixel capture.
 *  - Every screenshot is CLIPPED to one component locator, not the full
 *    page — the shared e2e server accumulates jobs/runs from every other
 *    spec file, so a full-page screenshot would never be reproducible.
 *  - Elapsed-time text (`Duration`/`RelativeTime`, real wall-clock values
 *    that vary run to run) is masked by locator rather than asserted on
 *    pixel-for-pixel.
 *
 * Only `-linux.png` baselines are committed (see e2e/visual.spec.ts-snapshots/).
 * Every test below skips itself on any other `process.platform` instead of
 * failing on a missing baseline — a developer's local `just ui-e2e` (which
 * runs the browser on whatever OS invoked `just`, not in a container) is
 * expected to report these as "skipped" off Linux. The real comparison runs
 * in CI's required `ui-e2e`/`ui-e2e-auth` checks (ubuntu-24.04). Committing a
 * `-darwin.png`/`-win32.png` baseline instead of skipping would not fix
 * this: CI never reads those files, so it would still start from "no linux
 * baseline" on first merge.
 */

test.use({ viewport: { width: 1280, height: 960 } });

test.beforeEach(async ({ page }, testInfo) => {
  testInfo.skip(process.platform !== "linux", "visual baselines are linux-only; see the file header comment above");

  // Force the deterministic fallback font stack (see src/index.css
  // --font-sans/--font-mono) instead of racing the network for Google Fonts.
  // fulfill() with an empty, successful response rather than abort(): an
  // aborted request makes Chrome log its own "Failed to load resource:
  // net::ERR_FAILED" console error, which failOnUnexpectedPageErrors()
  // (correctly) does not otherwise allowlist — an empty stylesheet response
  // declares no @font-face rules (so no font file request follows) without
  // the browser treating it as a failure.
  await page.route(/fonts\.(googleapis|gstatic)\.com/, (route) =>
    route.fulfill({ status: 200, contentType: "text/css", body: "" }),
  );
});

async function readyForScreenshot(page: Page): Promise<void> {
  await page.evaluate(() => document.fonts.ready);
}

/** Matches the elapsed-time text rendered by src/components/duration.tsx. */
const DURATION_TEXT = /^-?\d+(\.\d+)?(ms|s|m|h)$/;

test("Trigger Job dialog renders deterministically", async ({ page, request }) => {
  const definition = await loadFixtureDefinition("run-history.job.yaml");
  await applyDefinitions(request, definition);
  const job = await findJobByAlias(request, String(definition.metadata?.alias));

  await page.goto(`/jobs/${job.id}`);
  await page.getByRole("button", { name: "Trigger job" }).click();

  const dialog = page.getByRole("dialog", { name: "Trigger Job" });
  await expect(dialog).toBeVisible();
  await readyForScreenshot(page);

  await expect(dialog).toHaveScreenshot("trigger-job-dialog.png", { animations: "disabled" });
});

test("a fixed-shape branching DAG renders deterministically once terminal", async ({ page, request }) => {
  test.slow();

  // branching-demo always takes the fast-path branch (decide-path emits a
  // fixed `##caesium::branch fast-path`), so the node count, edges, and
  // per-node terminal statuses (succeeded/succeeded/skipped/succeeded) are
  // the same shape on every run.
  const { job, run } = await applyAndRun(request, "branching.job.yaml", { status: "succeeded" });

  await page.goto(`/jobs/${job.id}/runs/${run.id}`);
  await expect(page.getByRole("heading", { name: /Run / })).toBeVisible();

  const dagSection = page.getByTestId("run-interactive-dag-section");
  const dagNodes = dagSection.locator(".react-flow__node");
  await expect(dagNodes).toHaveCount(4);
  await dagSection.scrollIntoViewIfNeeded();
  await dagSection.getByRole("button", { name: /fit view/i }).click();

  // Fit against the visible run-page canvas after its scroll geometry has
  // settled. This prevents a visual baseline from encoding a node clipped by
  // an earlier, taller internal canvas.
  const canvasViewport = dagSection.getByTestId("run-dag-canvas-viewport");
  await expect.poll(async () => {
    const canvas = await canvasViewport.boundingBox();
    const nodes = await Promise.all(Array.from({ length: 4 }, (_unused, index) => dagNodes.nth(index).boundingBox()));
    return {
      fits: Boolean(
        canvas &&
          nodes.every(
            (node) =>
              node &&
              node.x >= canvas.x &&
              node.x + node.width <= canvas.x + canvas.width &&
              node.y >= canvas.y &&
              node.y + node.height <= canvas.y + canvas.height,
          ),
      ),
      canvas,
      nodes,
    };
  }).toMatchObject({ fits: true });
  await readyForScreenshot(page);

  // The skip reason identifies its branch task by run-scoped UUID. Keep the
  // skipped node and its stable "Skipped" label visible, but mask that one
  // dynamic reason alongside elapsed task durations.
  const branchSkipReason = dagSection.getByText(/^not selected by branch task [0-9a-f-]{8}-/);
  await expect(branchSkipReason).toHaveCount(1);
  const dynamicMasks: Locator[] = [
    dagSection.locator(".react-flow__node").getByText(DURATION_TEXT),
    branchSkipReason,
  ];

  await expect(dagSection).toHaveScreenshot("branching-dag.png", {
    animations: "disabled",
    mask: dynamicMasks,
  });
});

test("a fanned task's partition table renders deterministically", async ({ page, request }) => {
  test.slow();

  const { job, run } = await applyAndRun(request, "dynamic-fanout.job.yaml", { status: "succeeded" });

  await page.goto(`/jobs/${job.id}/runs/${run.id}`);
  await expect(page.getByRole("heading", { name: /Run / })).toBeVisible();

  await page.locator(".react-flow__node", { hasText: "process-file" }).click();
  const panel = page.getByTestId("task-detail-panel");
  await expect(panel).toBeVisible();
  await panel.getByRole("button", { name: "Details" }).click();

  const table = page.getByTestId("partition-table");
  await expect(table.getByTestId("partition-row")).toHaveCount(3);
  await readyForScreenshot(page);

  // Mask the Duration and Cache columns (4th and 5th direct children of each
  // row, per TaskDetailPanel.tsx's PartitionTable grid) — real elapsed time
  // and cache-hit state (the e2e server runs with caching enabled) are not
  // reproducible across runs, unlike the fixed value/status/attempt/
  // fingerprint/depends-on columns the fixture hardcodes.
  const dynamicCells = table.locator(
    '[data-testid="partition-row"] > span:nth-child(4), [data-testid="partition-row"] > span:nth-child(5)',
  );

  await expect(table).toHaveScreenshot("partition-table.png", {
    animations: "disabled",
    mask: [dynamicCells],
  });
});
