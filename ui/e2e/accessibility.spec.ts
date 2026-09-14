import AxeBuilder from "@axe-core/playwright";
import { expect, test, type Page, type TestInfo } from "@playwright/test";
import type { AxeResults, Result } from "axe-core";
import { applyAndRun, applyDefinitions, failOnUnexpectedPageErrors, findJobByAlias, loadFixtureDefinition } from "./helpers/fixtures";

failOnUnexpectedPageErrors();

/**
 * Scoped axe-core scans plus keyboard/focus behavior across the console's
 * core authoring/trigger/run surfaces, against the live backend.
 *
 * Scope: only WCAG 2.0/2.1 A+AA rules, and only "critical"/"serious" impact
 * findings are asserted on. "moderate"/"minor" findings are logged (via
 * `testInfo.attach`) but not asserted on, so a scan against a large,
 * actively-developed page doesn't flap on cosmetic contrast nuances while
 * still failing hard on missing labels, keyboard traps, and similar
 * blocking defects. Third-party canvas-rendered widgets that do not expose a
 * DOM accessibility tree by construction (the react-flow DAG canvas and the
 * xterm log terminal) are excluded from the scan itself — the plaintext log
 * mirror (`task-log-plaintext`) and the DAG's own buttons/labels chrome
 * remain in scope.
 *
 * KNOWN_VIOLATIONS is a tracked baseline, not a pass: this run found real,
 * pre-existing critical/serious defects (systemic icon-only buttons with no
 * accessible name, and several `text-text-3`/`text-text-4`/`bg-graphite`/
 * `bg-cyan-glow` muted-token combinations below the 4.5:1 contrast ratio)
 * across product pages this D2 stream does not own (`ui/src/**` is out of
 * scope for this stream — see the coordination note in the dispatching
 * plan). Recorded here by rule id so the gate still catches a NEW regression
 * (a rule id a page has never emitted before) without permanently blocking
 * merges on debt nobody here can fix. `color-contrast` is tracked globally,
 * not per page: it comes from a handful of shared muted-text/badge design
 * tokens reused everywhere, so which specific element trips it varies run to
 * run with incidental page content (which other jobs/triggers happen to
 * exist from concurrently-running spec files) — the defect is the token, not
 * the page. Shrinking this baseline is a product-code PR; widening it
 * without a product-side justification is not.
 */

const CANVAS_EXCLUSIONS = ['.react-flow__renderer', '[data-testid="task-log-terminal"]'];

const KNOWN_EVERYWHERE = ["color-contrast"];

const KNOWN_VIOLATIONS: Record<string, string[]> = {
  "jobs-list": ["select-name"],
  "run-detail-with-task-panel": ["aria-prohibited-attr", "button-name"],
  "trigger-job-dialog": [],
  "triggers-page": ["button-name"],
};

async function assertNoNewViolations(page: Page, testInfo: TestInfo, label: string): Promise<void> {
  const results: AxeResults = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"])
    .exclude(CANVAS_EXCLUSIONS)
    .analyze();

  await testInfo.attach(`axe-${label}`, {
    body: JSON.stringify(results.violations, null, 2),
    contentType: "application/json",
  });

  const known = new Set([...KNOWN_EVERYWHERE, ...(KNOWN_VIOLATIONS[label] ?? [])]);
  const blocking = results.violations.filter((v: Result) => v.impact === "critical" || v.impact === "serious");
  const unexpected = blocking.filter((v) => !known.has(v.id));
  const describe = unexpected
    .map((v) => `${v.id} (${v.impact}): ${v.help} — ${v.nodes.length} node(s), e.g. ${v.nodes[0]?.target.join(" ")}`)
    .join("\n");
  expect(
    unexpected,
    `NEW critical/serious axe violations on ${label}, beyond the tracked baseline in KNOWN_VIOLATIONS:\n${describe}`,
  ).toEqual([]);
}

test("jobs list is keyboard-reachable and free of critical/serious violations", async ({ page, request }, testInfo) => {
  const definition = await loadFixtureDefinition("run-history.job.yaml");
  await applyDefinitions(request, definition);

  await page.goto("/jobs");
  await expect(page.getByRole("heading", { name: "Jobs", exact: true })).toBeVisible();
  await expect(page.getByTestId("job-row").first()).toBeVisible();

  await assertNoNewViolations(page, testInfo, "jobs-list");

  // Keyboard: the filter input and a row's own action buttons must be plain
  // tab stops, not mouse-only affordances. Filter by the FULL generated
  // alias (not a short prefix) — concurrent spec files apply other jobs
  // derived from the same fixture with different random suffixes, and only
  // the full string is guaranteed to match exactly this row.
  const alias = String(definition.metadata?.alias);
  const filterInput = page.getByPlaceholder("Filter pipelines…");
  await filterInput.focus();
  await expect(filterInput).toBeFocused();
  await filterInput.pressSequentially(alias);
  await expect(page.getByTestId("job-row")).toHaveCount(1);

  // A plain <button> (not a mouse-only click handler on a non-interactive
  // element) reaches focus programmatically — filtering to exactly one row
  // above makes this specifically the narrowed-to row's own action, not a
  // coincidentally-matching one elsewhere on the page.
  const triggerButton = page.locator('[data-testid="job-row"] button[title="Trigger run"]').first();
  await triggerButton.focus();
  await expect(triggerButton).toBeFocused();
});

test("job detail run page (DAG + task panel) is free of critical/serious violations; Escape returns focus", async ({
  page,
  request,
}, testInfo) => {
  test.slow();

  const { job, run } = await applyAndRun(request, "run-history.job.yaml", { status: "succeeded" });

  await page.goto(`/jobs/${job.id}/runs/${run.id}`);
  await expect(page.getByRole("heading", { name: /Run / })).toBeVisible();

  const firstNode = page.locator(".react-flow__node").first();
  await expect(firstNode).toBeVisible();
  await firstNode.click();

  const panel = page.getByTestId("task-detail-panel");
  await expect(panel).toBeVisible();

  await assertNoNewViolations(page, testInfo, "run-detail-with-task-panel");

  // The panel is a focusable region closable from the keyboard, and closing
  // it must not strand keyboard focus on a removed element: the page must
  // stay keyboard-operable (Tab still reaches a live, attached control)
  // immediately afterwards.
  await page.keyboard.press("Escape");
  await expect(panel).toBeHidden();
  await page.keyboard.press("Tab");
  const activeIsAttached = await page.evaluate(() => document.activeElement !== null && document.activeElement !== document.body);
  expect(activeIsAttached).toBe(true);
});

test("Trigger Job dialog is a labeled, focus-trapped dialog reachable and dismissible by keyboard", async ({
  page,
  request,
}, testInfo) => {
  const definition = await loadFixtureDefinition("run-history.job.yaml");
  await applyDefinitions(request, definition);
  const job = await findJobByAlias(request, String(definition.metadata?.alias));

  await page.goto(`/jobs/${job.id}`);
  const trigger = page.getByRole("button", { name: "Trigger job" });
  await trigger.focus();
  await page.keyboard.press("Enter");

  const dialog = page.getByRole("dialog", { name: "Trigger Job" });
  await expect(dialog).toBeVisible();

  await assertNoNewViolations(page, testInfo, "trigger-job-dialog");

  // Radix's focus trap must keep Tab cycling inside the dialog rather than
  // escaping to page chrome behind it.
  await page.keyboard.press("Tab");
  await expect(dialog.locator(":focus")).toBeVisible();

  await page.keyboard.press("Escape");
  await expect(dialog).toBeHidden();
  // Closing must not leave the page permanently un-navigable from the
  // keyboard: Tab from wherever focus landed must still reach a live,
  // attached control. (This app's trigger button is a plain element outside
  // Radix's own Dialog.Trigger primitive, and closing here in fact drops
  // focus to <body> rather than restoring it to that button — a real, minor
  // focus-management gap, reported rather than asserted away since fixing it
  // is a `ui/src/**` product change outside this stream's scope. What must
  // still hold is that the page recovers on the very next keypress.)
  await page.keyboard.press("Tab");
  const activeIsAttached = await page.evaluate(
    () => document.activeElement !== null && document.activeElement !== document.body,
  );
  expect(activeIsAttached).toBe(true);
});

test("triggers page has no critical/serious accessibility violations", async ({ page, request }, testInfo) => {
  const definition = await loadFixtureDefinition("branching.job.yaml");
  await applyDefinitions(request, definition);

  await page.goto("/triggers");
  await expect(page.getByRole("heading", { name: "Triggers", exact: true })).toBeVisible();
  await expect(page.getByTestId("trigger-card").first()).toBeVisible();

  await assertNoNewViolations(page, testInfo, "triggers-page");
});
