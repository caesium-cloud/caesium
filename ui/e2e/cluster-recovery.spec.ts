import { execFileSync, type ChildProcess } from "node:child_process";
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
  assessClusterRecoveryGate,
  caesiumMemberSelector,
  chooseOwner,
  commandViolation,
  completedAtOrAfter,
  consoleRecoveryDefinition,
  containerSeen,
  convergenceIssues,
  ctrListingKind,
  endpointHost,
  extraEnvFromReleaseValues,
  formatClusterGateFailure,
  formatKillEvidence,
  helmAuthTemplateCommand,
  authModesFromPodList,
  apiKeyAuthViolation,
  criLogText,
  helmAuthUpgradeCommand,
  memberLogCommand,
  nodeLogSourcesFromPodList,
  nodePodLogCommand,
  helmGetValuesCommand,
  isTerminalRunStatus,
  killEvidenceShowsDeath,
  kubeletStopCommand,
  nodeImageListCommand,
  robustnessTaskImage,
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
  releaseSample,
  runHistoryLinkSelector,
  runIdFromHref,
  runTaskPods,
  serviceConsoleForward,
  startReadyChild,
  stopChildProcess,
  statusFromRowText,
  stripRuntimeContainerID,
  taskImageListed,
  taskListCommand,
  taskPlacementIssues,
  taskReleaseCommand,
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

// On a failed run the retained trace embeds the page video, and zipping that
// entry stalled worker teardown until the project timeout, leaving a truncated
// trace.zip (both live D3 failures). Keep the trace and failure screenshot.
test.use({ video: "off" });

let session: ClusterRecoverySession | undefined;
let owner: CaesiumMember | undefined;
let faultedNode: string | undefined;
let ownerForward: ChildProcess | undefined;
let serviceForward: ChildProcess | undefined;
let ownerOrigin = "";
let serviceOrigin = "";
let keys: AuthLaneKeys | undefined;
/** Timeline and observations for d3-evidence.json in the artifacts dir. No keys. */
const evidence: Record<string, unknown> = {};

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

test.afterAll(async () => {
  test.setTimeout(600_000);
  if (session && faultedNode) {
    for (const command of ownerRestartSequence({ kubeconfig: session.kubeconfig, node: faultedNode })) {
      try {
        run(command);
      } catch (err) {
        console.error(`cleanup ${command.description} failed: ${err instanceof Error ? err.message : String(err)}`);
      }
    }
  }
  await stopChildProcess(ownerForward);
  await stopChildProcess(serviceForward);
  ownerForward = undefined;
  serviceForward = undefined;
  if (session) {
    await captureFinalLogs(session);
    try {
      fs.writeFileSync(path.join(session.artifactsDir, "d3-evidence.json"), `${JSON.stringify(evidence, null, 2)}\n`);
    } catch (err) {
      console.error(`writing d3-evidence.json failed: ${err instanceof Error ? err.message : String(err)}`);
    }
  }
});

// A passing journey measures a few minutes; 900 s covers the sum of the
// per-step windows below so a slow step fails on its own message.
test("authenticated console observes the owner crash and converges on the durable outcome", async ({ page }) => {
  test.setTimeout(900_000);
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
  const taskImage = robustnessTaskImage(activeSession.robustnessId);
  const loaded = run(nodeImageListCommand(intendedOwner.node));
  if (!taskImageListed(loaded, taskImage)) {
    throw new Error(`task image ${taskImage} is not loaded on ${intendedOwner.node}`);
  }
  const definition = consoleRecoveryDefinition(alias, taskImage, marker);
  Object.assign(evidence, {
    robustnessId: activeSession.robustnessId,
    serverImage: activeSession.serverImage,
    taskImage,
    alias,
    marker,
    owner: { pod: intendedOwner.name, node: intendedOwner.node, ip: intendedOwner.ip },
  });
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
  Object.assign(evidence, { jobId: job.id, runId, triggeredAt: iso(Date.now()) });

  await expect(page.locator("h1").locator("xpath=..").locator("[data-status='running']")).toBeVisible({ timeout: 90_000 });

  const leaseBefore = await poll(90_000, 1_000, async () => {
    const lease = await readLease(ownerOrigin, adminKey, runId);
    if (endpointHost(lease.ownerNode) !== intendedOwner.ip) {
      throw new Error(`lease owner ${lease.ownerNode} is not the cordoned pod ${intendedOwner.name} at ${intendedOwner.ip}`);
    }
    return lease;
  });
  evidence.leaseBefore = leaseBefore;
  // Fault an owner whose run is executing: the hold container must be running
  // on a node other than the owner's before the kill.
  evidence.taskRunningBeforeFault = await poll(60_000, 1_000, async () => {
    const listed = JSON.parse(run(kubectlGetPodsCommand(activeSession.kubeconfig, activeSession.namespace))) as unknown;
    const issues = taskPlacementIssues(listed, runId, intendedOwner.node);
    if (issues.length > 0) throw new Error(issues.join("; "));
    const running = runTaskPods(listed, runId).filter((pod) => pod.running);
    if (running.length === 0) throw new Error(`no task pod for run ${runId} is running yet`);
    return running.map((pod) => `${pod.name}@${pod.node}`);
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

  const beforeFault = await readRun(ownerOrigin, adminKey, job.id, runId);
  if (beforeFault.status === "succeeded" || beforeFault.status === "failed" || beforeFault.status === "cancelled") {
    throw new Error(`run reached ${beforeFault.status} before the owner fault`);
  }
  const statusBefore = await headingStatus(page);
  let faultSignals = 0;
  let faultRecordedAt = 0;
  let watchFault = true;
  const noteFault = () => {
    faultSignals += 1;
    if (faultRecordedAt === 0) faultRecordedAt = Date.now();
  };
  const onFailed = (request: { url(): string }) => {
    if (!watchFault || !request.url().startsWith(ownerOrigin)) return;
    noteFault();
  };
  const onConsole = (message: ConsoleMessage) => {
    if (!watchFault || message.type() !== "error") return;
    if (!/net::ERR_/i.test(message.text())) return;
    noteFault();
  };
  page.on("requestfailed", onFailed);
  page.on("console", onConsole);
  const faultStartedAt = Date.now();

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
  if (faultRecordedAt === 0) faultRecordedAt = faultStartedAt;

  evidence.fault = {
    startedAt: iso(faultStartedAt),
    recordedAt: iso(faultRecordedAt),
    signals: faultSignals,
    headingBefore: statusBefore,
    headingDuring: statusDuring,
  };

  await stopChildProcess(ownerForward);
  ownerForward = undefined;
  serviceForward = await startServiceForward(activeSession);
  serviceOrigin = ownerOrigin;
  evidence.serviceForwardAt = iso(Date.now());

  let eventStreamAttempts = 0;
  let eventStreamAuthorized = 0;
  let authenticatedRunReads = 0;
  const runPath = `/v1/jobs/${job.id}/runs/${runId}`;
  const onEventResponse = (response: PlaywrightResponse) => {
    if (response.request().method() !== "GET") return;
    const started = response.request().timing().startTime;
    if (!Number.isFinite(started) || started < faultStartedAt) return;
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
    await expect(page.getByTestId("run-heading")).toBeVisible({ timeout: 30_000 });

    // The hold stays closed until the lease has moved to a survivor, so the
    // run cannot complete before the takeover is observed.
    let lastLease = "no lease read";
    const takeover = await poll(120_000, 1_000, async () => {
      const lease = await readLease(serviceOrigin, adminKey, runId);
      lastLease = `generation ${lease.generation} owner ${lease.ownerNode}`;
      if (lease.generation > leaseBefore.generation && endpointHost(lease.ownerNode) !== endpointHost(leaseBefore.ownerNode)) {
        return { lease, at: Date.now(), terminalStatus: "" };
      }
      const snapshot = await readRun(serviceOrigin, adminKey, job.id, runId);
      if (isTerminalRunStatus(snapshot.status)) return { lease, at: Date.now(), terminalStatus: snapshot.status };
      return undefined;
    }).catch((err: unknown) => {
      throw new Error(`survivor takeover was not observed within 120s (last lease ${lastLease}): ${errorText(err)}`);
    });
    evidence.takeover = {
      observedAt: iso(takeover.at),
      lease: takeover.lease,
      terminalBeforeTakeover: takeover.terminalStatus || null,
    };

    // Release every task pod of the run (the first attempt and any survivor
    // re-dispatch) until the run is terminal.
    const released = new Set<string>();
    const releases: { pod: string; startedAt: string; ok: boolean; detail: string }[] = [];
    const seenPods = new Set<string>();
    const strayPods = new Set<string>();
    let firstReleaseAt = 0;
    let lastStatus = "unread";
    // Placement is checked on every sample, the terminal one included; a run
    // that went terminal before the takeover is sampled once, with no release.
    const durable = await poll(150_000, 1_000, async () => {
      const snapshot = await readRun(serviceOrigin, adminKey, job.id, runId);
      lastStatus = snapshot.status;
      const listed = JSON.parse(run(kubectlGetPodsCommand(activeSession.kubeconfig, activeSession.namespace))) as unknown;
      const pods = runTaskPods(listed, runId);
      for (const pod of pods) seenPods.add(`${pod.name}@${pod.node || "unbound"}`);
      const sample = releaseSample({ status: snapshot.status, pods, ownerNode: intendedOwner.node, released });
      for (const issue of sample.issues) strayPods.add(issue);
      if (sample.terminal) {
        return { snapshot, lease: await readLease(serviceOrigin, adminKey, runId) };
      }
      for (const pod of sample.targets) {
        const startedAt = Date.now();
        const result = tryRun(taskReleaseCommand(activeSession.kubeconfig, activeSession.namespace, pod, runId));
        releases.push({ pod, startedAt: iso(startedAt), ok: result.ok, detail: result.detail.slice(0, 300) });
        if (!result.ok) continue;
        released.add(pod);
        if (firstReleaseAt === 0) firstReleaseAt = startedAt;
      }
      return undefined;
    }).catch((err: unknown) => {
      throw new Error(
        `run stayed ${lastStatus} after the survivor took over and the hold was released: ${errorText(err)}; releases=${JSON.stringify(releases)}`,
      );
    });
    Object.assign(evidence, {
      releases,
      taskPods: [...seenPods],
      durable: {
        status: durable.snapshot.status,
        completedAt: durable.snapshot.completedAt,
        lease: durable.lease,
      },
    });
    if (strayPods.size > 0) throw new Error(`task placement after the fault: ${[...strayPods].join("; ")}`);
    if (durable.snapshot.status === "succeeded") {
      if (!completedAtOrAfter(durable.snapshot.completedAt, faultStartedAt)) {
        throw new Error(`run completed_at ${durable.snapshot.completedAt || "missing"} is not after the owner fault`);
      }
      if (!completedAtOrAfter(durable.snapshot.completedAt, firstReleaseAt)) {
        throw new Error(
          `run completed_at ${durable.snapshot.completedAt || "missing"} is not after the first hold release (${firstReleaseAt ? iso(firstReleaseAt) : "never released"})`,
        );
      }
    }
    const taskId = durable.snapshot.tasks[0]?.taskId ?? "";
    const logExcerpt = taskId ? await readRetainedLog(serviceOrigin, adminKey, job.id, runId, taskId, marker) : "";

    await poll(30_000, 500, async () => ((await headingStatus(page)) === durable.snapshot.status ? true : undefined)).catch(
      () => undefined,
    );
    const beforeReload = await readRunSurface(page, job.id, runId, marker, durable.snapshot.status);
    await page.reload();
    await expect(page.getByPlaceholder("csk_live_...")).toBeVisible();
    await page.getByPlaceholder("csk_live_...").fill(authKeys.runner);
    const whoami = page.waitForResponse(
      (response) => new URL(response.url()).pathname === "/auth/whoami" && response.status() === 200,
    );
    await page.getByRole("button", { name: "Sign In" }).click();
    await whoami;
    const afterReload = await readRunSurface(page, job.id, runId, marker, durable.snapshot.status);

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
    Object.assign(evidence, {
      console: {
        headingStatus: beforeReload.headingStatus,
        reloadedHeadingStatus: afterReload.headingStatus,
        runRows: beforeReload.runRows,
        runListFirstRows: beforeReload.runListFirstRows,
        runListSettledMs: beforeReload.runListSettledMs,
        reloadedRunRows: afterReload.runRows,
        reloadedRunListFirstRows: afterReload.runListFirstRows,
        reloadedRunListSettledMs: afterReload.runListSettledMs,
        logSourceLabel: beforeReload.logSourceLabel,
        reloadedLogSourceLabel: afterReload.logSourceLabel,
        logHasMarker: beforeReload.logText.includes(marker),
        reloadedLogHasMarker: afterReload.logText.includes(marker),
        eventStreamAttempts,
        eventStreamAuthorized,
        authenticatedRunReads,
      },
      retainedLogExcerpt: logExcerpt,
      issues,
      checkedAt: iso(Date.now()),
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
  const listed = JSON.parse(run(kubectlGetPodsCommand(current.kubeconfig, current.namespace, caesiumMemberSelector()))) as unknown;
  const authViolation = apiKeyAuthViolation(listed);
  if (authViolation) throw new Error(`${authViolation} after the upgrade`);
  const modes = authModesFromPodList(listed);
  const dir = path.join(current.artifactsDir, "bootstrap-logs");
  fs.mkdirSync(dir, { recursive: true });
  const readKey = (stdoutPath: string, normalize: (text: string) => string = (text) => text): string | null => {
    const text = fs.existsSync(stdoutPath) ? fs.readFileSync(stdoutPath, "utf8") : "";
    const found = parseBootstrapAdminKey(normalize(text));
    if (found) process.env.CAESIUM_E2E_AUTH_ADMIN_KEY = found;
    return found;
  };
  for (const member of membersFromPodList(listed)) {
    for (const previous of [false, true]) {
      const suffix = previous ? "previous" : "current";
      const stdoutPath = path.join(dir, `${member.name}.${suffix}.log`);
      const stderrPath = path.join(dir, `${member.name}.${suffix}.err`);
      captureCommand(memberLogCommand(current, member.name, previous), stdoutPath, stderrPath);
      if (readKey(stdoutPath)) return;
    }
  }
  // The banner can rotate out of the live file while helm rolls the other
  // members at debug log level; read the rotated files on the node.
  for (const source of nodeLogSourcesFromPodList(listed)) {
    const stdoutPath = path.join(dir, `${source.name}.node.log`);
    const stderrPath = path.join(dir, `${source.name}.node.err`);
    let command: ShellCommand;
    try {
      command = nodePodLogCommand(current.namespace, source);
    } catch (error) {
      fs.writeFileSync(stderrPath, `${error instanceof Error ? error.message : String(error)}\n`);
      continue;
    }
    captureCommand(command, stdoutPath, stderrPath);
    if (readKey(stdoutPath, criLogText)) return;
  }
  throw new Error(`bootstrap admin API key was not in ${dir}; modes=${JSON.stringify(modes)}`);
}

async function readRunSurface(
  page: Page,
  jobId: string,
  runId: string,
  marker: string,
  durableStatus: string,
): Promise<{
  headingStatus: string;
  headingCount: number;
  logText: string;
  logSourceLabel: string;
  runRows: ConsoleRunRow[];
  runListFirstRows: ConsoleRunRow[];
  runListSettledMs: number;
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
  const headings = await page.getByTestId("run-heading").count();
  // Run detail has no history table. The job page's Runs tab is the console list.
  await page.locator(`a[href="/jobs/${jobId}"]`).first().click();
  await page.getByTestId("job-detail-view-tabs").getByRole("link", { name: "Run history" }).click();
  await expect(page.getByTestId("job-runs-list")).toBeVisible();
  // The list renders its cached query first and refetches every 15 s while the
  // event stream is unauthorized. Give it the heading's 30 s to converge; a row
  // that is still stale (or unreadable) then is rejected by convergenceIssues.
  const listOpenedAt = Date.now();
  const runListFirstRows = await readRunRows(page, jobId);
  let runRows = runListFirstRows;
  while (!rowsShow(runRows, runId, durableStatus) && Date.now() - listOpenedAt < 30_000) {
    await sleep(1_000);
    runRows = await readRunRows(page, jobId);
  }
  const runListSettledMs = Date.now() - listOpenedAt;
  await page.getByTestId("job-runs-list").locator(runHistoryLinkSelector(jobId, runId)).first().click();
  await expect(page.getByTestId("run-heading")).toBeVisible();
  return { headingStatus: heading, headingCount: headings, logText, logSourceLabel, runRows, runListFirstRows, runListSettledMs };
}

function rowsShow(rows: ConsoleRunRow[], runId: string, status: string): boolean {
  return rows.length > 0 && rows.every((row) => row.id === runId && row.status === status);
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
    // Read the status badge alone: the row's whole textContent glues the
    // duration to the badge ("2.4ssucceeded"), which defeats a word match.
    const badge = link.locator(":scope > div").last();
    const badgeText = (await badge.count()) > 0 ? ((await badge.textContent()) ?? "") : "";
    rows.push({ id, status: statusFromRowText(badgeText) });
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

async function waitForHealth(base: string, timeoutMs = 30_000): Promise<void> {
  await poll(timeoutMs, 500, async () => {
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

function captureCommand(command: ShellCommand, stdoutPath: string, stderrPath: string): void {
  const violation = commandViolation(command.argv);
  if (violation) throw new Error(`${violation}: ${command.argv.join(" ")}`);
  const out = fs.openSync(stdoutPath, "w");
  const err = fs.openSync(stderrPath, "w");
  try {
    execFileSync(command.argv[0], command.argv.slice(1), {
      stdio: ["ignore", out, err],
      timeout: 120_000,
      env: commandEnv(),
    });
  } catch (error) {
    fs.appendFileSync(stderrPath, `\n${error instanceof Error ? error.message : String(error)}\n`);
  } finally {
    fs.closeSync(out);
    fs.closeSync(err);
  }
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

function tryRun(command: ShellCommand, timeoutMs = 30_000): { ok: boolean; detail: string } {
  const violation = commandViolation(command.argv);
  if (violation) throw new Error(`${violation}: ${command.argv.join(" ")}`);
  try {
    const out = execFileSync(command.argv[0], command.argv.slice(1), {
      encoding: "utf8",
      timeout: timeoutMs,
      maxBuffer: 8 * 1024 * 1024,
      stdio: ["ignore", "pipe", "pipe"],
      env: commandEnv(),
    });
    return { ok: true, detail: out.trim() };
  } catch (err) {
    return { ok: false, detail: errorText(err) };
  }
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

/** A failed start stops and reaps its own child (startReadyChild). */
function startPortForward(command: ShellCommand): Promise<ChildProcess> {
  return startReadyChild(command, { readyText: "Forwarding from", timeoutMs: 20_000, env: commandEnv() });
}

/**
 * Reconnect through the Service on the page's local port. kubectl binds one
 * member when it starts; until the node controller marks the killed member
 * NotReady it can still pick that pod, whose kubelet is down, so retry until a
 * healthy member answers.
 */
async function startServiceForward(current: ClusterRecoverySession): Promise<ChildProcess> {
  const deadline = Date.now() + 90_000;
  let attempts = 0;
  let last = "no attempt";
  while (Date.now() < deadline) {
    attempts += 1;
    let child: ChildProcess | undefined;
    try {
      // A rejected start has already stopped its own child.
      child = await startPortForward(serviceConsoleForward(current, OWNER_CONSOLE_PORT));
      await waitForHealth(ownerOrigin, 10_000);
      evidence.serviceForwardAttempts = attempts;
      return child;
    } catch (err) {
      last = errorText(err);
      await stopChildProcess(child);
      await sleep(3_000);
    }
  }
  throw new Error(`service port-forward did not reach a healthy member after ${attempts} attempts: ${last}`);
}

/** Member logs once kubelet is back, for diagnosis. Failures are recorded, not thrown. */
async function captureFinalLogs(current: ClusterRecoverySession): Promise<void> {
  const dir = path.join(current.artifactsDir, "d3-final-logs");
  try {
    fs.mkdirSync(dir, { recursive: true });
    const listed = JSON.parse(run(kubectlGetPodsCommand(current.kubeconfig, current.namespace, caesiumMemberSelector()))) as unknown;
    for (const member of membersFromPodList(listed)) {
      const base = path.join(dir, member.name);
      if (member.name !== owner?.name) {
        captureCommand(memberLogCommand(current, member.name, false), `${base}.current.log`, `${base}.current.err`);
        continue;
      }
      // The owner's node kubelet was just restarted; its log API needs a moment.
      for (let attempt = 0; attempt < 6; attempt += 1) {
        captureCommand(memberLogCommand(current, member.name, true), `${base}.previous.log`, `${base}.previous.err`);
        if (fs.statSync(`${base}.previous.log`).size > 0) break;
        await sleep(10_000);
      }
    }
  } catch (err) {
    console.error(`final log capture failed: ${errorText(err)}`);
  }
}

function iso(ms: number): string {
  return Number.isFinite(ms) && ms > 0 ? new Date(ms).toISOString() : "";
}

function errorText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
