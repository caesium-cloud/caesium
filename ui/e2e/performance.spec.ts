import fs from "node:fs/promises";
import { expect, test, type APIRequestContext, type Page } from "@playwright/test";
import {
  applyDefinitions,
  failOnUnexpectedPageErrors,
  uniqueSuffix,
  type FixtureDefinition,
} from "./helpers/fixtures";

failOnUnexpectedPageErrors();

// scripts/performance.sh sets CAESIUM_PERF_BROWSER_OUT. Only that measured
// run changes browser settings; the ordinary e2e suite keeps the config's.
const PERF_RUN = Boolean(process.env.CAESIUM_PERF_BROWSER_OUT);
if (PERF_RUN) {
  test.use({
    // Chromium reports performance.memory in coarse buckets (observed: only
    // 11.9 MB or 12.7 MB) unless precise memory info is enabled. --expose-gc
    // lets the sampler read retained heap after a full collection instead of
    // whatever garbage happens to be pending.
    launchOptions: { args: ["--enable-precise-memory-info", "--js-flags=--expose-gc"] },
    // Keep a lightweight trace of every measured sample (actions, network,
    // console; no DOM snapshots or screenshots) so an outlier can be
    // attributed after a passing run.
    trace: { mode: "on", screenshots: false, snapshots: false, sources: false },
  });
}

test.beforeEach(async ({ page }) => {
  page.on("requestfailed", (request) => {
    const url = new URL(request.url());
    const resource = `${url.origin}${url.pathname}`;
    console.error(`browser request failed: ${request.method()} ${resource}: ${request.failure()?.errorText ?? "unknown"}`);
  });
  await page.addInitScript(installRenderProbe);
});

/**
 * Live-backend browser performance coverage for E3.
 *
 * These tests measure route readiness, action-to-render, and long-session
 * memory against the real server. They assert correctness (the page became
 * ready, the action rendered, the heap did not explode). They do not encode
 * calibrated SLOs — E4 owns budgets.json.
 *
 * Timing comes from the page's own clock, not from the test runner. Playwright
 * checks `toBeVisible()` once and then retries after 100 ms, 250 ms, ... so a
 * runner-side stopwatch around it measures the retry schedule: the recorded
 * action-to-render series had two modes (~80 ms and ~170 ms) set by whether
 * the run heading beat the first check. The init-script probe below records
 * `performance.now()` in the MutationObserver callback in which a matching
 * heading first becomes visible, and the click time in a capture-phase
 * listener, so the value is the render time itself. The runner stopwatch is
 * kept as `wall_ms` for diagnosis only.
 *
 * Measured runs also keep the internet out of route readiness: the UI's
 * render-blocking third-party stylesheet is warmed into the browser cache
 * before the live first navigation, and answered locally in the SYNTHETIC
 * test (see warmThirdPartyStylesheets).
 *
 * Tests that intercept HTTP are labelled SYNTHETIC. Everything else is live.
 * Chromium-only, matching D2/Q4: this file does not invent Firefox/WebKit
 * projects or skip rules for browsers the suite does not run.
 */

const PRIMARY_ROUTES: { path: string; heading: RegExp }[] = [
  { path: "/jobs", heading: /^Jobs$/ },
  { path: "/triggers", heading: /^Triggers$/ },
  { path: "/system", heading: /^System$/ },
  { path: "/jobdefs", heading: /^JobDefs$/ },
];


type ProbeHeading = { text: string; t: number };
type ProbeClick = { label: string; t: number };
type ProbeLongTask = { start: number; duration: number };
type RenderProbe = {
  headings: ProbeHeading[];
  // First time any element matching a fixed selector was visible.
  firsts: Record<string, number>;
  clicks: ProbeClick[];
  longTasks: ProbeLongTask[];
};
type ProbeSnapshot = RenderProbe & { timeOrigin: number };

/**
 * Runs in the page before any application script (page.addInitScript). It
 * must stay self-contained: Playwright serializes the function source.
 */
function installRenderProbe(): void {
  const host = window as unknown as { __caesiumPerfProbe?: RenderProbe };
  if (host.__caesiumPerfProbe) return;
  const probe: RenderProbe = { headings: [], firsts: {}, clicks: [], longTasks: [] };
  host.__caesiumPerfProbe = probe;

  const selector = 'h1,h2,h3,h4,h5,h6,[role="heading"]';
  const firstSelectors: Record<string, string> = { "job-row": '[data-testid="job-row"]' };
  const recorded = new WeakMap<Element, string>();
  const normalize = (value: string) => value.replace(/\s+/g, " ").trim();
  // Accessible-name approximation for plain-text headings: aria-label, else
  // the text outside aria-hidden descendants.
  const nameOf = (element: Element) => {
    const label = element.getAttribute("aria-label");
    if (label) return normalize(label);
    let text = "";
    const walker = document.createTreeWalker(element, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node; node = walker.nextNode()) {
      let hidden = false;
      for (let parent = node.parentElement; parent && parent !== element; parent = parent.parentElement) {
        if (parent.getAttribute("aria-hidden") === "true") {
          hidden = true;
          break;
        }
      }
      if (!hidden) text += node.nodeValue ?? "";
    }
    return normalize(text);
  };
  // Playwright's visibility rule: a non-empty box and not visibility:hidden.
  const isVisible = (element: Element) => {
    const rect = element.getBoundingClientRect();
    return rect.width > 0 && rect.height > 0 && getComputedStyle(element).visibility !== "hidden";
  };

  let pending = false;
  let idleFrames = 0;
  let frame = 0;
  // Each timestamp is taken AFTER isVisible() returns: its
  // getBoundingClientRect() synchronously runs the style/layout the mutation
  // left pending, so that cost belongs in the recorded readiness. Paint and
  // the next frame are not waited for: a next-rAF stamp would add frame
  // alignment (0-16.7 ms, unrelated to either build) to every sample.
  const scan = () => {
    pending = false;
    for (const element of Array.from(document.querySelectorAll(selector))) {
      const text = nameOf(element);
      if (recorded.get(element) === text) continue;
      if (isVisible(element)) {
        recorded.set(element, text);
        probe.headings.push({ text, t: performance.now() });
      } else {
        pending = true;
      }
    }
    for (const [key, css] of Object.entries(firstSelectors)) {
      if (key in probe.firsts) continue;
      const candidates = Array.from(document.querySelectorAll(css)).slice(0, 5);
      if (candidates.some(isVisible)) {
        probe.firsts[key] = performance.now();
      } else if (candidates.length > 0) {
        pending = true;
      }
    }
    // A heading can become visible through layout alone (no mutation). Keep
    // checking once per frame for a short while after the last mutation.
    if (pending && frame === 0 && idleFrames < 60) {
      frame = requestAnimationFrame(() => {
        frame = 0;
        idleFrames += 1;
        scan();
      });
    }
  };
  new MutationObserver(() => {
    idleFrames = 0;
    scan();
  }).observe(document, { subtree: true, childList: true, characterData: true, attributes: true });

  document.addEventListener(
    "click",
    (event) => {
      const target = event.target instanceof Element ? event.target.closest("button,a,[role='button']") : null;
      probe.clicks.push({
        label: target?.getAttribute("title") ?? target?.getAttribute("aria-label") ?? "",
        t: performance.now(),
      });
    },
    { capture: true },
  );

  try {
    new PerformanceObserver((list) => {
      for (const entry of list.getEntries()) {
        probe.longTasks.push({ start: entry.startTime, duration: entry.duration });
      }
    }).observe({ type: "longtask", buffered: true });
  } catch {
    // Long-task timing is diagnostic only.
  }
}

async function probeSnapshot(page: Page): Promise<ProbeSnapshot> {
  const snapshot = await page.evaluate(() => {
    const probe = (window as unknown as { __caesiumPerfProbe?: RenderProbe }).__caesiumPerfProbe;
    if (!probe) return null;
    return {
      headings: probe.headings.slice(),
      firsts: { ...probe.firsts },
      clicks: probe.clicks.slice(),
      longTasks: probe.longTasks.slice(),
      timeOrigin: performance.timeOrigin,
    };
  });
  if (!snapshot) throw new Error("render probe is not installed in this document");
  return snapshot;
}

function firstHeadingAt(snapshot: ProbeSnapshot, heading: RegExp, notBefore = 0): number {
  const hit = snapshot.headings.find((entry) => entry.t >= notBefore && heading.test(entry.text));
  if (!hit) {
    throw new Error(
      `render probe saw no visible heading matching ${heading} at or after ${notBefore} ms; ` +
        `recorded ${JSON.stringify(snapshot.headings)}`,
    );
  }
  return hit.t;
}

function roundMs(value: number): number {
  return Math.round(value * 10) / 10;
}

function sampleContext(): Record<string, unknown> {
  const context: Record<string, unknown> = {};
  if (process.env.CAESIUM_PERF_SIDE) context.side = process.env.CAESIUM_PERF_SIDE;
  if (process.env.CAESIUM_PERF_REPEAT) context.repeat = Number(process.env.CAESIUM_PERF_REPEAT);
  return context;
}

async function record(metric: string, value: number, extra: Record<string, unknown> = {}): Promise<void> {
  const dest = process.env.CAESIUM_PERF_BROWSER_OUT;
  if (!dest) return;
  await fs.appendFile(
    dest,
    `${JSON.stringify({ metric, value, at: new Date().toISOString(), ...sampleContext(), ...extra })}\n`,
  );
}

/**
 * Per-sample attribution evidence, kept apart from the compared series: the
 * probe's heading/click/long-task timeline plus this document's navigation and
 * resource timing, all on the page clock.
 */
async function recordDiagnostics(page: Page, metric: string, extra: Record<string, unknown>): Promise<void> {
  const dest = process.env.CAESIUM_PERF_BROWSER_DIAGNOSTICS_OUT;
  if (!dest) return;
  const [snapshot, timing] = await Promise.all([
    probeSnapshot(page),
    page.evaluate(() => {
      const nav = performance.getEntriesByType("navigation")[0] as PerformanceNavigationTiming | undefined;
      const resources = (performance.getEntriesByType("resource") as PerformanceResourceTiming[]).map((entry) => {
        let name = entry.name;
        try {
          name = new URL(entry.name).pathname;
        } catch {
          // keep the raw name
        }
        return {
          name,
          initiator: entry.initiatorType,
          start: Math.round(entry.startTime * 10) / 10,
          duration: Math.round(entry.duration * 10) / 10,
          transfer_bytes: entry.transferSize,
        };
      });
      return {
        url: location.pathname,
        navigation: nav
          ? {
              type: nav.type,
              response_end: Math.round(nav.responseEnd * 10) / 10,
              dom_content_loaded: Math.round(nav.domContentLoadedEventEnd * 10) / 10,
              load_event_end: Math.round(nav.loadEventEnd * 10) / 10,
            }
          : null,
        resources,
      };
    }),
  ]);
  await fs.appendFile(
    dest,
    `${JSON.stringify({
      metric,
      at: new Date().toISOString(),
      ...sampleContext(),
      ...extra,
      time_origin: snapshot.timeOrigin,
      headings: snapshot.headings,
      firsts: snapshot.firsts,
      clicks: snapshot.clicks,
      long_tasks: snapshot.longTasks,
      ...timing,
    })}\n`,
  );
}

async function heapUsed(page: Page): Promise<{ bytes: number; collected: boolean } | null> {
  return page.evaluate(() => {
    const collect = (window as unknown as { gc?: () => void }).gc;
    const collected = typeof collect === "function";
    if (collected) collect();
    const mem = (performance as Performance & { memory?: { usedJSHeapSize: number } }).memory;
    return mem && Number.isFinite(mem.usedJSHeapSize) ? { bytes: mem.usedJSHeapSize, collected } : null;
  });
}

/**
 * The UI's index.html links a render-blocking third-party stylesheet (Google
 * Fonts). On the first navigation of a fresh browser context it comes from the
 * internet, and in recorded runs /jobs readiness tracked that response time
 * (Pearson r 0.98-1.00 over 20 samples per run), not the app. Measured runs
 * load every cross-origin stylesheet the served index.html links into this
 * context's HTTP cache first, from a same-site document so the cache
 * partition matches. The app's own assets and API calls stay cold and live.
 * Returns the stylesheets that loaded.
 */
async function warmThirdPartyStylesheets(page: Page, request: APIRequestContext): Promise<string[]> {
  const response = await request.get("/");
  expect(response.ok(), "GET / must serve the UI's index.html").toBeTruthy();
  const html = await response.text();
  const appOrigin = new URL(response.url()).origin;
  const hrefs = Array.from(html.matchAll(/<link\b[^>]*>/gi))
    .map((match) => match[0])
    .filter((tag) => /\brel\s*=\s*["']?stylesheet\b/i.test(tag))
    .map((tag) => /\bhref\s*=\s*["']([^"']+)["']/i.exec(tag)?.[1]?.replace(/&amp;/g, "&"))
    .filter((href): href is string => Boolean(href) && /^https?:\/\//i.test(href as string))
    .filter((href) => new URL(href).origin !== appOrigin);
  if (hrefs.length === 0) return [];
  await page.goto("/health");
  const loaded = await page.evaluate(
    (urls) =>
      Promise.all(
        urls.map(
          (url) =>
            new Promise<boolean>((resolve) => {
              const link = document.createElement("link");
              link.rel = "stylesheet";
              link.href = url;
              link.onload = () => resolve(true);
              link.onerror = () => resolve(false);
              document.head.appendChild(link);
            }),
        ),
      ),
    hrefs,
  );
  return hrefs.filter((_, index) => loaded[index]);
}

test("render probe: work done by the visibility check reaches the recorded time", async ({ page }) => {
  // getBoundingClientRect() is where Chromium runs the style/layout a DOM
  // mutation left pending. Make that call cost a known 200 ms for two freshly
  // inserted elements (a heading and a job row) and require both recorded
  // timestamps to include it. Stamping before the visibility check records
  // ~0 ms here. No backend is involved: the document is served locally.
  const url = "http://render-probe.localhost/";
  await page.route(/^http:\/\/render-probe\.localhost\//, (route) =>
    route.fulfill({ status: 200, contentType: "text/html", body: "<!doctype html><html><body></body></html>" }),
  );
  await page.goto(url);
  const costMs = 200;
  const insertedAt = await page.evaluate((cost) => {
    const original = Element.prototype.getBoundingClientRect;
    Element.prototype.getBoundingClientRect = function (this: Element) {
      if (this.hasAttribute("data-probe-cost")) {
        const until = performance.now() + cost;
        while (performance.now() < until) {
          // simulated forced layout
        }
      }
      return original.call(this);
    };
    const heading = document.createElement("h2");
    heading.textContent = "Measured";
    heading.setAttribute("data-probe-cost", "");
    const row = document.createElement("div");
    row.setAttribute("data-testid", "job-row");
    row.setAttribute("data-probe-cost", "");
    row.textContent = "row";
    const at = performance.now();
    document.body.append(heading, row);
    return at;
  }, costMs);
  const snapshot = await probeSnapshot(page);
  expect(firstHeadingAt(snapshot, /^Measured$/) - insertedAt).toBeGreaterThanOrEqual(costMs);
  expect(snapshot.firsts["job-row"], "render probe did not see the job row").toBeDefined();
  expect((snapshot.firsts["job-row"] as number) - insertedAt).toBeGreaterThanOrEqual(costMs);
});

test("live route readiness: primary pages reach their heading", async ({ page, request }) => {
  const warmed = PERF_RUN ? await warmThirdPartyStylesheets(page, request) : [];
  for (const route of PRIMARY_ROUTES) {
    const started = Date.now();
    await page.goto(route.path);
    await expect(page.getByRole("heading", { name: route.heading })).toBeVisible();
    const wallMs = Date.now() - started;
    // performance.now() counts from this document's navigation start.
    const readyMs = firstHeadingAt(await probeSnapshot(page), route.heading);
    expect(readyMs, `${route.path} never became ready`).toBeGreaterThan(0);
    await record("route_readiness_ms", roundMs(readyMs), {
      route: route.path,
      kind: "live",
      clock: "navigation-start",
      wall_ms: wallMs,
      third_party_css: warmed.length > 0 ? "warmed" : "live",
    });
    await recordDiagnostics(page, "route_readiness_ms", { route: route.path, kind: "live", value: roundMs(readyMs) });
  }
});

test("live action-to-render: triggering a run paints the run heading", async ({ page, request }) => {
  test.slow();

  const alias = `perf-action-${uniqueSuffix()}`;
  const runHeading = new RegExp(`^${alias}$`);
  const definition = {
    apiVersion: "v1",
    kind: "Job",
    metadata: { alias },
    trigger: { type: "cron", configuration: { cron: "0 0 1 1 *" } },
    steps: [{ name: "noop", image: "alpine:3.23", command: ["sh", "-c", "true"] }],
  } as unknown as FixtureDefinition;
  await applyDefinitions(request, definition);

  await page.goto("/jobs");
  await expect(page.getByRole("heading", { name: "Jobs", exact: true })).toBeVisible();
  const row = page.locator('[data-testid="job-row"]', { hasText: alias }).first();
  await expect(row).toBeVisible();
  const before = await probeSnapshot(page);

  const started = Date.now();
  await row.locator('button[title="Trigger run"]').click();
  await page.waitForURL(/\/jobs\/[^/]+\/runs\/[^/]+$/);
  await expect(page.getByTestId("run-heading")).toBeVisible();
  const wallMs = Date.now() - started;

  const after = await probeSnapshot(page);
  // The run page is a client-side route transition; a new document would
  // reset the page clock and lose the click timestamp.
  expect(after.timeOrigin, "trigger replaced the document; in-page timing needs one document").toBe(before.timeOrigin);
  const clicks = after.clicks.slice(before.clicks.length).filter((click) => click.label === "Trigger run");
  expect(clicks, "exactly one Trigger run click must be observed in the page").toHaveLength(1);
  const clickedAt = clicks[0].t;
  const renderMs = firstHeadingAt(after, runHeading, clickedAt) - clickedAt;
  expect(renderMs).toBeGreaterThan(0);
  await record("action_to_render_ms", roundMs(renderMs), {
    action: "trigger-run",
    kind: "live",
    clock: "click-event",
    wall_ms: wallMs,
  });
  await recordDiagnostics(page, "action_to_render_ms", {
    action: "trigger-run",
    kind: "live",
    value: roundMs(renderMs),
    clicked_at: clickedAt,
  });
});

test("live long-session memory stays bounded across repeated in-app navigation", async ({ page }) => {
  await page.goto("/jobs");
  await expect(page.getByRole("heading", { name: "Jobs", exact: true })).toBeVisible();

  const first = await heapUsed(page);
  test.skip(first == null, "performance.memory is unavailable (Chromium-only capability)");
  const initial = first as { bytes: number; collected: boolean };

  // One long-lived SPA document: navigate with the real sidebar links, so app
  // globals, router state and caches live for the whole session and a leak in
  // ordinary client-side navigation can accumulate. A nonce set once on
  // window (plus the page clock origin) proves no step replaced the document.
  const documentIdentity = () =>
    page.evaluate(() => {
      const host = window as unknown as { __caesiumPerfDocument?: string };
      host.__caesiumPerfDocument ??= crypto.randomUUID();
      return `${host.__caesiumPerfDocument}@${performance.timeOrigin}`;
    });
  const identity = await documentIdentity();

  const samples: number[] = [initial.bytes];
  let navigations = 0;
  for (let i = 0; i < 8; i++) {
    for (const route of [PRIMARY_ROUTES[1], PRIMARY_ROUTES[2], PRIMARY_ROUTES[3], PRIMARY_ROUTES[0]]) {
      await page.locator(`aside nav a[href="${route.path}"]:visible`).first().click();
      await expect(page).toHaveURL(new RegExp(`${route.path}$`));
      await expect(page.getByRole("heading", { name: route.heading })).toBeVisible();
      navigations += 1;
    }
    expect(await documentIdentity(), "in-app navigation replaced the document").toBe(identity);
    const used = await heapUsed(page);
    expect(used).not.toBeNull();
    samples.push((used as { bytes: number }).bytes);
  }
  expect(await documentIdentity(), "in-app navigation replaced the document").toBe(identity);

  const last = samples[samples.length - 1];
  expect(last).toBeGreaterThan(0);
  // Smoke bound, not an SLO: an 8x climb across a short navigation loop is a
  // leak, not noise. Calibrated heap budgets belong in E4.
  expect(last / initial.bytes).toBeLessThan(8);
  await record("long_session_heap_bytes", last, {
    first: initial.bytes,
    kind: "live",
    samples: samples.length,
    after_gc: initial.collected,
    trajectory: samples,
    navigation: "in-app",
    in_app_navigations: navigations,
  });
});

test("SYNTHETIC: a large jobs list still reaches a ready heading", async ({ page, baseURL }) => {
  // Any page.route disables the HTTP cache and intercepts every request, so
  // the third-party stylesheet cannot be warmed for this test. Measured runs
  // answer cross-origin stylesheets with an empty one instead of the internet.
  if (PERF_RUN) {
    const appOrigin = new URL(baseURL ?? "http://127.0.0.1:8080").origin;
    await page.route(
      (url) => url.origin !== appOrigin,
      async (route) => {
        if (route.request().resourceType() === "stylesheet") {
          await route.fulfill({ status: 200, contentType: "text/css", body: "" });
          return;
        }
        await route.continue();
      },
    );
  }

  const now = new Date().toISOString();
  const jobs = Array.from({ length: 200 }, (_, i) => ({
    id: `00000000-0000-4000-8000-${String(i).padStart(12, "0")}`,
    alias: `perf-synth-${String(i).padStart(3, "0")}`,
    trigger_id: `00000000-0000-4000-8001-${String(i).padStart(12, "0")}`,
    labels: {},
    annotations: {},
    paused: false,
    created_at: now,
    updated_at: now,
  }));

  await page.route("**/v1/jobs", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname.replace(/\/$/, "") !== "/v1/jobs") {
      await route.continue();
      return;
    }
    if (route.request().method() !== "GET") {
      await route.continue();
      return;
    }
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(jobs),
    });
  });

  const started = Date.now();
  await page.goto("/jobs");
  await expect(page.getByRole("heading", { name: "Jobs", exact: true })).toBeVisible();
  await expect(page.getByTestId("job-row").first()).toBeVisible();
  const wallMs = Date.now() - started;
  // Ready means the heading AND the first synthetic row are visible.
  const snapshot = await probeSnapshot(page);
  const firstRowAt = snapshot.firsts["job-row"];
  expect(firstRowAt, "render probe did not see a visible job row").toBeDefined();
  const readyMs = Math.max(firstHeadingAt(snapshot, /^Jobs$/), firstRowAt as number);
  expect(readyMs).toBeGreaterThan(0);
  const count = await page.getByTestId("job-row").count();
  expect(count).toBeGreaterThan(0);
  await record("route_readiness_ms", roundMs(readyMs), {
    route: "/jobs",
    kind: "synthetic",
    rows: count,
    clock: "navigation-start",
    wall_ms: wallMs,
    third_party_css: PERF_RUN ? "empty-stylesheet" : "live",
  });
  await recordDiagnostics(page, "route_readiness_ms", { route: "/jobs", kind: "synthetic", value: roundMs(readyMs) });
});
