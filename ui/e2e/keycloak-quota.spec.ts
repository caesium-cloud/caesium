import { expect, test, type Response } from "@playwright/test";
import { seedAsKeycloakAdmin, ssoLogin } from "./helpers/auth";

// The "Held by namespace quota" label from a REAL start (identity-and-access
// F4 follow-up, G2). Runs only in the `keycloak` project: the lane policy
// (test/testdata/keycloak/policy.yaml) gives `marketing` quotas.maxConcurrentRuns
// of 1, bob is an operator there, and the server runs the run queue with its
// dequeuer. Nothing is injected with page.route: the queue row and its
// held_reason come from the live GET /v1/jobs/:id/queue.
//
// Seeding goes through the real API as the SSO admin in a throwaway context
// (see seedAsKeycloakAdmin); bob drives the Console from his own context.
// Unique aliases keep a rerun on a reused lane from colliding, and the holder
// sleeps a bounded 25 s, so nothing outlives the test.

const suffix = Date.now().toString(36);
const holderAlias = `kc-quota-holder-${suffix}`;
const waiterAlias = `kc-quota-waiter-${suffix}`;

test.describe.configure({ mode: "serial" });

type RunSummary = { id: string; status: string };

test("bob's second marketing start is held by the namespace quota and then runs", async ({ page, browser, baseURL }) => {
  const ids = await seedAsKeycloakAdmin(browser, baseURL, [
    { alias: holderAlias, namespace: "marketing", command: "sleep 25" },
    { alias: waiterAlias, namespace: "marketing", command: "echo released" },
  ]);
  const holderId = ids[holderAlias];
  const waiterId = ids[waiterAlias];

  await ssoLogin(page, "bob@example.com", baseURL);

  // First start: takes marketing's single slot and lands on the run page.
  await page.goto(`/jobs/${holderId}`);
  await expect(page.getByRole("heading", { name: holderAlias })).toBeVisible();
  const holderRun = page.waitForURL(new RegExp(`/jobs/${holderId}/runs/[^/]+$`));
  await page.getByRole("button", { name: "Trigger job" }).click();
  await page.getByRole("button", { name: "Confirm Trigger" }).click();
  await holderRun;

  // Second start in the same namespace: parked, not started.
  await page.goto(`/jobs/${waiterId}`);
  await expect(page.getByRole("heading", { name: waiterAlias })).toBeVisible();
  const isWaiterQueue = (response: Response) =>
    new URL(response.url()).pathname === `/v1/jobs/${waiterId}/queue` &&
    response.request().method() === "GET" &&
    response.status() === 200;
  // The label is the server's: the live queue response must carry held_reason.
  // Registered before the click, and matched on the held row, so neither the
  // page's initial (empty) queue read nor a missed response can satisfy it.
  const parked = page.waitForResponse(async (response) => {
    if (!isWaiterQueue(response)) return false;
    const rows = (await response.json().catch(() => [])) as { held_reason?: string }[];
    return rows.some((row) => row.held_reason === "namespace_quota");
  });
  await page.getByRole("button", { name: "Trigger job" }).click();
  await page.getByRole("button", { name: "Confirm Trigger" }).click();
  await expect(page.getByText("Job queued")).toBeVisible();
  expect((await parked).ok()).toBe(true);

  const queueRow = page.getByTestId("run-queue-row").first();
  await expect(queueRow).toBeVisible();
  await expect(queueRow.getByTestId("run-queue-quota-badge")).toHaveText("Held by namespace quota");
  await expect(queueRow).not.toContainText("Waiting for a run slot");

  // Released: the holder ends (25 s sleep plus a possible image pull), the
  // dequeuer starts the parked run (500 ms interval) and it finishes. Assertions
  // default to 15 s, so this poll carries its own budget inside the 120 s test.
  await expect
    .poll(
      async () => {
        const response = await page.request.get(`/v1/jobs/${waiterId}/runs`);
        expect(response.ok()).toBe(true);
        return ((await response.json()) as RunSummary[]).map((run) => run.status);
      },
      { timeout: 90_000, intervals: [1_000, 2_000, 3_000] },
    )
    .toContain("succeeded");
  const queue = await page.request.get(`/v1/jobs/${waiterId}/queue`);
  expect(queue.ok()).toBe(true);
  expect(await queue.json()).toHaveLength(0);
});
