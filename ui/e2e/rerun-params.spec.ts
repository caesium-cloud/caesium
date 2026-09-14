import { expect, test } from "@playwright/test";
import {
  applyDefinitions,
  awaitRun,
  failOnUnexpectedPageErrors,
  findJobByAlias,
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

function buildParameterizedDefinition(): FixtureDefinition {
  return {
    apiVersion: "v1",
    kind: "Job",
    metadata: { alias: `rerun-params-${uniqueSuffix()}` },
    trigger: {
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
