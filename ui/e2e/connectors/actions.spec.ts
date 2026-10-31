import { Buffer } from "node:buffer";
import { randomUUID } from "node:crypto";
import { expect, test, type APIRequestContext, type Locator, type Page, type Response, type TestInfo } from "@playwright/test";
import { authHeaders, loginAtUrl } from "../helpers/auth";
import { fixturePost, requireConnectorBrowserLane, startExternalWorkflow, type ConnectorBrowserLane, type FixtureWorkflow } from "../helpers/connectors";

interface Job { id: string; alias: string }
interface Actor { principal_kind: string; stable_id: string; subject: string; role: string; operation_id: string; binding_version: string }
interface Receipt {
  id: string; state: string; revision: number; attempt: number; external_update_id: string;
  execution_id: string; execution: { connection_id: string; coordinates: FixtureWorkflow };
  binding_name: string; binding_version: string; actor: Actor; evidence?: unknown; resend_audit?: unknown;
}
interface ActionResponse { operation: Receipt; created: boolean; conflict: boolean; pending: boolean }
interface BrowserSubmission extends ActionResponse { httpStatus: number; requestKey: string }
interface Admission { job_id: string; run_id: string; idempotency_key: string; outcome: string }
interface PublicationState { stage: string; eligible: boolean; approved: boolean; approval_effects: number; actor?: Actor; first?: Admission; second?: Admission }
interface JournalEvent {
  kind: string; workflow_id?: string; run_id?: string; update_id?: string; request_run_id?: string;
  reply_dropped?: boolean; rejected?: boolean; rpc_code?: string; method?: string; actor?: Actor;
  hint?: Admission; [key: string]: unknown;
}

let lane: ConnectorBrowserLane;
const started: FixtureWorkflow[] = [];
const suffix = `${Date.now()}-${process.pid}`;
const adminKey = () => process.env.CAESIUM_E2E_AUTH_ADMIN_KEY!.trim();

// The public stateless reference pins both real coordinates; Visibility lag
// cannot silently retarget an approval to the next run of a Workflow ID.
function executionId(workflow: FixtureWorkflow): string {
  return Buffer.from(JSON.stringify({ ConnectionID: lane.connectionId, Coordinates: { run_id: workflow.run_id, workflow_id: workflow.workflow_id } })).toString("base64url");
}
function detailPath(workflow: FixtureWorkflow): string {
  return `/connectors/${encodeURIComponent(lane.connectionId)}/executions/${encodeURIComponent(executionId(workflow))}`;
}
function operationPath(id: string): string {
  return `/v1/connectors/${encodeURIComponent(lane.connectionId)}/operations/${encodeURIComponent(id)}`;
}
function actionPath(workflow: FixtureWorkflow): string { return `/v1${detailPath(workflow)}/actions/approve_publication`; }
function actionInput(note: string) { return { binding_name: "publication", binding_version: "1", input: { note } }; }
function receiptRow(page: Page, id: string): Locator { return page.locator(`[data-testid="operation-receipt"][data-operation-id="${id}"]`); }
function isResponse(response: Response, method: string, path: string): boolean {
  return response.request().method() === method && new URL(response.url()).pathname === path;
}

async function getJSON<T>(request: APIRequestContext, path: string, key: string): Promise<T> {
  const response = await request.get(path, { headers: authHeaders(key) });
  expect(response.ok(), `real public GET ${path}: HTTP ${response.status()}`).toBe(true);
  return await response.json() as T;
}
async function createKey(request: APIRequestContext, role: "operator" | "viewer" | "runner", jobs?: string[]): Promise<string> {
  const response = await request.post("/v1/auth/keys", {
    headers: authHeaders(adminKey()),
    data: { role, description: `Console real approval ${role} ${suffix}`, ...(jobs ? { scope: { jobs } } : {}) },
  });
  expect(response.ok(), "public API-key creation must succeed").toBe(true);
  const value = await response.json() as { key: string };
  expect(value.key).toMatch(/^csk_/);
  return value.key;
}
async function journal(request: APIRequestContext): Promise<JournalEvent[]> {
  const response = await request.get(`${lane.controlURL}/evidence`);
  expect(response.ok()).toBe(true);
  return await response.json() as JournalEvent[];
}
function effects(events: JournalEvent[], workflow: FixtureWorkflow): JournalEvent[] {
  return events.filter(event => event.kind === "approval_effect" && event.run_id === workflow.run_id);
}
function updates(events: JournalEvent[], receipt: Receipt): JournalEvent[] {
  return events.filter(event => event.kind === "update_rpc" && event.update_id === receipt.external_update_id);
}
async function queryState(request: APIRequestContext, workflow: FixtureWorkflow, key: string): Promise<PublicationState> {
  const startedAt = Date.now();
  const response = await request.post(`/v1${detailPath(workflow)}/queries/publication_status`, {
    headers: authHeaders(key), data: { binding_name: "publication", binding_version: "1" },
  });
  let category = "unrecognized_response";
  if (!response.ok()) {
    try {
      const body = await response.body();
      if (body.byteLength <= 1024) {
        const value: unknown = JSON.parse(body.toString("utf8"));
        if (value && typeof value === "object" && "message" in value && typeof value.message === "string") {
          // Only fixed API messages become diagnostics; never echo arbitrary bodies or credentials.
          category = new Map([
            ["connector unavailable", "connector_unavailable"],
            ["connector read budget exhausted", "connector_budget_exhausted"],
            ["connector binding, request or operation revision conflicts", "connector_conflict"],
            ["invalid connector message body", "invalid_message"],
            ["invalid connector read request", "invalid_reference"],
            ["invalid connector action input", "invalid_input"],
            ["connector metadata budget exceeded", "metadata_budget_exceeded"],
            ["connector operation retention budget exceeded", "retention_budget_exceeded"],
            ["too many failed authentication attempts", "authentication_rate_limited"],
            ["Forbidden", "forbidden"], ["Unauthorized", "unauthorized"], ["Not Found", "not_found"],
          ]).get(value.message) ?? category;
        }
      }
    } catch { /* An unreadable error body must not conceal the failed HTTP response. */ }
  }
  expect(response.ok(), `explicit declared Query must reach the actual worker: HTTP ${response.status()}, category=${category}, elapsed_ms=${Date.now() - startedAt}, workflow_id=${workflow.workflow_id}, run_id=${workflow.run_id}`).toBe(true);
  return (await response.json() as { result: PublicationState }).result;
}
async function startPublication(request: APIRequestContext, name: string, eligible: boolean, jobs?: [Job, Job]): Promise<FixtureWorkflow> {
  const workflow = await fixturePost<FixtureWorkflow>(request, lane, "/start", {
    id: `console-action-${name}-${suffix}`, type: "publication",
    input: { eligible, ...(jobs ? { first_job: jobs[0].id, second_job: jobs[1].id } : { message_only: true }) },
  });
  started.push(workflow);
  // Each setup uses a new viewer and at most six explicit Queries, respecting
  // the production Query budget instead of loosening it for the browser lane.
  const observer = await createKey(request, "viewer");
  for (let attempt = 0; attempt < 6; attempt++) {
    const state = await queryState(request, workflow, observer);
    if (state.stage === "awaiting_approval") return workflow;
    expect(state.stage).toBe("starting");
    await new Promise(resolve => setTimeout(resolve, Math.min(1500 * 2 ** attempt, 10_000)));
  }
  throw new Error("real publication did not reach awaiting_approval within its explicit Query budget");
}
async function submit(page: Page, workflow: FixtureWorkflow, note: string, duplicateClick = false, allowConflict = false): Promise<BrowserSubmission> {
  const panel = page.getByRole("region", { name: "Declared actions", exact: true });
  await expect(panel).toContainText(workflow.run_id);
  await panel.getByLabel("note", { exact: true }).fill(note);
  const response = page.waitForResponse(value => isResponse(value, "POST", actionPath(workflow)));
  const button = panel.getByRole("button", { name: "Approve publication", exact: true });
  if (duplicateClick) await button.dblclick({ delay: 20 });
  else await button.click();
  const value = await response;
  expect(allowConflict ? [200, 201, 202, 409] : [200, 201, 202]).toContain(value.status());
  const body = await value.json() as ActionResponse;
  expect(body.operation.execution).toMatchObject({ connection_id: lane.connectionId, coordinates: workflow });
  expect(body.operation).toMatchObject({ execution_id: executionId(workflow), binding_name: "publication", binding_version: "1", attempt: 1 });
  await expect(receiptRow(page, body.operation.id)).toBeVisible();
  return { ...body, httpStatus: value.status(), requestKey: value.request().headers()["idempotency-key"] ?? "" };
}
async function refreshReceipt(page: Page, id: string): Promise<Receipt> {
  const response = page.waitForResponse(value => isResponse(value, "GET", operationPath(id)));
  await receiptRow(page, id).getByRole("button", { name: "Refresh receipt", exact: true }).click();
  const value = await response;
  expect(value.status()).toBe(200);
  return (await value.json() as ActionResponse).operation;
}
async function expectCompleted(page: Page, receipt: Receipt): Promise<void> {
  await refreshReceipt(page, receipt.id);
  await expect(receiptRow(page, receipt.id)).toHaveAttribute("data-state", "completed", { timeout: 30_000 });
  await expect(receiptRow(page, receipt.id).getByTestId("operation-state")).toHaveText("Completed");
  await expect(receiptRow(page, receipt.id).getByTestId("operation-result")).toContainText('"approved": true');
}
async function capture(page: Page, info: TestInfo, workflow: FixtureWorkflow, stage: string, receipts: Receipt[], request: APIRequestContext): Promise<void> {
  const path = info.outputPath(`approval-${stage}.png`);
  await page.screenshot({ path, fullPage: true });
  await info.attach(stage, { path, contentType: "image/png" });
  const events = (await journal(request)).filter(event => event.run_id === workflow.run_id || event.request_run_id === workflow.run_id || receipts.some(receipt => receipt.external_update_id === event.update_id));
  await info.attach(`${stage} real provider evidence`, { body: JSON.stringify({ candidate_sha: process.env.CAESIUM_CONNECTORS_CANDIDATE_SHA, workflow, receipts, events }, null, 2), contentType: "application/json" });
}
async function expectNoEnabledApproval(page: Page): Promise<void> {
  await expect(page.getByRole("button", { name: "Approve publication", exact: true }).and(page.locator(":enabled"))).toHaveCount(0);
}

test.beforeAll(async ({ request }) => {
  lane = await requireConnectorBrowserLane(request);
  expect(process.env.CAESIUM_CONNECTORS_ACTIONS_FIXTURE, "action browser scenarios require the selected real action fixture").toBe("true");
});
test.afterEach(async ({ request }) => { if (lane) await fixturePost(request, lane, "/fault", {}); });
test.afterAll(async ({ request }) => {
  if (!lane) return;
  for (const workflow of started) await fixturePost(request, lane, "/signal", { id: workflow.workflow_id, run_id: workflow.run_id, name: "fixture_finish", value: true });
  await fixturePost(request, lane, "/configure", { runner_key: lane.keys.runner });
});

test("real Console approval survives duplicate clicks, advances the second Caesium job and has one effect across terminal fresh keys", async ({ page, request }, info) => {
  test.slow();
  const operator = await createKey(request, "operator");
  const other = await createKey(request, "operator");
  const observer = await createKey(request, "viewer");
  const definitions = ["connector-fixture-publish", "connector-fixture-notify"].map(alias => ({
    apiVersion: "v1", kind: "Job", metadata: { alias, cache: false },
    trigger: { type: "cron", configuration: { cron: "0 0 1 1 *" } },
    steps: [{ name: "work", image: "alpine:3.23", command: ["sh", "-c", "echo console-approved-publication"] }],
  }));
  const applied = await request.post("/v1/jobdefs/apply", { headers: authHeaders(adminKey()), data: { definitions } });
  expect(applied.ok()).toBe(true);
  const allJobs = await getJSON<Job[]>(request, "/v1/jobs", adminKey());
  const jobs = definitions.map(definition => allJobs.find(job => job.alias === definition.metadata.alias)) as [Job, Job];
  expect(jobs.every(Boolean)).toBe(true);
  const runner = await createKey(request, "runner", jobs.map(job => job.alias));
  await fixturePost(request, lane, "/configure", { runner_key: runner });
  const workflow = await startPublication(request, "real-jobs", true, jobs);
  const initial = await queryState(request, workflow, observer);
  expect(initial.first?.job_id).toBe(jobs[0].id);
  expect(initial.first?.run_id).toBeTruthy();
  expect(initial.second).toBeUndefined();
  const sends: string[] = [];
  page.on("request", value => { if (value.method() === "POST" && new URL(value.url()).pathname === actionPath(workflow)) sends.push(value.headers()["idempotency-key"] ?? ""); });
  await loginAtUrl(page, detailPath(workflow), operator);
  const approved = await submit(page, workflow, "Approve the actual publication and notify job", true);
  await expectCompleted(page, approved.operation);
  expect(sends).toHaveLength(1);
  expect(sends[0]).not.toBe("");
  let second!: JournalEvent;
  await expect.poll(async () => {
    second = (await journal(request)).find(event => event.kind === "http_start" && event.workflow_id === workflow.workflow_id && event.hint?.job_id === jobs[1].id)!;
    return second?.hint?.run_id ?? "";
  }, { timeout: 45_000, intervals: [1500, 3000] }).not.toBe("");
  const runId = second.hint!.run_id;
  expect(runId).not.toBe(initial.first!.run_id);
  await expect.poll(async () => (await getJSON<{ status: string }>(request, `/v1/jobs/${jobs[1].id}/runs/${runId}`, observer)).status,
    { timeout: 45_000, intervals: [1500, 3000] }).toBe("succeeded");
  // The run can succeed before Temporal records its Activity result. This
  // observer used one Query above, leaving five within the six/minute budget.
  let state!: PublicationState;
  for (let attempt = 0; attempt < 5; attempt++) {
    state = await queryState(request, workflow, observer);
    expect(state).toMatchObject({ approved: true, approval_effects: 1, actor: approved.operation.actor });
    if (state.stage === "published") break;
    expect(state.stage, "approved publication must await its Activity result or be published").toBe("awaiting_approval");
    expect(state.second).toBeUndefined();
    if (attempt < 4) await new Promise(resolve => setTimeout(resolve, Math.min(1500 * 2 ** attempt, 10_000)));
  }
  expect(state).toMatchObject({ stage: "published", approved: true, approval_effects: 1, second: { job_id: jobs[1].id, run_id: runId }, actor: approved.operation.actor });
  expect(effects(await journal(request), workflow)).toHaveLength(1);
  // A fresh browser principal generates a fresh key and a distinct Update ID
  // after terminal receipt completion. The real handler must still apply once.
  await loginAtUrl(page, detailPath(workflow), other);
  const terminal = await submit(page, workflow, "A fresh terminal key must not publish twice");
  expect(terminal.operation.id).not.toBe(approved.operation.id);
  expect(terminal.operation.external_update_id).not.toBe(approved.operation.external_update_id);
  await expectCompleted(page, terminal.operation);
  expect(effects(await journal(request), workflow)).toHaveLength(1);
  expect((await journal(request)).filter(event => event.kind === "http_start" && event.workflow_id === workflow.workflow_id && event.hint?.job_id === jobs[1].id)).toHaveLength(1);
  await capture(page, info, workflow, "completed-real-jobs", [approved.operation, terminal.operation], request);
});

test("lost accepted reply recovers the server receipt after reload with empty browser storage without another Update", async ({ page, request }, info) => {
  const operator = await createKey(request, "operator");
  const workflow = await startPublication(request, "lost-accepted", true);
  await fixturePost(request, lane, "/fault", { workflow_id: workflow.workflow_id, run_id: workflow.run_id, mode: "accepted" });
  await loginAtUrl(page, detailPath(workflow), operator);
  const lost = await submit(page, workflow, "Lose the real accepted response");
  expect(lost.operation.state).toBe("unknown");
  const delivered = updates(await journal(request), lost.operation);
  expect(delivered).toHaveLength(1);
  expect(delivered[0]).toMatchObject({ reply_dropped: true, rejected: false, rpc_code: "OK", request_run_id: workflow.run_id });
  await fixturePost(request, lane, "/fault", {});
  // Authentication is intentionally memory-only. Clearing both browser stores
  // and signing in after reload also loses every client-generated request key.
  await page.evaluate(() => { localStorage.clear(); sessionStorage.clear(); });
  await page.reload();
  await expect(page.getByPlaceholder("csk_live_...")).toBeVisible();
  await loginAtUrl(page, detailPath(workflow), operator);
  await expect(receiptRow(page, lost.operation.id)).toBeVisible();
  await expectCompleted(page, lost.operation);
  const stored = await page.evaluate(() => [...Object.values(localStorage), ...Object.values(sessionStorage)]);
  expect(stored.join("\n")).not.toContain(lost.operation.external_update_id);
  expect(stored.join("\n")).not.toContain(lost.operation.id);
  expect(updates(await journal(request), lost.operation)).toHaveLength(1);
  await expect.poll(async () => effects(await journal(request), workflow).length, { timeout: 15_000 }).toBe(1);
  await capture(page, info, workflow, "lost-accepted-recovered", [lost.operation], request);
});

test("lost rejected reply stays unknown across two operators and eligibility changes until the original principal confirms the displayed revision", async ({ page, request, browser }, info) => {
  test.slow();
  const firstPrincipal = await createKey(request, "operator");
  const secondPrincipal = await createKey(request, "operator");
  const observer = await createKey(request, "viewer");
  const workflow = await startPublication(request, "lost-rejected", false);
  await fixturePost(request, lane, "/fault", { workflow_id: workflow.workflow_id, run_id: workflow.run_id, mode: "rejected" });
  const secondPage = await browser.newPage();
  try {
    // Both tabs observe an empty server list before either operator clicks.
    // Competing real form submissions exercise the UI's 409 recovery path;
    // sequentially opening the second tab would only exercise GET recovery.
    await Promise.all([loginAtUrl(page, detailPath(workflow), firstPrincipal), loginAtUrl(secondPage, detailPath(workflow), secondPrincipal)]);
    for (const operatorPage of [page, secondPage]) {
      await expect(operatorPage.getByRole("button", { name: "Approve publication", exact: true })).toBeEnabled();
      await expect(operatorPage.getByTestId("operation-receipt")).toHaveCount(0);
    }
    const submissions = await Promise.all([
      submit(page, workflow, "First operator explicitly submits an approval", false, true),
      submit(secondPage, workflow, "Second operator explicitly submits a fresh request", false, true),
    ]);
    expect(submissions.filter(value => value.conflict)).toHaveLength(1);
    const winnerIndex = submissions[0].conflict ? 1 : 0;
    const lost = submissions[winnerIndex];
    const conflict = submissions[1 - winnerIndex];
    const original = winnerIndex === 0 ? firstPrincipal : secondPrincipal;
    const other = winnerIndex === 0 ? secondPrincipal : firstPrincipal;
    const originalPage = winnerIndex === 0 ? page : secondPage;
    const otherPage = winnerIndex === 0 ? secondPage : page;
    expect(conflict.httpStatus).toBe(409);
    expect(conflict.operation).toMatchObject({ id: lost.operation.id, external_update_id: lost.operation.external_update_id });
    expect(lost.requestKey).not.toBe("");
    expect(conflict.requestKey).not.toBe("");
    expect(conflict.requestKey).not.toBe(lost.requestKey);
    expect(lost.operation.state).toBe("unknown");
    await expect(receiptRow(originalPage, lost.operation.id).getByTestId("operation-state")).toHaveText("Unknown — outcome unresolved");
    expect(updates(await journal(request), lost.operation)).toEqual([expect.objectContaining({ reply_dropped: true, rejected: true, rpc_code: "OK" })]);
    expect(effects(await journal(request), workflow)).toHaveLength(0);
    await fixturePost(request, lane, "/fault", {});
    let current = await refreshReceipt(originalPage, lost.operation.id);
    expect(current.state).toBe("unknown");
    expect(JSON.stringify(current.evidence)).toContain("NotFound");
    await expect(receiptRow(otherPage, lost.operation.id)).toBeVisible();
    await expectNoEnabledApproval(otherPage);
    await expect(receiptRow(otherPage, lost.operation.id).getByRole("button", { name: "Review resend", exact: true })).toHaveCount(0);
    await capture(otherPage, info, workflow, "second-operator-ui-conflict", [lost.operation, conflict.operation], request);
    const denied = await request.post(`${operationPath(lost.operation.id)}/resubmit`, { headers: authHeaders(other), data: { confirmed: true, revision: current.revision } });
    expect(denied.status()).toBe(403);
    await fixturePost(request, lane, "/signal", { id: workflow.workflow_id, run_id: workflow.run_id, name: "fixture_eligibility", value: true });
    await expect.poll(async () => (await queryState(request, workflow, observer)).eligible,
      { timeout: 5000, intervals: [1000, 1500] }).toBe(true);
    expect(await queryState(request, workflow, observer)).toMatchObject({ approved: false, approval_effects: 0 });
    // Observe two real background lookup cycles. Passing time or changed
    // eligibility is never permission to send a new Update.
    for (let cycle = 0; cycle < 2; cycle++) {
      const lookup = await originalPage.waitForResponse(value => isResponse(value, "GET", operationPath(lost.operation.id)), { timeout: 25_000 });
      expect(lookup.status()).toBe(200);
      expect((await lookup.json() as ActionResponse).operation.state).toBe("unknown");
      expect(updates(await journal(request), lost.operation)).toHaveLength(1);
      expect(effects(await journal(request), workflow)).toHaveLength(0);
    }
    // The competing tab's legitimate lookups advance the durable revision.
    // Close it before reviewing the revision the original actor will confirm.
    await otherPage.close();
    current = await refreshReceipt(originalPage, lost.operation.id);
    const stale = await request.post(`${operationPath(lost.operation.id)}/resubmit`, { headers: authHeaders(original), data: { confirmed: true, revision: lost.operation.revision } });
    expect(stale.status()).toBe(409);
    expect(updates(await journal(request), lost.operation)).toHaveLength(1);
    await capture(originalPage, info, workflow, "unknown-before-explicit-review", [lost.operation, current], request);
    const review = originalPage.waitForResponse(value => isResponse(value, "GET", operationPath(lost.operation.id)));
    await receiptRow(originalPage, lost.operation.id).getByRole("button", { name: "Review resend", exact: true }).click();
    const revision = (await (await review).json() as ActionResponse).operation.revision;
    const confirmation = receiptRow(originalPage, lost.operation.id).getByRole("checkbox", { name: `I confirm resending operation ${lost.operation.id} at revision ${revision} to its original run.`, exact: true });
    await expect(confirmation).toBeVisible();
    const confirm = receiptRow(originalPage, lost.operation.id).getByRole("button", { name: "Confirm resend", exact: true });
    await expect(confirm).toBeDisabled();
    await confirmation.check();
    const resent = originalPage.waitForResponse(value => isResponse(value, "POST", `${operationPath(lost.operation.id)}/resubmit`));
    await confirm.click();
    const response = await resent;
    expect([200, 202]).toContain(response.status());
    expect(response.request().postDataJSON()).toEqual({ confirmed: true, revision });
    const confirmed = (await response.json() as ActionResponse).operation;
    expect(confirmed).toMatchObject({ id: lost.operation.id, external_update_id: lost.operation.external_update_id, attempt: 2, actor: lost.operation.actor, execution: lost.operation.execution });
    await expectCompleted(originalPage, confirmed);
    await expect.poll(async () => effects(await journal(request), workflow).length, { timeout: 15_000 }).toBe(1);
    expect(updates(await journal(request), lost.operation)).toHaveLength(2);
    expect(effects(await journal(request), workflow)[0].actor).toEqual(lost.operation.actor);
    await capture(originalPage, info, workflow, "confirmed-same-update", [confirmed], request);
  } finally { if (!secondPage.isClosed()) await secondPage.close(); }
});

test("a real validator failure with unknown origin is visibly unresolved and never presented as a business rejection", async ({ page, request }, info) => {
  const operator = await createKey(request, "operator");
  const workflow = await startPublication(request, "observed-validator-failure", false);
  await loginAtUrl(page, detailPath(workflow), operator);
  const response = await submit(page, workflow, "The pinned validator refuses an ineligible workflow");
  expect(response.operation.state).toBe("unknown");
  expect(JSON.stringify(response.operation.evidence)).toContain("failure_origin_unknown");
  await expect(receiptRow(page, response.operation.id).getByTestId("operation-state")).toHaveText("Unknown — outcome unresolved");
  await expect(receiptRow(page, response.operation.id)).toContainText("failure_origin_unknown");
  await expect(receiptRow(page, response.operation.id)).toContainText("The provider returned a failure whose origin is unknown. This does not establish validator rejection.");
  expect(effects(await journal(request), workflow)).toHaveLength(0);
  expect(updates(await journal(request), response.operation)).toHaveLength(1);
  await capture(page, info, workflow, "validator-origin-unknown", [response.operation], request);
});

test("viewer, runner and scoped principals cannot approve; unbound and stale observations expose no usable approval", async ({ page, request, browser }, info) => {
  test.slow();
  const operator = await createKey(request, "operator");
  const viewer = await createKey(request, "viewer");
  const runner = await createKey(request, "runner");
  const scoped = await createKey(request, "operator", ["connector-fixture-publish"]);
  const workflow = await startPublication(request, "denied-and-stale", true);
  for (const key of [viewer, runner, scoped]) {
    const deniedPage = await browser.newPage();
    try {
      await loginAtUrl(deniedPage, detailPath(workflow), key);
      if (key === scoped) await expect(deniedPage).toHaveURL(/\/jobs$/);
      else await expect(deniedPage.getByTestId("external-run-id")).toHaveText(workflow.run_id);
      await expectNoEnabledApproval(deniedPage);
      const denied = await request.post(actionPath(workflow), { headers: { ...authHeaders(key), "Idempotency-Key": randomUUID() }, data: actionInput("This role or job scope must not dispatch") });
      expect(denied.status()).toBe(403);
    } finally { await deniedPage.close(); }
  }
  expect((await getJSON<{ operations: Receipt[] }>(request, `/v1${detailPath(workflow)}/operations`, viewer)).operations).toEqual([]);
  const unbound = await startExternalWorkflow(request, lane, "unbound-action");
  started.push(unbound);
  await loginAtUrl(page, detailPath(unbound), operator);
  await expect(page.getByTestId("external-run-id")).toHaveText(unbound.run_id);
  await expectNoEnabledApproval(page);
  await loginAtUrl(page, detailPath(workflow), operator);
  await expect(page.getByRole("button", { name: "Approve publication", exact: true })).toBeEnabled();
  await fixturePost(request, lane, "/fault", { outage: true });
  await expect(page.getByTestId("execution-availability")).toContainText("Stale observation", { timeout: 45_000 });
  await expectNoEnabledApproval(page);
  const events = await journal(request);
  expect(events.some(event => event.kind === "proxy_interruption")).toBe(true);
  expect(events.filter(event => event.kind === "update_rpc" && event.request_run_id === workflow.run_id)).toHaveLength(0);
  expect(effects(events, workflow)).toHaveLength(0);
  await capture(page, info, workflow, "stale-approval-disabled", [], request);
  await fixturePost(request, lane, "/fault", {});
  await expect(page.getByTestId("execution-availability")).toHaveText("Live provider observation", { timeout: 45_000 });
  await expect(page.getByRole("button", { name: "Approve publication", exact: true })).toBeEnabled();
});
