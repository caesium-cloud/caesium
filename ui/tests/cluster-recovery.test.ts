// @vitest-environment node

import { readFileSync } from "node:fs";
import path from "node:path";
import {
  CLUSTER_ENV,
  FOREIGN_CLUSTER_ID,
  UI_E2E_AUTH_HASH_SECRET,
  assessClusterRecoveryGate,
  chooseOwner,
  commandViolation,
  consoleRecoveryDefinition,
  countExtraEnvEntries,
  convergenceIssues,
  endpointHost,
  extraEnvFromReleaseValues,
  formatClusterGateFailure,
  formatKillEvidence,
  helmAuthUpgradeCommand,
  killEvidenceShowsDeath,
  leaseQueryBody,
  membersFromPodList,
  overlayPreservesEnv,
  ownerKillSequence,
  ownerRestartSequence,
  parseBootstrapAdminKey,
  parseLeaseResponse,
  robustnessTaskImage,
  planAuthExtraEnv,
  stripRuntimeContainerID,
  taskDeadFromListing,
  taskImageListed,
  taskPlacementIssues,
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

test("bootstrap admin key is the csk_live token from pod logs", () => {
  const text = [
    "==========================================================",
    "  BOOTSTRAP ADMIN API KEY (shown once, save it now):",
    "  csk_live_abcDEF123",
    "==========================================================",
  ].join("\n");
  expect(parseBootstrapAdminKey(text)).toBe("csk_live_abcDEF123");
  expect(parseBootstrapAdminKey("no key here")).toBeNull();
});

test("console job is a kubernetes hold that prints a marker", () => {
  const definition = consoleRecoveryDefinition("d3-console-abcdef123456", "example.invalid/task:1", "d3-marker-abcdef123456");
  const step = (definition.steps as { engine: string; command: string[] }[])[0];
  expect(step.engine).toBe("kubernetes");
  expect(step.command[2]).toContain("d3-marker-abcdef123456");
  expect(step.command[2]).toContain("sleep 900");
  expect(() => consoleRecoveryDefinition("Bad Alias", "img", "d3-marker-abcdef123456")).toThrow(/alias/);
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
    status: { phase: "Running", containerStatuses: [{ name: "atom" }] },
  };
}
