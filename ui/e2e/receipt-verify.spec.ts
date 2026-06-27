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

type E2EReceipt = {
  receipt_version: number;
  run_id: string;
  job_id: string;
  job_alias?: string;
  git_commit?: string;
  manifest_content_hash?: string;
  tasks: Array<{
    task_name: string;
    identity_hash: string;
    image: string;
    resolved_image_digest?: string;
    digest_pinned: boolean;
    degraded: boolean;
    degraded_reason?: string;
  }>;
  degraded: boolean;
  degraded_tasks?: string[];
  receipt_digest: string;
};

type ReceiptFixture = FixtureDefinition & {
  apiVersion: string;
  kind: string;
  metadata: Record<string, unknown> & {
    alias: string;
    cache: Record<string, unknown>;
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
    command: string[];
    next?: string[];
    dependsOn?: string[];
  }>;
};

const shellImage = "debian:12-slim";

test("operator can inspect receipts and verify a committed receipt against drift", async ({ page, request }) => {
  test.slow();

  const pinnedAlias = `receipt-verify-e2e-${Date.now().toString(36)}`;
  await applyDefinitions(request, buildReceiptDefinition(pinnedAlias, "v1", true));
  const pinnedJob = await findJobByAlias(request, pinnedAlias);
  const pinnedRun = await triggerRun(request, pinnedJob.id);
  await waitForSucceededRun(request, pinnedJob.id, pinnedRun.id);

  const committedReceipt = await getReceipt(request, pinnedJob.id, pinnedRun.id);
  // Reliable in CI: digest pinning resolves via the LOCAL docker daemon by
  // inspecting the image the run already pulled (internal/imagecheck/resolve.go),
  // not a remote registry round-trip — so the pinned job is non-degraded.
  expect(committedReceipt.degraded).toBe(false);

  await page.goto(`/jobs/${pinnedJob.id}/runs/${pinnedRun.id}`);
  await expect(page.getByTestId("receipt-panel")).toBeVisible({ timeout: 30_000 });
  await expect(page.getByTestId("receipt-digest")).toContainText(committedReceipt.receipt_digest);
  await expect(page.getByTestId("receipt-task-row").filter({ hasText: "prepare" })).toContainText("digest_pinned=true");
  await expect(page.getByTestId("receipt-task-row").filter({ hasText: "render" })).toContainText("identity_hash");

  await page.getByTestId("receipt-verify-input").fill(JSON.stringify(committedReceipt, null, 2));
  await page.getByTestId("receipt-verify-submit").click();
  await expect(page.getByTestId("receipt-verify-verdict")).toContainText("reproducible", {
    timeout: 30_000,
  });
  await expect(page.getByTestId("receipt-verify-no-drifts")).toContainText("No drift returned by the backend.");

  await applyDefinitions(request, buildReceiptDefinition(pinnedAlias, "v2", true));

  await page.getByTestId("receipt-verify-submit").click();
  await expect(page.getByTestId("receipt-verify-verdict")).toContainText(/manifest_changed|git_commit_changed/, {
    timeout: 30_000,
  });
  await expect(
    page.getByTestId("receipt-verify-drift-kind").filter({ hasText: /manifest_changed|git_commit_changed/ }),
  ).toBeVisible();
  await expect(page.getByTestId("receipt-verify-drift-kind").filter({ hasText: "image_digest_mismatch" })).toHaveCount(0);
  await expect(page.getByTestId("receipt-verify-drift-kind").filter({ hasText: "identity_hash_mismatch" })).toHaveCount(0);

  const unpinnedAlias = `receipt-unpinned-e2e-${Date.now().toString(36)}`;
  await applyDefinitions(request, buildReceiptDefinition(unpinnedAlias, "v1", false));
  const unpinnedJob = await findJobByAlias(request, unpinnedAlias);
  const unpinnedRun = await triggerRun(request, unpinnedJob.id);
  await waitForSucceededRun(request, unpinnedJob.id, unpinnedRun.id);

  await page.goto(`/jobs/${unpinnedJob.id}/runs/${unpinnedRun.id}`);
  await expect(page.getByTestId("receipt-panel")).toBeVisible({ timeout: 30_000 });
  await expect(page.getByTestId("receipt-degraded-status")).toContainText("degraded-unverifiable");
  await expect(page.getByTestId("receipt-task-unverifiable-marker").first()).toContainText("unverifiable");
  await expect(page.getByTestId("receipt-task-degraded-reason").first()).toContainText("image not digest-pinned");
  await expect(page.getByTestId("receipt-unverifiable-summary")).toContainText("Unverifiable tasks");
});

function buildReceiptDefinition(alias: string, variant: string, pinDigests: boolean): ReceiptFixture {
  return {
    apiVersion: "v1",
    kind: "Job",
    metadata: {
      alias,
      cache: {
        pinDigests,
        digestTTL: 0,
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
        name: "prepare",
        engine: "docker",
        image: shellImage,
        command: ["sh", "-c", "echo '##caesium::output {\"rows\": \"42\"}'"],
        next: ["render"],
      },
      {
        name: "render",
        engine: "docker",
        image: shellImage,
        command: ["sh", "-c", `echo receipt-${variant}`],
        dependsOn: ["prepare"],
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

async function waitForSucceededRun(request: APIRequestContext, jobId: string, runId: string): Promise<void> {
  await expect
    .poll(
      async () => {
        const response = await request.get(`/v1/jobs/${jobId}/runs/${runId}`);
        if (!response.ok()) {
          return `HTTP ${response.status()}`;
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

async function getReceipt(request: APIRequestContext, jobId: string, runId: string): Promise<E2EReceipt> {
  const response = await request.get(`/v1/jobs/${jobId}/runs/${runId}/receipt`);
  if (!response.ok()) {
    throw new Error(`failed to get receipt: ${response.status()} ${await response.text()}`);
  }
  return (await response.json()) as E2EReceipt;
}
