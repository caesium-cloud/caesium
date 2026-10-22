import { expect, test, type APIRequestContext, type Page } from "@playwright/test";
import { adminKeyFromEnv, authHeaders, createNamespacedKey, loginRawAtUrl } from "../helpers/auth";

// The namespace switcher, chips, foreign deep links and the quota-held label
// against the api-key auth lane. That server mounts no policy file, so any
// valid namespace string can own a job; marketing and finance stand in for two
// tenants. Nothing here asserts the full set of declared namespaces: other
// streams add fixture namespaces, so a scenario checks only the ones it uses.

let admin: string;
const suffix = Date.now().toString(36);
const marketingAlias = `ns-view-marketing-${suffix}`;
const financeAlias = `ns-view-finance-${suffix}`;

function definition(alias: string, namespace: string) {
  return {
    apiVersion: "v1",
    kind: "Job",
    metadata: { alias, namespace },
    trigger: { type: "cron", configuration: { cron: "0 3 * * *", timezone: "UTC" } },
    steps: [{ name: "only", engine: "docker", image: "alpine:3.23", command: ["sh", "-c", "echo ok"] }],
  };
}

async function jobId(request: APIRequestContext, alias: string): Promise<string> {
  const response = await request.get("/v1/jobs", { headers: authHeaders(admin) });
  expect(response.ok()).toBeTruthy();
  const found = ((await response.json()) as Array<{ id: string; alias: string }>).find((job) => job.alias === alias);
  if (!found) throw new Error(`job ${alias} not found`);
  return found.id;
}

test.beforeAll(async ({ request }) => {
  admin = adminKeyFromEnv();
  const response = await request.post("/v1/jobdefs/apply", {
    headers: { ...authHeaders(admin), "Content-Type": "application/json" },
    data: { definitions: [definition(marketingAlias, "marketing"), definition(financeAlias, "finance")] },
  });
  if (!response.ok()) {
    throw new Error(`failed to apply namespaced jobs: ${response.status()} ${await response.text()}`);
  }
});

function row(page: Page, alias: string) {
  return page.getByTestId("job-row").filter({ hasText: alias });
}

/** Sign in on the login screen the page is already showing (no navigation, storage untouched). */
async function signInHere(page: Page, key: string) {
  await expect(page.getByPlaceholder("csk_live_...")).toBeVisible();
  await page.getByPlaceholder("csk_live_...").fill(key);
  await page.getByRole("button", { name: "Sign In" }).click();
}

test("switching the namespace narrows the job list and persists across a reload", async ({ page, request }) => {
  const key = await createNamespacedKey(request, admin, "runner", `ns-view switch ${suffix}`, ["marketing", "finance"]);
  await loginRawAtUrl(page, "/jobs", key);

  await expect(row(page, marketingAlias)).toBeVisible();
  await expect(row(page, financeAlias)).toBeVisible();

  const switcher = page.getByTestId("namespace-switcher");
  await expect(switcher).toBeVisible();
  const narrowed = page.waitForRequest((req) => req.url().includes("/v1/jobs?namespace=marketing"));
  await switcher.selectOption("marketing");
  await narrowed;
  await expect(row(page, marketingAlias)).toBeVisible();
  await expect(row(page, financeAlias)).toHaveCount(0);

  // The key lives in module memory, so a reload returns to the login screen.
  // Signing in again without touching localStorage must restore the selection:
  // it is a view preference, and the credential is never persisted.
  await page.reload();
  await signInHere(page, key);
  await expect(page.getByTestId("namespace-switcher")).toHaveValue("marketing");
  await expect(row(page, marketingAlias)).toBeVisible();
  await expect(row(page, financeAlias)).toHaveCount(0);
  const stored = await page.evaluate(() => JSON.stringify(window.localStorage));
  expect(stored).not.toContain("csk_");

  await page.getByTestId("namespace-switcher").selectOption("");
  await expect(row(page, financeAlias)).toBeVisible();
});

test("every job shows its namespace as a chip, also for a single-namespace key", async ({ page, request }) => {
  const both = await createNamespacedKey(request, admin, "viewer", `ns-view chips ${suffix}`, ["marketing", "finance"]);
  await loginRawAtUrl(page, "/jobs", both);
  await expect(row(page, marketingAlias).getByTestId("namespace-chip")).toHaveText("marketing");
  await expect(row(page, financeAlias).getByTestId("namespace-chip")).toHaveText("finance");
});

test("a single-namespace key shows the chip but no switcher", async ({ page, request }) => {
  const key = await createNamespacedKey(request, admin, "viewer", `ns-view single ${suffix}`, ["marketing"]);
  await loginRawAtUrl(page, "/jobs", key);
  await expect(row(page, marketingAlias).getByTestId("namespace-chip")).toHaveText("marketing");
  await expect(page.getByTestId("namespace-switcher")).toHaveCount(0);
});

test("the job detail header carries the namespace chip", async ({ page, request }) => {
  const key = await createNamespacedKey(request, admin, "viewer", `ns-view detail ${suffix}`, ["marketing"]);
  const id = await jobId(request, marketingAlias);
  await loginRawAtUrl(page, `/jobs/${id}`, key);
  await expect(page.getByRole("heading", { name: marketingAlias })).toBeVisible();
  await expect(page.getByTestId("namespace-chip")).toHaveText("marketing");
});

test("a deep link to a job outside the grants renders InsufficientAccess", async ({ page, request }) => {
  const key = await createNamespacedKey(request, admin, "runner", `ns-view foreign ${suffix}`, ["marketing"]);
  const foreign = await jobId(request, financeAlias);
  await loginRawAtUrl(page, `/jobs/${foreign}`, key);

  const denied = page.getByTestId("job-insufficient-access");
  await expect(denied).toBeVisible();
  await expect(denied).toContainText("You do not have access to this job's namespace.");
  await expect(page.getByText(financeAlias)).toHaveCount(0);
  await expect(page.getByText(/job not found/i)).toHaveCount(0);
  await expect(page.getByText(/\b403\b/)).toHaveCount(0);
});

test("a queue row held by the namespace quota is labelled", async ({ page, request }) => {
  // The live quota journey needs a policy file with quotas.maxConcurrentRuns,
  // which this lane does not serve; the Keycloak lane's policy does (G2). Here
  // the wire shape (held_reason, pinned by the Go queue tests) is injected.
  const id = await jobId(request, marketingAlias);
  await page.route(`**/v1/jobs/${id}/queue`, async (route) => {
    if (route.request().method() !== "GET") return route.continue();
    return route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify([
        { id: "11111111-1111-4111-8111-111111111111", position: 1, priority: 2, enqueued_at: new Date().toISOString(), claim_state: "pending", stale: false, held_reason: "namespace_quota" },
        { id: "22222222-2222-4222-8222-222222222222", position: 2, priority: 2, enqueued_at: new Date().toISOString(), claim_state: "pending", stale: false },
      ]),
    });
  });
  await loginRawAtUrl(page, `/jobs/${id}`, admin);

  const rows = page.getByTestId("run-queue-row");
  await expect(rows).toHaveCount(2);
  await expect(rows.nth(0)).toContainText("Held by namespace quota");
  await expect(rows.nth(0).getByTestId("run-queue-quota-badge")).toBeVisible();
  await expect(rows.nth(1).getByTestId("run-queue-quota-badge")).toHaveCount(0);
  await expect(rows.nth(1)).toContainText("Waiting for a run slot");
});
