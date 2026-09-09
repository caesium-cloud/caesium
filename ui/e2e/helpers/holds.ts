import { expect, type APIRequestContext } from "@playwright/test";
import type {
  DatasetHold,
  DatasetHoldsResponse,
  JobRun,
} from "../../src/lib/api";

export function holdDefinition(
  alias: string,
  name: string,
  value: number,
  delta = true,
) {
  return {
    apiVersion: "v1",
    kind: "Job",
    metadata: { alias, cache: false },
    trigger: { type: "cron", configuration: { expression: "0 0 31 2 *" } },
    steps: [
      {
        name: "produce",
        image: "alpine:3.23",
        engine: "docker",
        command: [
          "sh",
          "-c",
          `echo '##caesium::metrics ${JSON.stringify({ dataset: name, rowCount: value })}'`,
        ],
        datasets: {
          produces: [
            {
              name,
              assertions: {
                rowCount: delta
                  ? { min: 1, deltaFromBaseline: "50%" }
                  : { min: 50 },
              },
              onViolation: "hold",
              release: "manual",
            },
          ],
        },
      },
    ],
  };
}

export function consumerDefinition(
  alias: string,
  source: string,
  output: string,
) {
  return {
    apiVersion: "v1",
    kind: "Job",
    metadata: { alias, cache: false },
    trigger: { type: "cron", configuration: { expression: "0 0 31 2 *" } },
    steps: [
      {
        name: "consume",
        image: "alpine:3.23",
        engine: "docker",
        command: ["sh", "-c", "echo consumed"],
        datasets: { consumes: [source], produces: [{ name: output }] },
      },
    ],
  };
}

export async function applyHoldJobs(
  request: APIRequestContext,
  definitions: unknown[],
  headers: Record<string, string> = {},
) {
  const response = await request.post("/v1/jobdefs/apply", {
    headers,
    data: { definitions },
  });
  expect(response.ok(), await response.text()).toBeTruthy();
  const jobs = await request.get("/v1/jobs", { headers });
  expect(jobs.ok()).toBeTruthy();
  return (await jobs.json()) as Array<{ id: string; alias: string }>;
}

export async function runHoldJob(
  request: APIRequestContext,
  jobId: string,
  status = "succeeded",
  headers: Record<string, string> = {},
) {
  const before = await request.get(`/v1/jobs/${jobId}/runs`, { headers });
  expect(before.ok()).toBeTruthy();
  const previous = new Set(
    ((await before.json()) as JobRun[]).map((run) => run.id),
  );
  const started = await request.post(`/v1/jobs/${jobId}/run`, { headers });
  expect(started.ok(), await started.text()).toBeTruthy();
  let current: JobRun | undefined;
  await expect
    .poll(
      async () => {
        const response = await request.get(`/v1/jobs/${jobId}/runs`, {
          headers,
        });
        expect(response.ok()).toBeTruthy();
        const list = (await response.json()) as JobRun[];
        current = list.find((run) => !previous.has(run.id));
        return current?.status;
      },
      { timeout: 120_000, intervals: [300, 500, 1_000] },
    )
    .toBe(status);
  const detail = await request.get(`/v1/jobs/${jobId}/runs/${current!.id}`, {
    headers,
  });
  expect(detail.ok()).toBeTruthy();
  return (await detail.json()) as JobRun;
}

export async function activeHold(
  request: APIRequestContext,
  name: string,
  headers: Record<string, string> = {},
): Promise<DatasetHold> {
  let hold: DatasetHold | undefined;
  await expect
    .poll(
      async () => {
        const response = await request.get(
          `/v1/datasets/holds?${new URLSearchParams({ namespace: "", name, status: "active" })}`,
          { headers },
        );
        expect(response.ok(), await response.text()).toBeTruthy();
        const result = (await response.json()) as DatasetHoldsResponse;
        expect(result.total).toBeLessThanOrEqual(1);
        hold = result.holds[0];
        return hold?.name;
      },
      { timeout: 15_000 },
    )
    .toBe(name);
  return hold!;
}

export const holdURL = (hold: DatasetHold) =>
  `/datasets/holds?${new URLSearchParams({ namespace: hold.namespace, name: hold.name, hold: hold.id })}`;
