import { parseAllDocuments } from "yaml";

export interface JobDefRuntimeHints {
  volumeCount: number;
  volumeMountCount: number;
  volumeMountedStepCount: number;
  serviceAccountNames: string[];
  hasPodAnnotations: boolean;
  hasAutomountTokenSetting: boolean;
  parseError?: string;
}

export function getJobDefRuntimeHints(yamlSource: string): JobDefRuntimeHints {
  const hints: JobDefRuntimeHints = {
    volumeCount: 0,
    volumeMountCount: 0,
    volumeMountedStepCount: 0,
    serviceAccountNames: [],
    hasPodAnnotations: false,
    hasAutomountTokenSetting: false,
  };
  const serviceAccounts = new Set<string>();

  try {
    const docs = parseAllDocuments(yamlSource);
    for (const doc of docs) {
      if (doc.errors.length > 0) {
        hints.parseError = doc.errors[0]?.message || "Invalid YAML";
        continue;
      }

      collectDocumentHints(doc.toJS(), hints, serviceAccounts);
    }
  } catch (err) {
    hints.parseError = err instanceof Error ? err.message : "Invalid YAML";
  }

  hints.serviceAccountNames = [...serviceAccounts];
  return hints;
}

function collectDocumentHints(
  value: unknown,
  hints: JobDefRuntimeHints,
  serviceAccounts: Set<string>,
) {
  if (!isRecord(value)) return;

  const volumes = value.volumes;
  if (Array.isArray(volumes)) {
    hints.volumeCount += volumes.length;
  }

  collectIdentityHints(value.metadata, hints, serviceAccounts);

  const steps = value.steps;
  if (!Array.isArray(steps)) return;

  for (const step of steps) {
    if (!isRecord(step)) continue;
    collectIdentityHints(step, hints, serviceAccounts);

    const mounts = step.volumeMounts;
    if (Array.isArray(mounts) && mounts.length > 0) {
      hints.volumeMountCount += mounts.length;
      hints.volumeMountedStepCount += 1;
    }
  }
}

function collectIdentityHints(
  value: unknown,
  hints: JobDefRuntimeHints,
  serviceAccounts: Set<string>,
) {
  if (!isRecord(value)) return;

  if (typeof value.serviceAccountName === "string") {
    const serviceAccountName = value.serviceAccountName.trim();
    if (serviceAccountName) serviceAccounts.add(serviceAccountName);
  }

  if (isRecord(value.podAnnotations) && Object.keys(value.podAnnotations).length > 0) {
    hints.hasPodAnnotations = true;
  }

  if (typeof value.automountServiceAccountToken === "boolean") {
    hints.hasAutomountTokenSetting = true;
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return !!value && typeof value === "object" && !Array.isArray(value);
}
