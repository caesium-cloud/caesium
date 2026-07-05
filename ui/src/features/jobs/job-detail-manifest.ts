import type { Atom, Job, JobDAGResponse, JobTask, Trigger } from "@/lib/api";

type JsonRecord = Record<string, unknown>;

type ManifestJob = Job & {
  priority?: string;
  concurrency?: unknown;
  rate_limits?: unknown;
  sla?: unknown;
  schema_validation?: string;
  replay_safe?: boolean;
};

type ManifestStep = {
  name: string;
  type?: string;
  engine?: string;
  image: string;
  command?: string[];
  nodeSelector?: unknown;
  next?: string[];
  retries?: number;
  retryDelay?: string;
  retryBackoff?: boolean;
  triggerRule?: string;
  serviceAccountName?: string;
  podAnnotations?: unknown;
  automountServiceAccountToken?: boolean;
  kueue?: { queueName: string };
  cache?: unknown;
  outputSchema?: unknown;
  inputSchema?: unknown;
  env?: unknown;
  workdir?: string;
  mounts?: unknown;
};

export type JobAuthoringManifest = {
  apiVersion: "v1";
  kind: "Job";
  metadata: JsonRecord & { alias: string };
  trigger: {
    type: string;
    configuration: JsonRecord;
    defaultParams?: Record<string, string>;
  };
  steps: ManifestStep[];
};

export function formatCommandForDisplay(command?: string | string[]): string {
  const normalized = normalizeCommand(command);
  return normalized.length > 0 ? normalized.join(" ") : "N/A";
}

export function normalizeCommand(command?: string | string[]): string[] {
  if (!command) {
    return [];
  }
  if (Array.isArray(command)) {
    return command.map(String);
  }

  const trimmed = command.trim();
  if (!trimmed) {
    return [];
  }

  try {
    const parsed = JSON.parse(trimmed);
    if (Array.isArray(parsed)) {
      return parsed.map(String);
    }
  } catch {
    // Non-JSON command strings are already displayable.
  }

  return [command];
}

export function buildJobAuthoringManifest({
  job,
  tasks,
  trigger,
  atoms,
  dag,
}: {
  job: Job;
  tasks?: JobTask[];
  trigger?: Trigger | null;
  atoms?: Record<string, Atom>;
  dag?: JobDAGResponse;
}): JobAuthoringManifest {
  const manifestJob = job as ManifestJob;
  const metadata = compactObject({
    alias: job.alias,
    labels: nonEmptyRecord(job.labels),
    annotations: nonEmptyRecord(job.annotations),
    maxParallelTasks: positiveNumber(job.max_parallel_tasks),
    taskTimeout: durationLiteral(job.task_timeout),
    runTimeout: durationLiteral(job.run_timeout),
    priority: nonEmptyString(manifestJob.priority),
    concurrency: structuredValue(manifestJob.concurrency),
    rateLimits: structuredValue(manifestJob.rate_limits),
    sla: structuredValue(manifestJob.sla),
    schemaValidation: nonEmptyString(manifestJob.schema_validation),
    replaySafe: manifestJob.replay_safe === true ? true : undefined,
    cache: structuredValue(job.cache_config),
  }) as JsonRecord & { alias: string };

  const resolvedTrigger = trigger ?? job.trigger ?? null;
  const triggerConfiguration = structuredRecord(resolvedTrigger?.configuration) ?? {};
  const defaultParams = stringRecord(triggerConfiguration.defaultParams);
  if (defaultParams) {
    delete triggerConfiguration.defaultParams;
  }

  return {
    apiVersion: "v1",
    kind: "Job",
    metadata,
    trigger: compactObject({
      type: resolvedTrigger?.type || "cron",
      configuration: triggerConfiguration,
      defaultParams,
    }) as JobAuthoringManifest["trigger"],
    steps: buildManifestSteps(tasks ?? [], atoms ?? {}, dag),
  };
}

function buildManifestSteps(tasks: JobTask[], atoms: Record<string, Atom>, dag?: JobDAGResponse): ManifestStep[] {
  const taskById = new Map(tasks.map((task) => [task.id, task]));
  const nameById = new Map(tasks.map((task) => [task.id, task.name || task.id]));
  const orderById = new Map(tasks.map((task, index) => [task.id, index]));
  const nodeTypeById = new Map(dag?.nodes?.map((node) => [node.id, node.type]) ?? []);
  const nextByTaskId = new Map<string, string[]>();

  dag?.edges?.forEach((edge) => {
    const source = taskById.get(edge.from);
    const targetName = nameById.get(edge.to);
    if (!source || !targetName) {
      return;
    }
    const next = nextByTaskId.get(edge.from) ?? [];
    next.push(targetName);
    nextByTaskId.set(edge.from, next);
  });

  for (const [taskId, next] of nextByTaskId) {
    next.sort((a, b) => {
      const aId = tasks.find((task) => task.name === a)?.id ?? "";
      const bId = tasks.find((task) => task.name === b)?.id ?? "";
      return (orderById.get(aId) ?? Number.MAX_SAFE_INTEGER) - (orderById.get(bId) ?? Number.MAX_SAFE_INTEGER);
    });
    nextByTaskId.set(taskId, next);
  }

  return tasks.map((task) => {
    const atom = atoms[task.atom_id];
    const spec = structuredRecord(atom?.spec);
    const kubernetes = structuredRecord(spec?.kubernetes);
    const queueName = nonEmptyString(kubernetes?.queueName);
    const next = nextByTaskId.get(task.id) ?? fallbackNext(task, nameById);

    return compactObject({
      name: task.name || task.id,
      type: nonDefaultType(nodeTypeById.get(task.id)),
      engine: nonDefaultEngine(atom?.engine),
      image: atom?.image ?? "",
      command: nonEmptyArray(normalizeCommand(atom?.command)),
      nodeSelector: nonEmptyRecord(task.node_selector),
      next: nonEmptyArray(next),
      retries: positiveNumber(task.retries),
      retryDelay: durationLiteral(task.retry_delay),
      retryBackoff: task.retry_backoff ? true : undefined,
      triggerRule: nonDefaultTriggerRule(task.trigger_rule),
      serviceAccountName: nonEmptyString(kubernetes?.serviceAccountName),
      podAnnotations: nonEmptyRecord(kubernetes?.podAnnotations),
      automountServiceAccountToken: typeof kubernetes?.automountServiceAccountToken === "boolean"
        ? kubernetes.automountServiceAccountToken
        : undefined,
      kueue: queueName ? { queueName } : undefined,
      cache: structuredValue(task.cache_config),
      outputSchema: structuredValue(task.output_schema),
      inputSchema: structuredValue(task.input_schema),
      env: nonEmptyRecord(spec?.env),
      workdir: nonEmptyString(spec?.workdir),
      mounts: nonEmptyArray(asArray(spec?.mounts)),
    }) as ManifestStep;
  });
}

function fallbackNext(task: JobTask, nameById: Map<string, string>): string[] {
  if (!task.next_id) {
    return [];
  }
  const nextName = nameById.get(task.next_id);
  return nextName ? [nextName] : [];
}

function structuredRecord(value: unknown): JsonRecord | undefined {
  const normalized = structuredValue(value);
  return isRecord(normalized) ? { ...normalized } : undefined;
}

function structuredValue(value: unknown): unknown {
  if (value === undefined || value === null) {
    return undefined;
  }
  if (typeof value === "string") {
    const trimmed = value.trim();
    if (!trimmed || trimmed === "null") {
      return undefined;
    }
    try {
      return structuredValue(JSON.parse(trimmed));
    } catch {
      return value;
    }
  }
  if (Array.isArray(value)) {
    return value.length > 0 ? value : undefined;
  }
  if (isRecord(value)) {
    return Object.keys(value).length > 0 ? value : undefined;
  }
  return value;
}

function compactObject<T extends JsonRecord>(input: T): Partial<T> {
  return Object.fromEntries(Object.entries(input).filter(([, value]) => value !== undefined)) as Partial<T>;
}

function isRecord(value: unknown): value is JsonRecord {
  return Boolean(value) && typeof value === "object" && !Array.isArray(value);
}

function nonEmptyRecord(value: unknown): JsonRecord | undefined {
  return isRecord(value) && Object.keys(value).length > 0 ? value : undefined;
}

function stringRecord(value: unknown): Record<string, string> | undefined {
  const normalized = structuredValue(value);
  if (!isRecord(normalized)) {
    return undefined;
  }
  const entries = Object.entries(normalized).filter(([, entryValue]) => entryValue !== undefined && entryValue !== null);
  if (entries.length === 0) {
    return undefined;
  }
  return Object.fromEntries(entries.map(([key, entryValue]) => [key, String(entryValue)]));
}

function asArray(value: unknown): unknown[] {
  return Array.isArray(value) ? value : [];
}

function nonEmptyArray<T>(value: T[]): T[] | undefined {
  return value.length > 0 ? value : undefined;
}

function nonEmptyString(value: unknown): string | undefined {
  return typeof value === "string" && value.trim() ? value : undefined;
}

function positiveNumber(value: unknown): number | undefined {
  return typeof value === "number" && Number.isFinite(value) && value > 0 ? value : undefined;
}

function nonDefaultEngine(engine: unknown): string | undefined {
  const value = nonEmptyString(engine);
  return value && value !== "docker" ? value : undefined;
}

function nonDefaultType(type: unknown): string | undefined {
  const value = nonEmptyString(type);
  return value && value !== "task" ? value : undefined;
}

function nonDefaultTriggerRule(rule: unknown): string | undefined {
  const value = nonEmptyString(rule);
  return value && value !== "all_success" ? value : undefined;
}

function durationLiteral(value: unknown): string | undefined {
  if (typeof value !== "number" || !Number.isFinite(value) || value <= 0) {
    return undefined;
  }

  const units = [
    ["h", 3_600_000_000_000],
    ["m", 60_000_000_000],
    ["s", 1_000_000_000],
    ["ms", 1_000_000],
    ["us", 1_000],
    ["ns", 1],
  ] as const;

  for (const [suffix, nanos] of units) {
    if (value % nanos === 0) {
      return `${value / nanos}${suffix}`;
    }
  }

  return `${value}ns`;
}
