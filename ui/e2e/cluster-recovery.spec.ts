import { execFileSync, spawn, type ChildProcess } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import {
  expect,
  request as playwrightRequest,
  test,
  type ConsoleMessage,
  type Page,
  type Response as PlaywrightResponse,
} from "@playwright/test";
import { loginAtUrl, obtainAuthKeys, type AuthLaneKeys } from "./helpers/auth";
import {
  OWNER_CONSOLE_PORT,
  SERVICE_CONSOLE_PORT,
  assessClusterRecoveryGate,
  caesiumMemberSelector,
  chooseOwner,
  commandViolation,
  consoleRecoveryDefinition,
  containerSeen,
  convergenceIssues,
  ctrListingKind,
  endpointHost,
  extraEnvFromReleaseValues,
  formatClusterGateFailure,
  formatKillEvidence,
  helmAuthTemplateCommand,
  helmAuthUpgradeCommand,
  helmGetValuesCommand,
  killEvidenceShowsDeath,
  kubeletStopCommand,
  kubectlGetPodsCommand,
  leaseQueryBody,
  membersFromPodList,
  overlayPreservesEnv,
  ownerConsoleForward,
  ownerKillSequence,
  ownerRestartSequence,
  parseBootstrapAdminKey,
  parseLeaseResponse,
  parseRunSnapshot,
  planAuthExtraEnv,
  podLogsCommand,
  runIdFromHref,
  serviceConsoleForward,
  statusFromRowText,
  stripRuntimeContainerID,
  taskListCommand,
  taskPlacementIssues,
  type ClusterRecoverySession,
  type CaesiumMember,
  type ConsoleRunRow,
  type DurableOutcome,
  type LeaseSnapshot,
  type ShellCommand,
} from "./helpers/cluster";
import { failOnUnexpectedPageErrors } from "./helpers/fixtures";

// The owner crash drops the connected browser. Chrome logs net::ERR_* for that
// cut; tolerate only that class, in this file, the same way network-recovery does.
failOnUnexpectedPageErrors({ allowNetworkLevelErrors: true });

let session: ClusterRecoverySession | undefined;
let owner: CaesiumMember | undefined;
let faultedNode: string | undefined;
let ownerForward: ChildProcess | undefined;
let serviceForward: ChildProcess | undefined;
let ownerOrigin = "";
let serviceOrigin = "";
let keys: AuthLaneKeys | undefined;

test.beforeAll(async () => {
  test.setTimeout(600_000);
  const gate = assessClusterRecoveryGate(process.env);
  if (!gate.ok) throw new Error(formatClusterGateFailure(gate.missing));
  session = gate.session;

  await ensureApiKeyAuth(session);
  const pods = JSON.parse(run(kubectlGetPodsCommand(session.kubeconfig, session.namespace, caesiumMemberSelector())));
  const chosen = chooseOwner(membersFromPodList(pods));
  if ("error" in chosen) throw new Error(chosen.error);
  owner = chosen.owner;

  // Register restart before the node is cordoned or kubelet is stopped.
  faultedNode = owner.node;
  const [cordon] = ownerKillSequence({
    kubeconfig: session.kubeconfig,
    node: owner.node,
    containerID: owner.containerID,
  });
  run(cordon);

  ownerForward = await startPortForward(ownerConsoleForward(session, owner.name));
  ownerOrigin = `http://127.0.0.1:${OWNER_CONSOLE_PORT}`;
  await waitForHealth(ownerOrigin);

  const api = await playwrightRequest.newContext({ baseURL: ownerOrigin });
  try {
    keys = await obtainAuthKeys(api);
  } finally {
    await api.dispose();
  }
});

test.afterAll(() => {
  if (session && faultedNode) {
    for (const command of ownerRestartSequence({ kubeconfig: session.kubeconfig, node: faultedNode })) {
      try {
        run(command);
      } catch (err) {
        console.error(`cleanup ${command.description} failed: ${err instanceof Error ? err.message : String(err)}`);
      }
    }
  }
  stopChild(ownerForward);
  stopChild(serviceForward);
});

test("authenticated console observes the owner crash and converges on the durable outcome", async ({ page }) => {
  test.setTimeout(600_000);
  if (!session || !owner || !keys || !ownerOrigin) {
    throw new Error("cluster-recovery fails closed: the session was not established");
  }
  const activeSession = session;
  const intendedOwner = owner;
  const authKeys = keys;
  const adminKey = process.env.CAESIUM_E2E_AUTH_ADMIN_KEY?.trim();
  if (!adminKey) throw new Error("bootstrap admin API key is unavailable");

  const suffix = crypto.randomUUID().replace(/-/g, "").slice(0, 12);
  const alias = `d3-console-${suffix}`;
  const marker = `d3-marker-${suffix}`;
  const definition = consoleRecoveryDefinition(alias, activeSession.taskImage, marker);
  const applied = await apiSend(ownerOrigin, adminKey, "POST", "/v1/jobdefs/apply", { definitions: [definition] });
  if (applied.status < 200 || applied.status >= 300) {
    throw new Error(`apply failed: ${applied.status} ${applied.text.slice(0, 500)}`);
  }
  const job = await findJob(ownerOrigin, adminKey, alias);

  await loginAtUrl(page, `${ownerOrigin}/jobs/${job.id}`, authKeys.viewer);
  await expect(page.getByRole("heading", { name: alias })).toBeVisible();
  await page.getByRole("button", { name: "Trigger job" }).click();
  await page.getByRole("button", { name: "Confirm Trigger" }).click();
  await expect(page.getByText(/Failed to trigger job: insufficient permissions/)).toBeVisible();
  const deniedRuns = await apiSend(ownerOrigin, adminKey, "GET", `/v1/jobs/${job.id}/runs`);
  expect(runsFromList(deniedRuns.body)).toEqual([]);

  await loginAtUrl(page, `${ownerOrigin}/jobs/${job.id}`, authKeys.runner);
  await expect(page.getByRole("heading", { name: alias })).toBeVisible();
  await page.getByRole("button", { name: "Trigger job" }).click();
  await page.getByRole("button", { name: "Confirm Trigger" }).click();
  await page.waitForURL(/\/jobs\/[^/]+\/runs\/[0-9a-f-]{36}$/i, { timeout: 30_000 });
  const runId = page.url().match(/\/runs\/([0-9a-f-]{36})/i)?.[1]?.toLowerCase();
  if (!runId) throw new Error(`console trigger did not open a run url (${page.url()})`);

  await expect(page.locator("h1").locator("xpath=..").locator("[data-status='running']")).toBeVisible({ timeout: 90_000 });

  const leaseBefore = await poll(90_000, 1_000, async () => {
    const lease = await readLease(ownerOrigin, adminKey, runId);
    if (endpointHost(lease.ownerNode) !== intendedOwner.ip) {
      throw new Error(`lease owner ${lease.ownerNode} is not the cordoned pod ${intendedOwner.name} at ${intendedOwner.ip}`);
    }
    return lease;
  });
  await poll(60_000, 1_000, async () => {
    const listed = JSON.parse(run(kubectlGetPodsCommand(activeSession.kubeconfig, activeSession.namespace)));
    const issues = taskPlacementIssues(listed, runId, intendedOwner.node);
    if (issues.length > 0) throw new Error(issues.join("; "));
    return true;
  });

  const refreshed = membersFromPodList(
    JSON.parse(run(kubectlGetPodsCommand(activeSession.kubeconfig, activeSession.namespace, caesiumMemberSelector()))),
  ).find((member) => member.name === intendedOwner.name);
  if (!refreshed?.containerID) throw new Error(`owner ${intendedOwner.name} lost its container id before the kill`);
  const containerID = stripRuntimeContainerID(refreshed.containerID);
  const killPlan = ownerKillSequence({
    kubeconfig: activeSession.kubeconfig,
    node: refreshed.node,
    containerID,
  });
  const before = run(killPlan[3]);
  if (ctrListingKind(before) !== "ok" || !containerSeen(before, containerID)) {
    throw new Error(`owner container was not in a valid ctr task list before SIGKILL\n${before}`);
  }

  const statusBefore = await headingStatus(page);
  let faultSignals = 0;
  let watchFault = true;
  const onFailed = () => {
    if (watchFault) faultSignals += 1;
  };
  const onConsole = (message: ConsoleMessage) => {
    if (!watchFault || message.type() !== "error") return;
    if (/net::ERR_|Failed to load resource|disconnected/i.test(message.text())) faultSignals += 1;
  };
  page.on("requestfailed", onFailed);
  page.on("console", onConsole);

  run(kubeletStopCommand(refreshed.node));
  let listing = before;
  let dead = false;
  for (let attempt = 0; attempt < 20 && !dead; attempt += 1) {
    runAllowFailure(killPlan[2]);
    try {
      listing = run(taskListCommand(refreshed.node));
    } catch (err) {
      listing = err instanceof Error ? err.message : String(err);
    }
    dead = killEvidenceShowsDeath(formatKillEvidence(refreshed.node, containerID, listing), containerID);
    if (!dead) await sleep(1_000);
  }
  if (!dead) throw new Error(`owner process did not die\n${listing}`);

  let statusDuring = statusBefore;
  const faultDeadline = Date.now() + 15_000;
  while (Date.now() < faultDeadline && faultSignals === 0 && statusDuring === statusBefore) {
    await sleep(500);
    statusDuring = await headingStatus(page);
  }
  watchFault = false;
  page.off("requestfailed", onFailed);
  page.off("console", onConsole);
  const faultRecordedAt = Date.now();

  serviceForward = await startPortForward(serviceConsoleForward(activeSession));
  serviceOrigin = `http://127.0.0.1:${SERVICE_CONSOLE_PORT}`;
  await waitForHealth(serviceOrigin);

  let eventStreamAttempts = 0;
  let eventStreamAuthorized = 0;
  let authenticatedRunReads = 0;
  const runPath = `/v1/jobs/${job.id}/runs/${runId}`;
  const onEventResponse = (response: PlaywrightResponse) => {
    if (response.request().method() !== "GET") return;
    const pathname = new URL(response.url()).pathname;
    if (pathname === "/v1/events") {
      eventStreamAttempts += 1;
      if (response.status() === 200) eventStreamAuthorized += 1;
      return;
    }
    if (pathname === runPath && response.status() === 200) authenticatedRunReads += 1;
  };
  page.on("response", onEventResponse);
  try {
    await loginAtUrl(page, `${serviceOrigin}/jobs/${job.id}/runs/${runId}`, authKeys.runner);
    await expect(page.getByRole("heading", { name: /^Run / })).toBeVisible({ timeout: 30_000 });
    const durable = await poll(120_000, 1_000, async () => {
      const snapshot = await readRun(serviceOrigin, adminKey, job.id, runId);
      const lease = await readLease(serviceOrigin, adminKey, runId);
      const tookOver = snapshot.status === "succeeded"
        && lease.generation > leaseBefore.generation
        && endpointHost(lease.ownerNode) !== endpointHost(leaseBefore.ownerNode);
      if (!tookOver && snapshot.status !== "failed" && snapshot.status !== "cancelled") return undefined;
      return { snapshot, lease };
    }).catch(async () => ({
      snapshot: await readRun(serviceOrigin, adminKey, job.id, runId),
      lease: await readLease(serviceOrigin, adminKey, runId),
    }));
    const taskId = durable.snapshot.tasks[0]?.taskId ?? "";
    const logExcerpt = taskId ? await readRetainedLog(serviceOrigin, adminKey, job.id, runId, taskId, marker) : "";

    await poll(30_000, 500, async () => ((await headingStatus(page)) === durable.snapshot.status ? true : undefined)).catch(
      () => undefined,
    );
    const beforeReload = await readRunSurface(page, job.id, runId, marker);
    await page.reload();
    await expect(page.getByPlaceholder("csk_live_...")).toBeVisible();
    await page.getByPlaceholder("csk_live_...").fill(authKeys.runner);
    const whoami = page.waitForResponse(
      (response) => new URL(response.url()).pathname === "/auth/whoami" && response.status() === 200,
    );
    await page.getByRole("button", { name: "Sign In" }).click();
    await whoami;
    const afterReload = await readRunSurface(page, job.id, runId, marker);

    const outcome: DurableOutcome = {
      runId,
      status: durable.snapshot.status,
      generationBefore: leaseBefore.generation,
      generationAfter: durable.lease.generation,
      ownerBefore: leaseBefore.ownerNode,
      ownerAfter: durable.lease.ownerNode,
      logExcerpt,
    };
    const issues = convergenceIssues({
      durable: outcome,
      fault: {
        connectedBeforeFault: statusBefore === "running",
        sawDisconnectOrStatusChange: faultSignals > 0 || statusDuring !== statusBefore,
        recordedAt: faultRecordedAt,
        assertionAt: Date.now(),
      },
      console: {
        ...beforeReload,
        showedSuccessBeforeFault: statusBefore === "succeeded" || statusBefore === "completed" || statusBefore === "success",
        eventStreamAttempts,
        eventStreamAuthorized,
        authenticatedRunReads,
        reloadedHeadingStatus: afterReload.headingStatus,
        reloadedHeadingCount: afterReload.headingCount,
        reloadedRunRows: afterReload.runRows,
        reloadedLogText: afterReload.logText,
        reloadedLogSourceLabel: afterReload.logSourceLabel,
      },
    });
    expect(issues, JSON.stringify(issues, null, 2)).toEqual([]);
  } finally {
    page.off("response", onEventResponse);
  }
});

async function ensureApiKeyAuth(current: ClusterRecoverySession): Promise<void> {
  const release = JSON.parse(run(helmGetValuesCommand(current))) as unknown;
  const extra = extraEnvFromReleaseValues(release);
  if ("error" in extra) throw new Error(extra.error);
  const plan = planAuthExtraEnv(extra, current.hashSecret);
  if (plan.action === "refused") throw new Error(plan.reason);
  if (plan.action === "append") {
    const preserved = overlayPreservesEnv(plan.overlayYaml, extra);
    if (preserved) throw new Error(preserved);
    const overlayPath = path.join(current.artifactsDir, "d3-auth-overlay.yaml");
    fs.writeFileSync(overlayPath, plan.overlayYaml, { mode: 0o600 });
    const rendered = run(helmAuthTemplateCommand(current, overlayPath));
    for (const name of ["CAESIUM_AUTH_MODE", "CAESIUM_AUTH_REQUIRE_TLS", "CAESIUM_EXECUTION_MODE", "CAESIUM_RUN_OWNER_ENABLED"]) {
      if (!rendered.includes(name)) throw new Error(`helm template is missing ${name}`);
    }
    run(helmAuthUpgradeCommand(current, overlayPath));
  }
  if (process.env.CAESIUM_E2E_AUTH_ADMIN_KEY?.trim()) return;
  let key: string | null = null;
  for (let attempt = 0; attempt < 30 && !key; attempt += 1) {
    try {
      key = parseBootstrapAdminKey(run(podLogsCommand(current)));
    } catch {
      key = null;
    }
    if (!key) await sleep(1_000);
  }
  if (!key) {
    throw new Error(
      "bootstrap admin API key was not in caesium pod logs; set CAESIUM_E2E_AUTH_ADMIN_KEY from the one-time bootstrap line",
    );
  }
  process.env.CAESIUM_E2E_AUTH_ADMIN_KEY = key;
}

async function readRunSurface(page: Page, jobId: string, runId: string, marker: string): Promise<{
  headingStatus: string;
  headingCount: number;
  logText: string;
  logSourceLabel: string;
  runRows: ConsoleRunRow[];
}> {
  const node = page.locator(".react-flow__node").first();
  await expect(node).toBeVisible({ timeout: 30_000 });
  await node.click();
  await expect(page.getByTestId("task-detail-panel")).toBeVisible();
  const plaintext = page.getByTestId("task-log-plaintext");
  await plaintext.waitFor({ state: "attached" });
  await expect(plaintext).toContainText(marker, { timeout: 30_000 }).catch(() => undefined);
  const source = page.getByTestId("log-source-badge");
  await expect(source).toHaveText("Retained snapshot", { timeout: 30_000 }).catch(() => undefined);
  // The plaintext node is screen-reader only, so innerText can be empty.
  const logText = (await plaintext.textContent()) ?? "";
  const logSourceLabel = (await source.count()) > 0 ? ((await source.textContent()) ?? "").trim() : "";
  const heading = await headingStatus(page);
  const headings = await page.getByRole("heading", { name: /^Run / }).count();
  // Run detail has no history table. The job page's Runs tab is the console list.
  await page.locator(`a[href="/jobs/${jobId}"]`).first().click();
  await page.getByTestId("job-detail-view-tabs").getByRole("link", { name: "Runs" }).click();
  await expect(page.getByTestId("job-runs-list")).toBeVisible();
  const runRows = await readRunRows(page, jobId);
  await page.locator(`a[href*="/runs/${runId}"]`).first().click();
  await expect(page.getByRole("heading", { name: /^Run / })).toBeVisible();
  return { headingStatus: heading, headingCount: headings, logText, logSourceLabel, runRows };
}

async function readRunRows(page: Page, jobId: string): Promise<ConsoleRunRow[]> {
  const links = page.getByTestId("job-runs-list").locator("a");
  const count = await links.count();
  const rows: ConsoleRunRow[] = [];
  for (let index = 0; index < count; index += 1) {
    const link = links.nth(index);
    const href = (await link.getAttribute("href")) ?? "";
    const id = runIdFromHref(href, jobId);
    if (!id) continue;
    rows.push({ id, status: statusFromRowText((await link.textContent()) ?? "") });
  }
  return rows;
}

async function headingStatus(page: Page): Promise<string> {
  try {
    const badge = page.locator("h1").locator("xpath=..").locator("[data-status]").first();
    return (await badge.getAttribute("data-status", { timeout: 2_000 })) ?? "";
  } catch {
    return "unreachable";
  }
}

async function readLease(base: string, adminKey: string, runId: string): Promise<LeaseSnapshot> {
  const body = leaseQueryBody(runId);
  if ("error" in body) throw new Error(body.error);
  const response = await apiSend(base, adminKey, "POST", "/v1/database/query", body);
  if (response.status !== 200) throw new Error(`lease query ${response.status}: ${response.text.slice(0, 400)}`);
  const parsed = parseLeaseResponse(response.body, runId);
  if ("error" in parsed) throw new Error(parsed.error);
  return parsed;
}

async function readRun(base: string, adminKey: string, jobId: string, runId: string) {
  const response = await apiSend(base, adminKey, "GET", `/v1/jobs/${jobId}/runs/${runId}`);
  if (response.status !== 200) throw new Error(`run read ${response.status}: ${response.text.slice(0, 400)}`);
  const parsed = parseRunSnapshot(response.body);
  if ("error" in parsed) throw new Error(parsed.error);
  return parsed;
}

async function readRetainedLog(
  base: string,
  adminKey: string,
  jobId: string,
  runId: string,
  taskId: string,
  marker: string,
): Promise<string> {
  try {
    const response = await fetch(`${base}/v1/jobs/${jobId}/runs/${runId}/logs?task_id=${encodeURIComponent(taskId)}`, {
      headers: { Authorization: `Bearer ${adminKey}` },
      signal: AbortSignal.timeout(20_000),
    });
    const text = await response.text();
    return response.ok && text.includes(marker) ? marker : "";
  } catch {
    return "";
  }
}

async function findJob(base: string, adminKey: string, alias: string): Promise<{ id: string; alias: string }> {
  const listed = await apiSend(base, adminKey, "GET", "/v1/jobs");
  if (listed.status !== 200) throw new Error(`list jobs ${listed.status}`);
  const job = jobsFromList(listed.body).find((candidate) => candidate.alias === alias);
  if (!job) throw new Error(`job ${alias} was not visible after apply`);
  return job;
}

async function apiSend(
  base: string,
  key: string,
  method: string,
  pathname: string,
  body?: unknown,
): Promise<{ status: number; body: unknown; text: string }> {
  const response = await fetch(new URL(pathname, base), {
    method,
    headers: {
      Authorization: `Bearer ${key}`,
      Accept: "application/json",
      ...(body === undefined ? {} : { "Content-Type": "application/json" }),
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await response.text();
  let parsed: unknown = text;
  if (text) {
    try {
      parsed = JSON.parse(text) as unknown;
    } catch {
      parsed = text;
    }
  }
  return { status: response.status, body: parsed, text };
}

function jobsFromList(payload: unknown): { id: string; alias: string }[] {
  const rows = Array.isArray(payload)
    ? payload
    : payload && typeof payload === "object" && Array.isArray((payload as { jobs?: unknown }).jobs)
      ? (payload as { jobs: unknown[] }).jobs
      : [];
  return rows.flatMap((row) => {
    if (!row || typeof row !== "object") return [];
    const id = (row as { id?: unknown }).id;
    const alias = (row as { alias?: unknown }).alias;
    return typeof id === "string" && typeof alias === "string" ? [{ id, alias }] : [];
  });
}

function runsFromList(payload: unknown): { id: string }[] {
  const rows = Array.isArray(payload) ? payload : [];
  return rows.flatMap((row) => {
    if (!row || typeof row !== "object") return [];
    const id = (row as { id?: unknown }).id;
    return typeof id === "string" ? [{ id }] : [];
  });
}

async function waitForHealth(base: string): Promise<void> {
  await poll(30_000, 500, async () => {
    const response = await fetch(`${base}/health`, { signal: AbortSignal.timeout(2_000) });
    return response.ok ? true : undefined;
  });
}

async function poll<T>(timeoutMs: number, intervalMs: number, read: () => Promise<T | undefined>): Promise<T> {
  const deadline = Date.now() + timeoutMs;
  let last = "no sample";
  while (Date.now() <= deadline) {
    try {
      const value = await read();
      if (value !== undefined) return value;
      last = "not ready";
    } catch (err) {
      last = err instanceof Error ? err.message : String(err);
    }
    await sleep(intervalMs);
  }
  throw new Error(`timed out after ${timeoutMs}ms: ${last}`);
}

function run(command: ShellCommand): string {
  const violation = commandViolation(command.argv);
  if (violation) throw new Error(`${violation}: ${command.argv.join(" ")}`);
  return execFileSync(command.argv[0], command.argv.slice(1), {
    encoding: "utf8",
    timeout: 300_000,
    maxBuffer: 32 * 1024 * 1024,
    env: commandEnv(),
  });
}

function runAllowFailure(command: ShellCommand): void {
  const violation = commandViolation(command.argv);
  if (violation) throw new Error(`${violation}: ${command.argv.join(" ")}`);
  try {
    execFileSync(command.argv[0], command.argv.slice(1), {
      encoding: "utf8",
      timeout: 30_000,
      maxBuffer: 8 * 1024 * 1024,
      env: commandEnv(),
    });
  } catch {
    // ctr kill is retried until the task listing shows the process is gone.
  }
}

function commandEnv(): NodeJS.ProcessEnv {
  const env = { ...process.env };
  if (session) env.KUBECONFIG = session.kubeconfig;
  return env;
}

async function startPortForward(command: ShellCommand): Promise<ChildProcess> {
  const violation = commandViolation(command.argv);
  if (violation) throw new Error(violation);
  const child = spawn(command.argv[0], command.argv.slice(1), {
    stdio: ["ignore", "pipe", "pipe"],
    env: commandEnv(),
  });
  let output = "";
  await new Promise<void>((resolve, reject) => {
    let settled = false;
    const timer = setTimeout(() => {
      if (settled) return;
      settled = true;
      reject(new Error(`port-forward did not become ready\n${output}`));
    }, 20_000);
    const finish = (error?: Error) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      if (error) reject(error);
      else resolve();
    };
    const onData = (chunk: Buffer) => {
      output += chunk.toString();
      if (output.includes("Forwarding from")) finish();
    };
    child.stdout?.on("data", onData);
    child.stderr?.on("data", onData);
    child.once("exit", (code) => finish(new Error(`port-forward exited ${code}\n${output}`)));
    child.once("error", (err) => finish(err));
  });
  return child;
}

function stopChild(child: ChildProcess | undefined): void {
  if (!child || child.exitCode !== null) return;
  child.kill("SIGTERM");
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
