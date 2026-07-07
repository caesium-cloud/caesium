import { expect, test, type APIRequestContext, type Page } from "@playwright/test";
import { applyDefinitions, type FixtureDefinition } from "./helpers/fixtures";

type E2EJob = {
  id: string;
  alias: string;
};

type E2ERun = {
  id: string;
  job_id: string;
  status: string;
  quarantine?: boolean;
};

type ReplayFixture = FixtureDefinition & {
  apiVersion: string;
  kind: string;
  metadata: Record<string, unknown> & {
    alias: string;
    replaySafe?: boolean;
    cache: {
      enabled: boolean;
      ttl: string;
    };
  };
  trigger: {
    type: string;
    configuration: {
      cron: string;
      timezone: string;
    };
  };
  steps: Array<{
    name: string;
    engine: string;
    image: string;
    command: string[];
    next?: string[];
    dependsOn?: string[];
  }>;
};

const shellImage = "alpine:3.23";

test("operator can launch cache-hit replay and sees typed refusals inline", async ({ page, request }) => {
  test.slow();

  const alias = `replay-ui-e2e-${Date.now().toString(36)}`;
  await applyDefinitions(request, buildReplayDefinition(alias, true, "deterministic"));
  const job = await findJobByAlias(request, alias);
  const baseline = await triggerRun(request, job.id, { mode: "seed" });
  await waitForSucceededRun(request, job.id, baseline.id);

  await openRunReplayDialog(page, job.id, baseline.id);
  await expect(page.getByTestId("replay-idempotency-key-input")).toHaveValue("");
  await page.getByTestId("replay-submit").click();

  const result = page.getByTestId("replay-result");
  await expect(result).toContainText("Quarantine", { timeout: 30_000 });
  await expect(page.getByTestId("replay-result-status")).toContainText("succeeded");
  await expect(page.getByTestId("replay-idempotency-key-input")).not.toHaveValue("");
  const replayRunId = (await page.getByTestId("replay-result-run-id").innerText()).trim();

  await expect.poll(async () => {
    const replayRun = await getRun(request, job.id, replayRunId);
    return `${replayRun.status}:${String(replayRun.quarantine)}`;
  }, {
    timeout: 30_000,
    intervals: [500, 1_000, 2_000],
  }).toBe("succeeded:true");

  await page.getByTestId("replay-show-diff").click();
  const diffPanel = page.getByTestId("replay-diff-panel");
  await expect(diffPanel).toBeVisible();
  await expect(diffPanel.getByTestId("run-diff-task-deterministic")).toBeVisible({ timeout: 30_000 });
  await expect(diffPanel.getByTestId("run-diff-verdict").first()).toBeVisible();

  const productionRuns = await listRuns(request, job.id);
  expect(productionRuns.map((run) => run.id)).toContain(baseline.id);
  expect(productionRuns.map((run) => run.id)).not.toContain(replayRunId);

  await page.getByRole("button", { name: "Close" }).click();
  await page.goto(`/jobs/${job.id}`);
  await page.getByRole("link", { name: "Runs", exact: true }).click();
  const runHistory = page.getByRole("dialog", { name: "Run History" });
  await expect(runHistory).toBeVisible();
  await expect(runHistory.locator(`a[href="/jobs/${job.id}/runs/${baseline.id}"]`)).toHaveCount(1);
  await expect(runHistory.locator(`a[href="/jobs/${job.id}/runs/${replayRunId}"]`)).toHaveCount(0);

  await openRunReplayDialog(page, job.id, baseline.id);
  await page.getByTestId("replay-set-key-0").fill("mode");
  await page.getByTestId("replay-set-value-0").fill("what-if");
  await page.getByTestId("replay-submit").click();
  await expect(page.getByTestId("replay-inline-error")).toContainText(
    "This replay re-executes tasks, which requires distributed execution mode.",
    { timeout: 30_000 },
  );
  await expect(page.getByTestId("replay-result")).toHaveCount(0);

  const unsafeAlias = `replay-ui-unsafe-${Date.now().toString(36)}`;
  await applyDefinitions(request, buildReplayDefinition(unsafeAlias, false, "unsafe-step"));
  const unsafeJob = await findJobByAlias(request, unsafeAlias);
  const unsafeBaseline = await triggerRun(request, unsafeJob.id, { mode: "seed" });
  await waitForSucceededRun(request, unsafeJob.id, unsafeBaseline.id);

  await openRunReplayDialog(page, unsafeJob.id, unsafeBaseline.id);
  await page.getByTestId("replay-set-key-0").fill("mode");
  await page.getByTestId("replay-set-value-0").fill("what-if");
  await page.getByTestId("replay-submit").click();
  await expect(page.getByTestId("replay-inline-error")).toContainText(
    "Replay-safe gate refused this replay:",
    { timeout: 30_000 },
  );
  await expect(page.getByTestId("replay-inline-error")).toContainText("unsafe-step");
  await expect(page.getByTestId("replay-result")).toHaveCount(0);
});

function buildReplayDefinition(alias: string, replaySafe: boolean, stepName: string): ReplayFixture {
  return {
    apiVersion: "v1",
    kind: "Job",
    metadata: {
      alias,
      replaySafe,
      cache: {
        enabled: true,
        ttl: "1h",
      },
    },
    trigger: {
      type: "cron",
      configuration: {
        cron: "0 2 * * *",
        timezone: "UTC",
      },
    },
    steps: [
      {
        name: stepName,
        engine: "docker",
        image: shellImage,
        command: ["sh", "-c", "echo '##caesium::output {\"rows\": \"42\"}'"],
        next: [`${stepName}-load`],
      },
      {
        name: `${stepName}-load`,
        engine: "docker",
        image: shellImage,
        command: ["sh", "-c", "echo loaded"],
        dependsOn: [stepName],
      },
    ],
  };
}

async function openRunReplayDialog(page: Page, jobId: string, runId: string): Promise<void> {
  await page.goto(`/jobs/${jobId}/runs/${runId}`);
  await expect(page.getByRole("heading", { name: /Run / })).toBeVisible({ timeout: 30_000 });
  await expect(page.getByTestId("run-replay-trigger")).toBeEnabled();
  await page.getByTestId("run-replay-trigger").click();
  await expect(page.getByTestId("replay-dialog")).toBeVisible();
}

async function findJobByAlias(request: APIRequestContext, alias: string): Promise<E2EJob> {
  const response = await request.get("/v1/jobs");
  if (!response.ok()) {
    throw new Error(`failed to list jobs: ${response.status()} ${await response.text()}`);
  }
  const jobs = (await response.json()) as E2EJob[];
  const job = jobs.find((candidate) => candidate.alias === alias);
  if (!job) {
    throw new Error(`job not found after apply: ${alias}`);
  }
  return job;
}

async function triggerRun(
  request: APIRequestContext,
  jobId: string,
  params: Record<string, string>,
): Promise<E2ERun> {
  const response = await request.post(`/v1/jobs/${jobId}/run`, {
    data: { params },
  });
  if (!response.ok()) {
    throw new Error(`failed to trigger run: ${response.status()} ${await response.text()}`);
  }
  return (await response.json()) as E2ERun;
}

async function waitForSucceededRun(
  request: APIRequestContext,
  jobId: string,
  runId: string,
): Promise<void> {
  await expect.poll(async () => {
    const run = await getRun(request, jobId, runId);
    return run.status;
  }, {
    timeout: 120_000,
    intervals: [500, 1_000, 2_000, 5_000],
  }).toBe("succeeded");
}

async function getRun(
  request: APIRequestContext,
  jobId: string,
  runId: string,
): Promise<E2ERun> {
  const response = await request.get(`/v1/jobs/${jobId}/runs/${runId}`);
  if (!response.ok()) {
    throw new Error(`failed to load run ${runId}: ${response.status()} ${await response.text()}`);
  }
  return (await response.json()) as E2ERun;
}

async function listRuns(request: APIRequestContext, jobId: string): Promise<E2ERun[]> {
  const response = await request.get(`/v1/jobs/${jobId}/runs`);
  if (!response.ok()) {
    throw new Error(`failed to list runs: ${response.status()} ${await response.text()}`);
  }
  return (await response.json()) as E2ERun[];
}
