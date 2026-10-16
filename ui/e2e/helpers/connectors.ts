import { expect, type APIRequestContext } from "@playwright/test";
import { authHeaders, obtainAuthKeys, type AuthLaneKeys } from "./auth";

export interface ConnectorBrowserLane {
  keys: AuthLaneKeys;
  controlURL: string;
  gateOffURL: string;
  connectionId: string;
}

export interface FixtureWorkflow {
  workflow_id: string;
  run_id: string;
}

function required(name: string): string {
  const value = process.env[name]?.trim();
  if (!value) throw new Error(`connectors browser project requires ${name}`);
  return value;
}

/** Selected connector tests fail closed. These are real lane prerequisites. */
export async function requireConnectorBrowserLane(request: APIRequestContext): Promise<ConnectorBrowserLane> {
  for (const marker of ["CAESIUM_CONNECTORS_FIXTURE", "CAESIUM_CONNECTORS_READ_FIXTURE"]) {
    if (required(marker) !== "true") throw new Error(`connectors browser project requires ${marker}=true`);
  }
  required("CAESIUM_E2E_AUTH_ADMIN_KEY");
  const controlURL = required("CAESIUM_CONNECTORS_FIXTURE_CONTROL_URL");
  const gateOffURL = required("CAESIUM_CONNECTORS_GATE_OFF_URL");
  const status = await request.get("/auth/status");
  expect(status.ok(), "authenticated server must expose auth status").toBe(true);
  expect((await status.json()).enabled, "connectors browser lane must enable authentication").toBe(true);
  const keys = await obtainAuthKeys(request);
  const features = await request.get("/v1/system/features", { headers: authHeaders(keys.viewer) });
  expect(features.ok(), "viewer must read real server features").toBe(true);
  expect(await features.json()).toMatchObject({ connectors_enabled: true, connectors_ready: true });
  const health = await request.get(`${controlURL}/health`);
  expect(health.ok(), "real Temporal worker must answer a live Query").toBe(true);
  expect(await health.json()).toMatchObject({ ready: true, sdk: "1.49.0", cli: "1.9.1", server: "1.32.0" });
  const evidence = await request.get(`${controlURL}/evidence`);
  expect(evidence.ok()).toBe(true);
  const events = await evidence.json() as Record<string, unknown>[];
  expect(events.some(event => event.kind === "fixture_started"), "fixture must independently record a real worker start").toBe(true);
  const candidate = required("CAESIUM_CONNECTORS_CANDIDATE_SHA");
  expect(candidate).toMatch(/^[a-f0-9]{40}$/);
  expect(events.some(event => event.kind === "fixture_started" && event.candidate === candidate), "fixture must be bound to the browser candidate").toBe(true);
  return { keys, controlURL, gateOffURL, connectionId: process.env.CAESIUM_CONNECTORS_CONNECTION_ID || "fixture" };
}

export async function fixturePost<T>(request: APIRequestContext, lane: ConnectorBrowserLane, path: string, data: unknown): Promise<T> {
  const response = await request.post(`${lane.controlURL}${path}`, { data });
  expect(response.ok(), `real fixture ${path} must succeed`).toBe(true);
  return await response.json() as T;
}

export async function startExternalWorkflow(request: APIRequestContext, lane: ConnectorBrowserLane, suffix: string): Promise<FixtureWorkflow> {
  const workflow = await fixturePost<FixtureWorkflow>(request, lane, "/start", { id: `console-external-${Date.now()}-${suffix}`, type: "external_only", input: {} });
  expect(workflow.workflow_id).not.toBe("");
  expect(workflow.run_id).toMatch(/^[a-f0-9-]{36}$/);
  return workflow;
}

export async function findExternalExecution(request: APIRequestContext, lane: ConnectorBrowserLane, workflow: FixtureWorkflow): Promise<string> {
  let id = "";
  await expect.poll(async () => {
    const response = await request.get(`/v1/connectors/${encodeURIComponent(lane.connectionId)}/executions?workflow_type=external_only&page_size=100`, { headers: authHeaders(lane.keys.viewer) });
    expect(response.ok()).toBe(true);
    const page = await response.json() as { executions: { id: string; metadata: FixtureWorkflow }[] };
    id = page.executions.find(execution => execution.metadata.workflow_id === workflow.workflow_id && execution.metadata.run_id === workflow.run_id)?.id ?? "";
    return id;
  }, { timeout: 30_000, intervals: [2000, 3000, 5000] }).not.toBe("");
  return id;
}
