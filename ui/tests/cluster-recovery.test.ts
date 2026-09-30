// @vitest-environment node

import { spawn, spawnSync, type ChildProcess } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import {
  CLUSTER_ENV,
  FOREIGN_CLUSTER_ID,
  HOLD_LIMIT_SECONDS,
  TASK_CONTAINER,
  TASK_RELEASE_FILE,
  UI_E2E_AUTH_HASH_SECRET,
  assessClusterRecoveryGate,
  chooseOwner,
  commandViolation,
  completedAtOrAfter,
  consoleHoldScript,
  consoleRecoveryDefinition,
  countExtraEnvEntries,
  convergenceIssues,
  endpointHost,
  extraEnvFromReleaseValues,
  formatClusterGateFailure,
  formatKillEvidence,
  helmAuthUpgradeCommand,
  isTerminalRunStatus,
  killEvidenceShowsDeath,
  leaseQueryBody,
  membersFromPodList,
  overlayPreservesEnv,
  ownerKillSequence,
  ownerRestartSequence,
  authModesFromPodList,
  apiKeyAuthViolation,
  criLogText,
  memberLogCommand,
  nodeLogSourcesFromPodList,
  nodePodLogCommand,
  parseBootstrapAdminKey,
  parseLeaseResponse,
  robustnessTaskImage,
  planAuthExtraEnv,
  releaseSample,
  releaseTargets,
  runHistoryLinkSelector,
  runTaskPods,
  startReadyChild,
  statusFromRowText,
  stopChildProcess,
  stripRuntimeContainerID,
  taskDeadFromListing,
  taskImageListed,
  taskPlacementIssues,
  taskReleaseCommand,
  type ConsoleSurface,
  type DurableOutcome,
  type FaultObservation,
} from "../e2e/helpers/cluster";

const uiRoot = process.cwd();
const runId = "11111111-1111-4111-8111-111111111111";
const containerID = "abcdef1234567890deadbeefcafebabe";

test("api-key hash secret used by ui-e2e-auth is long enough to boot", () => {
  expect(UI_E2E_AUTH_HASH_SECRET.length).toBeGreaterThanOrEqual(32);
});

test("cluster-recovery fails closed when kind env and artifacts are absent", () => {
  const gate = assessClusterRecoveryGate(
    {},
    {
      exists: () => false,
      readText: () => "",
      repoRoot: "/repo",
    },
  );
  expect(gate.ok).toBe(false);
  if (gate.ok) return;
  const text = formatClusterGateFailure(gate.missing);
  expect(text).toMatch(/fails closed/);
  expect(text).toMatch(/does not skip/);
  expect(gate.missing.join("\n")).toContain(CLUSTER_ENV.id);
  expect(gate.missing.join("\n")).toContain(CLUSTER_ENV.artifacts);
  expect(gate.missing.join("\n")).toContain("kubeconfig");
  expect(gate.missing.join("\n")).toContain(CLUSTER_ENV.taskImage);
  expect(gate.missing.join("\n")).toContain(CLUSTER_ENV.serverImage);
});

test("cluster-recovery refuses the foreign kind cluster and a short auth secret", () => {
  const foreign = assessClusterRecoveryGate(passingEnv(FOREIGN_CLUSTER_ID), passingIO());
  expect(foreign.ok).toBe(false);
  if (!foreign.ok) expect(foreign.missing.join("\n")).toContain(FOREIGN_CLUSTER_ID);

  const kubeconfig = assessClusterRecoveryGate(passingEnv("robustness-ownedcluster"), {
    ...passingIO(),
    readText: () => `current-context: ${FOREIGN_CLUSTER_ID}\n`,
  });
  expect(kubeconfig.ok).toBe(false);

  const shortSecret = assessClusterRecoveryGate(
    { ...passingEnv("robustness-ownedcluster"), [CLUSTER_ENV.hashSecret]: "too-short" },
    passingIO(),
  );
  expect(shortSecret.ok).toBe(false);
  if (!shortSecret.ok) expect(shortSecret.missing.join("\n")).toMatch(/32 characters/);
});

test("cluster-recovery gate accepts an owned robustness kubeconfig without contacting it", () => {
  const gate = assessClusterRecoveryGate(passingEnv("robustness-ownedcluster"), passingIO());
  expect(gate.ok).toBe(true);
  if (!gate.ok) return;
  expect(gate.session.kubeconfig).toBe("/tmp/robustness-owned/kubeconfig");
  expect(gate.session.namespace).toBe("robustness-ownedcluster");
  expect(gate.session.valuesFile).toBe("/repo/helm/caesium/ci/test-values-robustness.yaml");
  expect(gate.session.hashSecret).toBe(UI_E2E_AUTH_HASH_SECRET);
});

test("robustness values are not edited to turn auth on", () => {
  const values = readFileSync(path.join(uiRoot, "../helm/caesium/ci/test-values-robustness.yaml"), "utf8");
  expect(values).not.toContain("CAESIUM_AUTH_MODE");
  expect(countExtraEnvEntries(values)).toBe(14);
});

test("auth overlay is appended with helm --reuse-values and does not replace the shared values file", () => {
  const gate = assessClusterRecoveryGate(passingEnv("robustness-ownedcluster"), passingIO());
  if (!gate.ok) throw new Error("expected a session");
  const existing = [{ name: "CAESIUM_EXECUTION_MODE", value: "distributed" }];
  const plan = planAuthExtraEnv(existing, gate.session.hashSecret);
  expect(plan.action).toBe("append");
  if (plan.action !== "append") return;
  expect(overlayPreservesEnv(plan.overlayYaml, existing)).toBeNull();
  expect(plan.overlayYaml).toContain('name: "CAESIUM_AUTH_MODE"');
  expect(plan.overlayYaml).toContain('value: "api-key"');
  expect(plan.overlayYaml).toContain('name: "CAESIUM_AUTH_REQUIRE_TLS"');
  expect(plan.overlayYaml).toContain('value: "false"');
  expect(planAuthExtraEnv([...existing, { name: "CAESIUM_AUTH_MODE", value: "api-key" }], gate.session.hashSecret).action).toBe(
    "unchanged",
  );
  expect(planAuthExtraEnv([{ name: "CAESIUM_AUTH_MODE", value: "none" }], gate.session.hashSecret)).toEqual({
    action: "refused",
    reason: "release already sets CAESIUM_AUTH_MODE=none",
  });

  const overlay = "/tmp/robustness-owned/d3-auth-overlay.yaml";
  const upgrade = helmAuthUpgradeCommand(gate.session, overlay);
  expect(upgrade.argv).toContain("--reuse-values");
  expect(upgrade.argv).toContain("--values");
  expect(upgrade.argv).toContain(overlay);
  expect(upgrade.argv).not.toContain(gate.session.valuesFile);
  expect(commandViolation(upgrade.argv)).toBeNull();
  expect(() => helmAuthUpgradeCommand(gate.session, gate.session.valuesFile)).toThrow(/test-values-robustness\.yaml/);
});

test("helm release values parse extraEnv and reject a missing list", () => {
  expect(
    extraEnvFromReleaseValues({
      config: { extraEnv: [{ name: "CAESIUM_DATABASE_VOTERS", value: 3 }] },
    }),
  ).toEqual([{ name: "CAESIUM_DATABASE_VOTERS", value: "3" }]);
  expect(extraEnvFromReleaseValues({ config: {} })).toEqual({ error: "helm values config.extraEnv is not a list" });
});

test("owner fault commands cordon, stop kubelet, SIGKILL through ctr, then list tasks", () => {
  const commands = ownerKillSequence({
    kubeconfig: "/tmp/robustness-owned/kubeconfig",
    node: "robustness-worker",
    containerID: `containerd://${containerID}`,
  });
  expect(commands.map((command) => command.argv)).toEqual([
    ["kubectl", "--kubeconfig", "/tmp/robustness-owned/kubeconfig", "cordon", "robustness-worker"],
    ["docker", "exec", "robustness-worker", "systemctl", "stop", "kubelet"],
    ["docker", "exec", "robustness-worker", "ctr", "-n", "k8s.io", "tasks", "kill", "--signal", "SIGKILL", containerID],
    ["docker", "exec", "robustness-worker", "ctr", "-n", "k8s.io", "tasks", "list"],
  ]);
  for (const command of commands) expect(commandViolation(command.argv)).toBeNull();
  expect(ownerRestartSequence({ kubeconfig: "/tmp/robustness-owned/kubeconfig", node: "robustness-worker" }).map((command) => command.argv)).toEqual([
    ["docker", "exec", "robustness-worker", "systemctl", "start", "kubelet"],
    ["kubectl", "--kubeconfig", "/tmp/robustness-owned/kubeconfig", "uncordon", "robustness-worker"],
  ]);
  expect(stripRuntimeContainerID(`containerd://${containerID}`)).toBe(containerID);
  expect(commandViolation(["kubectl", "--kubeconfig", "k", "delete", "pod", "caesium-0"])).toMatch(/kubectl delete/);
  expect(commandViolation(["kind", "delete", "cluster"])).toMatch(/kind delete/);
  expect(commandViolation(["docker", "rm", "-f", "robustness-worker"])).toMatch(/kind node/);
  expect(() => ownerKillSequence({ kubeconfig: "k", node: "kind-control-plane", containerID })).toThrow(/node/);
});

test("kill evidence matches the harness death rules", () => {
  const refused = `kubelet stopped on worker\nctr kill ${containerID}\nctr: failed to dial containerd: connection refused\n`;
  expect(killEvidenceShowsDeath(refused, containerID)).toBe(false);
  const header = `kubelet stopped on worker\nctr kill ${containerID}\n`;
  expect(killEvidenceShowsDeath(header, containerID)).toBe(false);
  expect(killEvidenceShowsDeath(`${header}TASK PID STATUS\n${containerID} 1 RUNNING\n`, containerID)).toBe(false);
  expect(killEvidenceShowsDeath(`${header}TASK PID STATUS\n${containerID.slice(0, 12)} 1 STOPPED\n`, containerID)).toBe(true);
  expect(killEvidenceShowsDeath(`${header}TASK                                PID      STATUS\n`, containerID)).toBe(true);
  expect(taskDeadFromListing(containerID, "ctr: failed to dial containerd: connection refused", true).dead).toBe(false);
  expect(taskDeadFromListing(containerID, "TASK PID STATUS\n", false).dead).toBe(false);
  expect(taskDeadFromListing(containerID, "TASK PID STATUS\n", true).dead).toBe(true);
  expect(killEvidenceShowsDeath(formatKillEvidence("robustness-worker", containerID, "TASK PID STATUS\n"), containerID)).toBe(true);
});

test("owner selection skips the control plane and task pods must miss that node", () => {
  const pods = {
    items: [
      memberPod("caesium-0", "robustness-control-plane", "10.0.0.1", containerID),
      memberPod("caesium-2", "robustness-worker-b", "10.0.0.3", `${containerID.slice(0, -1)}b`),
      memberPod("caesium-1", "robustness-worker-a", "10.0.0.2", `${containerID.slice(0, -1)}a`),
    ],
  };
  const chosen = chooseOwner(membersFromPodList(pods));
  if ("error" in chosen) throw new Error(chosen.error);
  expect(chosen.owner.name).toBe("caesium-1");
  expect(chosen.survivors.map((member) => member.name)).toEqual(["caesium-2"]);

  const run = "22222222-2222-4222-8222-222222222222";
  expect(taskPlacementIssues({ items: [taskPod(`hold-${run}`, "robustness-worker-b")] }, run, "robustness-worker-a")).toEqual([]);
  expect(taskPlacementIssues({ items: [taskPod(`hold-${run}`, "robustness-worker-a")] }, run, "robustness-worker-a").join("\n")).toMatch(
    /owner node/,
  );
  expect(taskPlacementIssues({ items: [] }, run, "robustness-worker-a").join("\n")).toMatch(/no task pod/);
});

test("lease query accepts only a uuid and reads the harness row shape", () => {
  expect(leaseQueryBody("not-a-uuid")).toEqual({ error: "lease query refused unvalidated run id not-a-uuid" });
  const body = leaseQueryBody(runId);
  if ("error" in body) throw new Error(body.error);
  expect(body.sql).toContain(runId);
  expect(body.limit).toBe(1);
  expect(parseLeaseResponse({ rows: [[runId, "10.0.0.2:9001", 2, "2099-01-01T00:00:00Z"]] }, runId)).toEqual({
    runId,
    ownerNode: "10.0.0.2:9001",
    generation: 2,
  });
  expect(endpointHost("10.0.0.2:9001")).toBe("10.0.0.2");
});

test("bootstrap admin key is read from one pod's full caesium container log", () => {
  // kubectl logs defaults to --tail=10 only when a selector is given; a named
  // pod without --tail returns the whole live file.
  const command = memberLogCommand({
    robustnessId: "rb-d3-example",
    artifactsDir: "/tmp/caesium-d3-example",
    kubeconfig: "/tmp/caesium-d3-example/kubeconfig",
    namespace: "rb-d3-example",
    taskImage: "example.invalid/task:1",
    serverImage: "example.invalid/caesium:abc",
    chartDir: "/tmp/chart",
    valuesFile: "/tmp/values.yaml",
    repoRoot: "/tmp/repo",
    hashSecret: UI_E2E_AUTH_HASH_SECRET,
  }, "caesium-0", false);
  expect(command.argv).toEqual([
    "kubectl", "--kubeconfig", "/tmp/caesium-d3-example/kubeconfig", "--namespace", "rb-d3-example",
    "logs", "caesium-0", "-c", "caesium",
  ]);
  expect(command.argv).not.toContain("-l");
  expect(command.argv.some((arg) => arg.startsWith("--tail"))).toBe(false);
});

test("api-key auth must be on every listed member", () => {
  const withMode = (pod: ReturnType<typeof memberPod>, ...modes: string[]) => ({
    ...pod,
    spec: { ...pod.spec, containers: [{ name: "caesium", env: modes.map((value) => ({ name: "CAESIUM_AUTH_MODE", value })) }] },
  });
  const a = memberPod("caesium-0", "robustness-worker-a", "10.0.0.1", containerID);
  const b = memberPod("caesium-1", "robustness-worker-b", "10.0.0.2", `${containerID.slice(0, -1)}b`);
  expect(apiKeyAuthViolation({ items: [withMode(a, "api-key"), withMode(b, "api-key")] })).toBeNull();
  expect(apiKeyAuthViolation({ items: [withMode(a, "api-key"), withMode(b, "none")] })).toMatch(/caesium-1.*none/);
  expect(apiKeyAuthViolation({ items: [withMode(a, "api-key"), b] })).toMatch(/caesium-1.*\(unset\)/);
  expect(apiKeyAuthViolation({ items: [withMode(a, "api-key"), withMode(b, "api-key", "none")] })).toMatch(/caesium-1/);
  expect(apiKeyAuthViolation({ items: [] })).toMatch(/no caesium members/);
});

test("rotated node log files are a fallback source for the bootstrap banner", () => {
  const uid = "7c4f1a2e-0b3d-4e5f-8a9b-0c1d2e3f4a5b";
  const listed = {
    items: [
      { metadata: { name: "caesium-2", uid }, spec: { nodeName: "robustness-control-plane" } },
      { metadata: { name: "caesium-1" }, spec: { nodeName: "robustness-worker" } },
    ],
  };
  const sources = nodeLogSourcesFromPodList(listed);
  expect(sources).toEqual([{ name: "caesium-2", node: "robustness-control-plane", uid }]);
  const command = nodePodLogCommand("rb-d3-example", sources[0]);
  expect(command.argv.slice(0, 5)).toEqual(["docker", "exec", "robustness-control-plane", "sh", "-c"]);
  expect(command.argv.at(-1)).toBe(`/var/log/pods/rb-d3-example_caesium-2_${uid}/caesium`);
  expect(command.argv[5]).toContain("gzip -dc");
  expect(commandViolation(command.argv)).toBeNull();
  expect(() => nodePodLogCommand("rb-d3-example", { ...sources[0], uid: "../x" })).toThrow(/uid/);
  expect(() => nodePodLogCommand("rb_d3", sources[0])).toThrow(/namespace/);
  expect(() => nodePodLogCommand("rb-d3-example", { ...sources[0], node: FOREIGN_CLUSTER_ID })).toThrow(/node/);

  const cri = [
    '2026-09-26T20:08:00.000000001Z stderr F time=... VALUES ("id","csk_live_abcD","hmac")',
    "2026-09-26T20:08:00.000000002Z stdout F ==========================================================",
    "2026-09-26T20:08:00.000000003Z stdout F   BOOTSTRAP ADMIN API KEY (shown once, save it now):",
    "2026-09-26T20:08:00.000000004Z stdout P   csk_live_abcDEF1234",
    "2026-09-26T20:08:00.000000005Z stdout F 567890abcd",
  ].join("\n");
  expect(criLogText(cri)).toContain("BOOTSTRAP ADMIN API KEY (shown once, save it now):\n  csk_live_abcDEF1234567890abcd\n");
  expect(parseBootstrapAdminKey(criLogText(cri))).toBe("csk_live_abcDEF1234567890abcd");
});

test("auth mode is read from the caesium container spec and logs are per pod", () => {
  expect(authModesFromPodList({
    items: [{
      metadata: { name: "caesium-0" },
      spec: { containers: [{ name: "caesium", env: [{ name: "CAESIUM_AUTH_MODE", value: "api-key" }] }] },
    }],
  })).toEqual([{ name: "caesium-0", mode: "api-key" }]);
  const command = memberLogCommand({
    robustnessId: "rb-d3-example",
    artifactsDir: "/tmp/caesium-d3-example",
    kubeconfig: "/tmp/caesium-d3-example/kubeconfig",
    namespace: "rb-d3-example",
    taskImage: "example.invalid/task:1",
    serverImage: "example.invalid/caesium:abc",
    chartDir: "/tmp/chart",
    valuesFile: "/tmp/values.yaml",
    repoRoot: "/tmp/repo",
    hashSecret: UI_E2E_AUTH_HASH_SECRET,
  }, "caesium-0", true);
  expect(command.argv).toContain("caesium-0");
  expect(command.argv).toContain("--previous");
  expect(() => memberLogCommand({
    robustnessId: "rb-d3-example",
    artifactsDir: "/tmp/caesium-d3-example",
    kubeconfig: "/tmp/caesium-d3-example/kubeconfig",
    namespace: "rb-d3-example",
    taskImage: "example.invalid/task:1",
    serverImage: "example.invalid/caesium:abc",
    chartDir: "/tmp/chart",
    valuesFile: "/tmp/values.yaml",
    repoRoot: "/tmp/repo",
    hashSecret: UI_E2E_AUTH_HASH_SECRET,
  }, "Pod_Bad", false)).toThrow(/unsafe pod name/);
});

test("bootstrap admin key is the csk_live token from pod logs", () => {
  const text = [
    "==========================================================",
    "  BOOTSTRAP ADMIN API KEY (shown once, save it now):",
    "  csk_live_abcDEF1234567890abcd",
    "==========================================================",
  ].join("\n");
  expect(parseBootstrapAdminKey(text)).toBe("csk_live_abcDEF1234567890abcd");
  expect(parseBootstrapAdminKey("no key here")).toBeNull();
  const prefixed = [
    'VALUES ("id","csk_live_abcD","hmac")',
    "BOOTSTRAP ADMIN API KEY (shown once, save it now):",
    "  csk_live_abcDEF1234567890abcd",
  ].join("\n");
  expect(parseBootstrapAdminKey(prefixed)).toBe("csk_live_abcDEF1234567890abcd");
  expect(parseBootstrapAdminKey('key_prefix":"csk_live_abcD"')).toBeNull();
});

test("console job is a kubernetes hold that prints a marker and waits for a release", () => {
  const definition = consoleRecoveryDefinition("d3-console-abcdef123456", "example.invalid/task:1", "d3-marker-abcdef123456");
  const step = (definition.steps as { engine: string; command: string[] }[])[0];
  expect(step.engine).toBe("kubernetes");
  expect(step.command.slice(0, 2)).toEqual(["sh", "-c"]);
  expect(step.command[2]).toBe(consoleHoldScript("d3-marker-abcdef123456"));
  expect(step.command[2]).toContain("echo d3-marker-abcdef123456");
  expect(step.command[2]).toContain(`[ ! -e ${TASK_RELEASE_FILE} ]`);
  expect(step.command[2]).toContain(`-gt ${HOLD_LIMIT_SECONDS} ]`);
  expect(step.command[2]).not.toMatch(/sleep 9\d\d/);
  // The hold must outlive the spec's 900 s test budget.
  expect(HOLD_LIMIT_SECONDS).toBeGreaterThan(900);
  expect(() => consoleRecoveryDefinition("Bad Alias", "img", "d3-marker-abcdef123456")).toThrow(/alias/);
  expect(() => consoleHoldScript("d3-marker-abcdef123456", { releaseFile: "relative/file" })).toThrow(/release file/);
  expect(() => consoleHoldScript("d3-marker-abcdef123456", { releaseFile: "/tmp/x; rm -rf /" })).toThrow(/release file/);
  expect(() => consoleHoldScript("d3-marker-abcdef123456", { limitSeconds: 0 })).toThrow(/hold limit/);
});

test("the hold exits 0 only after its release file exists, and fails when never released", () => {
  const dir = mkdtempSync(path.join(os.tmpdir(), "d3-hold-"));
  try {
    const releaseFile = path.join(dir, "release");
    const marker = "d3-marker-abcdef123456";

    const unreleased = spawnSync("sh", ["-c", consoleHoldScript(marker, { releaseFile, limitSeconds: 1 })], {
      encoding: "utf8",
      timeout: 20_000,
    });
    expect(unreleased.status).toBe(1);
    expect(unreleased.stdout).toContain(marker);
    expect(unreleased.stdout).not.toContain(`${marker} released`);
    expect(unreleased.stderr).toContain("hold was never released");

    writeFileSync(releaseFile, "");
    const releasedRun = spawnSync("sh", ["-c", consoleHoldScript(marker, { releaseFile, limitSeconds: 1 })], {
      encoding: "utf8",
      timeout: 20_000,
    });
    expect(releasedRun.status).toBe(0);
    expect(releasedRun.stdout).toBe(`${marker}\n${marker} released\n`);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("release targets every running task pod of the run, once, and never the owner node", () => {
  const run = "22222222-2222-4222-8222-222222222222";
  const pods = {
    items: [
      memberPod("caesium-1", "robustness-worker-a", "10.0.0.2", containerID),
      taskPod(`hold-${run}-first`, "robustness-worker-b"),
      taskPod(`hold-${run}-attempt1-second`, "robustness-worker-c"),
      { ...taskPod(`hold-${run}-pending`, ""), status: { phase: "Pending", containerStatuses: [{ name: TASK_CONTAINER }] } },
      { ...taskPod(`hold-${run}-waiting`, "robustness-worker-c"), status: waitingStatus() },
      { ...taskPod(`hold-${run}-gone`, "robustness-worker-b"), metadata: { name: `hold-${run}-gone`, deletionTimestamp: "x" } },
      taskPod("hold-33333333-3333-4333-8333-333333333333-other", "robustness-worker-b"),
    ],
  };
  const listed = runTaskPods(pods, run);
  expect(listed.map((pod) => [pod.name, pod.node, pod.running])).toEqual([
    [`hold-${run}-first`, "robustness-worker-b", true],
    [`hold-${run}-attempt1-second`, "robustness-worker-c", true],
    [`hold-${run}-pending`, "", false],
    [`hold-${run}-waiting`, "robustness-worker-c", false],
  ]);
  expect(runTaskPods(pods, "not-a-uuid")).toEqual([]);

  const first = releaseTargets(listed, "robustness-worker-a", new Set());
  expect(first).toEqual({ targets: [`hold-${run}-first`, `hold-${run}-attempt1-second`], issues: [] });
  const again = releaseTargets(listed, "robustness-worker-a", new Set([`hold-${run}-first`]));
  expect(again.targets).toEqual([`hold-${run}-attempt1-second`]);

  const onOwner = releaseTargets([{ name: `hold-${run}-x`, node: "robustness-worker-a", running: true }], "robustness-worker-a", new Set());
  expect(onOwner.targets).toEqual([]);
  expect(onOwner.issues.join("\n")).toMatch(/owner node/);
});

test("placement is checked on the terminal sample too; release stops once the run is terminal", () => {
  const run = "22222222-2222-4222-8222-222222222222";
  const owner = "robustness-worker-a";
  const released = new Set<string>();
  // running: release the survivor pod, no placement issue
  const running = releaseSample({
    status: "running",
    pods: [{ name: `hold-${run}-attempt1-x`, node: "robustness-worker-b", running: true }],
    ownerNode: owner,
    released,
  });
  expect(running).toEqual({ terminal: false, targets: [`hold-${run}-attempt1-x`], issues: [] });
  // -> succeeded: a pod of the run bound to the faulted node on the final
  // sample is still a placement violation, and nothing more is released.
  const terminal = releaseSample({
    status: "succeeded",
    pods: [
      { name: `hold-${run}-attempt1-x`, node: "robustness-worker-b", running: false },
      { name: `hold-${run}-attempt3-y`, node: owner, running: false },
    ],
    ownerNode: owner,
    released: new Set([`hold-${run}-attempt1-x`]),
  });
  expect(terminal.terminal).toBe(true);
  expect(terminal.targets).toEqual([]);
  expect(terminal.issues).toEqual([`task pod hold-${run}-attempt3-y is on the owner node ${owner}`]);
  const failedEarly = releaseSample({
    status: "failed",
    pods: [{ name: `hold-${run}-attempt1-x`, node: "robustness-worker-b", running: true }],
    ownerNode: owner,
    released,
  });
  expect(failedEarly).toEqual({ terminal: true, targets: [], issues: [] });

  // The spec records placement issues before it returns on a terminal sample.
  const source = readFileSync(path.join(uiRoot, "e2e/cluster-recovery.spec.ts"), "utf8");
  const sampled = source.indexOf("releaseSample({ status: snapshot.status");
  const recorded = source.indexOf("for (const issue of sample.issues) strayPods.add(issue);");
  const terminalReturn = source.indexOf("if (sample.terminal) {");
  expect(sampled).toBeGreaterThan(0);
  expect(recorded).toBeGreaterThan(sampled);
  expect(terminalReturn).toBeGreaterThan(recorded);
  expect(source).not.toMatch(/if \(isTerminalRunStatus\(snapshot\.status\)\) \{\s*return \{ snapshot/);
});

test("a port-forward start that times out stops and reaps its child before rejecting", async () => {
  const spawned: ChildProcess[] = [];
  const spawnFn = (cmd: string, args: readonly string[], opts: { stdio: ["ignore", "pipe", "pipe"]; env: NodeJS.ProcessEnv }) => {
    const child = spawn(cmd, [...args], opts);
    spawned.push(child);
    return child;
  };
  const silent = { argv: ["sh", "-c", "exec sleep 30"], description: "silent forward" };
  await expect(startReadyChild(silent, { readyText: "Forwarding from", timeoutMs: 200, spawnFn })).rejects.toThrow(
    /silent forward did not become ready within 200ms/,
  );
  expect(spawned).toHaveLength(1);
  expect(spawned[0].exitCode !== null || spawned[0].signalCode !== null).toBe(true);

  const early = { argv: ["sh", "-c", "echo nope; exit 3"], description: "early forward" };
  await expect(startReadyChild(early, { readyText: "Forwarding from", timeoutMs: 5_000, spawnFn })).rejects.toThrow(
    /early forward exited 3[\s\S]*nope/,
  );

  const missing = { argv: ["/nonexistent/d3-kubectl"], description: "missing forward" };
  await expect(startReadyChild(missing, { readyText: "Forwarding from", timeoutMs: 5_000, spawnFn })).rejects.toThrow(/ENOENT/);

  const ready = { argv: ["sh", "-c", "echo 'Forwarding from 127.0.0.1:1 -> 2'; exec sleep 30"], description: "ready forward" };
  const child = await startReadyChild(ready, { readyText: "Forwarding from", timeoutMs: 5_000, spawnFn });
  expect(child.exitCode).toBeNull();
  await stopChildProcess(child);
  expect(child.signalCode).toBe("SIGTERM");

  await expect(
    startReadyChild({ argv: ["kubectl", "delete", "pod", "x"], description: "bad" }, { readyText: "x", timeoutMs: 1, spawnFn }),
  ).rejects.toThrow(/kubectl delete/);
  // Refused before spawning.
  expect(spawned).toHaveLength(4);
});

test("stopping a child that ignores SIGTERM escalates to SIGKILL", async () => {
  const stubborn = spawn("sh", ["-c", "trap '' TERM; echo up; while :; do sleep 1; done"], { stdio: ["ignore", "pipe", "pipe"] });
  await new Promise<void>((resolve) => stubborn.stdout?.once("data", () => resolve()));
  await stopChildProcess(stubborn, 300);
  expect(stubborn.signalCode).toBe("SIGKILL");
  await stopChildProcess(stubborn, 300);
  await stopChildProcess(undefined);
});

test("release is a kubectl exec touch in the task container of a pod named for the run", () => {
  const run = "22222222-2222-4222-8222-222222222222";
  const command = taskReleaseCommand("/tmp/robustness-owned/kubeconfig", "robust-1", `hold-${run}-abc`, run);
  expect(command.argv).toEqual([
    "kubectl",
    "--kubeconfig",
    "/tmp/robustness-owned/kubeconfig",
    "--namespace",
    "robust-1",
    "exec",
    `hold-${run}-abc`,
    "-c",
    TASK_CONTAINER,
    "--",
    "touch",
    TASK_RELEASE_FILE,
  ]);
  expect(commandViolation(command.argv)).toBeNull();
  expect(() => taskReleaseCommand("k", "ns", "caesium-0", run)).toThrow(/refusing to release/);
  expect(() => taskReleaseCommand("k", "ns", `Hold-${run}`, run)).toThrow(/refusing to release/);
  expect(() => taskReleaseCommand("k", "ns", `hold-${run};rm`, run)).toThrow(/refusing to release/);
  expect(() => taskReleaseCommand("k", "ns", "hold-x", "not-a-uuid")).toThrow(/unvalidated run id/);
});

test("the run-history click is scoped to the runs list, not the live overlay behind the dialog", () => {
  const job = "44444444-4444-4444-8444-444444444444";
  expect(runHistoryLinkSelector(job, runId)).toBe(`a[href*="/jobs/${job}/runs/${runId}"]`);
  expect(runHistoryLinkSelector(job.toUpperCase(), runId.toUpperCase())).toBe(`a[href*="/jobs/${job}/runs/${runId}"]`);
  expect(() => runHistoryLinkSelector(job, "x\"]")).toThrow(/run id/);
  expect(() => runHistoryLinkSelector("x", runId)).toThrow(/job id/);

  const source = readFileSync(path.join(uiRoot, "e2e/cluster-recovery.spec.ts"), "utf8");
  expect(source).toContain('page.getByTestId("job-runs-list").locator(runHistoryLinkSelector(jobId, runId))');
  expect(source).not.toMatch(/page\.locator\(`a\[href\*="\/runs\//);
});

test("run-history rows need a readable status; glued row text is not a status", () => {
  // Live a1 evidence: the whole row textContent glued duration and badge.
  expect(statusFromRowText("2026-09-27 01:47:06 UTCjust now · 982b3460 · 33.7ssucceeded")).toBeNull();
  expect(statusFromRowText("succeeded")).toBe("succeeded");
  expect(statusFromRowText(" Running ")).toBe("running");

  const unreadable = passingJourney();
  unreadable.console.runRows = [{ id: runId, status: null }];
  expect(convergenceIssues(unreadable)).toContainEqual({
    code: "stale_row",
    detail: `run list run ${runId} has no readable status`,
  });
  const unreadableAfterReload = passingJourney();
  unreadableAfterReload.console.reloadedRunRows = [{ id: runId, status: null }];
  expect(codes(unreadableAfterReload)).toEqual(["stale_row"]);
  expect(codes(passingJourney())).toEqual([]);
});

test("terminal status and completion-after-instant checks fail closed", () => {
  expect(isTerminalRunStatus("succeeded")).toBe(true);
  expect(isTerminalRunStatus(" Failed ")).toBe(true);
  expect(isTerminalRunStatus("cancelled")).toBe(true);
  expect(isTerminalRunStatus("running")).toBe(false);
  expect(isTerminalRunStatus("")).toBe(false);

  const release = Date.parse("2026-09-26T21:05:00.000Z");
  expect(completedAtOrAfter("2026-09-26T21:05:03.000Z", release)).toBe(true);
  expect(completedAtOrAfter("2026-09-26T21:04:59.000Z", release)).toBe(true);
  expect(completedAtOrAfter("2026-09-26T21:04:57.000Z", release)).toBe(false);
  expect(completedAtOrAfter("", release)).toBe(false);
  expect(completedAtOrAfter("not a time", release)).toBe(false);
  // Never released: a success cannot be attributed to a release.
  expect(completedAtOrAfter("2026-09-26T21:05:03.000Z", 0)).toBe(false);
});

test("convergence rejects duplicate, stale, false success, and a fault that was not seen while connected", () => {
  expect(convergenceIssues(passingJourney())).toEqual([]);

  const missingFault = passingJourney();
  missingFault.fault.connectedBeforeFault = false;
  expect(codes(missingFault)).toEqual(["missing_fault_while_connected"]);

  const lateRecord = passingJourney();
  lateRecord.fault.recordedAt = 5;
  lateRecord.fault.assertionAt = 5;
  expect(codes(lateRecord)).toContain("missing_fault_while_connected");

  const duplicate = passingJourney();
  duplicate.console.runRows = [
    { id: runId, status: "succeeded" },
    { id: runId, status: "succeeded" },
  ];
  expect(codes(duplicate)).toContain("duplicate_row");

  const stale = passingJourney();
  stale.console.runRows = [
    { id: runId, status: "succeeded" },
    { id: "33333333-3333-4333-8333-333333333333", status: "succeeded" },
  ];
  expect(codes(stale)).toContain("stale_row");

  const falseSuccess = passingJourney();
  falseSuccess.durable.ownerAfter = falseSuccess.durable.ownerBefore;
  falseSuccess.durable.generationAfter = falseSuccess.durable.generationBefore;
  expect(codes(falseSuccess)).toContain("false_terminal_success");
  expect(codes(falseSuccess)).toContain("survivor_not_recorded");

  const earlySuccess = passingJourney();
  earlySuccess.console.showedSuccessBeforeFault = true;
  expect(codes(earlySuccess)).toContain("false_terminal_success");

  const noStream = passingJourney();
  noStream.console.eventStreamAttempts = 0;
  noStream.console.eventStreamAuthorized = 0;
  noStream.console.authenticatedRunReads = 0;
  expect(codes(noStream)).toEqual(["event_stream_not_recovered"]);

  const sameGeneration = passingJourney();
  sameGeneration.durable.generationAfter = sameGeneration.durable.generationBefore;
  expect(codes(sameGeneration)).toContain("survivor_not_recorded");

  const sameOwner = passingJourney();
  sameOwner.durable.ownerAfter = sameOwner.durable.ownerBefore;
  expect(codes(sameOwner)).toContain("survivor_not_recorded");

  const unseenFault = passingJourney();
  unseenFault.fault.sawDisconnectOrStatusChange = false;
  expect(codes(unseenFault)).toEqual(["missing_fault_while_connected"]);

  const staleHeading = passingJourney();
  staleHeading.console.headingStatus = "running";
  expect(codes(staleHeading)).toContain("status_mismatch");

  const missingLog = passingJourney();
  missingLog.console.logText = "line without the marker";
  expect(codes(missingLog)).toContain("log_mismatch");

  const liveBadge = passingJourney();
  liveBadge.console.logSourceLabel = "Live stream";
  expect(codes(liveBadge)).toContain("retained_log_not_shown");

  const apiKeyFallback = passingJourney();
  apiKeyFallback.console.eventStreamAttempts = 2;
  apiKeyFallback.console.eventStreamAuthorized = 0;
  apiKeyFallback.console.authenticatedRunReads = 1;
  expect(codes(apiKeyFallback)).toEqual([]);

  const pollWithoutEvents = passingJourney();
  pollWithoutEvents.console.eventStreamAttempts = 0;
  pollWithoutEvents.console.eventStreamAuthorized = 0;
  pollWithoutEvents.console.authenticatedRunReads = 3;
  expect(codes(pollWithoutEvents)).toEqual(["event_stream_not_recovered"]);

  const unauthorizedEvents = passingJourney();
  unauthorizedEvents.console.eventStreamAttempts = 2;
  unauthorizedEvents.console.eventStreamAuthorized = 0;
  unauthorizedEvents.console.authenticatedRunReads = 0;
  expect(codes(unauthorizedEvents)).toEqual(["event_stream_not_recovered"]);

  const lostReload = passingJourney();
  lostReload.console.reloadedLogText = "";
  expect(codes(lostReload)).toContain("reload_lost_data");

  const staleStatus = passingJourney();
  staleStatus.console.runRows = [{ id: runId, status: "running" }];
  expect(codes(staleStatus)).toContain("stale_row");
});

test("the flattened task image must already be loaded on the node", () => {
  expect(robustnessTaskImage("robust-1")).toBe("caesium-robustness-task:robust-1");
  expect(taskImageListed("caesium-robustness-task:robust-1  sha256:abc\n", "caesium-robustness-task:robust-1")).toBe(true);
  expect(taskImageListed("alpine:3.23\n", "caesium-robustness-task:robust-1")).toBe(false);
});

test("the cluster spec fails closed instead of skipping", () => {
  const source = readFileSync(path.join(uiRoot, "e2e/cluster-recovery.spec.ts"), "utf8");
  expect(source).toContain("formatClusterGateFailure");
  expect(source).not.toMatch(/test\.skip\s*\(/);
  expect(source).not.toMatch(/test\.fixme\s*\(/);
  expect(source).toContain("SIGKILL");
  expect(source).not.toMatch(/kubectl["',\s]+delete/);
  // A failed run must keep an intact trace; the embedded video stalled its zip.
  expect(source).toContain('test.use({ video: "off" })');
  // Port-forwards start through startReadyChild, which reaps a failed start.
  expect(source).toContain("startReadyChild(command,");
  expect(source).not.toMatch(/\bspawn\(/);
});

function codes(input: { durable: DurableOutcome; console: ConsoleSurface; fault: FaultObservation }): string[] {
  return convergenceIssues(input).map((issue) => issue.code);
}

function passingJourney(): { durable: DurableOutcome; console: ConsoleSurface; fault: FaultObservation } {
  return {
    durable: {
      runId,
      status: "succeeded",
      generationBefore: 1,
      generationAfter: 2,
      ownerBefore: "10.0.0.1:9001",
      ownerAfter: "10.0.0.2:9001",
      logExcerpt: "d3-marker-abcdef123456",
    },
    fault: {
      connectedBeforeFault: true,
      sawDisconnectOrStatusChange: true,
      recordedAt: 1,
      assertionAt: 2,
    },
    console: {
      headingStatus: "succeeded",
      headingCount: 1,
      runRows: [{ id: runId, status: "succeeded" }],
      logText: "line\nd3-marker-abcdef123456\n",
      logSourceLabel: "Retained snapshot",
      eventStreamAttempts: 1,
      eventStreamAuthorized: 1,
      authenticatedRunReads: 0,
      showedSuccessBeforeFault: false,
      reloadedHeadingStatus: "succeeded",
      reloadedHeadingCount: 1,
      reloadedRunRows: [{ id: runId, status: "succeeded" }],
      reloadedLogText: "d3-marker-abcdef123456",
      reloadedLogSourceLabel: "Retained snapshot",
    },
  };
}

function passingEnv(id: string): NodeJS.ProcessEnv {
  return {
    [CLUSTER_ENV.id]: id,
    [CLUSTER_ENV.artifacts]: "/tmp/robustness-owned",
    [CLUSTER_ENV.taskImage]: "example.invalid/task:1",
    [CLUSTER_ENV.serverImage]: "caesiumcloud/caesium:abc123",
  };
}

function passingIO() {
  return {
    exists: () => true,
    readText: () => "apiVersion: v1\nclusters: []\n",
    repoRoot: "/repo",
  };
}

function memberPod(name: string, node: string, ip: string, id: string) {
  return {
    metadata: { name },
    spec: { nodeName: node },
    status: {
      phase: "Running",
      podIP: ip,
      conditions: [{ type: "Ready", status: "True" }],
      containerStatuses: [{ name: "caesium", containerID: `containerd://${id}`, ready: true }],
    },
  };
}

function taskPod(name: string, node: string) {
  return {
    metadata: { name },
    spec: { nodeName: node },
    status: { phase: "Running", containerStatuses: [{ name: "atom", state: { running: { startedAt: "2026-09-26T21:03:40Z" } } }] },
  };
}

function waitingStatus() {
  return { phase: "Running", containerStatuses: [{ name: "atom", state: { waiting: { reason: "ContainerCreating" } } }] };
}
