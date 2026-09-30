import { expect, test, type APIRequestContext } from "@playwright/test";
import { applyDefinitions, type FixtureDefinition } from "./helpers/fixtures";

type E2EJob = {
  id: string;
  alias: string;
};

// POST /v1/jobs/:id/run answers every accepted start with an outcome: the run
// itself for "created", or {outcome, job_id, queue_id} for a queued start.
type RunStart = {
  outcome: "created" | "queued" | "skipped";
  id?: string;
  queue_id?: string;
};

type QueueRow = {
  id: string;
  position: number;
  priority: number;
  params?: Record<string, string>;
  enqueued_at: string;
};

test("job detail shows pending queued runs", async ({ page, request }) => {
  const alias = `queue-panel-e2e-${Date.now().toString(36)}`;
  await applyDefinitions(request, buildQueueDefinition(alias));
  const job = await findJobByAlias(request, alias);

  const activeRun = await triggerRun(request, job.id, "low", { lane: "active" });
  expect(activeRun.outcome).toBe("created");
  expect(activeRun.id).toBeTruthy();

  const queuedRun = await triggerRun(request, job.id, "high", { lane: "queued" });
  expect(queuedRun.outcome).toBe("queued");
  expect(queuedRun.id, "a queued start has no run yet").toBeUndefined();
  expect(queuedRun.queue_id).toBeTruthy();

  await expect
    .poll(async () => {
      const rows = await getQueue(request, job.id);
      return rows[0]?.params?.lane ?? "";
    }, { timeout: 10_000 })
    .toBe("queued");
  const queuedRows = await getQueue(request, job.id);
  const queuedRow = queuedRows[0];
  if (!queuedRow) {
    throw new Error("queued row was not returned after polling");
  }
  expect(queuedRow.id).toBe(queuedRun.queue_id);

  await page.route(`**/v1/jobs/${job.id}/queue/${queuedRow.id}`, async (route) => {
    await route.fulfill({
      status: 409,
      contentType: "application/json",
      body: JSON.stringify({ message: "already started" }),
    });
  });

  await page.goto(`/jobs/${job.id}`);
  const panel = page.getByTestId("run-queue-panel");
  await expect(panel).toBeVisible();
  const row = panel.getByTestId("run-queue-row").filter({ hasText: "lane=queued" });
  await expect(row).toBeVisible();
  await expect(row).toContainText("#1");
  await expect(row).toContainText("high");
  await expect(row).toContainText("Waiting for a run slot");
  await expect(row).toContainText("enqueued");
  await expect(row.getByTestId("run-queue-inspect-link")).toHaveAttribute("href", new RegExp(`#queue-${queuedRow.id}$`));

  await row.getByRole("button", { name: /Cancel queued run/ }).click();
  await expect(page.getByText("Queued run already started")).toBeVisible();
});

function buildQueueDefinition(alias: string): FixtureDefinition {
  return {
    apiVersion: "v1",
    kind: "Job",
    metadata: {
      alias,
      concurrency: {
        maxRuns: 1,
        strategy: "queue",
      },
    },
    trigger: {
      type: "cron",
      configuration: {
        cron: "0 0 1 1 *",
      },
    },
    steps: [
      {
        name: "hold",
        image: "alpine:3.23",
        command: ["sh", "-c", "sleep 30"],
      },
    ],
  } as FixtureDefinition;
}

async function findJobByAlias(request: APIRequestContext, alias: string): Promise<E2EJob> {
  const response = await request.get("/v1/jobs");
  if (!response.ok()) {
    throw new Error(`failed to list jobs: ${response.status()} ${await response.text()}`);
  }
  const jobs = (await response.json()) as E2EJob[];
  const job = jobs.find((entry) => entry.alias === alias);
  if (!job) {
    throw new Error(`job not found after apply: ${alias}`);
  }
  return job;
}

async function triggerRun(
  request: APIRequestContext,
  jobId: string,
  priority: "low" | "normal" | "high",
  params: Record<string, string>,
): Promise<RunStart> {
  const response = await request.post(`/v1/jobs/${jobId}/run`, {
    data: { priority, params },
  });
  if (!response.ok()) {
    throw new Error(`failed to trigger run: ${response.status()} ${await response.text()}`);
  }
  return (await response.json()) as RunStart;
}

async function getQueue(request: APIRequestContext, jobId: string): Promise<QueueRow[]> {
  const response = await request.get(`/v1/jobs/${jobId}/queue`);
  if (!response.ok()) {
    throw new Error(`failed to load run queue: ${response.status()} ${await response.text()}`);
  }
  return (await response.json()) as QueueRow[];
}
