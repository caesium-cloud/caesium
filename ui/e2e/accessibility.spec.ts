import AxeBuilder from "@axe-core/playwright";
import { expect, test, type Page, type TestInfo } from "@playwright/test";
import type { AxeResults, NodeResult, Result } from "axe-core";
import { applyAndRun, applyDefinitions, failOnUnexpectedPageErrors, findJobByAlias, loadFixtureDefinition } from "./helpers/fixtures";

import { waitForFiniteAnimations } from "./helpers/animations";

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
 * KNOWN_VIOLATIONS is a tracked baseline, not a pass. Named-control and
 * dialog-focus defects from the original D2 scan are fixed in product code;
 * remaining entries (if any) are still real. Shrinking this baseline is
 * required when a defect is fixed; widening it without a product-side
 * justification is not.
 *
 * The baseline is tracked per VIOLATING NODE, not per whole rule ID, so it
 * cannot silently swallow a newly-broken control under an already-known rule
 * (see isKnownViolationNode below). Entries in KNOWN_VIOLATIONS are
 * `${ruleId}::${leaf-selector}` — the LEAF (innermost) segment only, not the
 * full ancestor chain: axe computes the shortest selector that is unique
 * given everything currently on the page, so both how many ancestor
 * `:nth-child()` hops it needs AND how many of the element's own classes it
 * includes grow or shrink with how much fixture data other
 * concurrently-running spec files happen to have created by the time this
 * scan runs (see the file's use of shared job/trigger state) — the exact
 * same button can be reported as `.w-5` on one run and
 * `.w-5.hover\:text-cyan-glow.hover\:bg-transparent` on another. Matching
 * (isKnownViolationNode/sameLeafElement) therefore compares leaf CLASS SETS
 * (order- and count-independent, via subset containment) rather than exact
 * selector strings, and ignores ancestor position entirely. A genuinely
 * different element (a different component with a different class set)
 * still fails to match and is therefore treated as NEW.
 *
 * `color-contrast` is tracked globally in KNOWN_CONTRAST_TOKENS, not
 * per-page/selector: it comes from a handful of shared muted-text/badge
 * design tokens reused everywhere, so which specific element trips it varies
 * run to run with incidental page content — the defect is the token, not the
 * page or its position. Matched by the violation's actual `fgColor` (from
 * axe's own contrast-check data, which is NOT selector-derived and so has
 * none of the instability above): any element using an already-known bad
 * FOREGROUND token stays known regardless of where it appears, while a
 * genuinely new foreground token (a new regression) is not.
 *
 * The match is a small RGB-distance tolerance (colorIsKnownContrastToken),
 * not exact equality, on BOTH ends: repeated runs against this same,
 * unmodified UI observed the "same" token's reported fgColor drift by a
 * handful of hex units run to run (e.g. `#646d80` vs `#6f788d` vs
 * `#707a8f`) — these muted-text utilities are themselves rendered at
 * fractional opacity, so the exact color axe reads back is a composite that
 * depends on subpixel/antialiasing detail, not a fixed literal. `bgColor` is
 * dropped from the match entirely for the same reason but more so: several
 * of these tokens sit on a semi-transparent background utility (e.g.
 * `bg-cyan-glow/NN`) that composites against whatever is rendered
 * underneath, so the same element's background hex can differ by much more
 * than the foreground does (observed `#1e7389` vs `#1f7a91`) depending on
 * what else happens to be on the page. KNOWN_CONTRAST_TOKENS therefore holds
 * one representative sample per distinct token family; distinct families
 * observed so far are >50 RGB-distance units apart, comfortably outside the
 * ~30-unit tolerance, so a genuinely new muted-color regression still won't
 * match.
 */

const CANVAS_EXCLUSIONS = ['.react-flow__renderer', '[data-testid="task-log-terminal"]'];

/** Known-bad `fgColor` token families behind remaining color-contrast debt (see file header; one representative sample per family). */
const KNOWN_CONTRAST_TOKENS = [
  // Near-black on dark surfaces (void / midnight / primary-foreground). Not
  // the muted text rungs; those were raised in dark theme to ≥4.5:1.
  "#0a0a12",
];

/** Max Euclidean RGB distance to treat two fgColor reads as the same token family (see file header). */
const CONTRAST_TOKEN_TOLERANCE = 30;

function hexToRgb(hex: string): [number, number, number] | null {
  const match = /^#([0-9a-f]{2})([0-9a-f]{2})([0-9a-f]{2})$/i.exec(hex);
  if (!match) return null;
  return [parseInt(match[1], 16), parseInt(match[2], 16), parseInt(match[3], 16)];
}

function colorIsKnownContrastToken(fgColor: string): boolean {
  const rgb = hexToRgb(fgColor);
  if (!rgb) return false;
  const [r, g, b] = rgb;
  return KNOWN_CONTRAST_TOKENS.some((known) => {
    const knownRgb = hexToRgb(known);
    if (!knownRgb) return false;
    const [kr, kg, kb] = knownRgb;
    return Math.hypot(r - kr, g - kg, b - kb) <= CONTRAST_TOKEN_TOLERANCE;
  });
}

/** `${ruleId}::${leaf-selector}` baselines — see file header for why only the leaf segment is tracked. */
const KNOWN_VIOLATIONS: Record<string, string[]> = {
  "jobs-list": [],
  "run-detail-with-task-panel": [],
  "trigger-job-dialog": [],
  "triggers-page": [],
};

/** The innermost (rightmost, descendant-combinator-separated) selector segment of an axe `target` — see file header. */
function leafSelectorSegment(target: NodeResult["target"]): string {
  const leafFrame = target[target.length - 1];
  const leafSelector = Array.isArray(leafFrame) ? leafFrame[leafFrame.length - 1] : leafFrame;
  const segments = String(leafSelector ?? "").split(">");
  return (segments[segments.length - 1] ?? "").trim();
}

/** Order-independent CSS class tokens on one selector segment, with positional pseudo-classes dropped. */
function classTokens(segment: string): Set<string> {
  const cleaned = segment.replace(/:nth-(?:child|of-type)\(\d+\)/g, "");
  const tokens = cleaned.match(/\.(?:\\.|[^.\s>])+/g) ?? [];
  return new Set(tokens);
}

/**
 * True if two leaf selector segments plausibly name the same kind of
 * element: same non-class remainder (e.g. a bare tag name like `select`),
 * and one's class set is fully contained in the other's — tolerating axe
 * including a different (page-content-dependent) number of disambiguating
 * classes for what is otherwise the same component (see file header).
 */
function sameLeafElement(a: string, b: string): boolean {
  const remainderA = a.replace(/\.(?:\\.|[^.\s>])+/g, "").trim();
  const remainderB = b.replace(/\.(?:\\.|[^.\s>])+/g, "").trim();
  if (remainderA !== remainderB) return false;

  const classesA = classTokens(a);
  const classesB = classTokens(b);
  if (classesA.size === 0 && classesB.size === 0) return true;
  if (classesA.size === 0 || classesB.size === 0) return false;

  const [small, large] = classesA.size <= classesB.size ? [classesA, classesB] : [classesB, classesA];
  for (const cls of small) {
    if (!large.has(cls)) return false;
  }
  return true;
}

function isKnownViolationNode(label: string, ruleId: string, node: NodeResult): boolean {
  if (ruleId === "color-contrast") {
    const data = (node.any[0]?.data ?? {}) as { fgColor?: string };
    return !!data.fgColor && colorIsKnownContrastToken(data.fgColor);
  }
  const leaf = leafSelectorSegment(node.target);
  return (KNOWN_VIOLATIONS[label] ?? []).some((entry) => {
    const separator = entry.indexOf("::");
    if (entry.slice(0, separator) !== ruleId) return false;
    return sameLeafElement(leaf, entry.slice(separator + 2));
  });
}

async function assertNoNewViolations(page: Page, testInfo: TestInfo, label: string, include?: string): Promise<void> {
  const scan = new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"])
    .exclude(CANVAS_EXCLUSIONS);
  if (include) scan.include(include);
  // Visible rows can still inherit AppShell's 500 ms entry fade, giving axe
  // a transient composite foreground rather than the settled design token.
  await waitForFiniteAnimations(page);
  const results: AxeResults = await scan.analyze();

  await testInfo.attach(`axe-${label}`, {
    body: JSON.stringify(results.violations, null, 2),
    contentType: "application/json",
  });

  const blocking = results.violations.filter((v: Result) => v.impact === "critical" || v.impact === "serious");

  const unexpected: { rule: Result; node: NodeResult }[] = [];
  for (const rule of blocking) {
    for (const node of rule.nodes) {
      if (!isKnownViolationNode(label, rule.id, node)) {
        unexpected.push({ rule, node });
      }
    }
  }
  const describe = unexpected
    .map(({ rule, node }) => `${rule.id} (${rule.impact}): ${rule.help} — target ${node.target.join(" ")}`)
    .join("\n");
  expect(
    unexpected,
    `NEW critical/serious axe violation node(s) on ${label}, beyond the tracked baseline in KNOWN_VIOLATIONS/KNOWN_CONTRAST_TOKENS:\n${describe}`,
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

test("jobs contrast scans wait for finite fades and still reject settled defects", async ({ page, request }, testInfo) => {
  await applyDefinitions(request, await loadFixtureDefinition("run-history.job.yaml"));
  await page.goto("/jobs");
  await expect(page.getByTestId("job-row").first()).toBeVisible();
  await page.mouse.move(0, 0); // Non-hover card rows, as in the failed CI scan.
  await waitForFiniteAnimations(page);

  const ids = '[data-testid="job-row"] .font-mono.text-text-4';
  await expect(page.locator(ids).first()).toBeVisible();
  await page.locator("main").evaluate((main) => {
    const fade = main.animate([{ opacity: 0.76 }, { opacity: 1 }], { duration: 60_000, fill: "both" });
    fade.pause();
    fade.currentTime = 0;
  });
  try {
    // A paused finite fade cannot be silently skipped or leave the scan hung.
    await expect(waitForFiniteAnimations(page, 200)).rejects.toThrow();

    let ready = false;
    const waiting = waitForFiniteAnimations(page).then(() => { ready = true; });
    await page.locator("main").evaluate((main) => {
      main.getAnimations().forEach((animation) => animation.cancel());
      const replacement = main.animate([{ opacity: 0.76 }, { opacity: 1 }], { duration: 60_000, fill: "both" });
      replacement.pause();
      replacement.currentTime = 0;
    });
    await page.evaluate(() => new Promise<void>((resolve) => requestAnimationFrame(() => requestAnimationFrame(() => resolve()))));
    expect(ready, "a replacement finite fade must remain pending").toBe(false);
    await page.locator("main").evaluate((main) => main.getAnimations().forEach((animation) => animation.finish()));
    await waiting;
    await expect(page.locator("main")).toHaveCSS("opacity", "1");
    await assertNoNewViolations(page, testInfo, "jobs-list");

    // Readiness does not excuse a genuinely bad settled foreground. Preserve
    // the full scan and its 4.5:1 rule, including this concrete failed CI color.
    await page.locator(ids).first().evaluate((id) => { (id as HTMLElement).style.color = "#727886"; });
    try {
      await expect(assertNoNewViolations(page, testInfo, "jobs-list")).rejects.toThrow("NEW critical/serious axe violation");
    } finally {
      await page.locator(ids).first().evaluate((id) => { (id as HTMLElement).style.removeProperty("color"); });
    }
  } finally {
    // Failure diagnostics must not wait on the deliberately paused long fade.
    await page.locator("main").evaluate((main) => main.getAnimations().forEach((animation) => animation.cancel()));
  }
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

  // Scan the dialog's controls, not page text intentionally dimmed behind its
  // overlay. Page accessibility is checked by the separate page scenarios.
  await assertNoNewViolations(page, testInfo, "trigger-job-dialog", '[role="dialog"]');

  // Radix's focus trap must cycle Tab within the dialog, wrapping at both
  // ends, rather than escaping to page chrome behind it. A single Tab from
  // the initially-focused field cannot prove this: this dialog has several
  // focusable controls (the params textarea, Cancel,
  // Confirm Trigger, and the header's Close button), so ordinary untrapped
  // browser tab order would also stay inside for exactly one step even with
  // no trap at all. Instead, explicitly focus each boundary control and
  // confirm Tab/Shift+Tab wrap past it to the OTHER end of the dialog — the
  // only outcome an untrapped page cannot produce, since it would escape to
  // page chrome instead.
  const paramsInput = dialog.getByLabel("Run parameters");
  const closeButton = dialog.getByRole("button", { name: "Close" });

  await closeButton.focus();
  await page.keyboard.press("Tab");
  await expect(paramsInput).toBeFocused();
  await expect(dialog.locator(":focus")).toBeVisible();

  await paramsInput.focus();
  await page.keyboard.press("Shift+Tab");
  await expect(closeButton).toBeFocused();
  await expect(dialog.locator(":focus")).toBeVisible();

  await page.keyboard.press("Escape");
  await expect(dialog).toBeHidden();
  await expect(trigger).toBeFocused();
});

test("triggers page has no critical/serious accessibility violations", async ({ page, request }, testInfo) => {
  const definition = await loadFixtureDefinition("branching.job.yaml");
  await applyDefinitions(request, definition);

  await page.goto("/triggers");
  await expect(page.getByRole("heading", { name: "Triggers", exact: true })).toBeVisible();
  await expect(page.getByTestId("trigger-card").first()).toBeVisible();

  await assertNoNewViolations(page, testInfo, "triggers-page");
});
