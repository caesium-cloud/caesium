import { expect, test, type APIRequestContext } from "@playwright/test";
import { applyDefinitions, type FixtureDefinition } from "./helpers/fixtures";

type E2EJob = {
  id: string;
  alias: string;
};

type E2ERun = {
  id: string;
  status: string;
};

type RunDiffFixture = FixtureDefinition & {
  apiVersion: string;
  kind: string;
  metadata: Record<string, unknown> & {
    alias: string;
    cache: boolean;
  };
  trigger: {
    type: string;
    configuration: Record<string, unknown> & {
      cron: string;
      timezone: string;
    };
  };
  steps: Array<Record<string, unknown> & {
    name: string;
    engine: string;
    image: string;
    cache?: boolean;
    command: string[];
    next?: string[];
    dependsOn?: string[];
  }>;
};

const neutralShellImage = "debian:12-slim";

test("operator can compare runs and see changed versus cache-hit tasks", async ({ page, request }) => {
  test.slow();

  const alias = `run-diff-e2e-${Date.now().toString(36)}`;

  await applyDefinitions(request, buildRunDiffDefinition(alias, "v1"));
  const job = await findJobByAlias(request, alias);

  const leftRun = await triggerRun(request, job.id);
  await waitForRun(request, job.id, leftRun.id);

  // In v1, changed run params are folded into every task hash. Updating only
  // the render command gives the UI one RERAN task and one WOULD_CACHE_HIT task
  // from the real run-diff endpoint without manufacturing selective param hits.
  await applyDefinitions(request, buildRunDiffDefinition(alias, "v2"));
  const rightRun = await triggerRun(request, job.id);
  await waitForRun(request, job.id, rightRun.id);

  await page.goto(`/jobs/${job.id}/runs/${leftRun.id}/diff?to=${rightRun.id}`);

  await expect(page.getByTestId("run-diff-container")).toBeVisible({ timeout: 30_000 });

  const changedRow = page.getByTestId("run-diff-task-row").filter({ hasText: "render" });
  await expect(changedRow.getByTestId("run-diff-verdict")).toContainText("Reran");
  await expect(changedRow.getByTestId("run-diff-discriminating-field")).toContainText("command");

  const unchangedRow = page.getByTestId("run-diff-task-row").filter({ hasText: "stable-source" });
  await expect(unchangedRow.getByTestId("run-diff-cache-hit-marker")).toContainText("WOULD_CACHE_HIT");
});

function buildRunDiffDefinition(alias: string, variant: string): RunDiffFixture {
  return {
    apiVersion: "v1",
    kind: "Job",
    metadata: {
      alias,
      cache: true,
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
        name: "stable-source",
        engine: "docker",
        image: neutralShellImage,
        command: ["sh", "-c", "echo '##caesium::output {\"rows\": \"42\"}'"],
        next: ["render"],
      },
      {
        name: "render",
        engine: "docker",
        image: neutralShellImage,
        command: ["sh", "-c", `echo render-${variant}`],
        dependsOn: ["stable-source"],
      },
    ],
  };
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

async function triggerRun(request: APIRequestContext, jobId: string): Promise<E2ERun> {
  const response = await request.post(`/v1/jobs/${jobId}/run`);
  if (!response.ok()) {
    throw new Error(`failed to trigger run: ${response.status()} ${await response.text()}`);
  }

  return (await response.json()) as E2ERun;
}

async function waitForRun(request: APIRequestContext, jobId: string, runId: string): Promise<void> {
  await expect
    .poll(
      async () => {
        const response = await request.get(`/v1/jobs/${jobId}/runs/${runId}`);
        if (!response.ok()) {
          throw new Error(`failed to load run ${runId}: ${response.status()} ${await response.text()}`);
        }
        const run = (await response.json()) as E2ERun;
        return run.status;
      },
      {
        timeout: 120_000,
        intervals: [1_000, 2_000, 5_000],
      },
    )
    .toBe("succeeded");
}
