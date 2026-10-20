import { expect, test, type Page, type Response, type TestInfo } from "@playwright/test";
import type { ExternalHistoryPage } from "../../src/features/connectors/types";
import { authHeaders, loginAtUrl } from "../helpers/auth";
import { findExternalExecution, fixturePost, requireConnectorBrowserLane, startExternalWorkflow, type ConnectorBrowserLane, type FixtureWorkflow } from "../helpers/connectors";

let lane: ConnectorBrowserLane;
let workflow: FixtureWorkflow;
let companion: FixtureWorkflow;
let executionId: string;
const detailPath = () => `/connectors/${encodeURIComponent(lane.connectionId)}/executions/${encodeURIComponent(executionId)}`;

function isRequestedHistoryResponse(response: Response, cursor?: string): boolean {
  const url = new URL(response.url());
  return response.request().method() === "GET" && url.pathname === `/v1${detailPath()}/history` &&
    url.searchParams.get("page_size") === "1" &&
    (cursor === undefined ? !url.searchParams.has("cursor") : url.searchParams.get("cursor") === cursor);
}

async function captureObservation(page: Page, testInfo: TestInfo, stage: string) {
  const observation = page.getByRole("region", { name: "Execution observation" });
  await observation.scrollIntoViewIfNeeded();
  await expect(page.getByTestId("execution-availability")).toBeVisible();
  await expect(observation.locator("time")).toBeVisible();
  const path = testInfo.outputPath(`external-observation-${stage}.png`);
  await page.screenshot({ path, fullPage: true });
  await testInfo.attach(`External execution — ${stage}`, { path, contentType: "image/png" });
}

test.beforeAll(async ({ request }) => {
  lane = await requireConnectorBrowserLane(request);
  companion = await startExternalWorkflow(request, lane, "pagination");
  workflow = await startExternalWorkflow(request, lane, "monitoring");
  executionId = await findExternalExecution(request, lane, workflow);
});

test.afterAll(async ({ request }) => {
  if (!lane) return;
  await fixturePost(request, lane, "/fault", { outage: false });
  for (const started of [workflow, companion]) if (started) await fixturePost(request, lane, "/signal", { id: started.workflow_id, run_id: started.run_id, name: "fixture_finish", value: true });
});

test("viewer filters and inspects a real external-only workflow, pages history and recovers dated observations after provider outage", async ({ page, request }, testInfo) => {
  await loginAtUrl(page, "/connectors", lane.keys.viewer);
  await expect(page.getByRole("heading", { name: "Connections", exact: true })).toBeVisible();
  await expect(page.getByRole("navigation").getByRole("link", { name: "Connections", exact: true })).toBeVisible();
  await page.getByLabel("Connection", { exact: true }).selectOption(lane.connectionId);
  await page.getByLabel("Workflow type", { exact: true }).fill("external_only");
  await page.getByRole("button", { name: "Apply filters" }).click();
  await expect(page).toHaveURL(/workflow_type=external_only/);
  await page.getByLabel("Executions per page").selectOption("1");
  await expect(page.getByTestId("external-execution-row")).toHaveCount(1);
  await expect(page.getByRole("button", { name: "Next executions", exact: true })).toBeEnabled();
  await page.getByRole("button", { name: "Next executions", exact: true }).click();
  await expect(page).toHaveURL(/cursor=/);
  await page.getByRole("button", { name: "First executions page" }).click();
  await page.getByLabel("Executions per page").selectOption("100");
  await page.getByRole("link", { name: workflow.workflow_id, exact: true }).click();
  await expect(page.getByTestId("external-workflow-id")).toHaveText(workflow.workflow_id);
  await expect(page.getByTestId("external-run-id")).toHaveText(workflow.run_id);
  await expect(page.getByTestId("external-native-status")).toHaveText("WORKFLOW_EXECUTION_STATUS_RUNNING");
  await expect(page.getByTestId("execution-availability")).toHaveText("Live provider observation");
  await expect(page.getByText(/Workflow inputs\/results and undeclared payloads are unavailable/)).toBeVisible();
  await expect(page.getByText(/Queries are never invoked automatically\./)).toBeVisible();
  await expect(page.getByText(/Actions require explicit submission\./)).toBeVisible();
  await expect(page.getByRole("button", { name: /approve|query|retry|cancel/i })).toHaveCount(0);
  const expectedUIBase = process.env.CAESIUM_CONNECTORS_EXPECTED_UI_URL;
  if (expectedUIBase) {
    const expectedURL = `${expectedUIBase.replace(/\/$/, "")}/namespaces/default/workflows/${encodeURIComponent(workflow.workflow_id)}/${encodeURIComponent(workflow.run_id)}/history`;
    await expect(page.getByRole("link", { name: "Open in Temporal UI ↗" })).toHaveAttribute("href", expectedURL);
  } else {
    await expect(page.getByRole("link", { name: "Open in Temporal UI ↗" })).toHaveCount(0);
  }
  const firstHistoryResponse = page.waitForResponse(response => isRequestedHistoryResponse(response));
  await page.getByLabel("Requested history page size", { exact: true }).selectOption("1");
  const firstResponse = await firstHistoryResponse;
  expect(firstResponse.status()).toBe(200);
  const firstHistory = await firstResponse.json() as ExternalHistoryPage;
  expect(firstHistory).toMatchObject({ workflow_id: workflow.workflow_id, run_id: workflow.run_id });
  const firstEvents = firstHistory.events ?? [];
  expect(firstEvents.length).toBeGreaterThan(0);
  expect(firstEvents[0].event_id).toBe(1);
  expect(firstHistory.cursor).toEqual(expect.any(String));
  expect(firstHistory.cursor).not.toBe("");
  // Temporal pages contain native storage batches. Prove every actual response
  // event is rendered, rather than assuming the requested size is a row count.
  await expect(page.getByTestId("external-history-event")).toHaveText(firstEvents.map(event => `#${event.event_id}${event.event_type}`));
  await expect(page.getByLabel("Requested history page size", { exact: true })).toHaveValue("1");
  await testInfo.attach("History first native page", { body: JSON.stringify(firstHistory, null, 2), contentType: "application/json" });
  const nextHistoryResponse = page.waitForResponse(response => isRequestedHistoryResponse(response, firstHistory.cursor));
  await page.getByRole("button", { name: "Next history", exact: true }).click();
  const nextResponse = await nextHistoryResponse;
  expect(nextResponse.status()).toBe(200);
  const nextHistory = await nextResponse.json() as ExternalHistoryPage;
  expect(nextHistory).toMatchObject({ workflow_id: workflow.workflow_id, run_id: workflow.run_id });
  const nextEvents = nextHistory.events ?? [];
  expect(nextEvents.length).toBeGreaterThan(0);
  const eventIDs = [...firstEvents, ...nextEvents].map(event => event.event_id);
  expect(new Set(eventIDs).size).toBe(eventIDs.length);
  expect(nextEvents[0].event_id).toBe(firstEvents[firstEvents.length - 1].event_id + 1);
  await expect(page.getByTestId("external-history-event")).toHaveText(nextEvents.map(event => `#${event.event_id}${event.event_type}`));
  await testInfo.attach("History next native page", { body: JSON.stringify(nextHistory, null, 2), contentType: "application/json" });
  const observed = await page.getByRole("region", { name: "Execution observation" }).locator("time").getAttribute("datetime");
  expect(Date.parse(observed!)).toBeGreaterThan(0);
  await captureObservation(page, testInfo, "live");
  try {
    await fixturePost(request, lane, "/fault", { outage: true });
    await expect(page.getByTestId("execution-availability")).toContainText("Stale observation", { timeout: 45_000 });
    await expect(page.getByTestId("external-run-id")).toHaveText(workflow.run_id);
    const staleObserved = await page.getByRole("region", { name: "Execution observation" }).locator("time").getAttribute("datetime");
    expect(Date.parse(staleObserved!)).toBeGreaterThan(0);
    // Unchanged direct polling does not rewrite the durable snapshot. Outage
    // fallback can therefore be older than the latest live response.
    expect(Date.parse(staleObserved!)).toBeLessThanOrEqual(Date.parse(observed!));
    await expect(page.getByText(/Cached details describe a previous observation/)).toBeVisible();
    await expect(page.getByText(/History unavailable/)).toBeVisible();
    const evidence = await request.get(`${lane.controlURL}/evidence`);
    const events = await evidence.json() as Record<string, unknown>[];
    expect(events.some(event => event.kind === "fault_armed" && event.outage === true)).toBe(true);
    expect(events.some(event => event.kind === "proxy_interruption")).toBe(true);
    await captureObservation(page, testInfo, "stale");
  } finally {
    await fixturePost(request, lane, "/fault", { outage: false });
  }
  await expect(page.getByTestId("execution-availability")).toHaveText("Live provider observation", { timeout: 45_000 });
  await expect(page.getByTestId("external-run-id")).toHaveText(workflow.run_id);
  await expect(page.getByRole("region", { name: "Execution observation" }).locator("time")).not.toHaveAttribute("datetime", observed!);
  await expect(page.getByText(/History unavailable/)).toHaveCount(0);
  await captureObservation(page, testInfo, "recovered");
});

test("job-scoped and anonymous direct routes disclose no workflow details", async ({ page, request, browser }) => {
  const connectorRequests: string[] = [];
  page.on("request", request => { if (new URL(request.url()).pathname.startsWith("/v1/connectors")) connectorRequests.push(request.url()); });
  await loginAtUrl(page, detailPath(), lane.keys.scoped);
  await expect(page).toHaveURL(/\/jobs$/);
  await expect(page.getByRole("navigation").getByRole("link", { name: "Connections", exact: true })).toHaveCount(0);
  await expect(page.getByTestId("external-workflow-id")).toHaveCount(0);
  await expect(page.getByText(workflow.run_id, { exact: true })).toHaveCount(0);
  expect(connectorRequests).toEqual([]);
  const forbidden = await request.get(`/v1/connectors/${encodeURIComponent(lane.connectionId)}/executions/${encodeURIComponent(executionId)}`, { headers: authHeaders(lane.keys.scoped) });
  expect(forbidden.status()).toBe(403);
  expect(await forbidden.text()).not.toContain(workflow.workflow_id);
  const anonymous = await browser.newPage();
  try {
    await anonymous.goto(new URL(detailPath(), page.url()).href);
    await expect(anonymous.getByPlaceholder("csk_live_...")).toBeVisible();
    await expect(anonymous.getByTestId("external-workflow-id")).toHaveCount(0);
    await expect(anonymous.getByText(workflow.run_id, { exact: true })).toHaveCount(0);
  } finally { await anonymous.close(); }
});

test("gate-off server hides navigation and refuses connector API and direct routes", async ({ browser, request }) => {
  const features = await request.get(`${lane.gateOffURL}/v1/system/features`);
  expect(features.ok()).toBe(true);
  expect((await features.json()).connectors_enabled).toBe(false);
  const absent = await request.get(`${lane.gateOffURL}/v1/connectors`);
  expect(absent.status()).toBe(404);
  const context = await browser.newContext({ baseURL: lane.gateOffURL });
  const page = await context.newPage();
  try {
    await page.goto(detailPath());
    await expect(page).toHaveURL(/\/jobs$/);
    await expect(page.getByRole("navigation").getByRole("link", { name: "Connections", exact: true })).toHaveCount(0);
    await expect(page.getByTestId("external-workflow-id")).toHaveCount(0);
    await expect(page.getByText(workflow.run_id, { exact: true })).toHaveCount(0);
  } finally { await context.close(); }
});
