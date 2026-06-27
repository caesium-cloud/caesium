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
  quarantine?: boolean;
};

type ReplayFixture = FixtureDefinition & {
  apiVersion: string;
  kind: string;
  metadata: Record<string, unknown> & {
    alias: string;
    replaySafe: boolean;
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
let keys: AuthLaneKeys;

test.beforeAll(async ({ request }) => {
  keys = await obtainAuthKeys(request);
});

test("runner sees replay and can launch a no-override quarantined replay", async ({ page, request }) => {
  test.slow();

  const { job, baseline } = await seedReplayBaseline(request, keys, `replay-auth-runner-${Date.now().toString(36)}`);
  // Navigate to the run-detail URL FIRST, then log in there. The api-key session
  // is in-memory only, so a goto AFTER login would drop it — see loginAtUrl.
  const principal = await loginAtUrl(page, `/jobs/${job.id}/runs/${baseline.id}`, keys.runner);
  expect(principal.role).toBe("runner");

  await expect(page.getByRole("heading", { name: /Run / })).toBeVisible({ timeout: 30_000 });
  await expect(page.getByTestId("run-replay-trigger")).toBeEnabled();
  await expect(page.getByTestId("run-replay-gate-reason")).toHaveCount(0);

  await page.getByTestId("run-replay-trigger").click();
  await expect(page.getByTestId("replay-dialog")).toBeVisible();
  await page.getByTestId("replay-submit").click();

  await expect(page.getByTestId("replay-result")).toBeVisible({ timeout: 30_000 });
  await expect(page.getByTestId("replay-quarantine-badge")).toBeVisible();
  await expect(page.getByTestId("replay-result-status")).toContainText("succeeded");
  const replayRunId = (await page.getByTestId("replay-result-run-id").innerText()).trim();
  expect(replayRunId).not.toHaveLength(0);
  await expect.poll(async () => {
    const run = await getRun(request, job.id, replayRunId, authHeaders(keys.runner));
    return `${run.status}:${String(run.quarantine)}`;
  }, {
    timeout: 30_000,
    intervals: [500, 1_000, 2_000],
  }).toBe("succeeded:true");
});

test("viewer gets a gated non-actionable replay affordance", async ({ page, request }) => {
  test.slow();

  const { job, baseline } = await seedReplayBaseline(request, keys, `replay-auth-viewer-${Date.now().toString(36)}`);
  const principal = await loginAtUrl(page, `/jobs/${job.id}/runs/${baseline.id}`, keys.viewer);
  expect(principal.role).toBe("viewer");

  await expect(page.getByRole("heading", { name: /Run / })).toBeVisible({ timeout: 30_000 });
  await expect(page.getByTestId("run-replay-trigger")).toBeVisible();
  await expect(page.getByTestId("run-replay-trigger")).toBeDisabled();
  await expect(page.getByTestId("run-replay-gate-reason")).toContainText("Requires runner role");
  await expect(page.getByTestId("replay-dialog")).toHaveCount(0);
});

async function seedReplayBaseline(
  request: APIRequestContext,
  authKeys: AuthLaneKeys,
  alias: string,
): Promise<{ job: E2EJob; baseline: E2ERun }> {
  const setup = setupAuth(authKeys);
  await applyDefinition(request, buildReplayDefinition(alias), setup.headers, setup.hasSetupKey);
  const job = await findJobByAlias(request, alias, authHeaders(authKeys.runner));
  const baseline = await triggerRun(request, job.id, { mode: "seed" }, authHeaders(authKeys.runner));
  await waitForSucceededRun(request, job.id, baseline.id, authHeaders(authKeys.runner));
  return { job, baseline };
}

function buildReplayDefinition(alias: string): ReplayFixture {
  return {
    apiVersion: "v1",
    kind: "Job",
    metadata: {
      alias,
      replaySafe: true,
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
        name: "deterministic",
        engine: "docker",
        image: shellImage,
        command: ["sh", "-c", "echo '##caesium::output {\"rows\": \"42\"}'"],
        next: ["load"],
      },
      {
        name: "load",
        engine: "docker",
        image: shellImage,
        command: ["sh", "-c", "echo loaded"],
        dependsOn: ["deterministic"],
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
        "auth replay e2e fixture setup requires CAESIUM_E2E_AUTH_OPERATOR_KEY or CAESIUM_E2E_AUTH_ADMIN_KEY",
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
  params: Record<string, string>,
  headers: Record<string, string>,
): Promise<E2ERun> {
  const response = await request.post(`/v1/jobs/${jobId}/run`, {
    headers,
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
  headers: Record<string, string>,
): Promise<void> {
  await expect.poll(async () => {
    const run = await getRun(request, jobId, runId, headers);
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
  headers: Record<string, string>,
): Promise<E2ERun> {
  const response = await request.get(`/v1/jobs/${jobId}/runs/${runId}`, { headers });
  if (!response.ok()) {
    throw new Error(`failed to load run ${runId}: ${response.status()} ${await response.text()}`);
  }
  return (await response.json()) as E2ERun;
}

function envString(name: string): string | null {
  const value = process.env[name]?.trim();
  return value ? value : null;
}
