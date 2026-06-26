import {
  expect,
  type APIRequestContext,
  type Page,
  test,
} from "@playwright/test";
import {
  applyDefinitions,
  loadFixtureDefinition,
  type FixtureDefinition,
} from "./helpers/fixtures";

type CacheableFixtureDefinition = FixtureDefinition & {
  metadata?: FixtureDefinition["metadata"] & {
    cache?: {
      enabled: boolean;
    };
  };
};

type JobSummary = {
  id: string;
  alias: string;
};

type RunSummary = {
  id: string;
  job_id: string;
  status: string;
};

test("task details explain cache miss and cache hit causation", async ({ page, request }) => {
  test.slow();

  const definition = await loadCacheableFixture();
  const alias = String(definition.metadata?.alias);
  await applyDefinitions(request, definition);

  const job = await findJobByAlias(request, alias);

  const seedRun = await triggerRun(request, job.id, { scenario: "seed" });
  await waitForSucceededRun(request, job.id, seedRun.id);

  const changedRun = await triggerRun(request, job.id, { scenario: "changed" });
  await waitForSucceededRun(request, job.id, changedRun.id);

  const cachedRun = await triggerRun(request, job.id, { scenario: "changed" });
  await waitForSucceededRun(request, job.id, cachedRun.id);

  await openFirstTaskDetails(page, job.id, changedRun.id);
  await expect(page.getByTestId("task-why-container")).toContainText("CACHE MISS", {
    timeout: 30_000,
  });
  await expect(page.getByTestId("task-why-discriminating-field")).toContainText(
    "runParams.scenario",
  );

  await openFirstTaskDetails(page, job.id, cachedRun.id);
  await expect(page.getByTestId("task-why-container")).toContainText("CACHE HIT", {
    timeout: 30_000,
  });
  await expect(page.getByTestId("task-why-discriminating-field")).toContainText(
    "hashEqual=true",
  );

  const sourceRunLink = page.getByTestId("task-why-source-run-link");
  await expect(sourceRunLink).toBeVisible();
  await expect(sourceRunLink).toHaveAttribute(
    "href",
    `/jobs/${job.id}/runs/${changedRun.id}`,
  );
});

async function loadCacheableFixture(): Promise<CacheableFixtureDefinition> {
  const definition = (await loadFixtureDefinition(
    "minimal.job.yaml",
  )) as CacheableFixtureDefinition;
  definition.metadata = {
    ...(definition.metadata ?? {}),
    cache: { enabled: true },
  };
  return definition;
}

async function findJobByAlias(request: APIRequestContext, alias: string): Promise<JobSummary> {
  const response = await request.get("/v1/jobs");
  if (!response.ok()) {
    throw new Error(`failed to list jobs: ${response.status()} ${await response.text()}`);
  }

  const jobs = (await response.json()) as JobSummary[];
  const job = jobs.find((candidate) => candidate.alias === alias);
  if (!job) {
    throw new Error(`failed to find applied job ${alias}`);
  }
  return job;
}

async function triggerRun(
  request: APIRequestContext,
  jobId: string,
  params: Record<string, string>,
): Promise<RunSummary> {
  const response = await request.post(`/v1/jobs/${jobId}/run`, {
    data: { params },
  });
  if (!response.ok()) {
    throw new Error(`failed to trigger run: ${response.status()} ${await response.text()}`);
  }
  return (await response.json()) as RunSummary;
}

async function waitForSucceededRun(
  request: APIRequestContext,
  jobId: string,
  runId: string,
) {
  await expect.poll(
    async () => {
      const response = await request.get(`/v1/jobs/${jobId}/runs/${runId}`);
      if (!response.ok()) {
        return `HTTP ${response.status()}`;
      }
      const run = (await response.json()) as RunSummary;
      return run.status;
    },
    {
      timeout: 90_000,
      intervals: [500, 1_000, 2_000],
    },
  ).toBe("succeeded");
}

async function openFirstTaskDetails(page: Page, jobId: string, runId: string) {
  await page.goto(`/jobs/${jobId}/runs/${runId}`);
  await expect(page.getByRole("heading", { name: /Run / })).toBeVisible();

  const node = page.locator(".react-flow__node").first();
  await expect(node).toBeVisible({ timeout: 30_000 });
  await node.click();

  const panel = page.getByTestId("task-detail-panel");
  await expect(panel).toBeVisible();
  await panel.getByRole("button", { name: "Details" }).click();
  await expect(page.getByTestId("task-why-container")).toBeVisible();
}
