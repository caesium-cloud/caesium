import fs from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { expect, test, type APIRequestContext } from "@playwright/test";
import { parseAllDocuments } from "yaml";

/**
 * Console v2 bug-sweep fixture index:
 * - docs/examples/callback-failure.job.yaml backs D3 failed-callback rendering.
 * - docs/examples/run-history.job.yaml backs A1 cron next-fire via cron-success-fast
 *   and B4/C1 failed-run history/DAG counts via cron-failure-fast.
 * - docs/examples/event-trigger.job.yaml backs A2 event-trigger rendering.
 * - docs/examples/branching.job.yaml and fanout-join.job.yaml back multi-step DAG views.
 */

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);
const manualTriggerAPIKey = process.env.CAESIUM_MANUAL_TRIGGER_API_KEY?.trim() || "e2e-test-key";
const terminalRunStatuses = new Set(["succeeded", "failed", "cancelled"]);
const defaultRunTimeoutMs = 180_000;
const pollIntervalMs = 1_000;

export type FixtureDefinition = {
  metadata?: Record<string, unknown> & { alias?: string };
  trigger?: {
    configuration?: Record<string, unknown> & { path?: string };
  };
};

export type E2EJob = {
  id: string;
  alias: string;
};

type E2EJobWithTrigger = E2EJob & {
  trigger_id?: string;
  trigger?: {
    id: string;
    type: string;
  };
};

export type E2ECallbackRun = {
  id: string;
  callback_id: string;
  status: string;
  error?: string;
  started_at: string;
  completed_at?: string;
} & Record<string, unknown>;

export type E2ETaskRun = {
  id: string;
  job_run_id: string;
  task_id: string;
  atom_id?: string;
  engine?: string;
  image?: string;
  command?: string[];
  runtime_id?: string;
  status: string;
  result?: string;
  error?: string;
  output?: Record<string, string>;
  cache_hit?: boolean;
  cache_origin_run_id?: string;
  started_at?: string;
  completed_at?: string;
  created_at?: string;
  updated_at?: string;
} & Record<string, unknown>;

export type E2ERun = {
  id: string;
  job_id: string;
  job_alias?: string;
  backfill_id?: string;
  trigger_type?: string;
  trigger_alias?: string;
  status: string;
  priority?: number;
  params?: Record<string, string>;
  quarantine?: boolean;
  started_at: string;
  completed_at?: string;
  created_at: string;
  updated_at: string;
  error?: string;
  tasks: E2ETaskRun[];
  callbacks: E2ECallbackRun[];
  cache_hits?: number;
  executed_tasks?: number;
  total_tasks?: number;
} & Record<string, unknown>;

export type TriggerJobOptions = {
  params?: Record<string, string>;
  priority?: "low" | "normal" | "high";
};

export type AwaitRunOptions = {
  status?: string | string[];
  timeoutMs?: number;
};

export type ApplyAndRunOptions = TriggerJobOptions &
  AwaitRunOptions & {
    definitionIndex?: number;
  };

/**
 * Load a job-definition YAML from docs/examples/, mutating the alias (and any
 * HTTP webhook path) so each test invocation gets a unique resource and tests
 * can run repeatedly without colliding with prior state.
 */
export async function loadFixtureDefinition(filename: string): Promise<FixtureDefinition> {
  const defs = await loadFixtureDefinitions(filename);
  const first = defs[0];
  if (!first) {
    throw new Error(`fixture did not contain any definitions: ${filename}`);
  }
  return first;
}

export async function loadFixtureDefinitions(filename: string): Promise<FixtureDefinition[]> {
  const fixturePath = path.resolve(__dirname, "../../../docs/examples", filename);
  const yaml = await fs.readFile(fixturePath, "utf8");
  const docs = parseAllDocuments(yaml);
  const suffix = crypto.randomUUID().split("-")[0];

  return docs.map((doc, index) => {
    const raw = doc.toJS();
    if (!raw || typeof raw !== "object" || Array.isArray(raw)) {
      throw new Error(`failed to parse fixture ${filename} document ${index}`);
    }

    const def = raw as FixtureDefinition;
    const baseAlias = String(def.metadata?.alias ?? path.basename(filename, ".job.yaml"));
    def.metadata = { ...(def.metadata ?? {}), alias: `${baseAlias}-${suffix}` };

    if (def.trigger?.configuration && typeof def.trigger.configuration.path === "string") {
      def.trigger.configuration.path = `/hooks/demo/${baseAlias}-${suffix}`;
    }

    return def;
  });
}

/** Short random suffix for building collision-free aliases/paths outside loadFixtureDefinitions. */
export function uniqueSuffix(): string {
  return crypto.randomUUID().split("-")[0];
}

/**
 * Chrome auto-logs a console error for every non-2xx HTTP response
 * ("Failed to load resource: the server responded with a status of ...").
 * That fires for every intentional SYNTHETIC 401/403/etc. response-mock test
 * in this directory, and for ordinary in-product error paths (e.g. a fanned
 * task's log viewer probing without a selected instance yet, which the
 * backend answers 400 by design — see LogViewer.tsx/taskLogsURL). It is
 * expected browser-generated noise mirroring the network tab, not a JS
 * exception — the real regression signal is a genuine console.error CALL
 * from application code or an uncaught pageerror, neither of which this
 * pattern matches. This exception applies globally, in every spec: an
 * HTTP-status resource failure is never a surprise regardless of which test
 * produces it.
 */
const HTTP_STATUS_RESOURCE_LOAD_ERROR = /^Failed to load resource: the server responded with a status of \d+/;

/**
 * Chrome also auto-logs a network-level failure ("Failed to load resource:
 * net::ERR_...", e.g. ERR_INTERNET_DISCONNECTED / ERR_NETWORK_CHANGED) for a
 * request that never got an HTTP response at all — this fires while
 * network-recovery.spec.ts's real `context.setOffline(true)` case is
 * offline. UNLIKE the HTTP-status case above, this is deliberately NOT
 * allowlisted globally: a genuine net::ERR_FAILED/net::ERR_CONNECTION_REFUSED
 * for an application request while the browser is expected to be online is
 * exactly the regression this guard exists to catch. Only spec files that
 * opt in via `failOnUnexpectedPageErrors({ allowNetworkLevelErrors: true })`
 * get a pass, and today that's only network-recovery.spec.ts — every other
 * spec keeps the guard strict.
 *
 * The opt-in is FILE-wide, not scoped to the one test that calls
 * `context.setOffline()`: CI (Linux/Chromium) was observed reporting
 * net::ERR_NETWORK_CHANGED — not always net::ERR_INTERNET_DISCONNECTED —
 * for the same induced cut, and NOT confined to that test's own
 * `setOffline(true)`/`setOffline(false)` interval or even to that single
 * test; it also surfaced against later, unrelated tests further down the
 * same file (real OS/CI-level network-interface noise from the cut,
 * observed bleeding across the shared browser process rather than staying
 * scoped to the originating browser context). A per-test/per-window
 * allowance could not reliably contain it, so the tolerance is file-scoped
 * instead — still a large narrowing from "every spec" (the actual review
 * finding) down to the one file that does real network manipulation.
 */
const NETWORK_LEVEL_RESOURCE_LOAD_ERROR = /^Failed to load resource: net::ERR_/;

/**
 * Registers a `beforeEach`/`afterEach` pair for the CALLING spec file that
 * fails a test if the page logged a `console.error` or threw an uncaught
 * `pageerror` during it. Call once at module scope (not inside a test body)
 * near the top of a spec file — Playwright collects hooks by when they run
 * during the file's synchronous load, not by which module the `test.*` call
 * physically lives in.
 *
 * `allowNetworkLevelErrors` additionally tolerates a browser-level
 * NETWORK_LEVEL_RESOURCE_LOAD_ERROR (see above) for every test in this
 * file — opt in only for a spec that deliberately induces a real network
 * failure (e.g. via `context.setOffline()`); HTTP-status resource failures
 * and genuine console.error/pageerror signals are still caught regardless.
 */
export function failOnUnexpectedPageErrors(options?: { allowNetworkLevelErrors?: boolean }): void {
  let errors: string[] = [];
  const allowNetworkLevelErrors = options?.allowNetworkLevelErrors ?? false;

  test.beforeEach(async ({ page }) => {
    errors = [];
    page.on("console", (msg) => {
      if (msg.type() !== "error") return;
      const text = msg.text();
      if (HTTP_STATUS_RESOURCE_LOAD_ERROR.test(text)) return;
      if (allowNetworkLevelErrors && NETWORK_LEVEL_RESOURCE_LOAD_ERROR.test(text)) return;
      errors.push(`console.error: ${text}`);
    });
    page.on("pageerror", (err) => {
      errors.push(`pageerror: ${err.stack ?? err.message}`);
    });
  });

  test.afterEach(() => {
    expect(errors, `unexpected browser console/page errors:\n${errors.join("\n")}`).toEqual([]);
  });
}

/**
 * A real, live-executed DAG with one root step fanning out into `width`
 * independent parallel leaf steps that all join into a single final step.
 * Every step is a fast `alpine:3.23` no-op so the whole run completes quickly
 * even at moderate width. Used by scale coverage to exercise a wide DAG against
 * the real backend (not a synthetic/mocked one) before any synthetic
 * partition-count coverage layers on top of it.
 */
export function buildFanDefinition(alias: string, width: number): FixtureDefinition {
  const leaves = Array.from({ length: width }, (_, i) => `leaf-${i}`);
  return {
    apiVersion: "v1",
    kind: "Job",
    metadata: { alias },
    trigger: {
      type: "cron",
      configuration: { cron: "0 0 1 1 *" },
    },
    steps: [
      {
        name: "root",
        engine: "docker",
        image: "alpine:3.23",
        command: ["sh", "-c", "true"],
        next: leaves,
      },
      ...leaves.map((name) => ({
        name,
        engine: "docker",
        image: "alpine:3.23",
        command: ["sh", "-c", "true"],
        dependsOn: ["root"],
        next: ["join"],
      })),
      {
        name: "join",
        engine: "docker",
        image: "alpine:3.23",
        command: ["sh", "-c", "true"],
        dependsOn: leaves,
      },
    ],
  } as unknown as FixtureDefinition;
}

export async function applyDefinitions(request: APIRequestContext, ...defs: FixtureDefinition[]): Promise<void> {
  const response = await request.post("/v1/jobdefs/apply", {
    data: { definitions: defs },
  });
  if (!response.ok()) {
    throw new Error(`failed to apply fixture: ${response.status()} ${await response.text()}`);
  }
}

export async function findJobByAlias(request: APIRequestContext, alias: string): Promise<E2EJob> {
  const job = await findJobWithTriggerByAlias(request, alias);
  return { id: job.id, alias: job.alias };
}

export async function triggerJob(
  request: APIRequestContext,
  jobId: string,
  params?: Record<string, string> | TriggerJobOptions,
): Promise<void> {
  const options = normalizeTriggerJobOptions(params);
  const job = await getJob(request, jobId);

  if (job.trigger?.type === "http") {
    if (!job.trigger_id) {
      throw new Error(`job ${jobId} did not include trigger_id`);
    }
    await fireHTTPTrigger(request, jobId, job.trigger_id, options);
    return;
  }

  await startJobRun(request, jobId, options);
}

export async function awaitRun(
  request: APIRequestContext,
  jobId: string,
  opts: AwaitRunOptions = {},
): Promise<E2ERun> {
  const expectedStatuses = normalizeExpectedStatuses(opts.status);
  const deadline = Date.now() + (opts.timeoutMs ?? defaultRunTimeoutMs);
  let lastRun: E2ERun | undefined;
  let lastStatus = "no runs";

  while (Date.now() <= deadline) {
    const runs = await listRuns(request, jobId);
    lastRun = latestRun(runs);
    if (lastRun) {
      lastStatus = lastRun.status;
      if (expectedStatuses.has(lastRun.status) || (!opts.status && terminalRunStatuses.has(lastRun.status))) {
        return getRun(request, jobId, lastRun.id);
      }
    }

    await delay(pollIntervalMs);
  }

  const wanted = opts.status ? [...expectedStatuses].join(", ") : "terminal status";
  throw new Error(`timed out waiting for job ${jobId} latest run to reach ${wanted}; last status: ${lastStatus}`);
}

export async function applyAndRun(
  request: APIRequestContext,
  filename: string,
  opts: ApplyAndRunOptions = {},
): Promise<{ job: E2EJob; run: E2ERun }> {
  const defs = await loadFixtureDefinitions(filename);
  if (defs.length === 0) {
    throw new Error(`fixture did not contain any definitions: ${filename}`);
  }

  const index = opts.definitionIndex ?? 0;
  const def = defs[index];
  if (!def) {
    throw new Error(`fixture ${filename} does not include document ${index}`);
  }
  const alias = String(def.metadata?.alias ?? "");
  if (!alias) {
    throw new Error(`fixture ${filename} document ${index} did not include an alias`);
  }

  await applyDefinitions(request, ...defs);
  const job = await findJobByAlias(request, alias);
  await triggerJob(request, job.id, { params: opts.params, priority: opts.priority });
  const run = await awaitRun(request, job.id, opts);

  return { job, run };
}

async function fireHTTPTrigger(
  request: APIRequestContext,
  jobId: string,
  triggerId: string,
  options: TriggerJobOptions,
): Promise<void> {
  const response = await request.post(`/v1/triggers/${triggerId}/fire`, {
    headers: {
      "X-Caesium-API-Key": manualTriggerAPIKey,
    },
    ...requestBody(options),
  });
  if (!response.ok()) {
    throw new Error(`failed to manually fire trigger for job ${jobId}: ${response.status()} ${await response.text()}`);
  }
}

async function startJobRun(
  request: APIRequestContext,
  jobId: string,
  options: TriggerJobOptions,
): Promise<void> {
  const response = await request.post(`/v1/jobs/${jobId}/run`, requestBody(options));
  if (!response.ok()) {
    throw new Error(`failed to trigger run for job ${jobId}: ${response.status()} ${await response.text()}`);
  }
}

async function findJobWithTriggerByAlias(request: APIRequestContext, alias: string): Promise<E2EJobWithTrigger> {
  const response = await request.get("/v1/jobs");
  if (!response.ok()) {
    throw new Error(`failed to list jobs: ${response.status()} ${await response.text()}`);
  }

  const jobs = (await response.json()) as E2EJobWithTrigger[];
  const job = jobs.find((candidate) => candidate.alias === alias);
  if (!job) {
    throw new Error(`job not found after apply: ${alias}`);
  }
  return job;
}

async function getJob(request: APIRequestContext, jobId: string): Promise<E2EJobWithTrigger> {
  const response = await request.get(`/v1/jobs/${jobId}`);
  if (!response.ok()) {
    throw new Error(`failed to load job ${jobId}: ${response.status()} ${await response.text()}`);
  }
  return (await response.json()) as E2EJobWithTrigger;
}

async function listRuns(request: APIRequestContext, jobId: string): Promise<E2ERun[]> {
  const response = await request.get(`/v1/jobs/${jobId}/runs`);
  if (!response.ok()) {
    throw new Error(`failed to list runs for job ${jobId}: ${response.status()} ${await response.text()}`);
  }
  return (await response.json()) as E2ERun[];
}

async function getRun(request: APIRequestContext, jobId: string, runId: string): Promise<E2ERun> {
  const response = await request.get(`/v1/jobs/${jobId}/runs/${runId}`);
  if (!response.ok()) {
    throw new Error(`failed to load run ${runId}: ${response.status()} ${await response.text()}`);
  }
  return (await response.json()) as E2ERun;
}

function requestBody(options: TriggerJobOptions): { data?: TriggerJobOptions } {
  const body: TriggerJobOptions = {};
  if (options.params && Object.keys(options.params).length > 0) {
    body.params = options.params;
  }
  if (options.priority) {
    body.priority = options.priority;
  }
  return Object.keys(body).length > 0 ? { data: body } : {};
}

function normalizeTriggerJobOptions(input: Record<string, string> | TriggerJobOptions | undefined): TriggerJobOptions {
  if (!input || Object.keys(input).length === 0) {
    return {};
  }
  if ("params" in input || "priority" in input) {
    return input as TriggerJobOptions;
  }
  return { params: input };
}

function normalizeExpectedStatuses(status: string | string[] | undefined): Set<string> {
  if (!status) {
    return new Set();
  }
  return new Set(Array.isArray(status) ? status : [status]);
}

// latestRun picks the most-recently-created run by comparing created_at
// (tie-broken by id, matching the server's own ORDER BY) rather than by
// array position. GET /v1/jobs/:id/runs is a client-facing contract, not an
// implementation detail this helper should hardcode an assumption about:
// the endpoint moved from oldest-first/unbounded to newest-first/paginated
// (issue #499), and a position-based pick (`runs.at(-1)` / `runs[0]`) would
// need updating again on the next ordering change. Comparing timestamps
// stays correct either way.
function latestRun(runs: E2ERun[]): E2ERun | undefined {
  if (runs.length === 0) {
    return undefined;
  }
  return runs.reduce((latest, run) => {
    const runTime = Date.parse(run.created_at);
    const latestTime = Date.parse(latest.created_at);
    if (runTime !== latestTime) {
      return runTime > latestTime ? run : latest;
    }
    return run.id > latest.id ? run : latest;
  });
}

async function delay(ms: number): Promise<void> {
  await new Promise((resolve) => setTimeout(resolve, ms));
}
