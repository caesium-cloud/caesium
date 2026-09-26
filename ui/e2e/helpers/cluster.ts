import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

/**
 * Pure command construction and evidence checks for the console owner-crash
 * journey. Nothing in this module contacts Docker, kind, or a cluster.
 * The Playwright spec is the only caller that executes the commands, and only
 * after the gate has the robustness kubeconfig in hand.
 */

export const FOREIGN_CLUSTER_ID = "caesium-qa-20260914";
export const ROBUSTNESS_RELEASE = "caesium";
export const CONSOLE_SERVICE = "caesium";
export const CONSOLE_SERVICE_PORT = 8080;
export const OWNER_CONSOLE_PORT = 18080;
export const SERVICE_CONSOLE_PORT = 18081;
export const UI_E2E_AUTH_HASH_SECRET = "ui-e2e-auth-key-hash-secret-000001";

export const CLUSTER_ENV = {
  id: "CAESIUM_ROBUSTNESS_ID",
  artifacts: "CAESIUM_ROBUSTNESS_ARTIFACTS",
  taskImage: "CAESIUM_ROBUSTNESS_TASK_IMAGE",
  serverImage: "CAESIUM_ROBUSTNESS_SERVER_IMAGE",
  hashSecret: "CAESIUM_AUTH_KEY_HASH_SECRET",
} as const;

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const SAFE_ID_RE = /^[a-z0-9][a-z0-9-]{7,80}$/;
const SAFE_NODE_RE = /^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$/;
const CONTAINER_ID_RE = /^[a-f0-9]{8,128}$/;
const RUN_STATUS_RE = /\b(succeeded|failed|cancelled|running|queued|skipped|cached)\b/gi;
const SUCCESS_STATUSES = new Set(["succeeded", "completed", "success"]);

const LISTING_ERROR_MARKERS = [
  "failed to dial",
  "connection refused",
  "cannot connect",
  "no such file or directory",
  "permission denied",
  "i/o timeout",
  "deadline exceeded",
  "rpc error",
  "unavailable",
  "transport is closing",
  "error response from daemon",
];

export type ShellCommand = {
  argv: readonly string[];
  description: string;
};

export type EnvVar = {
  name: string;
  value: string;
};

export type ClusterRecoverySession = {
  robustnessId: string;
  artifactsDir: string;
  kubeconfig: string;
  namespace: string;
  taskImage: string;
  serverImage: string;
  chartDir: string;
  valuesFile: string;
  repoRoot: string;
  hashSecret: string;
};

export type ClusterGate =
  | { ok: true; session: ClusterRecoverySession }
  | { ok: false; missing: string[] };

export type GateIO = {
  exists?: (target: string) => boolean;
  readText?: (target: string) => string;
  repoRoot?: string;
};

export type CaesiumMember = {
  name: string;
  node: string;
  ip: string;
  containerID: string;
  ready: boolean;
  controlPlane: boolean;
};

export type LeaseSnapshot = {
  runId: string;
  ownerNode: string;
  generation: number;
};

export type DurableRunSnapshot = {
  id: string;
  status: string;
  tasks: { id: string; taskId: string; status: string }[];
};

export type FaultObservation = {
  connectedBeforeFault: boolean;
  sawDisconnectOrStatusChange: boolean;
  /** Wall time when the disconnect or in-page change was stored. */
  recordedAt: number;
  /** Wall time of the final convergence check. Must be strictly later. */
  assertionAt: number;
};

export type ConsoleRunRow = {
  id: string;
  status: string | null;
};

export type ConsoleSurface = {
  headingStatus: string;
  headingCount: number;
  runRows: ConsoleRunRow[];
  logText: string;
  logSourceLabel: string;
  /** Browser GET /v1/events after reconnect, including unauthorized attempts. */
  eventStreamAttempts: number;
  /** Those event-stream responses that returned 200. */
  eventStreamAuthorized: number;
  /**
   * Browser GET /v1/jobs/:id/runs/:id that returned 200. API-key login keeps
   * the bearer token in page memory, and EventSource cannot send it, so a 401
   * on /v1/events is followed by this authenticated poll.
   */
  authenticatedRunReads: number;
  showedSuccessBeforeFault: boolean;
  reloadedHeadingStatus: string;
  reloadedHeadingCount: number;
  reloadedRunRows: ConsoleRunRow[];
  reloadedLogText: string;
  reloadedLogSourceLabel: string;
};

export type DurableOutcome = {
  runId: string;
  status: string;
  generationBefore: number;
  generationAfter: number;
  ownerBefore: string;
  ownerAfter: string;
  logExcerpt: string;
};

export type ConvergenceIssue = {
  code:
    | "duplicate_row"
    | "stale_row"
    | "false_terminal_success"
    | "missing_fault_while_connected"
    | "event_stream_not_recovered"
    | "status_mismatch"
    | "log_mismatch"
    | "retained_log_not_shown"
    | "reload_lost_data"
    | "survivor_not_recorded";
  detail: string;
};

type AuthPlan =
  | { action: "unchanged" }
  | { action: "append"; extraEnv: EnvVar[]; overlayYaml: string }
  | { action: "refused"; reason: string };

const defaultRepoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../..");

export function assessClusterRecoveryGate(env: NodeJS.ProcessEnv, io: GateIO = {}): ClusterGate {
  const exists = io.exists ?? fs.existsSync;
  const readText = io.readText ?? ((target: string) => fs.readFileSync(target, "utf8"));
  const repoRoot = io.repoRoot ?? defaultRepoRoot;
  const missing: string[] = [];
  const robustnessId = envString(env, CLUSTER_ENV.id);
  const artifactsDir = envString(env, CLUSTER_ENV.artifacts);
  const taskImage = envString(env, CLUSTER_ENV.taskImage);
  const serverImage = envString(env, CLUSTER_ENV.serverImage);
  const hashSecret = envString(env, CLUSTER_ENV.hashSecret) ?? UI_E2E_AUTH_HASH_SECRET;

  if (!robustnessId) missing.push(`${CLUSTER_ENV.id} is unset`);
  else if (!SAFE_ID_RE.test(robustnessId)) missing.push(`${CLUSTER_ENV.id} is not a safe cluster id`);
  if (robustnessId === FOREIGN_CLUSTER_ID) missing.push(`${CLUSTER_ENV.id} is the foreign cluster ${FOREIGN_CLUSTER_ID}`);

  if (!artifactsDir) missing.push(`${CLUSTER_ENV.artifacts} is unset (kubeconfig is $CAESIUM_ROBUSTNESS_ARTIFACTS/kubeconfig)`);
  else if (!path.isAbsolute(artifactsDir)) missing.push(`${CLUSTER_ENV.artifacts} must be an absolute artifacts directory`);
  else if (artifactsDir.includes(FOREIGN_CLUSTER_ID)) missing.push(`${CLUSTER_ENV.artifacts} points at ${FOREIGN_CLUSTER_ID}`);
  else if (!exists(artifactsDir)) missing.push(`${CLUSTER_ENV.artifacts} does not exist (${artifactsDir})`);

  const kubeconfig = artifactsDir ? path.join(artifactsDir, "kubeconfig") : "";
  if (artifactsDir && (!kubeconfig || !exists(kubeconfig))) {
    missing.push(`robustness kubeconfig is missing (${kubeconfig || "artifacts/kubeconfig"})`);
  } else if (kubeconfig && exists(kubeconfig)) {
    let text = "";
    try {
      text = readText(kubeconfig);
    } catch (err) {
      missing.push(`robustness kubeconfig is unreadable (${err instanceof Error ? err.message : String(err)})`);
    }
    if (text.includes(FOREIGN_CLUSTER_ID)) {
      missing.push(`kubeconfig names the foreign cluster ${FOREIGN_CLUSTER_ID}`);
    }
  }

  if (!taskImage) missing.push(`${CLUSTER_ENV.taskImage} is unset`);
  else if (taskImage.includes(FOREIGN_CLUSTER_ID)) missing.push(`${CLUSTER_ENV.taskImage} names ${FOREIGN_CLUSTER_ID}`);

  if (!serverImage) missing.push(`${CLUSTER_ENV.serverImage} is unset`);
  else if (!imageTag(serverImage)) missing.push(`${CLUSTER_ENV.serverImage} has no image tag`);

  if (hashSecret.length < 32) {
    missing.push(`${CLUSTER_ENV.hashSecret} must be at least 32 characters`);
  }

  const chartDir = path.join(repoRoot, "helm", "caesium");
  const valuesFile = path.join(chartDir, "ci", "test-values-robustness.yaml");
  if (!exists(chartDir)) missing.push(`helm chart is missing (${chartDir})`);
  if (!exists(valuesFile)) missing.push(`robustness values file is missing (${valuesFile})`);

  if (missing.length > 0 || !robustnessId || !artifactsDir || !taskImage || !serverImage) {
    return { ok: false, missing };
  }

  return {
    ok: true,
    session: {
      robustnessId,
      artifactsDir,
      kubeconfig,
      namespace: robustnessId,
      taskImage,
      serverImage,
      chartDir,
      valuesFile,
      repoRoot,
      hashSecret,
    },
  };
}

export function formatClusterGateFailure(missing: string[]): string {
  return [
    "cluster-recovery fails closed: kind cluster env or artifacts are missing.",
    "This project does not skip.",
    ...missing.map((item) => `- ${item}`),
  ].join("\n");
}

export function imageTag(reference: string): string | null {
  const slash = reference.lastIndexOf("/");
  const colon = reference.lastIndexOf(":");
  if (colon <= slash) return null;
  const tag = reference.slice(colon + 1).trim();
  if (!tag || tag.includes("@")) return null;
  return tag;
}

export function commandViolation(argv: readonly string[]): string | null {
  const kubectlAt = argv.findIndex((arg) => arg === "kubectl" || arg.endsWith("/kubectl"));
  if (kubectlAt >= 0 && argv.slice(kubectlAt + 1).includes("delete")) {
    return "kubectl delete is not the owner fault";
  }
  const helmAt = argv.findIndex((arg) => arg === "helm" || arg.endsWith("/helm"));
  if (helmAt >= 0) {
    const helmArgs = argv.slice(helmAt + 1);
    if (helmArgs.includes("uninstall") || helmArgs.includes("delete")) {
      return "helm uninstall is out of scope";
    }
  }
  const dockerAt = argv.findIndex((arg) => arg === "docker" || arg.endsWith("/docker"));
  if (dockerAt >= 0) {
    const sub = argv[dockerAt + 1];
    if (sub === "rm" || sub === "kill" || sub === "stop") {
      return "refusing to stop or remove the kind node container";
    }
  }
  const kindAt = argv.findIndex((arg) => arg === "kind" || arg.endsWith("/kind"));
  if (kindAt >= 0 && argv.slice(kindAt + 1).includes("delete")) {
    return "kind delete is out of scope";
  }
  return null;
}

export function stripRuntimeContainerID(raw: string): string {
  let id = raw.trim();
  for (const prefix of ["containerd://", "docker://", "cri-o://"]) {
    if (id.startsWith(prefix)) id = id.slice(prefix.length);
  }
  return id.trim();
}

export function isControlPlaneNode(node: string): boolean {
  const normalized = node.toLowerCase();
  return normalized.includes("control-plane") || normalized.includes("controlplane");
}

export function endpointHost(address: string): string {
  const trimmed = address.trim();
  if (trimmed.startsWith("[")) {
    const end = trimmed.indexOf("]");
    return end > 1 ? trimmed.slice(1, end) : trimmed;
  }
  const colon = trimmed.lastIndexOf(":");
  if (colon > 0 && trimmed.indexOf(":") === colon) return trimmed.slice(0, colon);
  return trimmed;
}

export function cordonCommand(kubeconfig: string, node: string): ShellCommand {
  rejectNode(node);
  return {
    argv: ["kubectl", "--kubeconfig", kubeconfig, "cordon", node],
    description: "cordon the owner worker before the fixture is triggered",
  };
}

export function uncordonCommand(kubeconfig: string, node: string): ShellCommand {
  rejectNode(node);
  return {
    argv: ["kubectl", "--kubeconfig", kubeconfig, "uncordon", node],
    description: "uncordon the owner worker after survivor completion",
  };
}

export function kubeletStopCommand(node: string): ShellCommand {
  rejectNode(node);
  return {
    argv: ["docker", "exec", node, "systemctl", "stop", "kubelet"],
    description: "stop kubelet on the owner worker before SIGKILL",
  };
}

export function kubeletStartCommand(node: string): ShellCommand {
  rejectNode(node);
  return {
    argv: ["docker", "exec", node, "systemctl", "start", "kubelet"],
    description: "start kubelet on the owner worker after survivor completion",
  };
}

export function containerKillCommand(node: string, containerID: string): ShellCommand {
  rejectNode(node);
  const id = requireContainerID(containerID);
  return {
    argv: ["docker", "exec", node, "ctr", "-n", "k8s.io", "tasks", "kill", "--signal", "SIGKILL", id],
    description: "SIGKILL the owner container through ctr",
  };
}

export function taskListCommand(node: string): ShellCommand {
  rejectNode(node);
  return {
    argv: ["docker", "exec", node, "ctr", "-n", "k8s.io", "tasks", "list"],
    description: "list ctr tasks to observe process death",
  };
}

/** A1 order: cordon, stop kubelet, ctr SIGKILL, then list tasks. No pod delete. */
export function ownerKillSequence(input: {
  kubeconfig: string;
  node: string;
  containerID: string;
}): ShellCommand[] {
  return [
    cordonCommand(input.kubeconfig, input.node),
    kubeletStopCommand(input.node),
    containerKillCommand(input.node, input.containerID),
    taskListCommand(input.node),
  ];
}

export function ownerRestartSequence(input: { kubeconfig: string; node: string }): ShellCommand[] {
  return [kubeletStartCommand(input.node), uncordonCommand(input.kubeconfig, input.node)];
}

export function kubectlGetPodsCommand(kubeconfig: string, namespace: string, selector?: string): ShellCommand {
  const argv = ["kubectl", "--kubeconfig", kubeconfig, "--namespace", namespace, "get", "pods", "-o", "json"];
  if (selector) argv.push("-l", selector);
  return { argv, description: selector ? `list pods matching ${selector}` : "list pods in the robustness namespace" };
}

export function caesiumMemberSelector(): string {
  return "app.kubernetes.io/name=caesium,app.kubernetes.io/instance=caesium";
}

export function portForwardCommand(input: {
  kubeconfig: string;
  namespace: string;
  target: string;
  localPort: number;
}): ShellCommand {
  if (!Number.isInteger(input.localPort) || input.localPort < 1 || input.localPort > 65535) {
    throw new Error(`refusing local port ${input.localPort}`);
  }
  return {
    argv: [
      "kubectl",
      "--kubeconfig",
      input.kubeconfig,
      "--namespace",
      input.namespace,
      "port-forward",
      "--address",
      "127.0.0.1",
      input.target,
      `${input.localPort}:${CONSOLE_SERVICE_PORT}`,
    ],
    description: `port-forward ${input.target} to 127.0.0.1:${input.localPort}`,
  };
}

export function ownerConsoleForward(session: ClusterRecoverySession, podName: string): ShellCommand {
  rejectResourceName(podName, "pod");
  return portForwardCommand({
    kubeconfig: session.kubeconfig,
    namespace: session.namespace,
    target: `pod/${podName}`,
    localPort: OWNER_CONSOLE_PORT,
  });
}

/** The Service, not a pod IP, is the supported console entry after the owner is gone. */
export function serviceConsoleForward(session: ClusterRecoverySession): ShellCommand {
  return portForwardCommand({
    kubeconfig: session.kubeconfig,
    namespace: session.namespace,
    target: `svc/${CONSOLE_SERVICE}`,
    localPort: SERVICE_CONSOLE_PORT,
  });
}

export function helmGetValuesCommand(session: ClusterRecoverySession): ShellCommand {
  return {
    argv: [
      "helm",
      "get",
      "values",
      ROBUSTNESS_RELEASE,
      "--kubeconfig",
      session.kubeconfig,
      "--namespace",
      session.namespace,
      "--output",
      "json",
    ],
    description: "read the live robustness release values",
  };
}

export function helmAuthTemplateCommand(session: ClusterRecoverySession, overlayPath: string): ShellCommand {
  return {
    argv: [
      "helm",
      "template",
      ROBUSTNESS_RELEASE,
      session.chartDir,
      "--namespace",
      session.namespace,
      "--values",
      session.valuesFile,
      "--values",
      overlayPath,
    ],
    description: "render the auth overlay locally before upgrade",
  };
}

export function helmAuthUpgradeCommand(session: ClusterRecoverySession, overlayPath: string): ShellCommand {
  if (path.resolve(overlayPath) === path.resolve(session.valuesFile)) {
    throw new Error("refusing to use test-values-robustness.yaml as the auth overlay");
  }
  return {
    argv: [
      "helm",
      "upgrade",
      ROBUSTNESS_RELEASE,
      session.chartDir,
      "--reuse-values",
      "--kubeconfig",
      session.kubeconfig,
      "--namespace",
      session.namespace,
      "--values",
      overlayPath,
      "--wait",
      "--timeout",
      "240s",
    ],
    description: "helm upgrade the robustness release with an owned api-key overlay",
  };
}

export function podLogsCommand(session: ClusterRecoverySession): ShellCommand {
  return {
    argv: [
      "kubectl",
      "--kubeconfig",
      session.kubeconfig,
      "--namespace",
      session.namespace,
      "logs",
      "-l",
      caesiumMemberSelector(),
      "--tail",
      "400",
    ],
    description: "read caesium pod logs for the bootstrap admin key",
  };
}

export function countExtraEnvEntries(valuesYaml: string): number {
  const lines = valuesYaml.split(/\r?\n/);
  let count = 0;
  let inside = false;
  let indent: number | null = null;
  for (const line of lines) {
    if (/^\s*extraEnv:\s*$/.test(line)) {
      inside = true;
      indent = null;
      continue;
    }
    if (!inside) continue;
    if (!line.trim()) continue;
    const item = /^(\s*)- name:/.exec(line);
    if (item) {
      if (indent === null) indent = item[1].length;
      if (item[1].length === indent) count += 1;
      continue;
    }
    if (indent !== null && line.length - line.trimStart().length <= indent && !line.trimStart().startsWith("value")) {
      break;
    }
  }
  return count;
}

export function extraEnvFromReleaseValues(payload: unknown): EnvVar[] | { error: string } {
  if (!payload || typeof payload !== "object" || Array.isArray(payload)) {
    return { error: "helm values are not an object" };
  }
  const config = (payload as { config?: unknown }).config;
  if (!config || typeof config !== "object" || Array.isArray(config)) {
    return { error: "helm values have no config object" };
  }
  const extra = (config as { extraEnv?: unknown }).extraEnv;
  if (!Array.isArray(extra)) return { error: "helm values config.extraEnv is not a list" };
  const parsed: EnvVar[] = [];
  for (const entry of extra) {
    if (!entry || typeof entry !== "object" || Array.isArray(entry)) {
      return { error: "helm extraEnv entry is not an object" };
    }
    const name = (entry as { name?: unknown }).name;
    const value = (entry as { value?: unknown }).value;
    if (typeof name !== "string" || name.trim() === "") return { error: "helm extraEnv entry is missing a name" };
    if (value === undefined || value === null) return { error: `helm extraEnv ${name} is missing a value` };
    parsed.push({ name, value: String(value) });
  }
  return parsed;
}

export function planAuthExtraEnv(existing: EnvVar[], hashSecret: string): AuthPlan {
  if (hashSecret.trim().length < 32) {
    return { action: "refused", reason: "CAESIUM_AUTH_KEY_HASH_SECRET must be at least 32 characters" };
  }
  const mode = existing.find((entry) => entry.name === "CAESIUM_AUTH_MODE");
  if (mode && mode.value !== "api-key") {
    return { action: "refused", reason: `release already sets CAESIUM_AUTH_MODE=${mode.value}` };
  }
  if (mode?.value === "api-key") return { action: "unchanged" };
  const extraEnv = [
    ...existing,
    { name: "CAESIUM_AUTH_MODE", value: "api-key" },
    { name: "CAESIUM_AUTH_KEY_HASH_SECRET", value: hashSecret },
    { name: "CAESIUM_AUTH_REQUIRE_TLS", value: "false" },
  ];
  return { action: "append", extraEnv, overlayYaml: renderExtraEnvOverlay(extraEnv) };
}

export function renderExtraEnvOverlay(extraEnv: EnvVar[]): string {
  const lines = ["config:", "  extraEnv:"];
  for (const entry of extraEnv) {
    lines.push(`    - name: ${yamlQuote(entry.name)}`);
    lines.push(`      value: ${yamlQuote(entry.value)}`);
  }
  lines.push("");
  return lines.join("\n");
}

export function overlayPreservesEnv(overlayYaml: string, existing: EnvVar[]): string | null {
  for (const entry of existing) {
    if (!overlayYaml.includes(`name: ${yamlQuote(entry.name)}`)) {
      return `auth overlay dropped ${entry.name}`;
    }
    if (!overlayYaml.includes(`value: ${yamlQuote(entry.value)}`)) {
      return `auth overlay changed ${entry.name}`;
    }
  }
  if (!overlayYaml.includes('name: "CAESIUM_AUTH_MODE"') || !overlayYaml.includes('value: "api-key"')) {
    return "auth overlay does not set CAESIUM_AUTH_MODE=api-key";
  }
  return null;
}

export function parseBootstrapAdminKey(text: string): string | null {
  const match = text.match(/csk_live_[A-Za-z0-9]+/);
  return match ? match[0] : null;
}

export function consoleRecoveryDefinition(alias: string, taskImage: string, marker: string): Record<string, unknown> {
  if (!/^[a-z0-9][a-z0-9-]{0,54}$/.test(alias)) throw new Error(`unsafe job alias ${alias}`);
  if (!/^[a-z0-9-]{8,80}$/.test(marker)) throw new Error(`unsafe log marker ${marker}`);
  if (!taskImage || taskImage.includes(" ") || taskImage.includes("\n")) {
    throw new Error("unsafe task image reference");
  }
  return {
    apiVersion: "v1",
    kind: "Job",
    metadata: {
      alias,
      labels: { "caesium-d3": "console-recovery" },
    },
    trigger: {
      type: "cron",
      configuration: { cron: "0 0 1 1 *" },
    },
    steps: [
      {
        name: "hold",
        engine: "kubernetes",
        image: taskImage,
        command: ["sh", "-c", `echo ${marker}; sleep 60`],
      },
    ],
  };
}

export function membersFromPodList(payload: unknown): CaesiumMember[] {
  const items = podItems(payload);
  const members: CaesiumMember[] = [];
  for (const pod of items) {
    const metadata = objectField(pod, "metadata");
    const spec = objectField(pod, "spec");
    const status = objectField(pod, "status");
    const name = stringField(metadata, "name");
    const node = stringField(spec, "nodeName");
    const ip = stringField(status, "podIP");
    if (!name || metadata?.deletionTimestamp) continue;
    const statuses = Array.isArray(status?.containerStatuses) ? status.containerStatuses : [];
    const caesium = statuses.find((entry) => objectField(entry, null) && stringField(entry as Record<string, unknown>, "name") === "caesium") as
      | Record<string, unknown>
      | undefined;
    if (!caesium) continue;
    const containerID = stripRuntimeContainerID(stringField(caesium, "containerID") ?? "");
    members.push({
      name,
      node,
      ip,
      containerID,
      ready: podReady(status),
      controlPlane: isControlPlaneNode(node),
    });
  }
  return members;
}

export function chooseOwner(members: CaesiumMember[]): { owner: CaesiumMember; survivors: CaesiumMember[] } | { error: string } {
  const ready = members.filter((member) => member.ready && member.ip && member.containerID && member.node && !member.controlPlane);
  ready.sort((left, right) => left.name.localeCompare(right.name));
  const owner = ready[0];
  if (!owner) return { error: "no ready caesium worker pod to own the run" };
  const survivors = ready.filter((member) => member.name !== owner.name && member.node !== owner.node);
  if (survivors.length === 0) return { error: `owner ${owner.name} on ${owner.node} has no survivor on another node` };
  return { owner, survivors };
}

export function taskPlacementIssues(payload: unknown, runId: string, ownerNode: string): string[] {
  if (!UUID_RE.test(runId)) return ["run id is not a uuid"];
  const issues: string[] = [];
  let seen = 0;
  for (const pod of podItems(payload)) {
    const metadata = objectField(pod, "metadata");
    const spec = objectField(pod, "spec");
    const name = stringField(metadata, "name");
    if (!name || !name.includes(runId) || metadata?.deletionTimestamp) continue;
    const statuses = objectField(pod, "status");
    const containers = Array.isArray(statuses?.containerStatuses) ? statuses.containerStatuses : [];
    const isMember = containers.some((entry) => stringField(entry as Record<string, unknown>, "name") === "caesium");
    if (isMember) continue;
    seen += 1;
    const node = stringField(spec, "nodeName");
    if (!node) issues.push(`task pod ${name} is not bound to a node`);
    else if (node === ownerNode) issues.push(`task pod ${name} is on the owner node ${ownerNode}`);
  }
  if (seen === 0) issues.push(`no task pod for run ${runId} is visible yet`);
  return issues;
}

export function leaseQueryBody(runId: string): { sql: string; limit: number } | { error: string } {
  if (!UUID_RE.test(runId)) return { error: `lease query refused unvalidated run id ${runId}` };
  return {
    sql: `SELECT run_id, owner_node, generation, lease_expires_at FROM run_leases WHERE run_id = '${runId.toLowerCase()}'`,
    limit: 1,
  };
}

export function parseLeaseResponse(payload: unknown, expectedRunId: string): LeaseSnapshot | { error: string } {
  if (!payload || typeof payload !== "object" || Array.isArray(payload)) return { error: "lease response is not an object" };
  const rows = (payload as { rows?: unknown }).rows;
  if (!Array.isArray(rows) || rows.length === 0) return { error: `no run_leases row for ${expectedRunId}` };
  const row = rows[0];
  if (!Array.isArray(row) || row.length < 4) return { error: "lease row does not have four columns" };
  const runId = String(row[0] ?? "");
  const ownerNode = String(row[1] ?? "");
  const generation = Number(row[2]);
  if (runId.toLowerCase() !== expectedRunId.toLowerCase()) return { error: `lease run id ${runId} != ${expectedRunId}` };
  if (!ownerNode) return { error: "lease owner_node is empty" };
  if (!Number.isInteger(generation) || generation < 1) return { error: `lease generation is not a positive integer (${String(row[2])})` };
  return { runId, ownerNode, generation };
}

export function parseRunSnapshot(payload: unknown): DurableRunSnapshot | { error: string } {
  if (!payload || typeof payload !== "object" || Array.isArray(payload)) return { error: "run response is not an object" };
  const id = stringField(payload as Record<string, unknown>, "id");
  const status = stringField(payload as Record<string, unknown>, "status");
  if (!id || !UUID_RE.test(id)) return { error: "run response has no uuid id" };
  if (!status) return { error: "run response has no status" };
  const rawTasks = (payload as { tasks?: unknown }).tasks;
  const tasks: DurableRunSnapshot["tasks"] = [];
  if (Array.isArray(rawTasks)) {
    for (const task of rawTasks) {
      if (!task || typeof task !== "object") continue;
      const taskRecord = task as Record<string, unknown>;
      const taskRunId = stringField(taskRecord, "id") ?? "";
      const taskId = stringField(taskRecord, "task_id") ?? "";
      const taskStatus = stringField(taskRecord, "status") ?? "";
      if (taskId) tasks.push({ id: taskRunId, taskId, status: taskStatus });
    }
  }
  return { id, status, tasks };
}

export function ctrListingKind(listing: string): "ok" | "error" | "invalid" {
  const stripped = listing.trim();
  if (!stripped) return "invalid";
  const header = listing.split(/\r?\n/).find((line) => line.trim() !== "") ?? "";
  const tokens = header.trim().split(/\s+/).map((token) => token.toLowerCase());
  const headerOK = tokens.includes("task") && (tokens.includes("pid") || tokens.includes("status"));
  const lower = stripped.toLowerCase();
  if (!headerOK) {
    const head = lower.slice(0, 120);
    if (lower.startsWith("ctr:") || head.includes("error:")) return "error";
    if (LISTING_ERROR_MARKERS.some((marker) => lower.includes(marker))) return "error";
    return "invalid";
  }
  return "ok";
}

export function containerSeen(listing: string, containerID: string): boolean {
  const id = stripRuntimeContainerID(containerID);
  const short = id.slice(0, 12);
  if (id.length < 8) return false;
  return listing.includes(id) || (short.length >= 8 && listing.includes(short));
}

export function taskDeadFromListing(containerID: string, listing: string, seen: boolean): { dead: boolean; detail: string } {
  const id = stripRuntimeContainerID(containerID);
  const short = id.slice(0, 12);
  if (!id || short.length < 8) return { dead: false, detail: "empty container id" };
  const kind = ctrListingKind(listing);
  if (kind !== "ok") return { dead: false, detail: `listing is ${kind}, not proof of death` };
  for (const line of listing.split(/\r?\n/)) {
    if (!line.includes(id) && !line.includes(short)) continue;
    const lower = line.toLowerCase();
    if (lower.includes("running")) return { dead: false, detail: "container still running" };
    if (lower.includes("stopped") || lower.includes("exited") || lower.includes("killed")) {
      return { dead: true, detail: "container stopped" };
    }
    return { dead: false, detail: "container still present without stopped evidence" };
  }
  if (seen) return { dead: true, detail: "container absent from valid listing" };
  return { dead: false, detail: "container id never appeared in ctr tasks list" };
}

export function formatKillEvidence(node: string, containerID: string, listing: string): string {
  const id = stripRuntimeContainerID(containerID);
  return `kubelet stopped on ${node}\nctr kill ${id}\n${listing.endsWith("\n") ? listing : `${listing}\n`}`;
}

export function killEvidenceShowsDeath(evidence: string, containerID: string): boolean {
  const id = stripRuntimeContainerID(containerID);
  const short = id.slice(0, 12);
  if (!evidence.trim() || !id) return false;
  if (!evidence.includes(short)) return false;
  const listingLines: string[] = [];
  for (const line of evidence.split(/\r?\n/)) {
    const lower = line.trim().toLowerCase();
    if (lower.startsWith("kubelet stopped") || lower.startsWith("ctr kill")) continue;
    listingLines.push(line);
  }
  return taskDeadFromListing(id, listingLines.join("\n"), true).dead;
}

export function runIdFromHref(href: string, jobId: string): string | null {
  const pathname = href.split("?")[0]?.split("#")[0] ?? "";
  const marker = `/jobs/${jobId}/runs/`;
  const at = pathname.indexOf(marker);
  if (at < 0) return null;
  const id = pathname.slice(at + marker.length).replace(/\/$/, "");
  return UUID_RE.test(id) ? id.toLowerCase() : null;
}

export function statusFromRowText(text: string): string | null {
  const found = text.match(RUN_STATUS_RE);
  if (!found || found.length === 0) return null;
  return found[found.length - 1].toLowerCase();
}

export function faultWasObserved(fault: FaultObservation): boolean {
  return fault.connectedBeforeFault
    && fault.sawDisconnectOrStatusChange
    && Number.isFinite(fault.recordedAt)
    && Number.isFinite(fault.assertionAt)
    && fault.recordedAt < fault.assertionAt;
}

export function survivorTookOver(outcome: DurableOutcome): boolean {
  return outcome.status === "succeeded"
    && outcome.generationAfter > outcome.generationBefore
    && endpointHost(outcome.ownerBefore) !== ""
    && endpointHost(outcome.ownerAfter) !== ""
    && endpointHost(outcome.ownerAfter) !== endpointHost(outcome.ownerBefore);
}

export function convergenceIssues(input: {
  durable: DurableOutcome;
  console: ConsoleSurface;
  fault: FaultObservation;
}): ConvergenceIssue[] {
  const issues: ConvergenceIssue[] = [];
  const { durable, console: surface, fault } = input;
  if (!faultWasObserved(fault)) {
    issues.push({
      code: "missing_fault_while_connected",
      detail: "the browser did not record a disconnect or in-page status change while connected, before the final check",
    });
  }
  if (surface.showedSuccessBeforeFault || showsSuccess(surface.headingStatus)) {
    if (surface.showedSuccessBeforeFault || !survivorTookOver(durable)) {
      issues.push({
        code: "false_terminal_success",
        detail: surface.showedSuccessBeforeFault
          ? "the console showed terminal success before the owner was killed"
          : `the console shows ${surface.headingStatus || "success"} but the durable outcome is not a survivor completion (${describeOutcome(durable)})`,
      });
    }
  }
  if (showsSuccess(surface.reloadedHeadingStatus) && !survivorTookOver(durable)) {
    issues.push({
      code: "false_terminal_success",
      detail: "reload shows terminal success without a survivor completion",
    });
  }
  issues.push(...rowIssues(surface.runRows, durable, "run list"));
  issues.push(...rowIssues(surface.reloadedRunRows, durable, "reloaded run list"));
  if (survivorTookOver(durable) && surface.runRows.length === 0) {
    issues.push({ code: "status_mismatch", detail: "run list has no rows" });
  }
  if (survivorTookOver(durable) && surface.reloadedRunRows.length === 0) {
    issues.push({ code: "reload_lost_data", detail: "reloaded run list has no rows" });
  }
  if (surface.headingCount > 1) {
    issues.push({ code: "duplicate_row", detail: `run detail rendered ${surface.headingCount} run headings` });
  }
  if (surface.reloadedHeadingCount > 1) {
    issues.push({ code: "duplicate_row", detail: `reloaded run detail rendered ${surface.reloadedHeadingCount} run headings` });
  }
  if (!survivorTookOver(durable)) {
    issues.push({
      code: "survivor_not_recorded",
      detail: `durable outcome did not move to a survivor (${describeOutcome(durable)})`,
    });
  } else if (surface.headingStatus !== durable.status || surface.reloadedHeadingStatus !== durable.status) {
    issues.push({
      code: "status_mismatch",
      detail: `console heading ${surface.headingStatus || "missing"} / reload ${surface.reloadedHeadingStatus || "missing"} != durable ${durable.status}`,
    });
  }
  if (!eventStreamRecovered(surface)) {
    issues.push({
      code: "event_stream_not_recovered",
      detail: surface.eventStreamAttempts < 1
        ? "the page did not open GET /v1/events after reconnect"
        : "GET /v1/events was not authorized and no authenticated run read recovered the page",
    });
  }
  if (!durable.logExcerpt) {
    issues.push({ code: "log_mismatch", detail: "independent log read has no retained excerpt" });
  } else {
    if (!surface.logText.includes(durable.logExcerpt)) {
      issues.push({ code: "log_mismatch", detail: "console log does not contain the independent retained excerpt" });
    }
    if (!surface.reloadedLogText.includes(durable.logExcerpt)) {
      issues.push({ code: "reload_lost_data", detail: "reloaded console log does not contain the retained excerpt" });
    }
  }
  if (survivorTookOver(durable)) {
    if (surface.logSourceLabel !== "Retained snapshot") {
      issues.push({
        code: "retained_log_not_shown",
        detail: `log source badge is ${surface.logSourceLabel || "missing"}, want Retained snapshot`,
      });
    }
    if (surface.reloadedLogSourceLabel !== "Retained snapshot") {
      issues.push({
        code: "reload_lost_data",
        detail: `reloaded log source badge is ${surface.reloadedLogSourceLabel || "missing"}, want Retained snapshot`,
      });
    }
    if (surface.reloadedHeadingCount < 1 || surface.headingCount < 1) {
      issues.push({ code: "reload_lost_data", detail: "run heading was not inspectable before and after reload" });
    }
  }
  return issues;
}

function rowIssues(rows: ConsoleRunRow[], durable: DurableOutcome, label: string): ConvergenceIssue[] {
  const issues: ConvergenceIssue[] = [];
  const wanted = durable.runId.toLowerCase();
  const matches = rows.filter((row) => row.id === wanted);
  if (matches.length > 1) {
    issues.push({ code: "duplicate_row", detail: `${label} contains ${matches.length} rows for ${wanted}` });
  }
  for (const row of rows) {
    if (row.id !== wanted) {
      issues.push({ code: "stale_row", detail: `${label} contains unexpected run ${row.id}` });
      continue;
    }
    if (row.status && row.status !== durable.status) {
      issues.push({
        code: row.status === "succeeded" && durable.status !== "succeeded" ? "false_terminal_success" : "stale_row",
        detail: `${label} run ${wanted} shows ${row.status}, durable status is ${durable.status}`,
      });
    }
  }
  return issues;
}

function eventStreamRecovered(surface: ConsoleSurface): boolean {
  if (surface.eventStreamAuthorized >= 1) return true;
  return surface.eventStreamAttempts >= 1 && surface.authenticatedRunReads >= 1;
}

function showsSuccess(status: string): boolean {
  return SUCCESS_STATUSES.has(status.trim().toLowerCase());
}

function describeOutcome(outcome: DurableOutcome): string {
  return `status=${outcome.status} generation ${outcome.generationBefore}->${outcome.generationAfter} owner ${outcome.ownerBefore || "?"} -> ${outcome.ownerAfter || "?"}`;
}

function podItems(payload: unknown): Record<string, unknown>[] {
  if (!payload || typeof payload !== "object") return [];
  const items = (payload as { items?: unknown }).items;
  if (!Array.isArray(items)) return [];
  return items.filter((item): item is Record<string, unknown> => !!item && typeof item === "object" && !Array.isArray(item));
}

function podReady(status: Record<string, unknown> | null): boolean {
  if (stringField(status, "phase") !== "Running") return false;
  const conditions = status && Array.isArray(status.conditions) ? status.conditions : [];
  return conditions.some((condition) => {
    if (!condition || typeof condition !== "object") return false;
    const record = condition as Record<string, unknown>;
    return record.type === "Ready" && record.status === "True";
  });
}

function objectField(value: unknown, key: string | null): Record<string, unknown> | null {
  const source = key === null ? value : value && typeof value === "object" ? (value as Record<string, unknown>)[key] : null;
  if (!source || typeof source !== "object" || Array.isArray(source)) return null;
  return source as Record<string, unknown>;
}

function stringField(record: Record<string, unknown> | null, key: string): string {
  if (!record) return "";
  const value = record[key];
  return typeof value === "string" ? value : "";
}

function envString(env: NodeJS.ProcessEnv, name: string): string | null {
  const value = env[name]?.trim();
  return value ? value : null;
}

function yamlQuote(value: string): string {
  return `"${value.replace(/\\/g, "\\\\").replace(/"/g, '\\"')}"`;
}

function rejectNode(node: string): void {
  if (!SAFE_NODE_RE.test(node) || node === FOREIGN_CLUSTER_ID || isControlPlaneNode(node)) {
    throw new Error(`refusing node name ${node}`);
  }
}

function rejectResourceName(name: string, kind: string): void {
  if (!/^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(name)) throw new Error(`refusing ${kind} name ${name}`);
}

function requireContainerID(raw: string): string {
  const id = stripRuntimeContainerID(raw);
  if (!CONTAINER_ID_RE.test(id)) throw new Error("refusing container id");
  return id;
}
