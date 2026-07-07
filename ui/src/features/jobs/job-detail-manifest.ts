import type { Atom, Job, JobDAGResponse, JobTask, Trigger } from "@/lib/api";

type JsonRecord = Record<string, unknown>;

type ManifestJob = Job & {
  priority?: string;
  concurrency?: unknown;
  rate_limits?: unknown;
  sla?: unknown;
  schema_validation?: string;
  replay_safe?: boolean;
  serviceAccountName?: string;
  service_account_name?: string;
  podAnnotations?: unknown;
  pod_annotations?: unknown;
  automountServiceAccountToken?: boolean;
  automount_service_account_token?: boolean;
  datasets?: unknown;
  remediation?: unknown;
  callbacks?: unknown;
  volumes?: unknown;
};

type ManifestTask = JobTask & {
  type?: string;
  dependsOn?: unknown;
  depends_on?: unknown;
  replaySafe?: boolean;
  replay_safe?: boolean;
  rateLimit?: unknown;
  rate_limit_resource?: string;
  rate_limit_units?: number;
  datasets?: unknown;
};

type ManifestAtom = Atom & {
  replaySafe?: boolean;
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
  dependsOn?: string[];
  retries?: number;
  retryDelay?: string;
  retryBackoff?: boolean;
  triggerRule?: string;
  replaySafe?: boolean;
  volumeMounts?: unknown[];
  serviceAccountName?: string;
  podAnnotations?: unknown;
  automountServiceAccountToken?: boolean;
  kueue?: { queueName: string };
  rateLimit?: unknown;
  cache?: unknown;
  outputSchema?: unknown;
  inputSchema?: unknown;
  datasets?: unknown;
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
  callbacks?: unknown[];
  volumes?: unknown[];
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
    serviceAccountName: nonEmptyString(firstDefined(manifestJob.serviceAccountName, manifestJob.service_account_name)),
    podAnnotations: structuredRecord(firstDefined(manifestJob.podAnnotations, manifestJob.pod_annotations)),
    automountServiceAccountToken: booleanValue(
      firstDefined(manifestJob.automountServiceAccountToken, manifestJob.automount_service_account_token),
    ),
    datasets: structuredValue(manifestJob.datasets),
    remediation: structuredValue(manifestJob.remediation),
  }) as JsonRecord & { alias: string };

  const resolvedTrigger = trigger ?? job.trigger ?? null;
  const triggerConfiguration = structuredRecord(resolvedTrigger?.configuration) ?? {};
  const defaultParams = stringRecord(triggerConfiguration.defaultParams);
  if (defaultParams) {
    delete triggerConfiguration.defaultParams;
  }

  const loadedVolumes = structuredArray(manifestJob.volumes);
  const reconstructedVolumes = loadedVolumes ? [] : buildVolumesFromResolvedMounts(tasks ?? [], atoms ?? {});
  const volumes = loadedVolumes ?? nonEmptyArray(reconstructedVolumes);
  const volumeNames = volumeNameSet(volumes);
  const callbacks = structuredArray(manifestJob.callbacks);

  return {
    apiVersion: "v1",
    kind: "Job",
    metadata,
    trigger: compactObject({
      type: resolvedTrigger?.type || "cron",
      configuration: triggerConfiguration,
      defaultParams,
    }) as JobAuthoringManifest["trigger"],
    ...(callbacks ? { callbacks } : {}),
    ...(volumes ? { volumes } : {}),
    steps: buildManifestSteps(tasks ?? [], atoms ?? {}, dag, volumeNames, manifestJob.replay_safe === true),
  };
}

function buildManifestSteps(
  tasks: ManifestTask[],
  atoms: Record<string, Atom>,
  dag?: JobDAGResponse,
  volumeNames = new Set<string>(),
  jobReplaySafe = false,
): ManifestStep[] {
  const manifestAtoms = atoms as Record<string, ManifestAtom>;
  const taskById = new Map(tasks.map((task) => [task.id, task]));
  const nameById = new Map(tasks.map((task) => [task.id, task.name || task.id]));
  const orderById = new Map(tasks.map((task, index) => [task.id, index]));
  const nodeTypeById = new Map(dag?.nodes?.map((node) => [node.id, node.type]) ?? []);
  const nextIdsByTaskId = new Map<string, string[]>();

  dag?.edges?.forEach((edge) => {
    if (!taskById.has(edge.from) || !nameById.has(edge.to)) {
      return;
    }
    const next = nextIdsByTaskId.get(edge.from) ?? [];
    next.push(edge.to);
    nextIdsByTaskId.set(edge.from, next);
  });

  for (const [taskId, nextIds] of nextIdsByTaskId) {
    nextIds.sort(
      (a, b) => (orderById.get(a) ?? Number.MAX_SAFE_INTEGER) - (orderById.get(b) ?? Number.MAX_SAFE_INTEGER),
    );
    nextIdsByTaskId.set(taskId, nextIds);
  }

  return tasks.map((task) => {
    const atom = manifestAtoms[task.atom_id];
    const spec = structuredRecord(atom?.spec);
    const kubernetes = structuredRecord(spec?.kubernetes);
    const queueName = nonEmptyString(kubernetes?.queueName);
    const nextIds = nextIdsByTaskId.get(task.id);
    const next = nextIds ? namesForIds(nextIds, nameById) : fallbackNext(task, nameById);
    const dependsOn = stringArray(firstDefined(task.dependsOn, task.depends_on));
    const volumeMounts = structuredArray(spec?.volumeMounts) ?? volumeMountsFromResolved(spec?.resolvedVolumeMounts, volumeNames);
    const replaySafe = firstDefined(task.replaySafe, task.replay_safe, atom?.replaySafe, atom?.replay_safe);

    return compactObject({
      name: task.name || task.id,
      type: nonDefaultType(firstDefined(task.type, nodeTypeById.get(task.id))),
      engine: nonDefaultEngine(atom?.engine),
      image: atom?.image ?? "",
      command: nonEmptyArray(normalizeCommand(atom?.command)),
      nodeSelector: structuredRecord(task.node_selector),
      next: nonEmptyArray(next),
      dependsOn,
      retries: positiveNumber(task.retries),
      retryDelay: durationLiteral(task.retry_delay),
      retryBackoff: task.retry_backoff ? true : undefined,
      triggerRule: nonDefaultTriggerRule(task.trigger_rule),
      replaySafe: !jobReplaySafe && replaySafe === true ? true : undefined,
      volumeMounts,
      serviceAccountName: nonEmptyString(kubernetes?.serviceAccountName),
      podAnnotations: nonEmptyRecord(kubernetes?.podAnnotations),
      automountServiceAccountToken: typeof kubernetes?.automountServiceAccountToken === "boolean"
        ? kubernetes.automountServiceAccountToken
        : undefined,
      kueue: queueName ? { queueName } : undefined,
      rateLimit: rateLimitForTask(task),
      cache: structuredValue(task.cache_config),
      outputSchema: structuredValue(task.output_schema),
      inputSchema: structuredValue(task.input_schema),
      datasets: structuredValue(firstDefined(task.datasets, spec?.datasets)),
      env: structuredRecord(spec?.env),
      workdir: nonEmptyString(spec?.workdir),
      mounts: structuredArray(spec?.mounts),
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

function namesForIds(ids: string[], nameById: Map<string, string>): string[] {
  return ids.map((id) => nameById.get(id)).filter((name): name is string => !!name);
}

function buildVolumesFromResolvedMounts(tasks: ManifestTask[], atoms: Record<string, Atom>): JsonRecord[] {
  const manifestAtoms = atoms as Record<string, ManifestAtom>;
  const volumesByName = new Map<string, JsonRecord>();

  for (const task of tasks) {
    const spec = structuredRecord(manifestAtoms[task.atom_id]?.spec);
    const resolvedMounts = structuredArray(spec?.resolvedVolumeMounts) ?? [];
    for (const rawMount of resolvedMounts) {
      if (!isRecord(rawMount)) {
        continue;
      }
      const name = nonEmptyString(rawMount.name);
      if (!name || volumesByName.has(name)) {
        continue;
      }
      const source = volumeSourceFromResolvedMount(rawMount);
      if (source) {
        volumesByName.set(name, { name, source });
      }
    }
  }

  return [...volumesByName.values()];
}

function volumeSourceFromResolvedMount(mount: JsonRecord): JsonRecord | undefined {
  const type = nonEmptyString(mount.type);
  const source = nonEmptyString(mount.source);

  switch (type) {
    case "bind":
      return source ? { bind: source } : undefined;
    case "volume":
      return source ? { volume: source } : undefined;
    case "tmpfs":
      return { tmpfs: structuredRecord(mount.tmpfs) ?? {} };
    case "pvc":
      return source ? { pvc: source } : undefined;
    case "claimTemplate": {
      const claimTemplate = structuredRecord(mount.claimTemplate);
      return claimTemplate ? { claimTemplate } : undefined;
    }
    case "volumeSource": {
      const volumeSource = structuredRecord(mount.volumeSource);
      return volumeSource ? { volumeSource } : undefined;
    }
    default:
      return undefined;
  }
}

function volumeNameSet(volumes?: unknown[]): Set<string> {
  const names = new Set<string>();
  for (const volume of volumes ?? []) {
    if (!isRecord(volume)) {
      continue;
    }
    const name = nonEmptyString(volume.name);
    if (name) {
      names.add(name);
    }
  }
  return names;
}

function volumeMountsFromResolved(value: unknown, volumeNames: Set<string>): unknown[] | undefined {
  const resolvedMounts = structuredArray(value) ?? [];
  const volumeMounts = resolvedMounts.flatMap((rawMount) => {
    if (!isRecord(rawMount)) {
      return [];
    }
    const volume = nonEmptyString(rawMount.name);
    const path = nonEmptyString(rawMount.target);
    if (!volume || !path || !volumeNames.has(volume)) {
      return [];
    }
    return [
      compactObject({
        volume,
        path,
        readOnly: rawMount.readOnly === true ? true : undefined,
        subPath: nonEmptyString(rawMount.subPath),
      }),
    ];
  });
  return nonEmptyArray(volumeMounts);
}

function rateLimitForTask(task: ManifestTask): unknown {
  const explicit = structuredValue(task.rateLimit);
  if (explicit !== undefined) {
    return explicit;
  }
  const resource = nonEmptyString(task.rate_limit_resource);
  if (!resource) {
    return undefined;
  }
  return compactObject({
    resource,
    units: positiveNumber(task.rate_limit_units),
  });
}

function structuredRecord(value: unknown): JsonRecord | undefined {
  const normalized = structuredValue(value);
  return isRecord(normalized) ? { ...normalized } : undefined;
}

function structuredArray(value: unknown): unknown[] | undefined {
  const normalized = structuredValue(value);
  return Array.isArray(normalized) && normalized.length > 0 ? normalized : undefined;
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

function stringArray(value: unknown): string[] | undefined {
  const normalized = structuredValue(value);
  if (!Array.isArray(normalized)) {
    return undefined;
  }
  const values = normalized.map((entry) => String(entry).trim()).filter(Boolean);
  return nonEmptyArray(values);
}

function nonEmptyArray<T>(value: T[]): T[] | undefined {
  return value.length > 0 ? value : undefined;
}

function nonEmptyString(value: unknown): string | undefined {
  return typeof value === "string" && value.trim() ? value : undefined;
}

function booleanValue(value: unknown): boolean | undefined {
  return typeof value === "boolean" ? value : undefined;
}

function firstDefined<T>(...values: Array<T | undefined>): T | undefined {
  return values.find((value) => value !== undefined);
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
