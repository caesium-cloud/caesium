import { expect, test } from "@playwright/test";
import {
  applyAndRun,
  applyDefinitions,
  awaitRun,
  expectNetworkFailuresDuring,
  failOnUnexpectedPageErrors,
  findJobByAlias,
  loadFixtureDefinition,
  triggerJob,
  uniqueSuffix,
  type FixtureDefinition,
} from "./helpers/fixtures";

failOnUnexpectedPageErrors();

/**
 * Reload, reconnect, and race-safety coverage against the live backend, plus
 * a small set of explicitly-labeled SYNTHETIC response-manipulation tests
 * for client contracts that this e2e server's configuration cannot exercise
 * for real: it runs without CAESIUM_AUTH_MODE set, so there is no enforced
 * credential/permission surface to deny a request live here. (Real
 * scope-based denial IS exercised against a real auth-enabled server in
 * ui/e2e/auth/lineage-scope.spec.ts and friends — this file covers the
 * generic CLIENT behavior when ANY request comes back 401/403, independent
 * of which backend produced it.) Synthetic tests do not replace those live
 * persistence/authorization checks.
 */

test("reload preserves a terminal run's detail view", async ({ page, request }) => {
  test.slow();

  const { job, run } = await applyAndRun(request, "run-history.job.yaml", { status: "succeeded" });

  await page.goto(`/jobs/${job.id}/runs/${run.id}`);
  await expect(page.getByRole("heading", { name: /Run / })).toBeVisible();
  await expect(page.getByText("succeeded", { exact: true }).first()).toBeVisible();
  await expect(page.locator(".react-flow__node")).toHaveCount(3);

  await page.reload();

  await expect(page).toHaveURL(new RegExp(`/jobs/${job.id}/runs/${run.id}$`));
  await expect(page.getByRole("heading", { name: /Run / })).toBeVisible();
  await expect(page.getByText("succeeded", { exact: true }).first()).toBeVisible();
  await expect(page.locator(".react-flow__node")).toHaveCount(3);
});

test("the console recovers live updates after a real network interruption", async ({ page, request, context }) => {
  test.slow();

  // The whole test body runs inside expectNetworkFailuresDuring: this is
  // THE test in the suite whose entire purpose is inducing a real
  // browser-level network cut, so Chrome's auto-logged net::ERR_* console
  // noise is expected throughout it — both from the cut itself and from
  // whatever the SSE client's reconnect attempts and the polling fallback
  // emit while re-establishing afterwards (CI has been observed to report
  // net::ERR_NETWORK_CHANGED rather than net::ERR_INTERNET_DISCONNECTED for
  // the same induced cut, and not necessarily inside the exact
  // setOffline(true)/setOffline(false) pair). The allowance is still scoped
  // to just THIS test/page, not "every spec" (see fixtures.ts) — an
  // unexpected network failure in any other test, in this file or any
  // other, still fails it.
  await expectNetworkFailuresDuring(page, async () => {
    // A deliberately slow single-step run, so it is still genuinely
    // "running" (not already terminal) at the moment the connection is cut.
    const alias = `net-recovery-${uniqueSuffix()}`;
    const definition: FixtureDefinition = {
      apiVersion: "v1",
      kind: "Job",
      metadata: { alias },
      trigger: { type: "cron", configuration: { cron: "0 0 1 1 *" } },
      steps: [{ name: "hold", image: "alpine:3.23", command: ["sh", "-c", "sleep 6"] }],
    } as unknown as FixtureDefinition;
    await applyDefinitions(request, definition);
    const job = await findJobByAlias(request, alias);

    await triggerJob(request, job.id);
    // Confirm via the API that the run is genuinely in flight BEFORE loading
    // the page: this removes the race between "has the browser's poll/SSE
    // picked the new run up yet" and "is it actually running", by making the
    // very first page load's own job fetch already reflect a running run.
    await awaitRun(request, job.id, { status: "running", timeoutMs: 15_000 });

    await page.goto(`/jobs/${job.id}`);
    await expect(page.getByRole("heading", { name: job.alias })).toBeVisible();
    await expect(page.getByTestId("dag-counters")).toContainText("running", { timeout: 15_000 });

    // A REAL browser-level network cut (not a mocked response) — this is
    // the actual condition the SSE client's onerror/reconnect path and the
    // polling fallback (JobDetailPage's streamHealthy-gated
    // refetchInterval) exist for.
    await context.setOffline(true);
    await page.waitForTimeout(2_000);
    await context.setOffline(false);

    // By the time the connection is restored and either the reconnected
    // stream or the polling fallback catches up, the held step should have
    // finished and the DAG counters should reflect it — without requiring a
    // manual reload.
    await expect(page.getByTestId("dag-counters")).toContainText("1 done", { timeout: 60_000 });
  });
});

test("SYNTHETIC: an expired credential is surfaced to the operator instead of silently retried", async ({
  page,
  request,
}) => {
  const definition = await loadFixtureDefinition("run-history.job.yaml");
  await applyDefinitions(request, definition);
  const job = await findJobByAlias(request, String(definition.metadata?.alias));

  await page.goto(`/jobs/${job.id}`);
  await expect(page.getByRole("heading", { name: job.alias })).toBeVisible();

  // The e2e server itself runs without auth enabled, so a real expired
  // credential can't be produced here — this exercises the CLIENT contract
  // in src/lib/api.ts: request() maps any 401 to
  // ApiError("Authentication required") and clears the in-memory key, and
  // the mutation's onError must surface that verbatim rather than a generic
  // failure or a silent retry.
  await page.route(`**/v1/jobs/${job.id}/run`, async (route) => {
    await route.fulfill({ status: 401, contentType: "application/json", body: JSON.stringify({}) });
  });

  await page.getByRole("button", { name: "Trigger job" }).click();
  await page.getByRole("button", { name: "Confirm Trigger" }).click();

  await expect(page.getByText(/Failed to trigger job: Authentication required/)).toBeVisible();
  // No navigation must have happened off the job page on a rejected trigger.
  await expect(page).toHaveURL(new RegExp(`/jobs/${job.id}$`));
});

test("SYNTHETIC: a denied mutation surfaces an error without corrupting displayed state", async ({ page, request }) => {
  const definition = await loadFixtureDefinition("run-history.job.yaml");
  await applyDefinitions(request, definition);
  const job = await findJobByAlias(request, String(definition.metadata?.alias));

  await page.goto("/jobs");
  await expect(page.getByRole("heading", { name: "Jobs", exact: true })).toBeVisible();
  await page.getByPlaceholder("Filter pipelines…").fill(job.alias);

  const row = page.getByTestId("job-row").filter({ hasText: job.alias });
  await expect(row).toBeVisible();
  const pauseButton = row.locator('button[title="Pause future runs"]');
  await expect(pauseButton).toBeVisible();

  await page.route(`**/v1/jobs/${job.id}/pause`, async (route) => {
    await route.fulfill({
      status: 403,
      contentType: "application/json",
      body: JSON.stringify({ message: "forbidden: caller is not permitted to pause this job" }),
    });
  });

  await pauseButton.click();

  await expect(page.getByText(/Failed to update: forbidden: caller is not permitted to pause this job/)).toBeVisible();
  // The optimistic UI must not have flipped to "paused" on a rejected write:
  // the same Pause affordance (not Unpause) is still showing.
  await expect(row.locator('button[title="Pause future runs"]')).toBeVisible();
  await expect(row.locator('button[title="Unpause"]')).toHaveCount(0);
});

test("SYNTHETIC: a slow stale job-detail request does not overwrite a faster later navigation", async ({
  page,
  request,
}) => {
  test.slow();

  const suffix = uniqueSuffix();
  const defA = await loadFixtureDefinition("run-history.job.yaml");
  defA.metadata = { ...(defA.metadata ?? {}), alias: `race-a-${suffix}` };
  const defB = await loadFixtureDefinition("run-history.job.yaml");
  defB.metadata = { ...(defB.metadata ?? {}), alias: `race-b-${suffix}` };
  await applyDefinitions(request, defA, defB);

  const jobA = await findJobByAlias(request, `race-a-${suffix}`);
  const jobB = await findJobByAlias(request, `race-b-${suffix}`);

  // Only job A's own detail fetch is delayed — long enough to still be
  // in flight after we've already navigated on to job B. Everything below
  // navigates via in-app <Link> clicks (client-side routing), not
  // page.goto: a real browser navigation would tear down the whole JS realm
  // and trivially "pass" by destroying A's in-flight fetch along with it,
  // which would not prove anything about the SPA's own stale-response
  // handling.
  await page.route(`**/v1/jobs/${jobA.id}`, async (route) => {
    await new Promise((resolve) => setTimeout(resolve, 3_000));
    await route.continue();
  });

  // NOTE: filter by the raw suffix, not "race-${suffix}" — the aliases are
  // "race-a-<suffix>"/"race-b-<suffix>", so "race-<suffix>" (skipping the
  // "-a-"/"-b-" in between) is not actually a substring of either.
  await page.goto("/jobs");
  await page.getByPlaceholder("Filter pipelines…").fill(suffix);
  await expect(page.getByTestId("job-row")).toHaveCount(2);

  await page.getByRole("link", { name: jobA.alias, exact: true }).click();
  await expect(page).toHaveURL(new RegExp(`/jobs/${jobA.id}$`));

  // Deliberately navigate away (client-side, via the sidebar) before job
  // A's delayed response has arrived.
  await page.locator("aside").getByRole("link", { name: /^Jobs\b/ }).click();
  await expect(page).toHaveURL(/\/jobs$/);
  await page.getByPlaceholder("Filter pipelines…").fill(suffix);
  await page.getByRole("link", { name: jobB.alias, exact: true }).click();

  await expect(page.getByRole("heading", { name: jobB.alias })).toBeVisible();

  // Give job A's delayed response time to resolve in the background, then
  // confirm it never clobbered the now-displayed job B.
  await page.waitForTimeout(4_000);
  await expect(page.getByRole("heading", { name: jobB.alias })).toBeVisible();
  await expect(page.getByRole("heading", { name: jobA.alias })).toHaveCount(0);
  await expect(page).toHaveURL(new RegExp(`/jobs/${jobB.id}$`));
});
