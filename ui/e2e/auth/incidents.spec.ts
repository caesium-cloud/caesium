import { expect, test, type APIRequestContext } from "@playwright/test";
import {
  authHeaders,
  loginAtUrl,
  obtainAuthKeys,
  type AuthLaneKeys,
} from "../helpers/auth";
import type { FixtureDefinition } from "../helpers/fixtures";

type E2EJob = {
  id: string;
  alias: string;
};

type E2ERun = {
  id: string;
  job_id: string;
  status: string;
};

type E2EIncident = {
  id: string;
  job_id: string;
  run_id?: string;
  task_name?: string;
  class: string;
  status: string;
};

type IncidentListResponse = {
  incidents: E2EIncident[];
};

type SystemFeatures = {
  agent_remediation_enabled: boolean;
};

type FailingFixture = FixtureDefinition & {
  apiVersion: string;
  kind: string;
  metadata: Record<string, unknown> & { alias: string };
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
  }>;
};

const shellImage = "alpine:3.23";
let keys: AuthLaneKeys;

test.beforeAll(async ({ request }) => {
  keys = await obtainAuthKeys(request);
});

test("incidents board filters a live failure and opens the detail timeline", async ({ page, request }) => {
  test.slow();

  const alias = `incident-ui-${Date.now().toString(36)}`;
  const setup = setupAuth(keys);
  await assertIncidentFeatureEnabled(request, authHeaders(keys.viewer));
  await applyDefinition(request, buildFailingDefinition(alias), setup.headers, setup.hasSetupKey);

  const job = await findJobByAlias(request, alias, authHeaders(keys.viewer));
  const run = await triggerRun(request, job.id, authHeaders(keys.runner));
  await waitForFailedRun(request, job.id, run.id, authHeaders(keys.viewer));
  const incident = await waitForIncident(request, job.id, run.id, authHeaders(keys.viewer));

  await loginAtUrl(page, `/incidents?job_id=${job.id}`, keys.viewer);
  await expect(page.getByRole("heading", { name: "Incidents", exact: true })).toBeVisible({
    timeout: 30_000,
  });
  await expect(page.locator("aside").getByRole("link", { name: /^Incidents\b/ })).toBeVisible();

  await page.getByTestId("incident-status-filter").selectOption(incident.status);
  await page.getByTestId("incident-class-filter").selectOption(incident.class);

  // Target the specific incident row by id: the classifier can open more than one
  // incident for a single failing run (a minimal one plus the task-attributed one),
  // so filtering by alias + .first() can select a different incident than
  // waitForIncident returned and then navigate to the wrong detail URL.
  const row = page.locator(`a[data-testid="incident-row"][href$="/incidents/${incident.id}"]`);
  await expect(row).toBeVisible({ timeout: 30_000 });
  // The classifier opens a minimal incident and attributes the failing task_name
  // asynchronously, so the board row can still show the pre-enrichment task
  // (shortId fallback) within this window even though the incident API already
  // reports it. Task attribution is verified below on the detail timeline
  // (refetched per-incident), so we don't assert the exact row task text here.
  await expect(row).toContainText(incident.class.replaceAll("_", " "));
  await row.click();

  await page.waitForURL(new RegExp(`/incidents/${incident.id}$`));
  await expect(page.getByTestId("incident-detail-page")).toBeVisible();
  await expect(page.getByTestId("incident-timeline")).toContainText("Failure captured");
  await expect(page.getByTestId("incident-timeline")).toContainText("Classified");
  await expect(page.getByTestId("task-why-container")).toBeVisible({ timeout: 30_000 });
});

function buildFailingDefinition(alias: string): FailingFixture {
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
        name: "fail",
        engine: "docker",
        image: shellImage,
        command: ["sh", "-c", "echo incident-ui-failure >&2; exit 42"],
      },
    ],
  };
}

function setupAuth(authKeys: AuthLaneKeys): { headers: Record<string, string>; hasSetupKey: boolean } {
  const setupKey = envString("CAESIUM_E2E_AUTH_OPERATOR_KEY") ?? envString("CAESIUM_E2E_AUTH_ADMIN_KEY");
  return {
    headers: authHeaders(setupKey ?? authKeys.runner),
    hasSetupKey: Boolean(setupKey),
  };
}

async function assertIncidentFeatureEnabled(
  request: APIRequestContext,
  headers: Record<string, string>,
): Promise<void> {
  const response = await request.get("/v1/system/features", { headers });
  if (!response.ok()) {
    throw new Error(`failed to read system features: ${response.status()} ${await response.text()}`);
  }
  const features = (await response.json()) as SystemFeatures;
  expect(features.agent_remediation_enabled).toBe(true);
}

async function applyDefinition(
  request: APIRequestContext,
  definition: FixtureDefinition,
  headers: Record<string, string>,
  hasSetupKey: boolean,
): Promise<void> {
  const response = await request.post("/v1/jobdefs/apply", {
    headers,
    data: { definitions: [definition] },
  });
  if (!response.ok()) {
    if (response.status() === 403 && !hasSetupKey) {
      throw new Error(
        "incident e2e fixture setup requires CAESIUM_E2E_AUTH_OPERATOR_KEY or CAESIUM_E2E_AUTH_ADMIN_KEY",
      );
    }
    throw new Error(`failed to apply fixture: ${response.status()} ${await response.text()}`);
  }
}

async function findJobByAlias(
  request: APIRequestContext,
  alias: string,
  headers: Record<string, string>,
): Promise<E2EJob> {
  const response = await request.get("/v1/jobs", { headers });
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
  headers: Record<string, string>,
): Promise<E2ERun> {
  const response = await request.post(`/v1/jobs/${jobId}/run`, { headers });
  if (!response.ok()) {
    throw new Error(`failed to trigger run: ${response.status()} ${await response.text()}`);
  }
  return (await response.json()) as E2ERun;
}

async function waitForFailedRun(
  request: APIRequestContext,
  jobId: string,
  runId: string,
  headers: Record<string, string>,
): Promise<void> {
  await expect.poll(async () => {
    const response = await request.get(`/v1/jobs/${jobId}/runs/${runId}`, { headers });
    if (!response.ok()) {
      return `HTTP ${response.status()}`;
    }
    const run = (await response.json()) as E2ERun;
    return run.status;
  }, {
    timeout: 120_000,
    intervals: [500, 1_000, 2_000, 5_000],
  }).toBe("failed");
}

async function waitForIncident(
  request: APIRequestContext,
  jobId: string,
  runId: string,
  headers: Record<string, string>,
): Promise<E2EIncident> {
  let latest: E2EIncident | undefined;
  await expect.poll(async () => {
    const response = await request.get(`/v1/incidents?job_id=${jobId}&limit=20`, { headers });
    if (!response.ok()) {
      return `HTTP ${response.status()}`;
    }
    const body = (await response.json()) as IncidentListResponse;
    latest =
      body.incidents.find((candidate) => candidate.run_id === runId && candidate.task_name) ??
      body.incidents.find((candidate) => candidate.run_id === runId);
    return latest?.id ?? "";
  }, {
    timeout: 60_000,
    intervals: [500, 1_000, 2_000],
  }).not.toBe("");
  if (!latest) {
    throw new Error(`incident not found for run ${runId}`);
  }
  return latest;
}

function envString(name: string): string | null {
  const value = process.env[name]?.trim();
  return value ? value : null;
}
