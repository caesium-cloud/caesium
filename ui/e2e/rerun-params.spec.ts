import { expect, test } from "@playwright/test";
import {
  applyDefinitions,
  awaitRun,
  failOnUnexpectedPageErrors,
  findJobByAlias,
  type E2ERun,
  type FixtureDefinition,
  triggerJob,
  uniqueSuffix,
} from "./helpers/fixtures";

failOnUnexpectedPageErrors();

const params = { mode: "specific" };

test("Re-run preserves the selected run parameters and task output", async ({ page, request }) => {
  test.slow();

  const definition = buildParameterizedDefinition();
  const alias = String(definition.metadata?.alias);
  await applyDefinitions(request, definition);
  const job = await findJobByAlias(request, alias);
  await triggerJob(request, job.id, params);
  const originalRun = await awaitRun(request, job.id, { status: "succeeded" });
  expect(originalRun.params).toEqual(params);
  expect(originalRun.tasks).toEqual(
    expect.arrayContaining([expect.objectContaining({ output: expect.objectContaining({ mode: params.mode }) })]),
  );

  await page.goto(`/jobs/${job.id}/runs/${originalRun.id}`);
  await expect(page.getByRole("heading", { name: /Run / })).toBeVisible({ timeout: 30_000 });

  const newRunURL = new RegExp(`/jobs/${job.id}/runs/(?!${originalRun.id}$)[^/]+$`);
  const rerunNavigation = page.waitForURL(newRunURL);
  await page.getByRole("button", { name: "Re-run" }).click();
  await rerunNavigation;

  const rerunId = new URL(page.url()).pathname.split("/").at(-1);
  expect(rerunId).toBeTruthy();
  expect(rerunId).not.toBe(originalRun.id);

  const rerun = await awaitRun(request, job.id, { status: "succeeded" });
  expect(rerun.id).toBe(rerunId);
  expect(rerun.params).toEqual(params);
  expect(rerun.tasks).toEqual(
    expect.arrayContaining([expect.objectContaining({ output: expect.objectContaining({ mode: params.mode }) })]),
  );
});

test("Re-run starts a fresh trigger chain without inheriting scheduler depth", async ({ page, request }) => {
  test.slow();

  const upstreamDefinition = buildParameterizedDefinition();
  const upstreamAlias = String(upstreamDefinition.metadata?.alias);
  const definition = buildParameterizedDefinition(upstreamAlias);
  const alias = String(definition.metadata?.alias);
  const downstreamDefinition = buildParameterizedDefinition(alias);
  await applyDefinitions(request, upstreamDefinition, definition, downstreamDefinition);
  const upstream = await findJobByAlias(request, upstreamAlias);
  const job = await findJobByAlias(request, alias);
  const downstream = await findJobByAlias(request, String(downstreamDefinition.metadata?.alias));

  await triggerJob(request, upstream.id, params);
  // Prior history can contain several chains; event delivery is at-least-once.
  // Exercise that history explicitly instead of assuming exactly two C runs.
  await triggerJob(request, upstream.id, params);
  const originalRun = await awaitRun(request, job.id, { status: "succeeded" });
  expect(originalRun.params).toEqual({ ...params, _trigger_depth: "1" });
  const originalDownstream = await awaitRun(request, downstream.id, { status: "succeeded" });
  expect(originalDownstream.params?._trigger_depth).toBe("2");
  const priorResponse = await request.get(`/v1/jobs/${downstream.id}/runs`);
  expect(priorResponse.ok()).toBe(true);
  const priorRuns = await priorResponse.json() as E2ERun[];
  const priorRunIds = new Set(priorRuns.map((run) => run.id));
  expect(priorRunIds.has(originalDownstream.id)).toBe(true);
  expect(priorRuns.every((run) => run.params?._trigger_depth === "2")).toBe(true);

  await page.goto(`/jobs/${job.id}/runs/${originalRun.id}`);
  await expect(page.getByRole("heading", { name: /Run / })).toBeVisible();
  const navigation = page.waitForURL(new RegExp(`/jobs/${job.id}/runs/(?!${originalRun.id}$)[^/]+$`));
  await page.getByRole("button", { name: "Re-run" }).click();
  await navigation;

  const rerunId = new URL(page.url()).pathname.split("/").at(-1);
  const rerunPath = `/v1/jobs/${job.id}/runs/${rerunId}`;
  await expect.poll(async () => {
    const response = await request.get(rerunPath);
    expect(response.ok()).toBe(true);
    return (await response.json() as E2ERun).status;
  }, { timeout: 60_000 }).toBe("succeeded");
  const rerunResponse = await request.get(rerunPath);
  expect(rerunResponse.ok()).toBe(true);
  const rerun = await rerunResponse.json() as E2ERun;
  expect(rerun.id).toBe(rerunId);
  expect(rerun.params).toEqual(params);
  expect(rerun.tasks).toEqual(
    expect.arrayContaining([expect.objectContaining({ output: expect.objectContaining({ mode: params.mode }) })]),
  );

  let freshDownstreamId: string | undefined;
  await expect.poll(async () => {
    const response = await request.get(`/v1/jobs/${downstream.id}/runs`);
    expect(response.ok()).toBe(true);
    const runs = await response.json() as E2ERun[];
    freshDownstreamId = runs.find((run) =>
      !priorRunIds.has(run.id) && run.status === "succeeded" && run.params?._trigger_depth === "1",
    )?.id;
    return freshDownstreamId;
  }, { timeout: 60_000 }).toBeTruthy();
  const freshResponse = await request.get(`/v1/jobs/${downstream.id}/runs/${freshDownstreamId}`);
  expect(freshResponse.ok()).toBe(true);
  const freshDownstream = await freshResponse.json() as E2ERun;
  expect(freshDownstream.id).toBe(freshDownstreamId);
  expect(freshDownstream.id).not.toBe(originalDownstream.id);
  expect(freshDownstream.status).toBe("succeeded");
  expect(freshDownstream.params).toEqual({ ...params, _trigger_depth: "1" });
  expect(freshDownstream.tasks).toEqual(
    expect.arrayContaining([expect.objectContaining({ output: expect.objectContaining({ mode: params.mode }) })]),
  );
});

function buildParameterizedDefinition(upstreamAlias?: string): FixtureDefinition {
  return {
    apiVersion: "v1",
    kind: "Job",
    metadata: { alias: `rerun-params-${uniqueSuffix()}` },
    trigger: upstreamAlias ? {
      type: "event",
      configuration: {
        events: [{ type: "run_completed", source: "caesium", filter: { job_alias: upstreamAlias } }],
        defaultParams: params,
      },
    } : {
      type: "cron",
      configuration: { cron: "0 0 1 1 *", timezone: "UTC" },
    },
    steps: [
      {
        name: "emit-mode",
        engine: "docker",
        image: "alpine:3.23",
        command: ["sh", "-c", 'echo "##caesium::output {\\"mode\\":\\"$CAESIUM_PARAM_MODE\\"}"'],
      },
    ],
  } as FixtureDefinition;
}
