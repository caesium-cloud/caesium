import { expect, test, type APIRequestContext } from "@playwright/test";

type E2EJob = {
  id: string;
  alias: string;
};

type E2ERun = {
  id: string;
  status: string;
};

type LineageImpactResult = {
  root_namespace: string;
  root_name: string;
  downstream: Array<{
    dataset_namespace: string;
    dataset_name: string;
    direction: string;
    producing_step?: string;
    job_id: string;
    job_alias: string;
    last_seen: string;
    depth: number;
  }>;
};

type LineageFixture = {
  apiVersion: string;
  kind: string;
  metadata: {
    alias: string;
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
    outputSchema?: Record<string, unknown>;
    inputSchema?: Record<string, Record<string, unknown>>;
    next?: string[];
    dependsOn?: string[];
  }>;
};

const shellImage = "debian:12-slim";

test("operator can inspect a dataset-keyed downstream lineage graph", async ({ page, request }) => {
  test.slow();

  const alias = `lineage-e2e-${Date.now().toString(36)}`;
  await applyDefinition(request, buildLineageDefinition(alias));
  const job = await findJobByAlias(request, alias);

  const run = await triggerRun(request, job.id);
  await waitForRun(request, job.id, run.id);

  const rootName = `${alias}.extract.output`;
  const downstreamName = `${alias}.transform.output`;
  const impact = await waitForImpact(request, "caesium", rootName, downstreamName);
  const downstream = impact.downstream.find((node) => node.dataset_name === downstreamName);

  expect(downstream?.job_id).toBe(job.id);
  expect(downstream?.job_alias).toBe(alias);

  await page.goto(`/lineage?namespace=caesium&name=${encodeURIComponent(rootName)}`);

  await expect(page.getByTestId("lineage-container")).toBeVisible();
  await expect(page.getByTestId("lineage-namespace-input")).toHaveValue("caesium");
  await expect(page.getByTestId("lineage-name-input")).toHaveValue(rootName);

  await expect(page.getByTestId("lineage-root-node")).toBeVisible();
  await expect(page.getByTestId("lineage-root-node")).toContainText(rootName);
  await expect(page.getByTestId("lineage-root-node")).toContainText("caesium");

  const impactNode = page.getByTestId(lineageImpactNodeTestId("caesium", downstreamName, job.id));
  await expect(impactNode).toBeVisible();
  await expect(impactNode).toContainText(downstreamName);
  await expect(impactNode).toContainText(alias);
  await expect(page.getByTestId("lineage-edge").first()).toContainText("downstream");

  await impactNode.click();
  await page.waitForURL(new RegExp(`/jobs/${job.id}$`));
  await expect(page.getByRole("heading", { name: alias, exact: true })).toBeVisible();

  await page.goto(`/lineage?namespace=caesium&name=${encodeURIComponent(`${alias}.empty.output`)}`);
  await expect(page.getByTestId("lineage-empty-state")).toBeVisible();
  await expect(page.getByTestId("lineage-empty-state")).toContainText(
    "no downstream impact recorded yet (lineage datasets populate as jobs declare/consume outputs)",
  );
});

function lineageImpactNodeTestId(namespace: string, name: string, jobId: string): string {
  return `lineage-impact-node:${namespace}:${name}:${jobId}`;
}

function buildLineageDefinition(alias: string): LineageFixture {
  return {
    apiVersion: "v1",
    kind: "Job",
    metadata: {
      alias,
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
        name: "extract",
        engine: "docker",
        image: shellImage,
        outputSchema: {
          type: "object",
          properties: {
            rows: { type: "string" },
          },
        },
        command: ["sh", "-c", "echo '##caesium::output {\"rows\": \"1\"}'"],
        next: ["transform"],
      },
      {
        name: "transform",
        engine: "docker",
        image: shellImage,
        inputSchema: {
          extract: {
            required: ["rows"],
            properties: {
              rows: { type: "string" },
            },
          },
        },
        outputSchema: {
          type: "object",
          properties: {
            clean: { type: "string" },
          },
        },
        command: [
          "sh",
          "-c",
          "echo got $CAESIUM_OUTPUT_EXTRACT_ROWS; echo '##caesium::output {\"clean\": \"yes\"}'",
        ],
        dependsOn: ["extract"],
      },
    ],
  };
}

async function applyDefinition(request: APIRequestContext, definition: LineageFixture): Promise<void> {
  const response = await request.post("/v1/jobdefs/apply", {
    data: { definitions: [definition] },
  });
  if (!response.ok()) {
    throw new Error(`failed to apply fixture: ${response.status()} ${await response.text()}`);
  }
}

async function findJobByAlias(request: APIRequestContext, alias: string): Promise<E2EJob> {
  let foundJob: E2EJob | undefined;
  await expect
    .poll(
      async () => {
        const response = await request.get("/v1/jobs");
        if (!response.ok()) {
          throw new Error(`failed to list jobs: ${response.status()} ${await response.text()}`);
        }
        const jobs = (await response.json()) as E2EJob[];
        foundJob = jobs.find((candidate) => candidate.alias === alias);
        return foundJob?.alias ?? "";
      },
      {
        timeout: 10_000,
        intervals: [250, 500, 1_000],
      },
    )
    .toBe(alias);

  return foundJob!;
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

async function waitForImpact(
  request: APIRequestContext,
  namespace: string,
  name: string,
  downstreamName: string,
): Promise<LineageImpactResult> {
  let latest: LineageImpactResult | undefined;
  await expect
    .poll(
      async () => {
        const response = await request.get(
          `/v1/lineage/impact?namespace=${encodeURIComponent(namespace)}&name=${encodeURIComponent(name)}`,
        );
        if (!response.ok()) {
          return "";
        }
        latest = (await response.json()) as LineageImpactResult;
        return latest.downstream.map((node) => node.dataset_name).join("\n");
      },
      {
        timeout: 30_000,
        intervals: [500, 1_000, 2_000],
      },
    )
    .toContain(downstreamName);

  if (!latest) {
    throw new Error("lineage impact endpoint did not return a result");
  }
  return latest;
}
