import { Buffer } from "node:buffer";
import { expect, test, type APIRequestContext, type Page, type TestInfo } from "@playwright/test";
import { authHeaders, loginAtUrl } from "../helpers/auth";
import { fixturePost, requireConnectorBrowserLane, startExternalWorkflow, type ConnectorBrowserLane, type FixtureWorkflow } from "../helpers/connectors";

interface Job { id: string; alias: string }
interface Correlation {
  id: string;
  admission_id: string;
  activity_id: string;
  activity_type: string;
  relation_type: string;
  idempotency_key: string;
  job_id: string;
  queue_id?: string;
  run_id?: string;
  outcome: string;
  execution_id: string;
  reference: { connection_id: string; coordinates: { workflow_id: string; run_id: string } };
}
interface NativeRelation { type: string; id: string; reference: Correlation["reference"] }
interface Relations { correlations: Correlation[]; relationships: NativeRelation[]; cursor?: string; incomplete: boolean }
interface DiscoveredExecution { id: string; reference: Correlation["reference"]; metadata: FixtureWorkflow }
interface JournalEvent { kind: string; workflow_id?: string; run_id?: string; activity_id?: string; idempotency_key?: string; attempt?: number; hint?: { outcome: string; queue_id?: string; run_id?: string }; [key: string]: unknown }

let lane: ConnectorBrowserLane;
const started: FixtureWorkflow[] = [];
const suffix = `${Date.now()}-${process.pid}`;
const dataset = `console-publication-${suffix}`;
const manualKey = () => {
  const key = process.env.CAESIUM_MANUAL_TRIGGER_API_KEY?.trim();
  if (!key) throw new Error("connector correlation browser journey requires CAESIUM_MANUAL_TRIGGER_API_KEY");
  return key;
};
const adminKey = () => process.env.CAESIUM_E2E_AUTH_ADMIN_KEY!.trim();

// A1's public stateless handle addresses the actual fixture start before
// eventually consistent Visibility catches up. No row is seeded or invented.
function executionId(workflow: FixtureWorkflow): string {
  return Buffer.from(JSON.stringify({ ConnectionID: lane.connectionId, Coordinates: { run_id: workflow.run_id, workflow_id: workflow.workflow_id } })).toString("base64url");
}
function executionPath(workflow: FixtureWorkflow): string {
  return `/connectors/${encodeURIComponent(lane.connectionId)}/executions/${encodeURIComponent(executionId(workflow))}`;
}

async function readJSON<T>(request: APIRequestContext, path: string, key: string): Promise<T> {
  const response = await request.get(path, { headers: authHeaders(key) });
  expect(response.ok(), `${path}: HTTP ${response.status()}`).toBe(true);
  return await response.json() as T;
}

async function createKey(request: APIRequestContext, role: "viewer" | "runner", jobs?: string[]): Promise<string> {
  const response = await request.post("/v1/auth/keys", {
    headers: authHeaders(adminKey()),
    data: { role, description: `Console real correlation ${role} ${suffix}`, ...(jobs ? { scope: { jobs } } : {}) },
  });
  expect(response.ok(), "public key creation must succeed").toBe(true);
  const value = await response.json() as { key: string };
  expect(value.key).toMatch(/^csk_/);
  return value.key;
}

async function applyPublicationJobs(request: APIRequestContext): Promise<[Job, Job]> {
  const definitions = ["connector-fixture-publish", "connector-fixture-notify"].map((alias, index) => ({
    apiVersion: "v1", kind: "Job",
    metadata: { alias, cache: false, concurrency: { maxRuns: 1, strategy: "queue" }, datasets: { skipWhenFresh: false } },
    trigger: { type: "cron", configuration: { cron: "0 0 1 1 *" } },
    steps: [{
      name: "work", image: "alpine:3.23",
      command: ["sh", "-c", index === 0
        ? `sleep 30; echo console-correlation-data; processed_through=$(date -u +%Y-%m-%dT%H:%M:%SZ); printf '##caesium::output {"rows":"42","processed_through":"%s"}\\n' "$processed_through"`
        : "echo console-publication-notified"],
      ...(index === 0 ? {
        outputSchema: { type: "object", properties: { rows: { type: "string" }, processed_through: { type: "string" } } },
        datasets: { produces: [{ name: dataset, freshness: "1h", watermark: { key: "processed_through" } }] },
      } : {}),
    }],
  }));
  const applied = await request.post("/v1/jobdefs/apply", { headers: authHeaders(adminKey()), data: { definitions } });
  expect(applied.ok(), "jobs must be applied through the public API").toBe(true);
  const jobs = await readJSON<Job[]>(request, "/v1/jobs", adminKey());
  const result = definitions.map(definition => jobs.find(job => job.alias === definition.metadata.alias));
  expect(result.every(Boolean), "both allowlisted jobs must exist").toBe(true);
  return result as [Job, Job];
}

async function relations(request: APIRequestContext, workflow: FixtureWorkflow, key: string): Promise<Relations> {
  return readJSON(request, `/v1${executionPath(workflow)}/relationships?page_size=100`, key);
}

async function discover(request: APIRequestContext, workflow: FixtureWorkflow, type: string, key: string): Promise<DiscoveredExecution> {
  let execution: DiscoveredExecution | undefined;
  await expect.poll(async () => {
    const page = await readJSON<{ executions: DiscoveredExecution[] }>(request, `/v1/connectors/${encodeURIComponent(lane.connectionId)}/executions?workflow_type=${type}&page_size=100`, key);
    execution = page.executions.find(candidate => candidate.metadata.workflow_id === workflow.workflow_id && candidate.metadata.run_id === workflow.run_id);
    return execution?.id ?? "";
  }, { timeout: 30_000, intervals: [2000, 3000, 5000] }).not.toBe("");
  expect(execution!.reference).toMatchObject({ connection_id: lane.connectionId, coordinates: workflow });
  expect(execution!.id).toBe(executionId(workflow));
  return execution!;
}

async function journal(request: APIRequestContext): Promise<JournalEvent[]> {
  const response = await request.get(`${lane.controlURL}/evidence`);
  expect(response.ok()).toBe(true);
  return await response.json() as JournalEvent[];
}

async function capture(page: Page, info: TestInfo, stage: string) {
  const path = info.outputPath(`correlation-${stage}.png`);
  await page.screenshot({ path, fullPage: true });
  await info.attach(stage, { path, contentType: "image/png" });
}

async function assertLocalEvidence(page: Page, runId: string, receiptDigest: string, watermark: string) {
  await expect(page.getByTestId("run-identity").getByRole("button", { name: /run id/ })).toHaveAttribute("title", runId);
  await expect(page.getByTestId("run-data-evidence")).toBeVisible();
  await page.locator(".react-flow__node").first().click();
  const task = page.getByTestId("task-detail-panel");
  await expect(task.getByTestId("task-log-plaintext")).toContainText("console-correlation-data", { timeout: 30_000 });
  await task.getByRole("button", { name: "Details", exact: true }).click();
  await expect(task).toContainText("rows:");
  await expect(task).toContainText("42");
  await expect(task).toContainText(watermark);
  await page.keyboard.press("Escape");
  await expect(task).toHaveCount(0);
  await page.getByTestId("run-reproducibility-toggle").click();
  await expect(page.getByTestId("receipt-panel")).toBeVisible();
  await expect(page.getByTestId("receipt-digest").getByRole("button")).toHaveAttribute("title", receiptDigest);
  await expect(page.getByTestId("receipt-run-id").getByRole("button")).toHaveAttribute("title", runId);
}

test.beforeAll(async ({ request }) => { lane = await requireConnectorBrowserLane(request); });

test.afterAll(async ({ request }) => {
  if (!lane) return;
  for (const workflow of started) await fixturePost(request, lane, "/signal", { id: workflow.workflow_id, run_id: workflow.run_id, name: "fixture_finish", value: true });
  await fixturePost(request, lane, "/configure", { runner_key: lane.keys.runner });
});

test("real queued delegation promotes one admission and joins existing run data, receipt and scoped evidence to typed workflow relationships", async ({ page, request, browser }, info) => {
  test.slow();
  const [job, notify] = await applyPublicationJobs(request);
  const aliases = [job.alias, notify.alias];
  const runner = await createKey(request, "runner", aliases);
  const reader = await createKey(request, "viewer");
  const observer = await createKey(request, "viewer");
  const scoped = await createKey(request, "viewer", [job.alias]);
  await fixturePost(request, lane, "/configure", { runner_key: runner });
  const earlierRuns = await readJSON<{ id: string }[]>(request, `/v1/jobs/${job.id}/runs?limit=100`, observer);
  const holder = await request.post(`/v1/jobs/${job.id}/run`, {
    headers: { ...authHeaders(runner), "X-Caesium-API-Key": manualKey(), "Idempotency-Key": `console-holder-${suffix}` }, data: { params: {} },
  });
  expect(holder.status()).toBe(202);
  const holderRun = await holder.json() as { id: string; outcome: string };
  expect(holderRun.outcome).toBe("created");
  const workflow = await fixturePost<FixtureWorkflow>(request, lane, "/start", {
    id: `console-publication-${suffix}`, type: "publication",
    input: { first_job: job.id, second_job: notify.id, eligible: true, retry_first: true, metadata_only: true, child: true },
  });
  started.push(workflow);
  let queued!: Correlation;
  await expect.poll(async () => {
    const page = await relations(request, workflow, observer);
    expect(page.correlations.length).toBeLessThanOrEqual(1);
    queued = page.correlations[0];
    return queued?.outcome;
  }, { timeout: 12_000, intervals: [1500, 2000] }).toBe("queued");
  expect(queued.queue_id).toBeTruthy();
  expect(queued.run_id).toBeUndefined();
  expect(queued).toMatchObject({ relation_type: "delegation", job_id: job.id, activity_type: "caesium.start", execution_id: executionId(workflow) });
  expect(queued.idempotency_key).toBe(`${workflow.run_id}/${queued.activity_id}`);
  await loginAtUrl(page, executionPath(workflow), reader);
  // The row itself owns the identity attributes; keep the locator pinned to it.
  const row = page.locator(`[data-testid="verified-delegation"][data-correlation-id="${queued.id}"]`);
  await expect(row).toHaveAttribute("data-admission-id", queued.admission_id);
  await expect(row).toContainText(queued.queue_id!);
  await expect(row).toContainText("queued");
  await expect(row.getByTestId("delegated-run-link")).toHaveCount(0);
  await capture(page, info, "queued-admission");
  // Resolve eventual discovery independently after recording the queued state,
  // so visibility lag cannot make that bounded admission window disappear.
  const discovered = await discover(request, workflow, "publication", observer);
  await expect(row.getByTestId("delegated-run-link")).toBeVisible({ timeout: 50_000 });
  const promotedPage = await relations(request, workflow, observer);
  expect(promotedPage.correlations).toHaveLength(1);
  const promoted = promotedPage.correlations[0];
  expect(promoted).toMatchObject({ id: queued.id, admission_id: queued.admission_id, queue_id: queued.queue_id, execution_id: executionId(workflow) });
  expect(promoted.run_id).toBeTruthy();
  expect(promoted.run_id).not.toBe(holderRun.id);
  await expect(row.getByTestId("delegated-run-link")).toHaveAttribute("href", `/jobs/${job.id}/runs/${promoted.run_id}`);
  await row.getByTestId("delegated-run-link").click();
  await expect(page).toHaveURL(new RegExp(`/jobs/${job.id}/runs/${promoted.run_id}$`));
  await expect.poll(async () => (await readJSON<{ status: string }>(request, `/v1/jobs/${job.id}/runs/${promoted.run_id}`, observer)).status,
    { timeout: 90_000, intervals: [2000, 4000] }).toBe("succeeded");
  const completed = await readJSON<{ tasks: { status: string; cache_hit: boolean; output: { rows: string; processed_through: string } }[] }>(request, `/v1/jobs/${job.id}/runs/${promoted.run_id}`, observer);
  expect(completed.tasks).toHaveLength(1);
  expect(completed.tasks[0]).toMatchObject({ status: "succeeded", cache_hit: false });
  const watermark = completed.tasks[0].output.processed_through;
  expect(completed.tasks[0].output.rows).toBe("42");
  expect(Date.parse(watermark)).toBeGreaterThan(0);
  const finalRuns = await readJSON<{ id: string }[]>(request, `/v1/jobs/${job.id}/runs?limit=100`, observer);
  const priorIDs = new Set(earlierRuns.map(run => run.id));
  expect(new Set(finalRuns.filter(run => !priorIDs.has(run.id)).map(run => run.id))).toEqual(new Set([holderRun.id, promoted.run_id]));
  const receipt = await readJSON<{ receipt_digest: string; run_id: string; job_id: string }>(request, `/v1/jobs/${job.id}/runs/${promoted.run_id}/receipt`, observer);
  expect(receipt).toMatchObject({ run_id: promoted.run_id, job_id: job.id });
  await assertLocalEvidence(page, promoted.run_id!, receipt.receipt_digest, watermark);
  const declaration = page.getByTestId("run-dataset-declaration");
  await expect(declaration).toContainText(`work · produces · ${dataset}`);
  await expect(page.getByTestId("run-data-evidence")).toContainText(/declaration/i);
  await capture(page, info, "existing-run-receipt-and-data");
  let datasetEvidence!: { state: { last_run_id: string; watermark: string } };
  await expect.poll(async () => {
    datasetEvidence = await readJSON(request, `/v1/datasets/_/${encodeURIComponent(dataset)}`, observer);
    return datasetEvidence.state;
  }, { timeout: 20_000, intervals: [1000, 2000] }).toMatchObject({ last_run_id: promoted.run_id, watermark });
  const reversePath = `/v1/jobs/${job.id}/runs/${promoted.run_id}/external-executions`;
  const reverse = await readJSON<{ executions: Correlation[] }>(request, reversePath, observer);
  expect(reverse.executions).toHaveLength(1);
  expect(reverse.executions[0]).toMatchObject({ id: queued.id, admission_id: queued.admission_id, execution_id: executionId(workflow), reference: { coordinates: workflow } });
  const back = page.getByTestId("run-external-executions").getByTestId("external-execution-link");
  await expect(back).toHaveAttribute("href", executionPath(workflow));
  await declaration.getByTestId("declared-dataset-link").click();
  await expect(page.getByTestId("dataset-detail-panel")).toContainText(dataset);
  await expect(page.getByTestId("dataset-detail-panel")).toContainText(watermark);
  await capture(page, info, "observed-dataset-watermark");
  await page.goBack();
  await expect(page).toHaveURL(new RegExp(`/jobs/${job.id}/runs/${promoted.run_id}$`));
  await back.click();
  await expect(page.getByTestId("external-run-id")).toHaveText(workflow.run_id);
  await page.getByLabel("Requested relationships page size", { exact: true }).selectOption("100");
  const childRow = page.locator('[data-testid="external-relationship"][data-relationship-type="child"]');
  await expect(childRow).toBeVisible({ timeout: 30_000 });
  const native = await relations(request, workflow, observer);
  const child = native.relationships.find(relation => relation.type === "child")!;
  expect(child.reference.coordinates.workflow_id).toBe(`${workflow.workflow_id}/child`);
  expect(child.reference.coordinates.run_id).not.toBe(workflow.run_id);
  await childRow.getByRole("link").click();
  await expect(page.getByTestId("external-run-id")).toHaveText(child.reference.coordinates.run_id);
  const parentRow = page.locator('[data-testid="external-relationship"][data-relationship-type="parent"]');
  await expect(parentRow).toContainText(workflow.workflow_id);
  await parentRow.getByRole("link").click();
  await expect(page.getByTestId("external-run-id")).toHaveText(workflow.run_id);
  await page.getByLabel("Requested relationships page size", { exact: true }).selectOption("100");

  // Continuation setup uses the shipped HTTP action, independently of the E3
  // form. Only the exact original Run ID receives this explicit approval.
  const approval = await request.post(`/v1${executionPath(workflow)}/actions/approve_publication`, {
    headers: { ...authHeaders(adminKey()), "Idempotency-Key": `console-continuation-${suffix}` },
    data: { binding_name: "publication", binding_version: "1", input: { note: "Prepare a real continuation identity for the relationships browser journey" } },
  });
  expect(approval.ok()).toBe(true);
  await expect.poll(async () => {
    const events = await journal(request);
    return events.filter(event => event.kind === "approval_effect" && event.run_id === workflow.run_id).length;
  }, { timeout: 30_000, intervals: [2000, 3000] }).toBe(1);
  await fixturePost(request, lane, "/signal", { id: workflow.workflow_id, run_id: workflow.run_id, name: "fixture_continue", value: true });
  let continued!: NativeRelation;
  await expect.poll(async () => {
    continued = (await relations(request, workflow, observer)).relationships.find(relation => relation.type === "continuation")!;
    return continued?.reference.coordinates.run_id ?? "";
  }, { timeout: 30_000, intervals: [2000, 3000] }).not.toBe("");
  expect(continued.reference.coordinates.workflow_id).toBe(workflow.workflow_id);
  expect(continued.reference.coordinates.run_id).not.toBe(workflow.run_id);
  const nextWorkflow = continued.reference.coordinates;
  started.splice(started.indexOf(workflow), 1, nextWorkflow);
  const continuationRow = page.locator('[data-testid="external-relationship"][data-relationship-type="continuation"]');
  await expect(continuationRow).toContainText(nextWorkflow.run_id, { timeout: 30_000 });
  await capture(page, info, "child-and-continuation-relationships");
  await continuationRow.getByRole("link").click();
  await expect(page.getByTestId("external-run-id")).toHaveText(nextWorkflow.run_id);
  await expect(page.getByTestId("verified-delegation")).toHaveCount(0);
  const nextRelations = await relations(request, nextWorkflow, observer);
  expect(nextRelations.correlations).toEqual([]);

  const scopedPage = await browser.newPage();
  const externalReads: string[] = [];
  scopedPage.on("request", request => { if (/\/v1\/connectors|\/external-executions$/.test(new URL(request.url()).pathname)) externalReads.push(request.url()); });
  try {
    await loginAtUrl(scopedPage, `/jobs/${job.id}/runs/${promoted.run_id}`, scoped);
    await assertLocalEvidence(scopedPage, promoted.run_id!, receipt.receipt_digest, watermark);
    await expect(scopedPage.getByTestId("run-dataset-declaration")).toContainText(dataset);
    await expect(scopedPage.getByTestId("declared-dataset-link")).toHaveCount(0);
    await expect(scopedPage.getByTestId("run-external-executions")).toHaveCount(0);
    await expect(scopedPage.getByText(workflow.workflow_id, { exact: true })).toHaveCount(0);
    await expect(scopedPage.getByText(workflow.run_id, { exact: true })).toHaveCount(0);
    expect(externalReads).toEqual([]);
    await capture(scopedPage, info, "scoped-local-run-evidence");
    expect((await readJSON<{ executions: Correlation[] }>(request, reversePath, scoped)).executions).toEqual([]);
    const denied = await request.get(`/v1${executionPath(workflow)}`, { headers: authHeaders(scoped) });
    expect(denied.status()).toBe(403);
    expect(await denied.text()).not.toContain(workflow.workflow_id);
  } finally { await scopedPage.close(); }

  const events = (await journal(request)).filter(event => event.workflow_id === workflow.workflow_id || event.run_id === workflow.run_id);
  const attempts = events.filter(event => event.kind === "activity_attempt" && event.activity_id === queued.activity_id);
  expect(attempts.length).toBeGreaterThanOrEqual(2);
  expect(new Set(attempts.map(event => event.idempotency_key))).toEqual(new Set([queued.idempotency_key]));
  expect(events.some(event => event.kind === "http_start" && event.hint?.outcome === "queued" && event.hint.queue_id === queued.queue_id)).toBe(true);
  expect(events.some(event => event.kind === "http_start" && event.hint?.run_id === promoted.run_id)).toBe(true);
  await info.attach("Real admission, run, native identities and independent fixture journal", {
    body: JSON.stringify({ candidate: process.env.CAESIUM_CONNECTORS_CANDIDATE_SHA, workflow, discovered, holder: holderRun, new_runs: finalRuns.filter(run => !priorIDs.has(run.id)), queued, promoted, reverse, child, continued, dataset: datasetEvidence, receipt, events }, null, 2), contentType: "application/json",
  });
});

test("opaque external-only work has no inferred Caesium runs, receipts, lineage or freshness", async ({ page, request }, info) => {
  const reader = await createKey(request, "viewer");
  const workflow = await startExternalWorkflow(request, lane, `opaque-${suffix}`);
  started.push(workflow);
  await discover(request, workflow, "external_only", reader);
  await loginAtUrl(page, executionPath(workflow), reader);
  await expect(page.getByTestId("external-workflow-id")).toHaveText(workflow.workflow_id);
  await expect(page.getByTestId("external-run-id")).toHaveText(workflow.run_id);
  const actual = await relations(request, workflow, reader);
  expect(actual.correlations).toEqual([]);
  expect(actual.relationships).toEqual([]);
  expect(actual.incomplete).toBe(false);
  await expect(page.getByTestId("verified-delegations")).toBeVisible();
  await expect(page.getByTestId("verified-delegation")).toHaveCount(0);
  await expect(page.getByTestId("delegated-run-link")).toHaveCount(0);
  await expect(page.getByTestId("run-data-evidence")).toHaveCount(0);
  await expect(page.getByTestId("receipt-panel")).toHaveCount(0);
  await expect(page.locator("main").getByRole("link", { name: /dataset|lineage|freshness/i })).toHaveCount(0);
  await capture(page, info, "opaque-work-has-no-local-data-evidence");
  await info.attach("Opaque external-only public relationships", { body: JSON.stringify({ workflow, relationships: actual }, null, 2), contentType: "application/json" });
});
